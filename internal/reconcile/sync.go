package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/source"
	"github.com/zzstar101/mytoken/internal/store"
)

type RulesAppender interface {
	AppendRelayRules(context.Context, string, []pricing.Rule) error
}
type ClientFactory func(context.Context, relay.Site, source.Credential) (relay.Client, error)
type Credentials func(context.Context) ([]source.Credential, error)
type retryState struct {
	next     time.Time
	failures int
}

// Syncer keeps no credentials between calls. A single operation is serialized
// per instance so manual sync and the background loop cannot race a cursor.
type Syncer struct {
	st      *store.Store
	creds   Credentials
	factory ClientFactory
	rules   RulesAppender
	now     func() time.Time
	mu      sync.Mutex
	retry   map[[2]string]retryState
}

func NewSyncer(st *store.Store, creds Credentials, factory ClientFactory, rules RulesAppender) *Syncer {
	return &Syncer{st: st, creds: creds, factory: factory, rules: rules, now: time.Now, retry: map[[2]string]retryState{}}
}

func (s *Syncer) Sync(ctx context.Context, origin, keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.sync(ctx, origin, keyID)
	key := [2]string{origin, keyID}
	state := s.retry[key]
	delay := 5 * time.Minute
	if err != nil {
		state.failures++
		for i := 1; i < state.failures && delay < 30*time.Minute; i++ {
			delay *= 2
		}
		if delay > 30*time.Minute {
			delay = 30 * time.Minute
		}
	} else {
		state.failures = 0
	}
	state.next = s.now().Add(delay)
	s.retry[key] = state
	return err
}
func (s *Syncer) sync(ctx context.Context, origin, keyID string) error {
	site, err := s.st.RelaySite(ctx, origin, keyID)
	if err != nil {
		return errors.New("reconcile: site is not configured")
	}
	if site.Layers == 0 {
		return errors.New("reconcile: site is disabled")
	}
	cursor, err := s.st.RelayCursor(ctx, origin, keyID)
	if err != nil {
		return errors.New("reconcile: could not read sync cursor")
	}
	// Raw transport/source/SQL errors are deliberately never surfaced. They may
	// contain a server-reflected credential, URL query, or SQL parameter value.
	fail := func(stage string) error {
		e := fmt.Errorf("reconcile: sync failed during %s", stage)
		cursor.LastSync = s.now()
		cursor.LastError = e.Error()
		_ = s.st.PutRelayCursor(ctx, cursor)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return e
	}
	if s.creds == nil || s.factory == nil {
		return fail("credential lookup")
	}
	creds, err := s.creds(ctx)
	if err != nil {
		return fail("credential lookup")
	}
	var cred source.Credential
	found := false
	for _, c := range creds {
		if c.Origin == origin && c.KeyID == keyID {
			cred = c
			found = true
			break
		}
	}
	if !found || cred.Secret.Reveal() == "" {
		return fail("credential lookup")
	}
	client, err := s.factory(ctx, site, cred)
	if err != nil {
		return fail("client setup")
	}
	secret := cred.Secret.Reveal()
	encodedSecret, _ := json.Marshal(secret)
	secretNeedle := string(encodedSecret[1 : len(encodedSecret)-1])
	safe := func(v any) bool {
		raw, err := json.Marshal(v)
		return err == nil && !strings.Contains(string(raw), secretNeedle)
	}
	var rules []pricing.Rule
	if site.Layers.Has(relay.LayerRatio) {
		snap, err := client.Snapshot(ctx)
		if err != nil && !errors.Is(err, relay.ErrUnsupported) {
			return fail("ratio fetch")
		}
		if err == nil {
			if !safe(snap) {
				return fail("response validation")
			}
			if snap.At.IsZero() {
				snap.At = s.now()
			}
			rules = append(rules, relay.RulesFromSnapshot(site, snap)...)
		}
	}
	if site.Layers.Has(relay.LayerBalance) {
		balance, err := client.Balance(ctx)
		if err != nil && !errors.Is(err, relay.ErrUnsupported) {
			return fail("balance fetch")
		}
		if err == nil {
			balance.Origin = origin
			balance.KeyID = keyID
			if balance.At.IsZero() {
				balance.At = s.now()
			}
			if !safe(balance) {
				return fail("response validation")
			}
			if err = s.st.PutRelayBalance(ctx, balance); err != nil {
				return fail("balance persistence")
			}
		}
	}
	maxID := cursor.LastBillID
	if site.Layers.Has(relay.LayerBills) {
		bills, err := client.Bills(ctx, cursor.LastBillID)
		if err != nil && !errors.Is(err, relay.ErrUnsupported) {
			return fail("bill fetch")
		}
		if err == nil {
			for i := range bills {
				bills[i].Origin = origin
				bills[i].KeyID = keyID
				if bills[i].ID > maxID {
					maxID = bills[i].ID
				}
			}
			if !safe(bills) {
				return fail("response validation")
			}
			if err = s.st.PutRelayBills(ctx, bills); err != nil {
				return fail("bill persistence")
			}
			if site.Layers.Has(relay.LayerRatio) {
				rules = append(rules, relay.RulesFromBills(site, bills)...)
			}
		} else {
			now := s.now()
			days, e := client.Daily(ctx, floorDay(now).AddDate(0, 0, -30), floorDay(now).AddDate(0, 0, 1), time.Local)
			if e != nil && !errors.Is(e, relay.ErrUnsupported) {
				return fail("daily fetch")
			}
			if e == nil {
				for i := range days {
					days[i].Origin = origin
					days[i].KeyID = keyID
				}
				if !safe(days) {
					return fail("response validation")
				}
				if e = s.st.PutRelayDaily(ctx, days); e != nil {
					return fail("daily persistence")
				}
			}
		}
	}
	if len(rules) > 0 {
		if s.rules == nil {
			return fail("price-rule update")
		}
		if err = s.rules.AppendRelayRules(ctx, origin, rules); err != nil {
			return fail("price-rule update")
		}
	}
	if site.Layers.Has(relay.LayerBills) {
		if err = s.reconcile(ctx, site); err != nil {
			return fail("reconciliation")
		}
	}
	cursor.LastBillID = maxID
	cursor.LastSync = s.now()
	cursor.LastError = ""
	if err = s.st.PutRelayCursor(ctx, cursor); err != nil {
		return fail("cursor persistence")
	}
	return nil
}

