package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/source"
	"github.com/zzstar101/mytoken/internal/store"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCCSwitchImport(t *testing.T) {
	for _, column := range []bool{true, false} {
		t.Run(map[bool]string{true: "local-column", false: "upstream-meta"}[column], func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "cc.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			extra := ""
			values := ""
			if column {
				extra = ",cost_multiplier TEXT"
				values = ",'0.5'"
			}
			_, err = db.Exec(`PRAGMA journal_mode=WAL;CREATE TABLE model_pricing(model_id TEXT,display_name TEXT,input_cost_per_million TEXT,output_cost_per_million TEXT,cache_read_cost_per_million TEXT,cache_creation_cost_per_million TEXT);INSERT INTO model_pricing VALUES('fixture','Fixture','2','3','0.2','4'),('fixture-20260101','Dated fixture','99','3','0.2','4');CREATE TABLE providers(id TEXT,app_type TEXT,name TEXT,meta TEXT` + extra + `);INSERT INTO providers VALUES('p','codex','Relay','{"costMultiplier":"0.5"}'` + values + `);`)
			if err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			p := pricing.New("")
			s := NewSettingsWithSources(st, p, source.NewCCSwitch(path))
			in := 7.0
			if err = s.SetPriceRules(ctx, []PriceRule{{Model: "user", Input: &in, Source: "user"}, {Model: "old", Source: "cc-switch"}}); err != nil {
				t.Fatal(err)
			}
			// An active writer in WAL mode must not hide committed prices or block import.
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err = tx.Exec("UPDATE model_pricing SET input_cost_per_million='99'"); err != nil {
				t.Fatal(err)
			}
			n, err := s.ImportCCSwitch(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if n != 3 {
				t.Fatalf("imported=%d", n)
			}
			rules, err := s.PriceRules(ctx)
			if err != nil || len(rules) != 4 {
				t.Fatalf("rules=%+v err=%v", rules, err)
			}
			// Imported model rules carry no multiplier: the provider rule owns it.
			for _, r := range rules {
				if r.Model == "" {
					continue
				}
				if r.Multiplier != 0 {
					t.Fatalf("model rule %q/%q must leave the multiplier unset, got %v", r.Provider, r.Model, r.Multiplier)
				}
			}
			e := model.UsageEvent{Model: "fixture", Provider: "Relay", Tokens: model.Tokens{Input: 1000000}}
			if cost, ok := p.Evaluate(e); !ok || cost != 1 {
				t.Fatal(cost, ok)
			}
			n, err = s.ImportCCSwitch(ctx)
			if err != nil || n != 3 {
				t.Fatal(n, err)
			}
			if _, err = osStatMissingCCSwitch(ctx, filepath.Join(t.TempDir(), "absent.db")); err == nil {
				t.Fatal("missing source must fail")
			}
		})
	}
}
func osStatMissingCCSwitch(ctx context.Context, path string) ([]PriceRule, error) {
	rules, err := source.NewCCSwitch(path).PriceRules(ctx)
	if err != nil {
		return nil, err
	}
	return fromPricingRules(rules), nil
}

