package pricing

import (
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

// Rule is the pricing-layer equivalent of the public settings contract.
//
// Provider/Model select what the rule applies to. Multiplier scales the
// computed cost; a zero Multiplier means "unset" so it never overrides a
// multiplier set elsewhere. Input/Output/CacheRead/CacheWrite replace the
// matching per-1M-token rate (nil keeps the value found further down the
// price chain).
//
// From is when the rule starts applying (zero = always). Several rules may
// share (Provider, Model); the one with the latest From ≤ the lookup time
// wins. Time-of-day windows are not modelled yet, but From leaves room for
// them.
type Rule struct {
	Provider, Model                      string
	Multiplier                           float64
	Input, Output, CacheRead, CacheWrite *float64
	Source                               string
	From                                 time.Time `json:"from,omitzero"`
}

func cloneFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}
func (p *Pricer) SetRules(rules []Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = map[[2]string][]Rule{}
	// Rules are appended least authoritative first, so a later append wins a
	// tie and the stable sort below stays deterministic: cc-switch's import,
	// then a gateway's own ratios, then the user's own rules.
	for _, rank := range []int{0, 1, 2} {
		for _, r := range rules {
			if sourceRank(r) != rank {
				continue
			}
			r.Input = cloneFloat(r.Input)
			r.Output = cloneFloat(r.Output)
			r.CacheRead = cloneFloat(r.CacheRead)
			r.CacheWrite = cloneFloat(r.CacheWrite)
			if !r.From.IsZero() {
				r.From = r.From.UTC()
			}
			p.rules[[2]string{r.Provider, Normalize(r.Model)}] = append(p.rules[[2]string{r.Provider, Normalize(r.Model)}], r)
		}
	}
	for key, list := range p.rules {
		// Authority first (user over a gateway over the cc-switch import), then
		// ascending From inside an authority level, so ruleAt picks the newest
		// rule effective at a moment within the most authoritative level that
		// has one. Authority comes first because a user's own price must stay
		// an override no matter when a gateway reported its ratios.
		sort.SliceStable(list, func(i, j int) bool {
			if ri, rj := sourceRank(list[i]), sourceRank(list[j]); ri != rj {
				return ri < rj
			}
			return list[i].From.Before(list[j].From)
		})
		p.rules[key] = list
	}
}