// Run checks once immediately, then every minute, respecting per-account
// five-minute cadence and exponential failures capped at thirty minutes.
func (s *Syncer) Run(ctx context.Context) {
	s.runDue(ctx)
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.runDue(ctx)
		}
	}
}
func (s *Syncer) runDue(ctx context.Context) {
	sites, err := s.st.RelaySites(ctx)
	if err != nil {
		return
	}
	for _, site := range sites {
		if ctx.Err() != nil {
			return
		}
		if site.Layers == 0 {
			continue
		}
		key := [2]string{site.Origin, site.KeyID}
		s.mu.Lock()
		r, ok := s.retry[key]
		s.mu.Unlock()
		if !ok {
			cur, err := s.st.RelayCursor(ctx, site.Origin, site.KeyID)
			if err != nil {
				continue
			}
			delay := 5 * time.Minute
			if cur.LastError != "" {
				delay = 30 * time.Minute
			}
			if !cur.LastSync.IsZero() {
				r.next = cur.LastSync.Add(delay)
			}
		}
		if !s.now().Before(r.next) {
			_ = s.Sync(ctx, site.Origin, site.KeyID)
		}
	}
}

// SiteStatus contains only safe offline metadata, never a credential.
type SiteStatus struct {
	relay.Site
	Providers []string
	HasKey    bool
	Balance   *relay.Balance
	LastSync  time.Time
	LastError string
}

// Service is the common GUI/CLI facade. Constructing it performs no network I/O.
type Service struct {
	st     *store.Store
	creds  Credentials
	http   *relay.HTTP
	syncer *Syncer
}

