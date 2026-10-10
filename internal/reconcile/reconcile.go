// Package reconcile compares persisted relay charges with local usage. Reports
// never perform network I/O; only their derived match annotations are written.
package reconcile

import (
	"context"
	"database/sql"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/store"
)

type Category string

const (
	Matched        Category = "matched"
	PriceDiff      Category = "price-diff"
	TokenSemantics Category = "token-semantics"
	BillOnly       Category = "bill-only"
	EventOnly      Category = "event-only"
	Refund         Category = "refund"
)

type Line struct {
	Category                         Category
	Count                            int64
	LocalUSD, FormulaUSD, ChargedUSD float64
	// FormulaMissing counts the charges whose list cost could not be
	// computed (an unsupported ratio expression, or no list cost in daily
	// usage); FormulaUSD leaves them out.
	FormulaMissing int64
	Note           string
}
type ModelLine struct {
	Model                            string
	Count                            int64
	LocalUSD, FormulaUSD, ChargedUSD float64
}
type DayLine struct {
	Day                              time.Time
	Count                            int64
	LocalUSD, FormulaUSD, ChargedUSD float64
	// Daily-only decomposition. Nil means catalog prices or the relay's original
	// list cost were unavailable; no inferred estimate is presented as exact.
	MultiplierDiffUSD, UsageDiffUSD *float64
	Unpriced                        bool
}
type Report struct {
	Origin, KeyID        string
	From, To             time.Time
	Coverage             time.Time
	LocalUSD, ChargedUSD float64
	ImpliedMultiplier    *float64
	Lines                []Line
	ByModel              []ModelLine
	ByDay                []DayLine
	Balance              *relay.Balance
}
type dimension struct {
	harness, model, provider string
	price                    pricing.Price
	priced                   bool
}
type event struct {
	key, request string
	dim          *dimension
	at           int64
	tokens       model.Tokens
	cost         float64
	priced       bool
	used         bool
	count        int64
}
type alias struct{ From, To, Provider string }

var dateSuffix = regexp.MustCompile(`[-:]\d{4}-?\d{2}-?\d{2}$`)

type readMode int

const (
	fullMatch readMode = iota
	pendingMatch
	storedMatch
)

func normalize(name, provider string, aliases []alias) string {
	for _, a := range aliases {
		if a.From == name && a.Provider == provider {
			name = a.To
			goto done
		}
	}
	for _, a := range aliases {
		if a.From == name && a.Provider == "" {
			name = a.To
			break
		}
	}
done:
	return strings.ToLower(dateSuffix.ReplaceAllString(strings.TrimSpace(name), ""))
}
func catalogCost(d *dimension, t model.Tokens) float64 {
	p := d.price
	r := p.Output
	if p.Reasoning != nil {
		r = *p.Reasoning
	}
	return (float64(t.Input)*p.Input + float64(t.Output)*p.Output + float64(t.CacheRead)*p.CacheRead + float64(t.CacheWrite)*p.CacheWrite + float64(t.Reasoning)*r) / 1e6
}
func formula(b relay.Bill) (float64, bool) {
	v, ok := b.Formula()
	if b.Type == "refund" {
		return -math.Abs(v), ok
	}
	return v, ok
}
func closeMoney(a, b float64) bool { return math.Abs(a-b) <= math.Max(.0005, math.Abs(b)*.02) }
func floorDay(at time.Time) time.Time {
	at = at.In(time.Local)
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.Local)
}

type tokenKey struct {
	model         string
	output, input int64
}

// nearest resolves ties by event time then stable event key. used events are
// skipped, so one local request can never explain two consumption charges.
func nearest(events []event, ids []int, at int64, window int64) int {
	p := sort.Search(len(ids), func(i int) bool { return events[ids[i]].at >= at })
	best := -1
	dist := int64(math.MaxInt64)
	for l, r := p-1, p; l >= 0 || r < len(ids); {
		ld, rd := int64(math.MaxInt64), int64(math.MaxInt64)
		if l >= 0 {
			ld = at - events[ids[l]].at
		}
		if r < len(ids) {
			rd = events[ids[r]].at - at
		}
		var i int
		if ld <= rd {
			i = ids[l]
			l--
		} else {
			i = ids[r]
			r++
		}
		d := ld
		if rd < ld {
			d = rd
		}
		if d > dist || d > window {
			break
		}
		if !events[i].used && (d < dist || best < 0 || events[i].key < events[best].key) {
			best = i
			dist = d
		}
	}
	return best
}

type reportBuilder struct {
	r                              Report
	lines                          map[Category]*Line
	models                         map[string]*ModelLine
	days                           map[int64]*DayLine
	catalogMatched, chargedMatched float64
	allMatchedPriced               bool
}

