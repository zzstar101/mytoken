package pricing

import (
	"github.com/zzstar/mytoken/internal/model"
	"testing"
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

func TestRulePrecedence(t *testing.T) {
	p := New("")
	input := 2.0
	zero := 0.0
	p.SetRules([]Rule{{Provider: "relay", Multiplier: 0.5}, {Model: "claude-opus-4-6", Input: &input, Multiplier: 2}, {Provider: "relay", Model: "claude-opus-4-6", Output: &zero}})
	e := model.UsageEvent{Provider: "relay", Model: "claude-opus-4-6", Tokens: model.Tokens{Input: 1000000, Output: 1000000}}
	catalog, _ := p.Lookup(e.Model)
	if cost, ok := p.Evaluate(e); !ok || cost != catalog.Input {
		t.Fatalf("specific cost=%v known=%v", cost, ok)
	}
	e.Provider = "other"
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
