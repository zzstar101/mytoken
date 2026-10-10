// Package relay detects the kind of a gateway site (中转站) and reads the
// little that gateway exposes: its per-model ratios, the balance of one key,
// and — for the gateways that keep per-request logs — the bills themselves.
//
// The package is deliberately read-only and network-shy: it only ever sends GET
// requests to the origin a user already configured, with the key that harness
// or cc-switch already stores, and it never persists that key. See
// docs/RELAY.md §2 for the privacy contract.
package relay

import (
	"context"
	"errors"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/source"
)

// Kind is a site's flavour. The official APIs only support L2 (balance); new-api
// and sub2api support all three layers.
type Kind string

const (
	KindNewAPI     Kind = "newapi"
	KindSub2API    Kind = "sub2api"
	KindOpenRouter Kind = "openrouter"
	KindDeepSeek   Kind = "deepseek"
	KindSilicon    Kind = "siliconflow"
	KindMoonshot   Kind = "moonshot"
	KindZAI        Kind = "zai"
	KindMiniMax    Kind = "minimax"
	KindUnknown    Kind = "unknown"
)

// Layer is one of the three data levels. A Site enables any combination.
type Layer uint8

const (
	// LayerRatio is L1: per-model ratios turned into dated price rules.
	LayerRatio Layer = 1 << iota
	// LayerBalance is L2: remaining and used quota, snapshotted over time.
	LayerBalance
	// LayerBills is L3: per-request logs (new-api) or daily usage (sub2api).
	LayerBills
)

// String implements fmt.Stringer for CLI flags and JSON output.
func (l Layer) String() string {
	switch l {
	case LayerRatio:
		return "ratio"
	case LayerBalance:
		return "balance"
	case LayerBills:
		return "bills"
	}
	return "off"
}

// LayerNames lists the enabled layers in a stable order.
func (l Layer) LayerNames() []string {
	var out []string
	for _, x := range []Layer{LayerRatio, LayerBalance, LayerBills} {
		if l.Has(x) {
			out = append(out, x.String())
		}
	}
	return out
}

// Has reports whether the layer is enabled.
func (l Layer) Has(x Layer) bool { return l&x != 0 }

// ParseLayer turns "ratio,balance,bills" into a Layer, 0 when nothing matches.
func ParseLayer(s string) Layer {
	var out Layer
	for _, name := range splitCommas(s) {
		switch name {
		case "ratio":
			out |= LayerRatio
		case "balance":
			out |= LayerBalance
		case "bills":
			out |= LayerBills
		}
	}
	return out
}