func newBuilder(site relay.Site, from, to time.Time) *reportBuilder {
	return &reportBuilder{r: Report{Origin: site.Origin, KeyID: site.KeyID, From: from, To: to, Lines: []Line{}, ByModel: []ModelLine{}, ByDay: []DayLine{}}, lines: map[Category]*Line{}, models: map[string]*ModelLine{}, days: map[int64]*DayLine{}, allMatchedPriced: true}
}
func (b *reportBuilder) add(cat Category, name string, at time.Time, count int64, local, form, charged float64) {
	l := b.lines[cat]
	if l == nil {
		l = &Line{Category: cat}
		b.lines[cat] = l
	}
	l.Count += count
	l.LocalUSD += local
	l.FormulaUSD += form
	l.ChargedUSD += charged
	m := b.models[name]
	if m == nil {
		m = &ModelLine{Model: name}
		b.models[name] = m
	}
	m.Count += count
	m.LocalUSD += local
	m.FormulaUSD += form
	m.ChargedUSD += charged
	day := floorDay(at)
	d := b.days[day.Unix()]
	if d == nil {
		d = &DayLine{Day: day}
		b.days[day.Unix()] = d
	}
	d.Count += count
	d.LocalUSD += local
	d.FormulaUSD += form
	d.ChargedUSD += charged
	b.r.LocalUSD += local
	b.r.ChargedUSD += charged
}
func (b *reportBuilder) finish() Report {
	for _, c := range []Category{Matched, PriceDiff, TokenSemantics, BillOnly, EventOnly, Refund} {
		if l := b.lines[c]; l != nil {
			if c == EventOnly {
				l.Note = "may belong to another key or tool, or be free"
			}
			if c == BillOnly {
				l.Note = "may be a shared key, another device, or an untracked request"
			}
			b.r.Lines = append(b.r.Lines, *l)
		}
	}
	for _, m := range b.models {
		b.r.ByModel = append(b.r.ByModel, *m)
	}
	sort.Slice(b.r.ByModel, func(i, j int) bool { return b.r.ByModel[i].Model < b.r.ByModel[j].Model })
	for _, d := range b.days {
		b.r.ByDay = append(b.r.ByDay, *d)
	}
	sort.Slice(b.r.ByDay, func(i, j int) bool { return b.r.ByDay[i].Day.Before(b.r.ByDay[j].Day) })
	if b.allMatchedPriced && b.catalogMatched > 0 {
		v := b.chargedMatched / b.catalogMatched
		b.r.ImpliedMultiplier = &v
	}
	return b.r
}

// Reconcile rebuilds all assignments. Run this cold path in the background.
func Reconcile(ctx context.Context, st *store.Store, site relay.Site, from, to time.Time) (Report, error) {
	return reconcile(ctx, st, site, from, to, fullMatch)
}

// StoredReport reads persisted assignments and current local aggregates only.
func StoredReport(ctx context.Context, st *store.Store, site relay.Site, from, to time.Time) (Report, error) {
	return reconcile(ctx, st, site, from, to, storedMatch)
}

