package query

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/store"
)

// TestRelayRulesRoundTrip covers the gateway-facing half of docs/RELAY.md §5.5:
// AppendRelayRules stores gateway ratios as relay: rules, RelayRules lists only
// those, and a jittering ratio does not rewrite the table.
func TestRelayRulesRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := pricing.New("")
	s := NewSettings(st, p)
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	origin := "https://relay.example"

	user := 2.0
	if err = s.SetPriceRules(ctx, []PriceRule{{Provider: "XLAB", Model: "claude-sonnet-4-5", Input: &user, Source: "user"}}); err != nil {
		t.Fatal(err)
	}
	rules := []pricing.Rule{{Provider: "XLAB", Model: "claude-sonnet-4-5", Input: &user, Source: relay.RuleSource(origin, "k1"), From: at}}
	if err = s.AppendRelayRules(ctx, rules); err != nil {
		t.Fatal(err)
	}
	relayOnly, err := s.RelayRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The first rule a site gives prices the history before it, too.
	if len(relayOnly) != 1 || relayOnly[0].Source != relay.RuleSource(origin, "k1") || !relayOnly[0].From.IsZero() {
		t.Fatalf("relay rules=%+v", relayOnly)
	}
	// Appending the same prices again keeps the stored rule as it was.
	if err = s.AppendRelayRules(ctx, rules); err != nil {
		t.Fatal(err)
	}
	all, err := s.PriceRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("rules=%+v", all)
	}
	for _, r := range all {
		if r.Source == "user" && r.Model == "claude-sonnet-4-5" && *r.Input != user {
			t.Fatalf("user rule clobbered: %+v", r)
		}
	}
	// A real ratio move is dated from when it was seen: the old gateway rule
	// keeps pricing the time before, and the user's rule is left alone.
	moved, later := 2.08, at.AddDate(0, 1, 0)
	if err = s.AppendRelayRules(ctx, []pricing.Rule{{Provider: "XLAB", Model: "claude-sonnet-4-5", Input: &moved, Source: relay.RuleSource(origin, "k1"), From: later}}); err != nil {
		t.Fatal(err)
	}
	all, err = s.PriceRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("rules=%+v", all)
	}
	var sawOld, sawNew bool
	for _, r := range all {
		if r.Source == "user" && *r.Input != user {
			t.Fatalf("user rule changed: %+v", r)
		}
		if r.Source == relay.RuleSource(origin, "k1") {
			switch {
			case r.From.IsZero() && *r.Input == user:
				sawOld = true
			case r.From.Equal(later) && *r.Input == moved:
				sawNew = true
			default:
				t.Fatalf("unexpected gateway rule: %+v", r)
			}
		}
	}
	if !sawOld || !sawNew {
		t.Fatalf("want the old rule kept and the move appended: %+v", all)
	}
}

// TestRelayRuleBeatsCCSwitchImport is the priority rule of §5.5: a gateway's
// own ratios outrank the cc-switch import for the same model, and the user's
// own rule still outranks both.
func TestRelayRuleBeatsCCSwitchImport(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := pricing.New("")
	s := NewSettings(st, p)
	origin := "https://relay.example"

	user, relayPrice, ccPrice := 2.0, 4.0, 8.0
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	event := model.UsageEvent{Provider: "XLAB", Model: "claude-sonnet-4-5", Timestamp: at, Tokens: model.Tokens{Input: 1000000}}
	if err = s.SetPriceRules(ctx, []PriceRule{
		{Provider: "XLAB", Model: "claude-sonnet-4-5", Input: &ccPrice, Source: "cc-switch"},
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.AppendRelayRules(ctx, []pricing.Rule{{Provider: "XLAB", Model: "claude-sonnet-4-5", Input: &relayPrice, Source: relay.RuleSource(origin, "k1"), From: at}}); err != nil {
		t.Fatal(err)
	}
	if cost, ok := p.Evaluate(event); !ok || cost != relayPrice {
		t.Fatalf("cost=%v ok=%v, want the gateway price %v", cost, ok, relayPrice)
	}
	// The user's own rule still wins over both.
	if err = s.SetPriceRules(ctx, []PriceRule{
		{Provider: "XLAB", Model: "claude-sonnet-4-5", Input: &user, Source: "user"},
		{Provider: "XLAB", Model: "claude-sonnet-4-5", Input: &ccPrice, Source: "cc-switch"},
		{Provider: "XLAB", Model: "claude-sonnet-4-5", Input: &relayPrice, Source: relay.RuleSource(origin, "k1"), From: at},
	}); err != nil {
		t.Fatal(err)
	}
	if cost, ok := p.Evaluate(event); !ok || cost != user {
		t.Fatalf("cost=%v ok=%v, want the user price %v", cost, ok, user)
	}
}
