package pricing

import (
	"math"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

func TestCatalogSuggestions(t *testing.T) {
	p := New("")
	for _, q := range []string{"claude-opus-4-6-high-preview-expires-20270101", "deepseek-v41"} {
		got := p.SearchCatalog(q, 5)
		if len(got) == 0 || len(got) > 5 {
			t.Fatal(q, got)
		}
		if q == "claude-opus-4-6-high-preview-expires-20270101" && got[0] != "claude-opus-4-6" {
			t.Fatal(q, got)
		}
	}
}

// TestRulePrecedence pins the split lookup: unit prices walk provider+model →
// model → provider class by class, multipliers prefer the provider rule.
func TestRulePrecedence(t *testing.T) {
	p := New("")
	input := 2.0
	zero := 0.0
	p.SetRules([]Rule{{Provider: "relay", Multiplier: 0.5}, {Model: "claude-opus-4-6", Input: &input, Multiplier: 2}, {Provider: "relay", Model: "claude-opus-4-6", Output: &zero}})
	e := model.UsageEvent{Provider: "relay", Model: "claude-opus-4-6", Tokens: model.Tokens{Input: 1000000, Output: 1000000}}
	catalog, _ := p.Lookup(e.Model)
	// Output comes from {relay, model} (0), input falls back to the {"", model}
	// rule (2), multiplier from {relay, ""} (0.5): the model rule's Multiplier
	// 2 is unset, not applied.
	if cost, ok := p.Evaluate(e); !ok || cost != (input+zero)*0.5 {
		t.Fatalf("specific cost=%v known=%v", cost, ok)
	}
	e.Provider = "other"
	// Price from {"", model}; no provider rule, so the model rule's own
	// multiplier 2 is the last fallback before 1.
	if cost, ok := p.Evaluate(e); !ok || cost != (2+catalog.Output)*2 {
		t.Fatalf("model cost=%v known=%v", cost, ok)
	}
	e.Provider = "relay"
	e.Model = "gpt-4o"
	catalog, _ = p.Lookup(e.Model)
	if cost, _ := p.Evaluate(e); cost != (catalog.Input+catalog.Output)*0.5 {
		t.Fatal(cost)
	}
	e.Model = "unknown"
	if _, ok := p.Evaluate(e); ok {
		t.Fatal("multiplier must not make unknown model priced")
	}
	p.SetRules([]Rule{{Model: "unknown", Input: &zero, Output: &zero, CacheRead: &zero, CacheWrite: &zero}})
	if cost, ok := p.Evaluate(e); cost != 0 || !ok {
		t.Fatalf("free cost=%v priced=%v", cost, ok)
	}
	authoritative := 9.0
	e.CostUSD = &authoritative
	if cost, ok := p.Evaluate(e); cost != 9 || !ok {
		t.Fatal(cost, ok)
	}
}

// event prices 1M input tokens for provider/model at ts.
func event(provider, name string, ts time.Time) model.UsageEvent {
	e := model.UsageEvent{Provider: provider, Model: name, Tokens: model.Tokens{Input: 1000000}}
	if !ts.IsZero() {
		e.Timestamp = ts
	}
	return e
}

func rate(v float64) *float64 { return &v }

// TestPriceAndMultiplierLookup covers the regression behind 0.1.x: a relay
// selling at 0.3× with a $3/M input price must cost $0.90, not $3.
func TestPriceAndMultiplierLookup(t *testing.T) {
	cases := []struct {
		name  string
		rules []Rule
		e     model.UsageEvent
		cost  float64
		known bool
	}{
		{
			name: "gateway multiplier with cc-switch model price",
			// What cc-switch import produced before the fix: a model rule with
			// Multiplier 1 that cancelled the relay's own 0.3.
			rules: []Rule{
				{Model: "sonnet-proxy", Input: rate(3), Output: rate(15), CacheRead: rate(0.3), CacheWrite: rate(0), Source: "cc-switch"},
				{Provider: "relay", Multiplier: 0.3, Source: "cc-switch"},
			},
			e:     event("relay", "sonnet-proxy", time.Time{}),
			cost:  0.9,
			known: true,
		},
		{
			name: "provider multiplier only",
			rules: []Rule{
				{Provider: "relay", Multiplier: 0.3},
			},
			e:    event("relay", "claude-sonnet-4-5", time.Time{}),
			cost: 0, known: true, // filled from the catalog below
		},
		{
			name: "model price only keeps multiplier 1",
			rules: []Rule{
				{Model: "sonnet-proxy", Input: rate(3), Output: rate(15), CacheRead: rate(0.3), CacheWrite: rate(0)},
			},
			e:    event("relay", "sonnet-proxy", time.Time{}),
			cost: 3, known: true,
		},
		{
			name: "model multiplier 0 is unset and does not zero the cost",
			rules: []Rule{
				{Provider: "relay", Multiplier: 0.3},
				{Model: "sonnet-proxy", Input: rate(3), Output: rate(15), CacheRead: rate(0.3), CacheWrite: rate(0)},
			},
			e:    event("relay", "sonnet-proxy", time.Time{}),
			cost: 0.9, known: true,
		},
		{
			name: "provider model rule with explicit multiplier wins",
			rules: []Rule{
				{Provider: "relay", Multiplier: 0.3},
				{Provider: "relay", Model: "sonnet-proxy", Input: rate(3), Output: rate(15), CacheRead: rate(0.3), CacheWrite: rate(0), Multiplier: 0.5},
			},
			e:    event("relay", "sonnet-proxy", time.Time{}),
			cost: 1.5, known: true,
		},
		{
			name: "user model rule beats imported model rule",
			rules: []Rule{
				{Model: "sonnet-proxy", Input: rate(3), Output: rate(15), CacheRead: rate(0.3), CacheWrite: rate(0), Source: "cc-switch"},
				{Model: "sonnet-proxy", Input: rate(1), Output: rate(2), CacheRead: rate(0), CacheWrite: rate(0), Source: "user"},
			},
			e:    event("relay", "sonnet-proxy", time.Time{}),
			cost: 1, known: true,
		},
		{
			name: "user model rule price with imported provider multiplier",
			rules: []Rule{
				{Model: "sonnet-proxy", Input: rate(5), Output: rate(10), CacheRead: rate(0), CacheWrite: rate(0), Source: "user"},
				{Provider: "relay", Multiplier: 0.4, Source: "cc-switch"},
			},
			e:    event("relay", "sonnet-proxy", time.Time{}),
			cost: 2, known: true,
		},
		{
			name: "imported model multiplier ignored, provider rule still applies",
			rules: []Rule{
				{Model: "sonnet-proxy", Input: rate(5), Output: rate(10), CacheRead: rate(0), CacheWrite: rate(0), Multiplier: 9, Source: "cc-switch"},
				{Provider: "relay", Multiplier: 2, Source: "user"},
			},
			e:    event("relay", "sonnet-proxy", time.Time{}),
			cost: 10, known: true,
		},
		{
			// The multiplier chain ends at a model-wide rule, so a model
			// multiplier set on its own still scales the cost.
			name: "model multiplier alone still applies",
			rules: []Rule{
				{Model: "sonnet-proxy", Input: rate(3), Output: rate(15), CacheRead: rate(0.3), CacheWrite: rate(0), Multiplier: 2},
			},
			e:    event("relay", "sonnet-proxy", time.Time{}),
			cost: 6, known: true,
		},
		{
			name: "provider multiplier wins over model multiplier",
			rules: []Rule{
				{Provider: "relay", Multiplier: 0.3},
				{Model: "sonnet-proxy", Input: rate(3), Output: rate(15), CacheRead: rate(0.3), CacheWrite: rate(0), Multiplier: 2},
			},
			e:    event("relay", "sonnet-proxy", time.Time{}),
			cost: 0.9, known: true,
		},
		{
			// What an old cc-switch library stored: model rules with
			// Multiplier 1. They only matter when no provider rule exists,
			// and then the result is the same 1 as before.
			name: "old cc-switch model multiplier 1 with relay multiplier",
			rules: []Rule{
				{Model: "sonnet-proxy", Input: rate(1), Output: rate(1), CacheRead: rate(0), CacheWrite: rate(0), Multiplier: 1, Source: "cc-switch"},
				{Provider: "relay", Multiplier: 0.3, Source: "cc-switch"},
			},
			e:    event("relay", "sonnet-proxy", time.Time{}),
			cost: 0.3, known: true,
		},
		{
			// docs/SPEC.md §7: a provider may carry its own unit prices.
			name: "provider rule rates price that provider's models",
			rules: []Rule{
				{Provider: "relay", Input: rate(2), Output: rate(2), CacheRead: rate(0), CacheWrite: rate(0)},
			},
			e:    event("relay", "never-seen-model", time.Time{}),
			cost: 2, known: true,
		},
		{
			name: "provider rule rates override the catalog",
			rules: []Rule{
				{Provider: "relay", Input: rate(2)},
			},
			e:    event("relay", "claude-sonnet-4-5", time.Time{}),
			cost: 2, known: true,
		},
		{
			// Each rate class falls back on its own: the provider+model rule
			// wins for output, the model rule for input, the provider rule for
			// the cache classes.
			name: "per class fallback across the three rule shapes",
			rules: []Rule{
				{Provider: "relay", Model: "sonnet-proxy", Output: rate(1)},
				{Model: "sonnet-proxy", Input: rate(3)},
				{Provider: "relay", CacheRead: rate(0.5), CacheWrite: rate(0.25)},
			},
			e: model.UsageEvent{
				Provider: "relay",
				Model:    "sonnet-proxy",
				Tokens:   model.Tokens{Input: 1000000, Output: 1000000, CacheRead: 1000000, CacheWrite: 1000000},
			},
			cost: 4.75, known: true,
		},
		{
			name:  "provider rule prices only the classes it sets",
			rules: []Rule{{Provider: "relay", Input: rate(3)}},
			e:     event("relay", "never-seen-model", time.Time{}),
			cost:  3,
			known: true,
		},
		{
			name:  "provider rule leaves the classes it lacks unpriced",
			rules: []Rule{{Provider: "relay", Input: rate(3)}},
			e: model.UsageEvent{
				Provider: "relay",
				Model:    "never-seen-model",
				Tokens:   model.Tokens{Input: 1000000, Output: 1000000},
			},
			cost:  0,
			known: false,
		},
		{
			name: "log cost wins over every rule",
			rules: []Rule{
				{Provider: "relay", Multiplier: 0.3},
				{Model: "sonnet-proxy", Input: rate(3), Output: rate(15), CacheRead: rate(0.3), CacheWrite: rate(0)},
			},
			e: func() model.UsageEvent {
				e := event("relay", "sonnet-proxy", time.Time{})
				e.CostUSD = rate(7)
				return e
			}(),
			cost: 7, known: true,
		},
		{
			name: "partial model rule prices only the classes it sets",
			rules: []Rule{
				{Model: "sonnet-proxy", Input: rate(3)},
			},
			e:    event("relay", "sonnet-proxy", time.Time{}),
			cost: 3, known: true,
		},
		{
			name:  "model rule without provider still prices unknown models",
			rules: []Rule{{Model: "sonnet-proxy", Input: rate(3), Output: rate(15), CacheRead: rate(0.3), CacheWrite: rate(0)}},
			e:     event("other", "sonnet-proxy", time.Time{}),
			cost:  3,
			known: true,
		},
		{
			name:  "provider multiplier does not invent a price",
			rules: []Rule{{Provider: "relay", Multiplier: 0.3}},
			e:     event("relay", "never-seen-model", time.Time{}),
			cost:  0,
			known: false,
		},
	}
	p := New("")
	sonnet, _ := p.Lookup("claude-sonnet-4-5")
	for _, c := range cases {
		if c.cost == 0 && c.known {
			// Catalog-priced case: keep the expectation tied to the snapshot.
			c.cost = sonnet.Input * 0.3
		}
		p.SetRules(c.rules)
		cost, ok := p.Evaluate(c.e)
		if ok != c.known || !closeEnough(cost, c.cost) {
			t.Errorf("%s: cost=%v known=%v want cost=%v known=%v", c.name, cost, ok, c.cost, c.known)
		}
	}
}

// closeEnough compares costs computed through floating point multipliers.
func closeEnough(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}

// TestRulesEffectiveFrom checks that the rule chosen is the latest one whose
// From is at or before the event's timestamp.
func TestRulesEffectiveFrom(t *testing.T) {
	p := New("")
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	p.SetRules([]Rule{
		{Model: "dated", Input: rate(2), From: start.AddDate(0, 0, 10)},
		{Model: "dated", Input: rate(1)},
	})
	cases := []struct {
		name string
		ts   time.Time
		cost float64
	}{
		{"before any dated rule", start, 1},
		{"on the later rule's start", start.AddDate(0, 0, 10), 2},
		{"after the later rule", start.AddDate(0, 1, 0), 2},
	}
	for _, c := range cases {
		if cost, ok := p.Evaluate(event("relay", "dated", c.ts)); !ok || cost != c.cost {
			t.Errorf("%s: cost=%v known=%v want %v", c.name, cost, ok, c.cost)
		}
	}
	// A future rule never prices an event that predates it, and the fallback
	// for events without a timestamp uses the rules effective now.
	p.SetRules([]Rule{{Model: "dated", Input: rate(9), From: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}})
	if cost, ok := p.Evaluate(event("relay", "dated", start)); ok {
		t.Fatalf("future rule priced a past event: %v", cost)
	}
	if _, ok := p.Evaluate(event("relay", "dated", time.Time{})); ok {
		t.Fatal("future rule must not price a timestamp-less event either")
	}
	if p.HasPrice("relay", "dated") {
		t.Fatal("HasPrice should ignore rules that are not effective yet")
	}
	p.SetRules([]Rule{{Model: "dated", Input: rate(9), From: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)}})
	if cost, ok := p.Evaluate(event("relay", "dated", time.Time{})); !ok || cost != 9 {
		t.Fatalf("past rule on timestamp-less event: cost=%v known=%v", cost, ok)
	}
}

// TestRuleEffectiveFromTies checks that equally fresh rules prefer the user one
// and that an undated rule still governs events before any dated rule starts.
func TestRuleEffectiveFromTies(t *testing.T) {
	p := New("")
	from := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)
	p.SetRules([]Rule{
		{Provider: "relay", Multiplier: 0.1, Source: "cc-switch"},
		{Provider: "relay", Multiplier: 0.3, From: from, Source: "cc-switch"},
		{Provider: "relay", Multiplier: 0.7, From: from, Source: "user"},
	})
	if cost, _ := p.Evaluate(event("relay", "claude-sonnet-4-5", from.Add(time.Hour))); cost != sonnetCost(p)*0.7 {
		t.Fatalf("tie must prefer the user rule: %v", cost)
	}
	// Before the dated rules start, only the undated one is effective.
	if cost, _ := p.Evaluate(event("relay", "claude-sonnet-4-5", from.Add(-time.Hour))); cost != sonnetCost(p)*0.1 {
		t.Fatalf("undated rule must still apply: %v", cost)
	}
}

func sonnetCost(p *Pricer) float64 {
	catalog, _ := p.Lookup("claude-sonnet-4-5")
	return catalog.Input
}