// ReconcilePending assigns new/bill-only rows without stealing existing matches.
func ReconcilePending(ctx context.Context, st *store.Store, site relay.Site) error {
	_, err := reconcile(ctx, st, site, time.Time{}, time.Time{}, pendingMatch)
	return err
}
func reconcile(ctx context.Context, st *store.Store, site relay.Site, from, to time.Time, mode readMode) (Report, error) {
	b := newBuilder(site, from, to)
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		return b.finish(), nil
	}
	balance, err := st.LatestRelayBalance(ctx, site.Origin, site.KeyID)
	if err != nil {
		return Report{}, err
	}
	b.r.Balance = balance
	var bills []relay.Bill
	if mode == pendingMatch {
		bills, err = st.PendingRelayBills(ctx, site.Origin, site.KeyID)
	} else {
		bills, err = st.RelayBills(ctx, site.Origin, site.KeyID, from, to)
	}
	if err != nil {
		return Report{}, err
	}
	if mode == pendingMatch && (len(bills) == 0 || site.Kind == relay.KindSub2API) {
		return b.finish(), nil
	}
	var persisted map[int64]store.RelayMatch
	if mode != fullMatch {
		persisted, err = st.RelayMatches(ctx, site.Origin, site.KeyID)
		if err != nil {
			return Report{}, err
		}
	}
	// Local requests from before the oldest charge the site has given us
	// cannot be checked: new-api only serves a key's latest 1000 log rows,
	// sub2api a bounded number of days. They are left out instead of being
	// reported as charges the site never made.
	evFrom, evTo := from, to
	covered := false
	if site.Kind == relay.KindSub2API {
		var day sql.NullString
		if err = st.DB().QueryRowContext(ctx, "SELECT min(day) FROM relay_daily WHERE origin=? AND key_id=?", site.Origin, site.KeyID).Scan(&day); err != nil {
			return Report{}, err
		}
		if day.Valid {
			covered = true
			if at, err := time.ParseInLocation("2006-01-02", day.String, time.Local); err == nil {
				b.r.Coverage = at.UTC()
				if at.After(evFrom) {
					evFrom = at
				}
			}
		}
	} else {
		var coverage sql.NullInt64
		if err = st.DB().QueryRowContext(ctx, "SELECT min(at) FROM relay_bills WHERE origin=? AND key_id=?", site.Origin, site.KeyID).Scan(&coverage); err != nil {
			return Report{}, err
		}
		if coverage.Valid {
			covered = true
			b.r.Coverage = time.Unix(0, coverage.Int64).UTC()
			// the matching window reaches 120 s before a charge
			if at := b.r.Coverage.Add(-120 * time.Second); at.After(evFrom) {
				evFrom = at
			}
		}
	}
	if !to.IsZero() && !evFrom.Before(to) {
		evFrom = to
	}
	if !covered {
		// Nothing charged yet (the key was rejected, the site keeps no
		// usage, or it was never synced): there is no window to check.
		evFrom, evTo = time.Unix(1, 0), time.Unix(1, 0)
	}
	events, totals, aliases, err := loadEvents(ctx, st, site, evFrom, evTo, bills, mode, persisted)
	if err != nil {
		return Report{}, err
	}
	if site.Kind == relay.KindSub2API {
		return dailyReport(ctx, st, site, b, totals, aliases)
	}
	provider := ""
	if names := relay.ProviderNames(site.Providers); len(names) > 0 {
		provider = names[0]
	}
	matched := make([]int, len(bills))
	modes := make([]int, len(bills))
	for i := range matched {
		matched[i] = -1
	}
	if mode == pendingMatch {
		reserved := map[[2]string]bool{}
		for _, m := range persisted {
			if m.DedupKey != "" {
				reserved[[2]string{m.Harness, m.DedupKey}] = true
			}
		}
		for i := range events {
			events[i].used = reserved[[2]string{events[i].dim.harness, events[i].key}]
		}
	}
	if mode != storedMatch {
		var keys [3]map[tokenKey][]int
		for i := range keys {
			keys[i] = map[tokenKey][]int{}
		}
		requests := map[string][]int{}
		sort.Slice(events, func(i, j int) bool {
			if events[i].at == events[j].at {
				return events[i].key < events[j].key
			}
			return events[i].at < events[j].at
		})
		for i, e := range events {
			if e.request != "" {
				requests[e.request] = append(requests[e.request], i)
			}
			inputs := [3]int64{e.tokens.Input, e.tokens.Input + e.tokens.CacheRead, e.tokens.Input + e.tokens.CacheRead + e.tokens.CacheWrite}
			for mode, input := range inputs {
				k := tokenKey{e.dim.model, e.tokens.Output, input}
				keys[mode][k] = append(keys[mode][k], i)
			}
		}
		// Reserve exact-ID matches before a weaker token match can take their event.
		for i, bill := range bills {
			if bill.Type == "refund" {
				continue
			}
			for _, id := range []string{bill.RequestID, bill.UpstreamRequestID} {
				if id == "" {
					continue
				}
				j := nearest(events, requests[id], bill.At.UnixNano(), math.MaxInt64)
				if j >= 0 {
					matched[i] = j
					e := events[j]
					if bill.Tokens.Input != e.tokens.Input && bill.Tokens.Output == e.tokens.Output {
						if bill.Tokens.Input == e.tokens.Input+e.tokens.CacheRead {
							modes[i] = 1
						} else if bill.Tokens.Input == e.tokens.Input+e.tokens.CacheRead+e.tokens.CacheWrite {
							modes[i] = 2
						}
					}
					events[j].used = true
					break
				}
			}
		}
		for i, bill := range bills {
			if bill.Type == "refund" || matched[i] >= 0 {
				continue
			}
			names := map[string]bool{normalize(bill.Model, "", aliases): true}
			for _, p := range relay.ProviderNames(site.Providers) {
				names[normalize(bill.Model, p, aliases)] = true
			}
			best := -1
			distance := int64(math.MaxInt64)
			bestMode := 0
			for mode := 0; mode < 3; mode++ {
				for name := range names {
					j := nearest(events, keys[mode][tokenKey{name, bill.Tokens.Output, bill.Tokens.Input}], bill.At.UnixNano(), int64(120*time.Second))
					if j < 0 {
						continue
					}
					delta := events[j].at - bill.At.UnixNano()
					if delta < 0 {
						delta = -delta
					}
					if best < 0 || delta < distance || (delta == distance && mode == bestMode && events[j].key < events[best].key) {
						best = j
						distance = delta
						bestMode = mode
					}
				}
			}
			if best >= 0 {
				matched[i] = best
				modes[i] = bestMode
				events[best].used = true
			}
		}
	} else {
		byKey := map[[2]string]int{}
		for i, e := range events {
			byKey[[2]string{e.dim.harness, e.key}] = i
		}
		for i, bill := range bills {
			m := persisted[bill.ID]
			if m.DedupKey == "" || bill.Type == "refund" {
				continue
			}
			if j, ok := byKey[[2]string{m.Harness, m.DedupKey}]; ok && !events[j].used {
				matched[i] = j
				events[j].used = true
				if m.Kind == string(TokenSemantics) {
					modes[i] = 1
				}
			}
		}
	}
	matches := make([]store.RelayMatch, 0, len(bills))
	for i, bill := range bills {
		cat := BillOnly
		local := 0.0
		name := normalize(bill.Model, provider, aliases)
		m := store.RelayMatch{ID: bill.ID}
		if bill.Type == "refund" {
			cat = Refund
		} else if j := matched[i]; j >= 0 {
			e := events[j]
			local = e.cost
			name = e.dim.model
			m.Harness = e.dim.harness
			m.DedupKey = e.key
			cat = Matched
			if modes[i] > 0 {
				cat = TokenSemantics
			} else if !closeMoney(bill.ChargedUSD, local) {
				cat = PriceDiff
			}
			if e.dim.priced {
				b.catalogMatched += catalogCost(e.dim, e.tokens)
			} else {
				b.allMatchedPriced = false
			}
			b.chargedMatched += bill.ChargedUSD
		}
		m.Kind = string(cat)
		matches = append(matches, m)
		form, ok := formula(bill)
		b.add(cat, name, bill.At, 1, local, form, bill.ChargedUSD)
		if !ok {
			b.lines[cat].FormulaMissing++
		}
	}
	type totalKey struct {
		dim *dimension
		at  int64
	}
	remaining := map[totalKey]*event{}
	for i := range totals {
		e := &totals[i]
		k := totalKey{e.dim, e.at}
		if prev := remaining[k]; prev != nil {
			prev.count += e.count
			prev.cost += e.cost
			e.count = 0
			e.cost = 0
		} else {
			remaining[k] = e
		}
	}
	for _, e := range events {
		if e.used {
			day := floorDay(time.Unix(0, e.at)).UnixNano()
			if t := remaining[totalKey{e.dim, day}]; t != nil {
				t.count--
				t.cost -= e.cost
			}
		}
	}
	for _, e := range totals {
		if e.count > 0 {
			b.add(EventOnly, e.dim.model, time.Unix(0, e.at), e.count, e.cost, 0, 0)
		}
	}
	if mode != storedMatch {
		if err = st.SaveRelayMatches(ctx, site.Origin, site.KeyID, matches); err != nil {
			return Report{}, err
		}
	}
	return b.finish(), nil
}

