package relay

import (
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/source"
)

func float(v float64) *float64 { return &v }

// TestUnitPrices pins docs/RELAY.md §4.1: new-api's model_ratio of 1 is $2 per
// 1M input tokens, and every other class is a ratio of that.
func TestUnitPrices(t *testing.T) {
	in, out, cr, cw := unitPrices(Ratios{Model: 2, Completion: 5, Cache: 0.1, CacheCreate: 1.25, Group: 0.3})
	if in != 1.2 || out != 6 || cr != 0.12 || cw != 1.5 {
		t.Fatalf("unitPrices = %v/%v/%v/%v", in, out, cr, cw)
	}
	// A zero group ratio means "no group discount", not "free".
	if in, out, cr, cw = unitPrices(Ratios{Model: 1}); in != 2 || out != 2 || cr != 2 || cw != 2 {
		t.Fatalf("unitPrices(defaults) = %v/%v/%v/%v", in, out, cr, cw)
	}
	// Per-call models keep no token price at all.
	if in, out, cr, cw = unitPrices(Ratios{Model: 3, FixedPrice: 2.5}); in != 0 || out != 0 || cr != 0 || cw != 0 {
		t.Fatalf("unitPrices(fixed) = %v/%v/%v/%v", in, out, cr, cw)
	}
}

// TestRulesFromSnapshot turns one ratio snapshot into dated rules, one per
// provider the site serves.
func TestRulesFromSnapshot(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	site := Site{Origin: "https://relay.example", KeyID: "abc123", Providers: []string{"XLAB", "XLAB-Backup"}}
	rules := RulesFromSnapshot(site, Snapshot{At: at, Ratios: map[string]Ratios{
		"claude-sonnet-4-5": {Model: 2, Completion: 5, Cache: 0.1, Group: 1},
		"":                  {Model: 9},
	}})
	if len(rules) != 2 {
		t.Fatalf("rules=%+v", rules)
	}
	for _, r := range rules {
		if r.Model != "claude-sonnet-4-5" {
			t.Fatalf("model=%q, the empty name must be skipped", r.Model)
		}
		if !r.From.Equal(at) {
			t.Fatalf("from=%v, want %v", r.From, at)
		}
		if r.Source != RuleSource(site.Origin) {
			t.Fatalf("source=%q", r.Source)
		}
		if r.Input == nil || *r.Input != 4 || r.Output == nil || *r.Output != 20 {
			t.Fatalf("prices=%+v", r)
		}
	}
	// No providers means nothing to price.
	if got := RulesFromSnapshot(Site{}, Snapshot{At: at, Ratios: map[string]Ratios{"m": {}}}); got != nil {
		t.Fatalf("rules=%+v, want none", got)
	}
}

// TestRulesFromBills derives ratios from the newest bill per (model, group).
func TestRulesFromBills(t *testing.T) {
	site := Site{Origin: "https://relay.example", Providers: []string{"XLAB"}}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := old.AddDate(0, 0, 3)
	bills := []Bill{
		{Model: "claude-sonnet-4-5", Group: "vip", At: old, Ratios: Ratios{Model: 1, Group: 1}},
		{Model: "claude-sonnet-4-5", Group: "vip", At: newer, Ratios: Ratios{Model: 2, Group: 1}},
		{Model: "gpt-5-codex", Group: "default", At: old, Ratios: Ratios{Model: 0.5, Group: 1}},
		{Model: "unpriced", Group: "default", At: old, Ratios: Ratios{Model: 0}},
	}
	rules := RulesFromBills(site, bills)
	if len(rules) != 2 {
		t.Fatalf("rules=%+v", rules)
	}
	byModel := map[string]pricing.Rule{}
	for _, r := range rules {
		byModel[r.Model] = r
	}
	if r := byModel["claude-sonnet-4-5"]; !r.From.Equal(newer) || r.Input == nil || *r.Input != 4 {
		t.Fatalf("sonnet=%+v", r)
	}
	if r := byModel["gpt-5-codex"]; !r.From.Equal(old) || r.Input == nil || *r.Input != 1 {
		t.Fatalf("gpt=%+v", r)
	}
}

