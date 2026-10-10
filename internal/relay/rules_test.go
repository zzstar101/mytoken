package relay

import (
	"math"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/source"
)

func float(v float64) *float64 { return &v }

// TestUnitPrices pins docs/RELAY.md §4.1: new-api's model_ratio of 1 is $2 per
// 1M input tokens, and every other class is a ratio of that.
func TestUnitPrices(t *testing.T) {
	p, ok := unitPrices(Ratios{Model: 2, Completion: 5, Cache: 0.1, CacheCreate: 1.25, Group: 0.3})
	if !ok || !near(p.input, 1.2) || !near(p.output, 6) || !near(p.cacheRead, 0.12) || !near(p.cacheWrite, 1.5) {
		t.Fatalf("unitPrices = %+v %v", p, ok)
	}
	// A zero group ratio means "no group discount", not "free".
	if p, ok = unitPrices(Ratios{Model: 1}); !ok || p.input != 2 || p.output != 2 || p.cacheRead != 2 || p.cacheWrite != 2 {
		t.Fatalf("unitPrices(defaults) = %+v", p)
	}
	// Per-call models and a missing model ratio give no token price at all
	// (a zero ratio used to be read as 1: $2/M for every tiered model).
	if _, ok = unitPrices(Ratios{Model: 3, FixedPrice: 2.5}); ok {
		t.Fatal("per-call model priced per token")
	}
	if _, ok = unitPrices(Ratios{Model: 0}); ok {
		t.Fatal("model ratio 0 priced")
	}
	// An expression is $/1M already; the group ratio still applies.
	p, ok = unitPrices(Ratios{Expr: `tier("standard", p * 2 + c * 10 + cr * 0.2 + cc * 2.5)`, Group: 0.5})
	if !ok || !near(p.input, 1) || !near(p.output, 5) || !near(p.cacheRead, 0.1) || !near(p.cacheWrite, 1.25) {
		t.Fatalf("expr prices = %+v %v", p, ok)
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

// TestExprPrices pins which billing expressions become a price list.
func TestExprPrices(t *testing.T) {
	cases := []struct {
		expr          string
		ok            bool
		in, out, read float64
	}{
		{`tier("standard", p * 0.15 + cr * 0.03 + cc * 0 + c * 0.5)`, true, 0.15, 0.5, 0.03},
		{`tier("base", p * 2.5 + c * 15 + cr * 0.25 + cc * 3.125)`, true, 2.5, 15, 0.25},
		// a context-length split prices by its short-context tier
		{`len <= 272000 ? tier("standard", p * 2 + c * 10 + cr * 0.2 + cc * 2.5) : tier("long_context", p * 4 + c * 15 + cr * 0.4 + cc * 5)`, true, 2, 10, 0.2},
		// time of day: no single price
		{`weekday("UTC") >= 1 && hour("UTC") < 4 ? tier("peak", p * 0.3 + c * 1.2) : tier("off_peak", p * 0.15 + c * 0.6)`, false, 0, 0, 0},
		// all zero: free or priced elsewhere
		{`tier("base", p * 0 + c * 0)`, false, 0, 0, 0},
		{`tier("img", img * 0.04)`, false, 0, 0, 0},
		{`garbage`, false, 0, 0, 0},
	}
	for _, c := range cases {
		p, ok := exprPrices(c.expr)
		if ok != c.ok || (ok && (!near(p.input, c.in) || !near(p.output, c.out) || !near(p.cacheRead, c.read))) {
			t.Errorf("exprPrices(%q) = %+v %v", c.expr, p, ok)
		}
	}
}

// TestRulesFromSnapshot turns one ratio snapshot into rules, one per provider
// the site serves, priced at the key's group.
func TestRulesFromSnapshot(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	site := Site{Origin: "https://relay.example", KeyID: "abc123", Providers: []string{"XLAB", "XLAB-Backup"}}
	snap := Snapshot{At: at, Ratios: map[string]Ratios{
		"claude-sonnet-4-5": {Model: 2, Completion: 5, Cache: 0.1},
		"gpt-image":         {FixedPrice: 0.04},
		"":                  {Model: 9},
	}, Groups: map[string]float64{"default": 1, "cheap": 0.5}}
	rules := RulesFromSnapshot(site, snap, "cheap")
	if len(rules) != 2 {
		t.Fatalf("rules=%+v", rules)
	}
	for _, r := range rules {
		if r.Model != "claude-sonnet-4-5" {
			t.Fatalf("model=%q, empty and per-call models must be skipped", r.Model)
		}
		if !r.From.Equal(at) || r.Source != RuleSource(site.Origin, site.KeyID) || r.Multiplier != 1 {
			t.Fatalf("rule=%+v", r)
		}
		if r.Input == nil || *r.Input != 2 || r.Output == nil || *r.Output != 10 {
			t.Fatalf("prices=%+v", r)
		}
	}
	// Groups that differ and no known key group: no guess.
	if got := RulesFromSnapshot(site, snap, ""); len(got) != 0 {
		t.Fatalf("rules=%+v, want none", got)
	}
	// Groups that all cost the same need no key group.
	snap.Groups = map[string]float64{"default": 1, "vip": 1}
	if got := RulesFromSnapshot(site, snap, ""); len(got) != 2 {
		t.Fatalf("rules=%+v", got)
	}
	// No providers means nothing to price.
	if got := RulesFromSnapshot(Site{}, snap, ""); got != nil {
		t.Fatalf("rules=%+v, want none", got)
	}
	// sub2api: the key's multiplier as a provider-wide rule.
	m := 0.25
	got := RulesFromSnapshot(site, Snapshot{At: at, Multiplier: &m}, "")
	if len(got) != 2 || got[0].Model != "" || got[0].Multiplier != 0.25 || got[0].Input != nil {
		t.Fatalf("multiplier rules=%+v", got)
	}
}

// TestRulesFromBills derives prices from the newest bill per model.
func TestRulesFromBills(t *testing.T) {
	site := Site{Origin: "https://relay.example", Providers: []string{"XLAB"}}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := old.AddDate(0, 0, 3)
	bills := []Bill{
		{Type: "consume", Model: "claude-sonnet-4-5", Group: "vip", At: old, Ratios: Ratios{Model: 1, Group: 1}},
		{Type: "consume", Model: "claude-sonnet-4-5", Group: "default", At: newer, Ratios: Ratios{Model: 2, Group: 1}},
		{Type: "consume", Model: "gpt-5-codex", Group: "default", At: old, Ratios: Ratios{Model: 0.5, Group: 0.5}},
		{Type: "consume", Model: "deepseek-flash", At: old, Ratios: Ratios{Expr: `tier("x", p * 0.15 + c * 0.6)`, Group: 1}},
		{Type: "consume", Model: "unpriced", Group: "default", At: old, Ratios: Ratios{Model: 0}},
		{Type: "refund", Model: "refunded", At: old, Ratios: Ratios{Model: 1}},
	}
	rules := RulesFromBills(site, bills)
	if len(rules) != 3 {
		t.Fatalf("rules=%+v", rules)
	}
	byModel := map[string]pricing.Rule{}
	for _, r := range rules {
		byModel[r.Model] = r
	}
	if r := byModel["claude-sonnet-4-5"]; !r.From.Equal(newer) || r.Input == nil || *r.Input != 4 {
		t.Fatalf("sonnet=%+v", r)
	}
	if r := byModel["gpt-5-codex"]; r.Input == nil || *r.Input != 0.5 {
		t.Fatalf("gpt=%+v", r)
	}
	if r := byModel["deepseek-flash"]; r.Output == nil || !near(*r.Output, 0.6) {
		t.Fatalf("expr=%+v", r)
	}
	if KeyGroup(bills) != "default" {
		t.Fatalf("key group = %q", KeyGroup(bills))
	}
}

// TestMergeRelayRules: the first rule from a site prices all history; a
// jitter under 1% leaves the table alone; a real change is appended dated from
// its observation, so earlier events keep the old price; other sources are
// never touched.
func TestMergeRelayRules(t *testing.T) {
	site := Site{Origin: "https://relay.example", Providers: []string{"XLAB"}}
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	user := pricing.Rule{Provider: "XLAB", Model: "gpt-5-codex", Input: float(2), Source: "user"}
	snap := func(at time.Time, ratio float64) []pricing.Rule {
		return RulesFromSnapshot(site, Snapshot{At: at, Ratios: map[string]Ratios{
			"claude-sonnet-4-5": {Model: ratio, Completion: 5},
		}}, "")
	}
	merged := MergeRelayRules([]pricing.Rule{user}, snap(t0, 2))
	if len(merged) != 2 || !merged[1].From.IsZero() || *merged[1].Input != 4 {
		t.Fatalf("first=%+v", merged)
	}
	if again := MergeRelayRules(merged, snap(t0.Add(time.Hour), 2.01)); len(again) != 2 {
		t.Fatalf("jitter grew the table: %+v", again)
	}
	t1 := t0.AddDate(0, 0, 7)
	moved := MergeRelayRules(merged, snap(t1, 2.5))
	if len(moved) != 3 || !moved[1].From.IsZero() || !moved[2].From.Equal(t1) || *moved[2].Input != 5 {
		t.Fatalf("moved=%+v", moved)
	}
	// Compared against the newest rule, not the first one.
	if again := MergeRelayRules(moved, snap(t1.Add(time.Hour), 2.5)); len(again) != 3 {
		t.Fatalf("re-added: %+v", again)
	}
	// A multiplier change is a change.
	m1, m2 := 0.25, 0.3
	a := MergeRelayRules(nil, RulesFromSnapshot(site, Snapshot{At: t0, Multiplier: &m1}, ""))
	b := MergeRelayRules(a, RulesFromSnapshot(site, Snapshot{At: t1, Multiplier: &m2}, ""))
	if len(b) != 2 || b[1].Multiplier != 0.3 || !b[1].From.Equal(t1) {
		t.Fatalf("multiplier=%+v", b)
	}
	for _, r := range moved {
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
	// Under 1% (jitter) is not a change, so the rule is kept.
	if changed(pricing.Rule{Input: float(4)}, pricing.Rule{Input: float(4.03)}) {
		t.Fatal("a 0.75% move should not count as a change")
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
		// a route: the config knows the URL but not the key
		{Origin: "https://relay.example", Provider: "dsh-name"},
	}
	sites := Candidates(creds)
	if len(sites) != 2 {
		t.Fatalf("sites=%+v", sites)
	}
	if sites[0].Origin != "https://relay.example" || sites[0].KeyID != source.KeyID(key) {
		t.Fatalf("site=%+v", sites[0])
	}
	if len(sites[0].Providers) != 3 || sites[0].Providers[0] != "XLAB" || sites[0].Providers[2] != "dsh-name" {
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

// TestBillFormulaCacheWrite1h prices 1-hour cache writes at their own rate:
// new-api's cc1h term in a tiered expression, or cache_creation_ratio_1h.
func TestBillFormulaCacheWrite1h(t *testing.T) {
	tokens := model.Tokens{Input: 2, Output: 1674, CacheRead: 180635, CacheWrite: 485}
	expr := Ratios{Expr: `tier("standard", p * 4 + cr * 0.2 + cc * 5 + cc1h * 8 + c * 20)`, Group: 0.8}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	// A real charge: all 485 written tokens were 1-hour writes.
	if got, ok := (Bill{Tokens: tokens, CacheWrite1h: 485, Ratios: expr}).Formula(); !ok || !near(got, (2*4+180635*0.2+485*8+1674*20)*0.8/1e6) {
		t.Fatalf("1h formula = %v %v", got, ok)
	}
	if got, ok := (Bill{Tokens: tokens, Ratios: expr}).Formula(); !ok || !near(got, (2*4+180635*0.2+485*5+1674*20)*0.8/1e6) {
		t.Fatalf("5m formula = %v %v", got, ok)
	}
	// Split writes, and a 1-hour count larger than the writes is clamped.
	if got, _ := (Bill{Tokens: tokens, CacheWrite1h: 85, Ratios: expr}).Formula(); !near(got, (2*4+180635*0.2+400*5+85*8+1674*20)*0.8/1e6) {
		t.Fatalf("split formula = %v", got)
	}
	if a, _ := (Bill{Tokens: tokens, CacheWrite1h: 9999, Ratios: expr}).Formula(); !near(a, (2*4+180635*0.2+485*8+1674*20)*0.8/1e6) {
		t.Fatalf("clamped formula = %v", a)
	}
	ratio := Ratios{Model: 1.5, Completion: 5, Cache: 0.1, CacheCreate: 1.25, CacheCreate1h: 2, Group: 1}
	w := model.Tokens{CacheWrite: 1_000_000}
	if got, _ := (Bill{Tokens: w, CacheWrite1h: 1_000_000, Ratios: ratio}).Formula(); !near(got, 6) {
		t.Fatalf("ratio 1h formula = %v, want 6", got)
	}
	if got, _ := ratio.Formula(w); !near(got, 3.75) {
		t.Fatalf("ratio 5m formula = %v, want 3.75", got)
	}
}

// TestMergeRelayRulesDropsStaleProviders: a key's fresh rules name the
// providers it prices; its stored rules for other providers go, and so do
// rules under the old per-site source tag. Other keys and users are kept.
func TestMergeRelayRulesDropsStaleProviders(t *testing.T) {
	origin := "https://relay.example"
	one := float(1)
	k1, k2 := RuleSource(origin, "k1"), RuleSource(origin, "k2")
	stored := []pricing.Rule{
		{Provider: "pi/a", Model: "m", Input: one, Source: k1},
		{Provider: "dsh/b", Model: "m", Input: one, Source: k1},
		{Provider: "pi/a", Model: "m", Input: one, Source: k2},
		{Provider: "a", Model: "m", Input: one, Source: "relay:" + origin},
		{Provider: "a", Model: "m", Input: one, Source: "relay:https://other.example"},
		{Provider: "a", Model: "m", Input: one, Source: "user"},
	}
	merged := MergeRelayRules(stored, []pricing.Rule{{Provider: "pi/a", Model: "m", Input: one, Source: k1}})
	got := map[string]bool{}
	for _, r := range merged {
		got[r.Source+" "+r.Provider] = true
	}
	want := []string{k1 + " pi/a", k2 + " pi/a", "relay:https://other.example a", "user a"}
	if len(merged) != len(want) {
		t.Fatalf("merged=%+v", merged)
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("missing %q in %+v", w, merged)
		}
	}
}