func dailyReport(ctx context.Context, st *store.Store, site relay.Site, b *reportBuilder, events []event, aliases []alias) (Report, error) {
	from, to := "", ""
	if !b.r.From.IsZero() {
		from = floorDay(b.r.From).Format("2006-01-02")
	}
	if !b.r.To.IsZero() {
		to = floorDay(b.r.To).Format("2006-01-02")
		if !b.r.To.Equal(floorDay(b.r.To)) {
			to = floorDay(b.r.To).AddDate(0, 0, 1).Format("2006-01-02")
		}
	}
	days, err := st.RelayDaily(ctx, site.Origin, site.KeyID, from, to)
	if err != nil {
		return Report{}, err
	}
	type key struct{ day, model string }
	type total struct {
		local, catalog float64
		count          int64
		unknown        bool
	}
	totals := map[key]total{}
	for _, e := range events {
		day := floorDay(time.Unix(0, e.at)).Format("2006-01-02")
		for _, name := range []string{e.dim.model, ""} {
			k := key{day, name}
			t := totals[k]
			t.local += e.cost
			t.catalog += catalogCost(e.dim, e.tokens)
			t.count += e.count
			t.unknown = t.unknown || !e.dim.priced
			totals[k] = t
		}
	}
	modelDays := map[string]bool{}
	for _, d := range days {
		if d.Model != "" {
			modelDays[d.Day] = true
		}
	}
	provider := ""
	if names := relay.ProviderNames(site.Providers); len(names) > 0 {
		provider = names[0]
	}
	usedModels := map[key]bool{}
	usedDays := map[string]bool{}
	var coverage sql.NullString
	if err = st.DB().QueryRowContext(ctx, "SELECT min(day) FROM relay_daily WHERE origin=? AND key_id=?", site.Origin, site.KeyID).Scan(&coverage); err != nil {
		return Report{}, err
	}
	if coverage.Valid {
		b.r.Coverage, _ = time.ParseInLocation("2006-01-02", coverage.String, time.Local)
	}
	for _, d := range days {
		if d.Model == "" && modelDays[d.Day] {
			continue
		}
		at, err := time.ParseInLocation("2006-01-02", d.Day, time.Local)
		if err != nil {
			return Report{}, err
		}
		if b.r.Coverage.IsZero() || at.Before(b.r.Coverage) {
			b.r.Coverage = at
		}
		name := normalize(d.Model, provider, aliases)
		t := totals[key{d.Day, name}]
		if name == "" {
			usedDays[d.Day] = true
		} else {
			usedModels[key{d.Day, name}] = true
		}
		if d.Requests == 0 && d.ChargedUSD == 0 && d.ListUSD == 0 && t.count == 0 {
			continue // a zero-filled day: nothing on either side
		}
		addDaily(b, name, at, d, t.count, t.local)
		line := b.days[at.Unix()]
		if t.unknown || d.ListUSD <= 0 {
			line.Unpriced = true
			line.MultiplierDiffUSD = nil
			line.UsageDiffUSD = nil
			b.allMatchedPriced = false
		} else if !line.Unpriced {
			m := d.ChargedUSD / d.ListUSD
			md := t.catalog*m - t.local
			ud := d.ChargedUSD - t.catalog*m
			if line.MultiplierDiffUSD == nil {
				line.MultiplierDiffUSD = new(float64)
				line.UsageDiffUSD = new(float64)
			}
			*line.MultiplierDiffUSD += md
			*line.UsageDiffUSD += ud
		}
		b.catalogMatched += t.catalog
		b.chargedMatched += d.ChargedUSD
	}
	for k, t := range totals {
		if k.model == "" || usedDays[k.day] || usedModels[k] {
			continue
		}
		at, err := time.ParseInLocation("2006-01-02", k.day, time.Local)
		if err != nil {
			return Report{}, err
		}
		b.add(EventOnly, k.model, at, t.count, t.local, 0, 0)
		if t.unknown {
			line := b.days[at.Unix()]
			line.Unpriced = true
			line.MultiplierDiffUSD = nil
			line.UsageDiffUSD = nil
		}
	}
	return b.finish(), nil
}