// sourceRank orders rules by how much the user's own edits outrank them: the
// user's rules win, a gateway's ratios come next, and cc-switch's import last.
func sourceRank(r Rule) int {
	switch {
	case strings.HasPrefix(r.Source, "relay:"):
		return 1
	case r.Source == "cc-switch":
		return 0
	}
	return 2
}
func (p *Pricer) SetAliases(aliases map[[2]string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.modelAliases = map[[2]string]string{}
	for k, v := range aliases {
		p.modelAliases[k] = v
	}
}
func (p *Pricer) canonical(provider, name string) string {
	if v := p.modelAliases[[2]string{provider, name}]; v != "" {
		return v
	}
	if v := p.modelAliases[[2]string{"", name}]; v != "" {
		return v
	}
	return name
}
func (p *Pricer) CanonicalModel(provider, name string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.canonical(provider, name)
}

// ruleAt returns the rule effective at t for key: the latest From ≤ t, ties
// broken toward user rules (SetRules appends them last).
func (p *Pricer) ruleAt(key [2]string, t time.Time) (Rule, bool) {
	list := p.rules[key]
	for i := len(list) - 1; i >= 0; i-- {
		if !list[i].From.After(t) {
			return list[i], true
		}
	}
	return Rule{}, false
}

// priceSources lists the rules that may supply unit prices for a model, most
// specific first: provider+model, model-only, then provider-wide. A nil entry
// means no rule of that shape is effective at t.
func (p *Pricer) priceSources(provider, name string, t time.Time) [3]*Rule {
	var out [3]*Rule
	for i, key := range [][2]string{{provider, name}, {"", name}, {provider, ""}} {
		if r, ok := p.ruleAt(key, t); ok {
			rule := r
			out[i] = &rule
		}
	}
	return out
}

// classPrice returns the first rate set for a token class, walking the price
// sources from most to least specific, so a narrower rule can override one
// class while a wider one fills the rest.
func classPrice(src [3]*Rule, class func(Rule) *float64) (*float64, bool) {
	for _, r := range src {
		if r == nil {
			continue
		}
		if v := class(*r); v != nil {
			return v, true
		}
	}
	return nil, false
}

func ruleInput(r Rule) *float64      { return r.Input }
func ruleOutput(r Rule) *float64     { return r.Output }
func ruleCacheRead(r Rule) *float64  { return r.CacheRead }
func ruleCacheWrite(r Rule) *float64 { return r.CacheWrite }

// multiplierFor returns the cost multiplier. Multipliers are provider-first: a
// provider+model rule, then a provider rule, then the provider table; a
// model-wide multiplier is the last fallback before the default 1. A rule's
// zero Multiplier means unset and is skipped, so an imported model rule never
// cancels a provider's multiplier.
func (p *Pricer) multiplierFor(provider, name string, t time.Time) float64 {
	for _, key := range [][2]string{{provider, name}, {provider, ""}} {
		if r, ok := p.ruleAt(key, t); ok && r.Multiplier != 0 {
			return r.Multiplier
		}
	}
	if v, ok := p.multipliers[provider]; ok {
		return v
	}
	if r, ok := p.ruleAt([2]string{"", name}, t); ok && r.Multiplier != 0 {
		return r.Multiplier
	}
	return 1
}

// ruleMoment returns the instant rule selection uses: the event's own
// timestamp, or now when the caller left it zero (bulk recompute paths that
// drop timestamps still price with the rules effective today).
func ruleMoment(e model.UsageEvent) time.Time {
	if e.Timestamp.IsZero() {
		return time.Now()
	}
	return e.Timestamp
}

func (p *Pricer) effective(provider, name string, at time.Time) (Price, bool, float64) {
	name = p.canonical(provider, name)
	price, known := p.lookup(name)
	n := Normalize(name)
	if v, ok := p.overrides[provider][n]; ok {
		price = v
		known = true
	}
	multiplier := p.multiplierFor(provider, n, at)
	// Unit prices fall back class by class, from the most specific rule to the
	// widest one; a class no rule sets keeps the catalog or override rate.
	src := p.priceSources(provider, n, at)
	in, inSet := classPrice(src, ruleInput)
	out, outSet := classPrice(src, ruleOutput)
	cr, crSet := classPrice(src, ruleCacheRead)
	cw, cwSet := classPrice(src, ruleCacheWrite)
	if inSet {
		price.Input = *in
	}
	if outSet {
		price.Output = *out
	}
	if crSet {
		price.CacheRead = *cr
	}
	if cwSet {
		price.CacheWrite = *cw
	}
	// A missing catalog requires explicit rates for all token classes.
	known = known || (inSet && outSet && crSet && cwSet)
	return price, known, multiplier
}
func (p *Pricer) Evaluate(e model.UsageEvent) (float64, bool) {
	if e.CostUSD != nil {
		return *e.CostUSD, true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	at := ruleMoment(e)
	price, known, multiplier := p.effective(e.Provider, e.Model, at)
	// Partial custom rules can price an unknown model if no missing class is
	// used; the classes fall back across the same chain as the price itself.
	if !known {
		n := Normalize(p.canonical(e.Provider, e.Model))
		src := p.priceSources(e.Provider, n, at)
		_, inSet := classPrice(src, ruleInput)
		_, outSet := classPrice(src, ruleOutput)
		_, crSet := classPrice(src, ruleCacheRead)
		_, cwSet := classPrice(src, ruleCacheWrite)
		t := e.Tokens
		known = (t.Input == 0 || inSet) && (t.Output == 0 && t.Reasoning == 0 || outSet) && (t.CacheRead == 0 || crSet) && (t.CacheWrite == 0 || cwSet) && (inSet || outSet || crSet || cwSet)
	}
	if !known {
		return 0, false
	}
	reasoning := price.Output
	if price.Reasoning != nil {
		reasoning = *price.Reasoning
	}
	t := e.Tokens
	return (float64(t.Input)*price.Input + float64(t.Output)*price.Output + float64(t.CacheRead)*price.CacheRead + float64(t.CacheWrite)*price.CacheWrite + float64(t.Reasoning)*reasoning) / 1e6 * multiplier, true
}
func (p *Pricer) SearchCatalog(q string, limit int) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if limit <= 0 {
		return []string{}
	}
	q = searchName(q)
	type candidate struct {
		name  string
		score int
	}
	items := []candidate{}
	for name := range p.aliases {
		n := searchName(name)
		score := distance(q, n) * 4
		if q == n {
			score = 0
		} else if q == "" || strings.Contains(n, q) {
			score = 1
		}
		items = append(items, candidate{name, score})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score < items[j].score
		}
		return items[i].name < items[j].name
	})
	out := []string{}
	for _, v := range items {
		out = append(out, v.name)
		if len(out) == limit {
			break
		}
	}
	return out
}
func searchName(q string) string {
	q = Normalize(q)
	for _, suffix := range []string{"-high", "-medium", "-low", "-preview", "-expires"} {
		if i := strings.Index(q, suffix); i >= 0 {
			q = q[:i]
		}
	}
	return strings.NewReplacer("-", "", ".", "", "_", "", ":", "").Replace(q)
}
func distance(a, b string) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		next := make([]int, len(b)+1)
		next[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			next[j] = min(next[j-1]+1, previous[j]+1, previous[j-1]+cost)
		}
		previous = next
	}
	return previous[len(b)]
}
