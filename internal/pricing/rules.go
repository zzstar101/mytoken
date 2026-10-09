package pricing

import (
	"github.com/zzstar/mytoken/internal/model"
	"sort"
	"strings"
)

// Rule is the pricing-layer equivalent of the public settings contract.
type Rule struct {
	Provider, Model                      string
	Multiplier                           float64
	Input, Output, CacheRead, CacheWrite *float64
	Source                               string
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
	p.rules = map[[2]string]Rule{}
	// User rules win ties against imported rules regardless of list ordering.
	for _, user := range []bool{false, true} {
		for _, r := range rules {
			if (r.Source != "cc-switch") != user {
				continue
			}
			r.Input = cloneFloat(r.Input)
			r.Output = cloneFloat(r.Output)
			r.CacheRead = cloneFloat(r.CacheRead)
			r.CacheWrite = cloneFloat(r.CacheWrite)
			p.rules[[2]string{r.Provider, Normalize(r.Model)}] = r
		}
	}
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
func (p *Pricer) effective(provider, name string) (Price, bool, float64) {
	name = p.canonical(provider, name)
	price, known := p.lookup(name)
	n := Normalize(name)
	if v, ok := p.overrides[provider][n]; ok {
		price = v
		known = true
	}
	multiplier := 1.0
	if v, ok := p.multipliers[provider]; ok {
		multiplier = v
	}
	var rule Rule
	found := false
	for _, key := range [][2]string{{provider, n}, {"", n}, {provider, ""}} {
		if r, ok := p.rules[key]; ok {
			rule = r
			found = true
			break
		}
	}
	if found {
		multiplier = rule.Multiplier
		if multiplier == 0 {
			multiplier = 1
		}
		if rule.Input != nil {
			price.Input = *rule.Input
		}
		if rule.Output != nil {
			price.Output = *rule.Output
		}
		if rule.CacheRead != nil {
			price.CacheRead = *rule.CacheRead
		}
		if rule.CacheWrite != nil {
			price.CacheWrite = *rule.CacheWrite
		}
		// A missing catalog requires explicit rates for all token classes.
		known = known || (rule.Input != nil && rule.Output != nil && rule.CacheRead != nil && rule.CacheWrite != nil)
	}
	return price, known, multiplier
}
func (p *Pricer) Evaluate(e model.UsageEvent) (float64, bool) {
	if e.CostUSD != nil {
		return *e.CostUSD, true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	price, known, multiplier := p.effective(e.Provider, e.Model)
	// Partial custom rules can price an unknown model if no missing class is used.
	if !known {
		n := Normalize(p.canonical(e.Provider, e.Model))
		for _, key := range [][2]string{{e.Provider, n}, {"", n}, {e.Provider, ""}} {
			if r, ok := p.rules[key]; ok {
				t := e.Tokens
				known = (t.Input == 0 || r.Input != nil) && (t.Output == 0 && t.Reasoning == 0 || r.Output != nil) && (t.CacheRead == 0 || r.CacheRead != nil) && (t.CacheWrite == 0 || r.CacheWrite != nil) && (r.Input != nil || r.Output != nil || r.CacheRead != nil || r.CacheWrite != nil)
				break
			}
		}
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
