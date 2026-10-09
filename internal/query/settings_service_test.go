package query

import (
	"context"
	"database/sql"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/store"
	"path/filepath"
	"testing"
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
			s := NewSettings(st, p).(*settingsService)
			s.ccPath = path
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
	return importCCSwitch(ctx, path)
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