// TestMergeRelayRules is the 1% threshold: a ratio that jitters leaves the
// stored rule alone, a real change replaces it, and other sources are never
// touched.
func TestMergeRelayRules(t *testing.T) {
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	old := []pricing.Rule{
		{Provider: "XLAB", Model: "claude-sonnet-4-5", Input: float(4), Output: float(20), Source: RuleSource("https://relay.example"), From: at},
		{Provider: "XLAB", Model: "gpt-5-codex", Input: float(2), Source: "user", From: at},
	}
	same := RulesFromSnapshot(Site{Origin: "https://relay.example", Providers: []string{"XLAB"}}, Snapshot{At: at, Ratios: map[string]Ratios{
		"claude-sonnet-4-5": {Model: 2, Completion: 5, Group: 1},
	}})
	merged := MergeRelayRules(old, same)
	if len(merged) != 2 {
		t.Fatalf("merged=%+v", merged)
	}
	if !merged[0].From.Equal(at) || *merged[0].Input != 4 {
		t.Fatalf("unchanged rule rewritten: %+v", merged[0])
	}
	// A 2% move is above the threshold and replaces the rule.
	moved := RulesFromSnapshot(Site{Origin: "https://relay.example", Providers: []string{"XLAB"}}, Snapshot{At: at, Ratios: map[string]Ratios{
		"claude-sonnet-4-5": {Model: 2.04, Completion: 5, Group: 1},
	}})
	merged = MergeRelayRules(old, moved)
	if len(merged) != 2 || *merged[0].Input != 4.08 {
		t.Fatalf("merged=%+v", merged)
	}
	// A new model is appended, not folded into an existing rule.
	added := RulesFromSnapshot(Site{Origin: "https://relay.example", Providers: []string{"XLAB"}}, Snapshot{At: at, Ratios: map[string]Ratios{
		"claude-sonnet-4-5": {Model: 2, Completion: 5, Group: 1},
		"deepseek-v4.1":     {Model: 0.5, Group: 1},
	}})
	merged = MergeRelayRules(old, added)
	if len(merged) != 3 {
		t.Fatalf("merged=%+v", merged)
	}
	// The user rule survived untouched.
	for _, r := range merged {
		if r.Model == "gpt-5-codex" && r.Source != "user" {
			t.Fatalf("user rule rewritten: %+v", r)
		}
	}
}

// TestRelativeChange pins the threshold arithmetic.
func TestRelativeChange(t *testing.T) {
	cases := []struct {
		old, fresh, want float64
	}{
		{4, 4, 0},
		{4, 4.04, 0.01},
		{4, 4.5, 0.125},
		{4, 3, 0.25},
		{0, 0, 0},
		{0, 5, 1},
	}
	for _, c := range cases {
		got := relativeChange(c.old, c.fresh)
		// The threshold is compared with >, so a value exactly at the boundary
		// must not count as a change; compare with the same slack.
		if got > c.want+1e-9 || got < c.want-1e-9 {
			t.Fatalf("relativeChange(%v,%v) = %v, want %v", c.old, c.fresh, got, c.want)
		}
	}
	// Exactly 1% is not "more than 1%", so the rule is kept.
	if !changed(pricing.Rule{Input: float(4)}, pricing.Rule{Input: float(4.04)}) {
		t.Fatal("a 1% move should not count as a change")
	}
	if !changed(pricing.Rule{Input: float(4)}, pricing.Rule{Input: float(4.5)}) {
		t.Fatal("a 12.5% move should count as a change")
	}
	// A class that appears or disappears is always a change.
	if !changed(pricing.Rule{Input: float(4)}, pricing.Rule{}) {
		t.Fatal("dropping a class should count as a change")
	}
	if !changed(pricing.Rule{}, pricing.Rule{Output: float(1)}) {
		t.Fatal("adding a class should count as a change")
	}
	if changed(pricing.Rule{}, pricing.Rule{}) {
		t.Fatal("nothing changed")
	}
}

// TestCandidates turns credentials into sites: deduplicated, key ids attached,
// and the cc-switch local proxy filtered out.
func TestCandidates(t *testing.T) {
	key := "sk-sentinel-DO-NOT-LOG-0000"
	creds := []source.Credential{
		{Origin: "https://relay.example", KeyID: source.KeyID(key), Provider: "XLAB"},
		{Origin: "https://relay.example", KeyID: source.KeyID(key), Provider: "XLAB-Backup"},
		{Origin: "https://relay.example", KeyID: source.KeyID("another-key"), Provider: "XLAB"},
		{Origin: source.LocalProxyOrigin, KeyID: source.KeyID(key), Provider: "Local"},
		{Origin: "https://other.example", Provider: "NoKeyID"},
	}
	sites := Candidates(creds)
	if len(sites) != 2 {
		t.Fatalf("sites=%+v", sites)
	}
	if sites[0].Origin != "https://relay.example" || sites[0].KeyID != source.KeyID(key) {
		t.Fatalf("site=%+v", sites[0])
	}
	if len(sites[0].Providers) != 2 || sites[0].Providers[0] != "XLAB" {
		t.Fatalf("providers=%v", sites[0].Providers)
	}
	if sites[1].KeyID != source.KeyID("another-key") {
		t.Fatalf("site=%+v", sites[1])
	}
	for _, s := range sites {
		if s.Origin == source.LocalProxyOrigin {
			t.Fatal("local proxy became a site")
		}
	}
}

// TestLayerParsing covers the flag and display helpers.
func TestLayerParsing(t *testing.T) {
	if got := ParseLayer("ratio,balance,bills"); got != LayerRatio|LayerBalance|LayerBills {
		t.Fatalf("ParseLayer = %v", got)
	}
	if got := ParseLayer("bills"); got != LayerBills {
		t.Fatalf("ParseLayer = %v", got)
	}
	if got := ParseLayer("nonsense"); got != 0 {
		t.Fatalf("ParseLayer = %v", got)
	}
	if got := (LayerRatio | LayerBills).LayerNames(); len(got) != 2 || got[0] != "ratio" || got[1] != "bills" {
		t.Fatalf("LayerNames = %v", got)
	}
	if got := (LayerRatio | LayerBills).String(); got != "off" {
		t.Fatalf("String = %q, want off for a combination", got)
	}
}
