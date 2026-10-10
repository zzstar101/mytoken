package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

// fingerprintVersion changes when Evaluate reads something new: 2 = rules
// scoped to a tool's provider, so stored costs recomputed without the
// event's harness are stale.
const fingerprintVersion = 2

// Fingerprint identifies every input to Evaluate, not refresh timestamps.
// Rules are hashed with their From instant, so adding or moving a rule's
// effective time changes the fingerprint and forces a cost recompute.
func (p *Pricer) Fingerprint() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	type alias struct{ Provider, Model, Target string }
	rules := make([]Rule, 0, len(p.rules))
	for _, list := range p.rules {
		rules = append(rules, list...)
	}
	sort.Slice(rules, func(i, j int) bool {
		return ruleKey(rules[i]) < ruleKey(rules[j])
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
	}{fingerprintVersion, p.prices, p.overrides, p.multipliers, rules, aliases})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ruleKey orders rules deterministically regardless of map iteration order:
// selector, then the fixed-width UTC start instant (so From sorts
// chronologically), then source and the marshalled rates as a tie-break.
func ruleKey(r Rule) string {
	stamp := time.Time{}.Format(time.RFC3339)
	if !r.From.IsZero() {
		stamp = r.From.UTC().Format(time.RFC3339)
	}
	raw, _ := json.Marshal(r)
	return r.Provider + "\x00" + r.Model + "\x00" + stamp + "\x00" + r.Source + "\x00" + string(raw)
}
