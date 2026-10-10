package relay

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/pricing"
)

// RuleSourcePrefix marks rules that came from a gateway, so they can be told
// apart from user rules and cc-switch imports.
const RuleSourcePrefix = "relay:"

// RuleSource is the Source tag rules from one key of a gateway carry. Keys of
// the same site can bill in different groups at different prices, so each
// key's rules are tracked on their own ("relay:https://x.example#1a2b3c").
func RuleSource(origin, keyID string) string {
	if keyID == "" {
		return RuleSourcePrefix + origin
	}
	return RuleSourcePrefix + origin + "#" + keyID
}

// ratioChangeThreshold is the relative change below which a re-derived ratio is
// treated as the same rule: a site that jitters by a fraction of a percent must
// not grow the rule table (docs/RELAY.md §5.5).
const ratioChangeThreshold = 0.01

// Relay rules state what the site charges, all multipliers included, so a
// unit-price rule carries Multiplier 1: a provider multiplier the user set by
// hand must not apply a second time on top of the site's own prices.
var one = 1.0

// RulesFromSnapshot turns a gateway's snapshot into price rules for every
// provider mapped to the site:
//
//   - sub2api: the key's effective multiplier, as a provider-wide rule on top
//     of the catalog prices sub2api itself bills from;
//   - new-api: per-model unit prices from /api/pricing, times the ratio of
//     the group the key bills in. group is that group's name when known
//     (from the key's bills); without it the snapshot is used only when every
//     group costs the same. Per-call models, and expression-billed models
//     whose price depends on the time of day, get no rule.
func RulesFromSnapshot(site Site, snap Snapshot, group string) []pricing.Rule {
	if len(site.Providers) == 0 {
		return nil
	}
	at := snap.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var out []pricing.Rule
	if snap.Multiplier != nil && *snap.Multiplier > 0 {
		m := *snap.Multiplier
		for _, provider := range site.Providers {
			out = append(out, pricing.Rule{Provider: provider, Multiplier: m, From: at, Source: RuleSource(site.Origin, site.KeyID)})
		}
	}
	if len(snap.Ratios) == 0 {
		return out
	}
	g, ok := groupRatio(snap.Groups, group)
	if !ok {
		return out
	}
	models := make([]string, 0, len(snap.Ratios))
	for m := range snap.Ratios {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, model := range models {
		r := snap.Ratios[model]
		if model == "" {
			continue
		}
		if r.Group == 0 {
			r.Group = g
		}
		p, ok := unitPrices(r)
		if !ok {
			continue
		}
		out = append(out, priceRules(site, model, at, p)...)
	}
	return dedupe(out)
}

// groupRatio picks the ratio of the key's group: the named group when the
// site lists it, else the one ratio every group shares. A snapshot without a
// group table has nothing to scale by (ratio 1).
func groupRatio(groups map[string]float64, name string) (float64, bool) {
	if len(groups) == 0 {
		return 1, true
	}
	if v, ok := groups[name]; ok && name != "" {
		return v, v > 0
	}
	var first float64
	for _, v := range groups {
		if first == 0 {
			first = v
		}
		if v != first {
			return 0, false
		}
	}
	return first, first > 0
}

// KeyGroup is the group the newest consume bill was charged in, or "".
func KeyGroup(bills []Bill) string {
	var newest Bill
	for _, b := range bills {
		if b.Type == "consume" && b.Group != "" && b.At.After(newest.At) {
			newest = b
		}
	}
	return newest.Group
}

// RulesFromBills derives prices from the bills a site actually charged, which
// is the only source of ratios when /api/pricing is locked behind a web
// session. The newest bill per model wins; bills priced by an expression or
// per call carry no usable ratios and are skipped.
func RulesFromBills(site Site, bills []Bill) []pricing.Rule {
	if len(site.Providers) == 0 {
		return nil
	}
	best := map[string]Bill{}
	for _, b := range bills {
		if b.Model == "" || b.Type != "consume" || b.Ratios.FixedPrice > 0 || (b.Ratios.Model <= 0 && b.Ratios.Expr == "") {
			continue
		}
		k := pricing.Normalize(b.Model)
		if prev, ok := best[k]; !ok || b.At.After(prev.At) {
			best[k] = b
		}
	}
	keys := make([]string, 0, len(best))
	for k := range best {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []pricing.Rule
	for _, k := range keys {
		b := best[k]
		p, ok := unitPrices(b.Ratios)
		if !ok {
			continue
		}
		out = append(out, priceRules(site, b.Model, b.At, p)...)
	}
	return dedupe(out)
}

func priceRules(site Site, model string, at time.Time, p prices) []pricing.Rule {
	out := make([]pricing.Rule, 0, len(site.Providers))
	for _, provider := range site.Providers {
		in, o, cr, cw := p.input, p.output, p.cacheRead, p.cacheWrite
		out = append(out, pricing.Rule{
			Provider:   provider,
			Model:      model,
			Multiplier: one,
			From:       at,
			Input:      &in,
			Output:     &o,
			CacheRead:  &cr,
			CacheWrite: &cw,
			Source:     RuleSource(site.Origin, site.KeyID),
		})
	}
	return out
}

// dedupe keeps one rule per (provider, normalized model): the last one, so
// callers append more specific sources (bills) after general ones.
func dedupe(rules []pricing.Rule) []pricing.Rule {
	idx := map[[2]string]int{}
	var out []pricing.Rule
	for _, r := range rules {
		k := [2]string{r.Provider, pricing.Normalize(r.Model)}
		if i, ok := idx[k]; ok {
			out[i] = r
			continue
		}
		idx[k] = len(out)
		out = append(out, r)
	}
	return out
}

type prices struct{ input, output, cacheRead, cacheWrite, cacheWrite1h float64 }

// Formula is what the site's own ratios say these tokens cost, in USD: the
// per-call price, the billing expression, or the ratio formula, group
// ratio included. ok is false when the ratios do not say.
func (r Ratios) Formula(t model.Tokens) (usd float64, ok bool) {
	return r.formula(t, 0)
}

// Formula is what the site's ratios say this charge costs, pricing its
// 1-hour cache writes at their own rate.
func (b Bill) Formula() (usd float64, ok bool) {
	return b.Ratios.formula(b.Tokens, b.CacheWrite1h)
}

func (r Ratios) formula(t model.Tokens, cacheWrite1h int64) (usd float64, ok bool) {
	if r.FixedPrice > 0 {
		return r.FixedPrice * ratioOr(r.Group, 1), true
	}
	p, ok := unitPrices(r)
	if !ok {
		return 0, false
	}
	cacheWrite1h = min(max(cacheWrite1h, 0), t.CacheWrite)
	return (float64(t.Input)*p.input + float64(t.Output+t.Reasoning)*p.output + float64(t.CacheRead)*p.cacheRead +
		float64(t.CacheWrite-cacheWrite1h)*p.cacheWrite + float64(cacheWrite1h)*p.cacheWrite1h) / 1e6, true
}

// unitPrices converts a gateway's ratios into $/1M token prices, group ratio
// included. new-api's model_ratio of 1 means $2 per 1M input tokens
// (docs/RELAY.md §4.1). A linear billing expression is read directly; it is
// already in $/1M. Per-call models and anything else not priced per token
// report false.
func unitPrices(r Ratios) (prices, bool) {
	group := r.Group
	if group <= 0 {
		group = 1
	}
	if r.FixedPrice > 0 {
		return prices{}, false
	}
	if r.Expr != "" {
		p, ok := exprPrices(r.Expr)
		if !ok {
			return prices{}, false
		}
		p.input *= group
		p.output *= group
		p.cacheRead *= group
		p.cacheWrite *= group
		p.cacheWrite1h *= group
		return p, true
	}
	if r.Model <= 0 {
		return prices{}, false
	}
	in := 2 * r.Model * group
	p := prices{
		input:      in,
		output:     in * ratioOr(r.Completion, 1),
		cacheRead:  in * ratioOr(r.Cache, 1),
		cacheWrite: in * ratioOr(r.CacheCreate, 1),
	}
	p.cacheWrite1h = p.cacheWrite
	if r.CacheCreate1h > 0 {
		p.cacheWrite1h = in * r.CacheCreate1h
	}
	return p, true
}

var (
	tierCall = regexp.MustCompile(`^tier\(\s*"[^"]*"\s*,\s*(.+)\)$`)
	exprTerm = regexp.MustCompile(`^([a-z][a-z0-9]*)\s*\*\s*([0-9.eE+-]+)$|^([0-9.eE+-]+)\s*\*\s*([a-z][a-z0-9]*)$`)
	lenCond  = regexp.MustCompile(`^len\s*(<=|<)\s*[0-9]+\s*\?\s*(tier\(.+?\))\s*:\s*tier\(.+\)$`)
)

// exprPrices reads a new-api billing expression (pkg/billingexpr) when it is a
// plain price list: tier("name", p * 2 + c * 10 + cr * 0.2 + cc * 2.5). A
// context-length split (len <= N ? tier(...) : tier(...)) uses its first,
// short-context tier, which is what nearly every request pays. Anything that
// depends on the time of day, or any other construct, reports false.
func exprPrices(expr string) (prices, bool) {
	e := strings.TrimSpace(expr)
	if m := lenCond.FindStringSubmatch(e); m != nil {
		e = m[2]
	}
	m := tierCall.FindStringSubmatch(e)
	if m == nil {
		return prices{}, false
	}
	var p prices
	cacheWrite1h := -1.0
	for _, term := range strings.Split(m[1], "+") {
		t := exprTerm.FindStringSubmatch(strings.TrimSpace(term))
		if t == nil {
			return prices{}, false
		}
		name, num := t[1], t[2]
		if name == "" {
			name, num = t[4], t[3]
		}
		v, err := strconv.ParseFloat(num, 64)
		if err != nil || math.IsNaN(v) || v < 0 {
			return prices{}, false
		}
		switch name {
		case "p":
			p.input = v
		case "c":
			p.output = v
		case "cr":
			p.cacheRead = v
		case "cc":
			p.cacheWrite = v
		case "cc1h":
			cacheWrite1h = v
		default:
			// images, audio, … : not a token price list this model can use
			return prices{}, false
		}
	}
	if p.cacheWrite == 0 && cacheWrite1h > 0 {
		p.cacheWrite = cacheWrite1h
	}
	p.cacheWrite1h = p.cacheWrite
	if cacheWrite1h > 0 {
		p.cacheWrite1h = cacheWrite1h
	}
	if p.input == 0 && p.output == 0 {
		// tier("base", p * 0 + c * 0): free, or priced by something else
		return prices{}, false
	}
	return p, true
}

// MergeRelayRules adds the rules a gateway just produced to the ones already
// stored. The first rule a site gives for a selector starts at the zero time,
// so it prices the history recorded before the site was turned on. Later it is
// only superseded when a price or the multiplier moves by more than 1%: the
// new rule is appended, dated from its observation, and the old one keeps
// pricing the time before. A site that jitters does not grow the table.
//
// The fresh rules of a source say which providers that key prices: stored
// rules of the same source for any other provider are dropped (the key no
// longer serves it), and so are rules stored under the source tag used before
// rules were tracked per key ("relay:<origin>" without "#keyID").
func MergeRelayRules(old, fresh []pricing.Rule) []pricing.Rule {
	providers := map[string]map[string]bool{}
	legacy := map[string]bool{}
	for _, f := range fresh {
		if providers[f.Source] == nil {
			providers[f.Source] = map[string]bool{}
		}
		providers[f.Source][f.Provider] = true
		if origin, _, ok := strings.Cut(strings.TrimPrefix(f.Source, RuleSourcePrefix), "#"); ok && strings.HasPrefix(f.Source, RuleSourcePrefix) {
			legacy[RuleSourcePrefix+origin] = true
		}
	}
	out := make([]pricing.Rule, 0, len(old)+len(fresh))
	for _, r := range old {
		if legacy[r.Source] {
			continue
		}
		if ps, ok := providers[r.Source]; ok && !ps[r.Provider] {
			continue
		}
		out = append(out, r)
	}
	for _, f := range dedupe(fresh) {
		i := latestRule(out, f)
		switch {
		case i < 0:
			f.From = time.Time{}
			out = append(out, f)
		case changed(out[i], f):
			if !f.From.After(out[i].From) {
				// same moment or clock skew: correct the rule in place
				f.From = out[i].From
				out[i] = f
				continue
			}
			out = append(out, f)
		}
	}
	return out
}

// latestRule finds the newest stored rule for the same selector and source.
func latestRule(rules []pricing.Rule, want pricing.Rule) int {
	best := -1
	for i, r := range rules {
		if r.Provider == want.Provider && pricing.Normalize(r.Model) == pricing.Normalize(want.Model) && r.Source == want.Source {
			if best < 0 || r.From.After(rules[best].From) {
				best = i
			}
		}
	}
	return best
}

// changed reports whether any price class or the multiplier moved by more
// than 1%, or whether the new rule sets a class the old one did not.
func changed(old, fresh pricing.Rule) bool {
	if relativeChange(old.Multiplier, fresh.Multiplier) > ratioChangeThreshold {
		return true
	}
	for _, pair := range [][2]*float64{
		{old.Input, fresh.Input},
		{old.Output, fresh.Output},
		{old.CacheRead, fresh.CacheRead},
		{old.CacheWrite, fresh.CacheWrite},
	} {
		if pair[0] == nil || pair[1] == nil {
			if (pair[0] == nil) != (pair[1] == nil) {
				return true
			}
			continue
		}
		if relativeChange(*pair[0], *pair[1]) > ratioChangeThreshold {
			return true
		}
	}
	return false
}

// relativeChange is |new-old| / |old|, or 1 when old is zero and new is not.
func relativeChange(old, fresh float64) float64 {
	if old == 0 {
		if fresh == 0 {
			return 0
		}
		return 1
	}
	return math.Abs(fresh-old) / math.Abs(old)
}
