package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Fingerprint identifies every input to Evaluate, not refresh timestamps.
func (p *Pricer) Fingerprint() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	type alias struct{ Provider, Model, Target string }
	rules := make([]Rule, 0, len(p.rules))
	for _, r := range p.rules {
		rules = append(rules, r)
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Provider != rules[j].Provider {
			return rules[i].Provider < rules[j].Provider
		}
		return rules[i].Model < rules[j].Model
	})
	aliases := make([]alias, 0, len(p.modelAliases))
	for k, v := range p.modelAliases {
		aliases = append(aliases, alias{k[0], k[1], v})
	}
	sort.Slice(aliases, func(i, j int) bool {
		if aliases[i].Provider != aliases[j].Provider {
			return aliases[i].Provider < aliases[j].Provider
		}
		return aliases[i].Model < aliases[j].Model
	})
	raw, _ := json.Marshal(struct {
		Version     int
		Prices      map[string]Price
		Overrides   map[string]map[string]Price
		Multipliers map[string]float64
		Rules       []Rule
		Aliases     []alias
	}{1, p.prices, p.overrides, p.multipliers, rules, aliases})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