// addDaily splits one day (or day × model) of daily usage into categories.
// Daily totals carry no request identity, so when the request counts
// disagree the surplus is split off pro rata: the site's extra requests are
// bill-only, extra local ones event-only, and the rest is compared as money.
func addDaily(b *reportBuilder, name string, at time.Time, d relay.Daily, count int64, local float64) {
	site := d.Requests
	switch {
	case count == 0:
		b.add(BillOnly, name, at, site, 0, d.ListUSD, d.ChargedUSD)
		b.noList(BillOnly, d, site)
		return
	case site == 0 && d.ChargedUSD == 0:
		b.add(EventOnly, name, at, count, local, 0, 0)
		return
	}
	list, charged := d.ListUSD, d.ChargedUSD
	if site > 0 && float64(site) > float64(count)*1.1+1 {
		f := float64(site-count) / float64(site)
		b.add(BillOnly, name, at, site-count, 0, list*f, charged*f)
		b.noList(BillOnly, d, site-count)
		list, charged, site = list*(1-f), charged*(1-f), count
	} else if site > 0 && float64(count) > float64(site)*1.1+1 {
		f := float64(count-site) / float64(count)
		b.add(EventOnly, name, at, count-site, local*f, 0, 0)
		local, count = local*(1-f), site
	}
	cat := Matched
	if !closeMoney(charged, local) {
		cat = PriceDiff
	}
	b.add(cat, name, at, site, local, list, charged)
	b.noList(cat, d, site)
}

// noList marks daily requests that came without the site's list cost.
func (b *reportBuilder) noList(cat Category, d relay.Daily, n int64) {
	if d.ListUSD <= 0 && n > 0 {
		b.lines[cat].FormulaMissing += n
	}
}