func splitCommas(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if part := trimSpace(s[start:i]); part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		last := s[len(s)-1]
		if last != ' ' && last != '\t' && last != '\n' && last != '\r' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// Site is one (origin, keyID) account a user chose to reconcile.
type Site struct {
	Origin, KeyID string
	Kind          Kind
	Version       string  // e.g. new-api "v1.0.0-rc.39"
	QuotaPerUnit  float64 // new-api only; 500000 by default
	Layers        Layer   // enabled layers; 0 = off
	DetectedAt    time.Time
	Providers     []string // provider names mapped to this (origin,key)
	// Routes are the Providers known only by route: the tool's config points
	// at the origin but keeps its key elsewhere, so it may use another key.
	// Filled from the current configs; not stored.
	Routes []string
}

// Ratios are the per-model multipliers a gateway reports. FixedPrice is the
// per-call price for models billed per request instead of per token.
type Ratios struct {
	Model       float64
	Completion  float64
	Cache       float64
	CacheCreate float64
	Group       float64
	FixedPrice  float64 // per-call model_price; 0 when billed by tokens
	// Expr is new-api's billing expression for models billed by one
	// (billing_mode "tiered_expr"); its coefficients are $/1M tokens and the
	// ratios above are then unused.
	Expr string
	// CacheCreate1h is the ratio for 1-hour cache writes (Claude); 0 means
	// the site did not say and they cost what 5-minute writes do.
	CacheCreate1h float64 `json:",omitempty"`
}

// Balance is a snapshot of one key's quota.
type Balance struct {
	Origin, KeyID string
	At            time.Time
	RemainingUSD  *float64
	UsedUSD       *float64
	Unlimited     bool
	// Currency is the ISO code the amounts are in when it is not USD: some
	// official APIs only quote their own (DeepSeek: CNY). MyToken does no
	// currency conversion.
	Currency string
}

// Bill is one per-request charge (new-api).
type Bill struct {
	Origin, KeyID     string
	ID                int64 // relay log id; unique per origin
	At                time.Time
	Type              string // "consume" | "refund"
	Model, Group      string
	RequestID         string
	UpstreamRequestID string
	Tokens            model.Tokens // normalized: Input excludes cache
	// CacheWrite1h is the part of Tokens.CacheWrite written with a 1-hour
	// lifetime, which Claude prices higher.
	CacheWrite1h int64
	ChargedUSD   float64
	Ratios       Ratios
	Stream       bool
	LatencyMS    int64
}

// Daily is one day × model of usage as the relay reports it (sub2api).
type Daily struct {
	Origin, KeyID string
	Day           string // YYYY-MM-DD in the requested time zone
	Model         string // "" = whole day
	Requests      int64
	Tokens        model.Tokens
	ListUSD       float64 // cost before multipliers
	ChargedUSD    float64 // actual_cost
}

// Snapshot is the L1 data a gateway exposes: per-model ratios, and for sub2api
// the account-wide multiplier and peak window.
type Snapshot struct {
	Ratios     map[string]Ratios  // by model, when L1 is available
	Groups     map[string]float64 // new-api group ratios the key may bill in
	Multiplier *float64           // sub2api effective multiplier
	Peak       *Peak              // sub2api peak window, nil when none
	At         time.Time
}

// Peak is sub2api's time-of-day surcharge window. It is carried so the shape
// exists even though 0.2 only reads the multiplier out of it.
type Peak struct {
	Start      string // "HH:MM" local
	End        string // "HH:MM" local
	Multiplier float64
}

// Client is one site's adapter. Snapshot, Bills and Daily return ErrUnsupported
// when the layer is not available for that kind.
type Client interface {
	Kind() Kind
	Balance(ctx context.Context) (Balance, error)                                       // L2
	Snapshot(ctx context.Context) (Snapshot, error)                                     // L1; ErrUnsupported allowed
	Bills(ctx context.Context, afterID int64) ([]Bill, error)                           // L3 per-request; ErrUnsupported allowed
	Daily(ctx context.Context, from, to time.Time, loc *time.Location) ([]Daily, error) // L3 daily; ErrUnsupported allowed
}

// ErrUnsupported is returned by a Client capability the site does not offer.
var ErrUnsupported = errors.New("relay: not supported by this site")

// Candidates enumerates the candidate sites offline: every provider a source
// knows about, normalized to an origin, deduplicated by (origin, keyID), with
// the cc-switch local proxy filtered out because it is not a site of its own.
// A credential without a key (a route: the config names the provider and its
// URL but keeps the key elsewhere) is not a site; its provider name is added
// to every site at that origin, because its traffic lands there too.
func Candidates(creds []source.Credential) []Site {
	seen := map[string]int{}
	var out []Site
	for _, c := range creds {
		if c.Origin == "" || c.KeyID == "" || isLocalProxy(c.Origin) {
			continue
		}
		key := c.Origin + "\x00" + c.KeyID
		i, ok := seen[key]
		if !ok {
			i = len(out)
			seen[key] = i
			out = append(out, Site{Origin: c.Origin, KeyID: c.KeyID})
		}
		if p := ScopedProvider(c.Harness, c.Provider); p != "" && !containsStr(out[i].Providers, p) {
			out[i].Providers = append(out[i].Providers, p)
		}
	}
	for _, c := range creds {
		if c.KeyID != "" || c.Provider == "" {
			continue
		}
		p := ScopedProvider(c.Harness, c.Provider)
		for i := range out {
			if out[i].Origin == c.Origin && !containsStr(out[i].Providers, p) {
				out[i].Providers = append(out[i].Providers, p)
			}
		}
	}
	return out
}

// ScopedProvider names a provider as one harness's ("pi/kami"). Provider
// names are chosen per tool: the same name can mean different sites in two
// harnesses, so a credential that knows its harness only claims that
// harness's events. Without a harness the bare name claims every harness.
func ScopedProvider(h model.Harness, provider string) string {
	if provider == "" || !h.Known() {
		return provider
	}
	return string(h) + "/" + provider
}

// SplitProvider undoes ScopedProvider; harness is "" for a bare name.
func SplitProvider(entry string) (model.Harness, string) {
	for i := 0; i < len(entry); i++ {
		if entry[i] == '/' {
			if h := model.Harness(entry[:i]); h.Known() {
				return h, entry[i+1:]
			}
			break
		}
	}
	return "", entry
}

// ProviderNames lists the distinct provider names of a site's entries, in
// order, without their harness scopes: what a person reads.
func ProviderNames(entries []string) []string {
	var out []string
	for _, e := range entries {
		if _, p := SplitProvider(e); p != "" && !containsStr(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// LocalProxyOrigin is cc-switch's own local proxy: requests to it belong to the
// provider it forwards to, not to a gateway of its own.
const LocalProxyOrigin = source.LocalProxyOrigin

func isLocalProxy(origin string) bool {
	return origin == source.LocalProxyOrigin || origin == "http://localhost:15721"
}

// Detect identifies a site by trying the steps in docs/RELAY.md §3 in order.
func Detect(ctx context.Context, h *HTTP, origin string, cred source.Credential) (Site, error) {
	return detect(ctx, h, origin, cred)
}

// New builds the client for a detected site.
func New(h *HTTP, site Site, cred source.Credential) (Client, error) {
	return newClient(h, site, cred)
}