// TestPriceRuleEffectiveFrom covers dated rules end to end: storage keeps them,
// and Evaluate picks the rule that was effective when the event happened.
func TestPriceRuleEffectiveFrom(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := pricing.New("")
	s := NewSettings(st, p)
	cheap, pricey := 1.0, 2.0
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if err = s.SetPriceRules(ctx, []PriceRule{
		{Model: "dated", Input: &cheap, Output: &cheap, CacheRead: &cheap, CacheWrite: &cheap},
		{Model: "dated", Input: &pricey, Output: &pricey, CacheRead: &pricey, CacheWrite: &pricey, From: from},
	}); err != nil {
		t.Fatal(err)
	}
	rules, err := s.PriceRules(ctx)
	if err != nil || len(rules) != 2 {
		t.Fatalf("rules=%+v err=%v", rules, err)
	}
	if !rules[1].From.Equal(from) {
		t.Fatalf("from=%v", rules[1].From)
	}
	// A dated rule is stored with its "from"; only the zero value is omitted.
	raw, err := st.Setting(ctx, "price-rules")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"from":"2026-02-01T00:00:00Z"`) {
		t.Fatalf("dated rule not stored: %s", raw)
	}
	// The same selector with the same start instant is still a duplicate.
	if err = s.SetPriceRules(ctx, []PriceRule{{Model: "dated", Input: &cheap}, {Model: "dated", Input: &pricey, From: from}, {Model: "dated", Input: &pricey, From: from}}); err == nil {
		t.Fatal("duplicate dated rule accepted")
	}
	before := model.UsageEvent{Model: "dated", Timestamp: from.Add(-time.Hour), Tokens: model.Tokens{Input: 1000000}}
	if cost, ok := p.Evaluate(before); !ok || cost != 1 {
		t.Fatal(cost, ok)
	}
	after := model.UsageEvent{Model: "dated", Timestamp: from, Tokens: model.Tokens{Input: 1000000}}
	if cost, ok := p.Evaluate(after); !ok || cost != 2 {
		t.Fatal(cost, ok)
	}
	// A provider multiplier applies whatever the model rule prices.
	mult := 0.5
	if err = s.SetPriceRules(ctx, []PriceRule{{Provider: "relay", Multiplier: mult}, {Model: "dated", Input: &pricey, Output: &pricey, CacheRead: &pricey, CacheWrite: &pricey, From: from}}); err != nil {
		t.Fatal(err)
	}
	after.Provider = "relay"
	if cost, ok := p.Evaluate(after); !ok || cost != 1 {
		t.Fatal(cost, ok)
	}
}

// TestPriceRulesReadOldFormat checks that rules written before From existed
// load losslessly (zero From = always effective), that a model-level
// multiplier set on its own still prices the same way it did, and that a
// provider multiplier now takes precedence over that fallback.
func TestPriceRulesReadOldFormat(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := pricing.New("")
	s := NewSettings(st, p)
	old := `[{"provider":"relay","multiplier":0.3,"source":"user"},{"model":"legacy","multiplier":0.5,"input":2,"output":3,"source":"user"}]`
	if err = st.SetSetting(ctx, "price-rules", old); err != nil {
		t.Fatal(err)
	}
	if err = LoadPricingSettings(ctx, st, p); err != nil {
		t.Fatal(err)
	}
	rules, err := s.PriceRules(ctx)
	if err != nil || len(rules) != 2 {
		t.Fatalf("rules=%+v err=%v", rules, err)
	}
	for _, r := range rules {
		if !r.From.IsZero() {
			t.Fatalf("old rule gained a start time: %+v", r)
		}
	}
	relay := model.UsageEvent{Model: "legacy", Provider: "relay", Tokens: model.Tokens{Input: 1000000}}
	other := model.UsageEvent{Model: "legacy", Provider: "gateway", Tokens: model.Tokens{Input: 1000000}}
	// Without a provider rule the model-level multiplier is the last fallback,
	// so an old model-only config keeps computing exactly what it did before.
	if cost, ok := p.Evaluate(other); !ok || cost != 2*0.5 {
		t.Fatal(cost, ok)
	}
	// With one, the provider multiplier wins: 0.3, not the model's 0.5.
	if cost, ok := p.Evaluate(relay); !ok || cost != 2*0.3 {
		t.Fatal(cost, ok)
	}
	// Round-tripping keeps the stored shape readable by an older binary, and
	// the always-effective rules carry no "from" field at all.
	if err = s.SetPriceRules(ctx, rules); err != nil {
		t.Fatal(err)
	}
	raw, err := st.Setting(ctx, "price-rules")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, `"from"`) {
		t.Fatalf("zero From must be omitted: %s", raw)
	}
	var back []PriceRule
	if err = json.Unmarshal([]byte(raw), &back); err != nil || len(back) != 2 || !back[0].From.IsZero() {
		t.Fatalf("raw=%s err=%v", raw, err)
	}
}

func TestSettingsRepriceAndAliases(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := pricing.New("")
	s := NewSettings(st, p)
	zero := 0.0
	events := []model.UsageEvent{{Harness: model.Codex, SessionID: "s", DedupKey: "a", Model: "unlisted", Tokens: model.Tokens{Input: 1000000}}, {Harness: model.Codex, SessionID: "s", DedupKey: "b", Model: "unlisted", CostUSD: &zero}}
	if err = st.Commit(ctx, model.Codex, "log", harness.Batch{Events: events}, nil); err != nil {
		t.Fatal(err)
	}
	ch, stop := st.Subscribe()
	defer stop()
	rate := 2.0
	if err = s.SetPriceRules(ctx, []PriceRule{{Model: "unlisted", Input: &rate}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	default:
		t.Fatal("no notification")
	}
	total, err := NewService(st).Totals(ctx, Filter{})
	if err != nil || total.Unpriced != 0 || total.CostUSD != 2 {
		t.Fatal(total, err)
	}
	if err = s.SetPriceRules(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.SetModelAliases(ctx, []ModelAlias{{From: "unlisted", To: "gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	_, _, items, err := NewService(st).Session(ctx, model.Codex, "s")
	if err != nil || items[0].Model != "gpt-4o" || !items[0].Priced {
		t.Fatal(items, err)
	}
	if err = s.SetModelAliases(ctx, nil); err != nil {
		t.Fatal(err)
	}
	total, err = NewService(st).Totals(ctx, Filter{})
	if err != nil || total.Unpriced != 1 || total.CostUSD != 0 {
		t.Fatal(total, err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err = s.SetPriceRules(cancelCtx, []PriceRule{{Model: "unlisted", Input: &rate}}); err == nil {
		t.Fatal("cancelled mutation succeeded")
	}
	rules, _ := s.PriceRules(ctx)
	if len(rules) != 0 {
		t.Fatal(rules)
	}
}