func NewService(st *store.Store, creds func(context.Context) ([]source.Credential, error), h *relay.HTTP, rules RulesAppender) *Service {
	if h == nil {
		h = relay.NewHTTP(nil)
	}
	s := &Service{st: st, creds: creds, http: h}
	s.syncer = NewSyncer(st, creds, func(_ context.Context, site relay.Site, cred source.Credential) (relay.Client, error) {
		return relay.New(h, site, cred)
	}, rules)
	return s
}
func (s *Service) Sites(ctx context.Context) ([]SiteStatus, error) {
	var creds []source.Credential
	var err error
	if s.creds != nil {
		creds, err = s.creds(ctx)
		if err != nil {
			return nil, errors.New("reconcile: credential lookup failed")
		}
	}
	persisted, err := s.st.RelaySites(ctx)
	if err != nil {
		return nil, errors.New("reconcile: could not read sites")
	}
	sites := map[[2]string]relay.Site{}
	has := map[[2]string]bool{}
	for _, site := range relay.Candidates(creds) {
		key := [2]string{site.Origin, site.KeyID}
		sites[key] = site
		has[key] = true
	}
	for _, site := range persisted {
		key := [2]string{site.Origin, site.KeyID}
		p := map[string]bool{}
		for _, v := range sites[key].Providers {
			p[v] = true
		}
		for _, v := range site.Providers {
			p[v] = true
		}
		site.Providers = nil
		for v := range p {
			site.Providers = append(site.Providers, v)
		}
		sort.Strings(site.Providers)
		sites[key] = site
	}
	out := make([]SiteStatus, 0, len(sites))
	for key, site := range sites {
		v := SiteStatus{Site: site, Providers: site.Providers, HasKey: has[key]}
		v.Balance, err = s.st.LatestRelayBalance(ctx, site.Origin, site.KeyID)
		if err != nil {
			return nil, errors.New("reconcile: could not read balance")
		}
		cur, err := s.st.RelayCursor(ctx, site.Origin, site.KeyID)
		if err != nil {
			return nil, errors.New("reconcile: could not read sync cursor")
		}
		v.LastSync = cur.LastSync
		v.LastError = cur.LastError
		for _, c := range creds {
			if secret := c.Secret.Reveal(); secret != "" {
				v.LastError = strings.ReplaceAll(v.LastError, secret, "[redacted]")
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Origin == out[j].Origin {
			return out[i].KeyID < out[j].KeyID
		}
		return out[i].Origin < out[j].Origin
	})
	return out, nil
}
func (s *Service) Enable(ctx context.Context, origin, keyID string, layers relay.Layer) (SiteStatus, error) {
	if layers == 0 || layers&^(relay.LayerRatio|relay.LayerBalance|relay.LayerBills) != 0 {
		return SiteStatus{}, errors.New("reconcile: select at least one valid layer")
	}
	if s.creds == nil {
		return SiteStatus{}, errors.New("reconcile: credential lookup failed")
	}
	creds, err := s.creds(ctx)
	if err != nil {
		return SiteStatus{}, errors.New("reconcile: credential lookup failed")
	}
	var cred source.Credential
	found := false
	for _, c := range creds {
		if c.Origin == origin && c.KeyID == keyID {
			cred = c
			found = true
			break
		}
	}
	if !found {
		return SiteStatus{}, errors.New("reconcile: credential unavailable")
	}
	site, err := s.st.RelaySite(ctx, origin, keyID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return SiteStatus{}, errors.New("reconcile: could not read site")
	}
	if errors.Is(err, store.ErrNotFound) || site.Kind == "" || site.Kind == relay.KindUnknown {
		site, err = relay.Detect(ctx, s.http, origin, cred)
		if err != nil {
			return SiteStatus{}, errors.New("reconcile: site detection failed")
		}
		// Nothing to sync from a site we cannot read: leave it off rather
		// than store an enabled site that fails every five minutes.
		if site.Kind == "" || site.Kind == relay.KindUnknown {
			return SiteStatus{}, errors.New("reconcile: not a supported relay (new-api, sub2api or a known official API)")
		}
	}
	site.Origin = origin
	site.KeyID = keyID
	site.Layers = layers
	for _, candidate := range relay.Candidates(creds) {
		if candidate.Origin == origin && candidate.KeyID == keyID {
			site.Providers = candidate.Providers
		}
	}
	if err = s.st.PutRelaySite(ctx, site); err != nil {
		return SiteStatus{}, errors.New("reconcile: could not persist site")
	}
	syncErr := s.Sync(ctx, origin, keyID)
	sites, err := s.Sites(ctx)
	if err != nil {
		return SiteStatus{}, err
	}
	for _, v := range sites {
		if v.Origin == origin && v.KeyID == keyID {
			return v, syncErr
		}
	}
	return SiteStatus{}, syncErr
}
func (s *Service) Disable(ctx context.Context, origin, keyID string) error {
	site, err := s.st.RelaySite(ctx, origin, keyID)
	if err != nil {
		return errors.New("reconcile: site is not configured")
	}
	site.Layers = 0
	if err = s.st.PutRelaySite(ctx, site); err != nil {
		return errors.New("reconcile: could not persist site")
	}
	return nil
}
func (s *Service) Sync(ctx context.Context, origin, keyID string) error {
	return s.syncer.Sync(ctx, origin, keyID)
}
func (s *Service) Run(ctx context.Context) { s.syncer.Run(ctx) }
func (s *Service) Report(ctx context.Context, origin, keyID string, from, to time.Time) (Report, error) {
	site, err := s.st.RelaySite(ctx, origin, keyID)
	if err != nil {
		return Report{}, errors.New("reconcile: site is not configured")
	}
	return StoredReport(ctx, s.st, site, from, to)
}
