package reconcile

import (
	"context"
	"math"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/store"
)

func TestRandomizedConservation(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	site := relay.Site{Origin: "https://relay.example", KeyID: "key-id", Kind: relay.KindNewAPI, Providers: []string{"proxy"}}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewSource(7))
	const n = 500
	events := make([]model.UsageEvent, n)
	res := make([]store.Resolution, n)
	var bills []relay.Bill
	want := map[Category]int64{Matched: 100, PriceDiff: 100, TokenSemantics: 200, EventOnly: 100, BillOnly: 1, Refund: 1}
	for i := range events {
		tok := model.Tokens{Input: int64(1000 + i), Output: int64(100 + i), CacheRead: 100, CacheWrite: 50}
		at := start.Add(time.Duration(i) * 10 * time.Minute)
		events[i] = model.UsageEvent{SessionID: "s", DedupKey: at.String(), Timestamp: at, Model: "alias", Tokens: tok, BaseURL: site.Origin + "/v1"}
		res[i] = store.Resolution{Provider: "proxy", Cost: 1, Priced: true}
		if i%5 == 4 {
			continue
		}
		b := relay.Bill{Origin: site.Origin, KeyID: site.KeyID, ID: int64(i + 1), At: at.Add(time.Duration(rng.Intn(239)-119) * time.Second), Type: "consume", Model: "MODEL-20260101", Tokens: tok, ChargedUSD: 1, Ratios: relay.Ratios{Model: 1, Group: 1, Completion: 1}}
		switch i % 5 {
		case 1:
			b.ChargedUSD = 2
		case 2:
			b.Tokens.Input += tok.CacheRead
		case 3:
			b.Tokens.Input += tok.CacheRead + tok.CacheWrite
		}
		bills = append(bills, b)
	}
	bills = append(bills, relay.Bill{Origin: site.Origin, KeyID: site.KeyID, ID: 1001, At: start, Type: "consume", Model: "other", ChargedUSD: 3}, relay.Bill{Origin: site.Origin, KeyID: site.KeyID, ID: 1002, At: start, Type: "refund", ChargedUSD: -.5})
	if err = st.Exec(ctx, `INSERT INTO settings(key,value) VALUES('model-aliases','[{"from":"alias","to":"model"}]')`); err != nil {
		t.Fatal(err)
	}
	if err = st.Commit(ctx, model.ClaudeCode, "source", harness.Batch{Events: events}, res); err != nil {
		t.Fatal(err)
	}
	if err = st.PutRelayBills(ctx, bills); err != nil {
		t.Fatal(err)
	}
	report, err := Reconcile(ctx, st, site, start.Add(-time.Hour), start.Add(100*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var local, charged float64
	for _, line := range report.Lines {
		if line.Count != want[line.Category] {
			t.Errorf("%s count=%d want=%d", line.Category, line.Count, want[line.Category])
		}
		local += line.LocalUSD
		charged += line.ChargedUSD
		delete(want, line.Category)
	}
	if len(want) != 0 {
		t.Errorf("missing categories %v", want)
	}
	if math.Abs(local-report.LocalUSD) > 1e-9 || math.Abs(charged-report.ChargedUSD) > 1e-9 || local != 500 || charged != 502.5 {
		t.Fatalf("conservation local %g/%g charged %g/%g", local, report.LocalUSD, charged, report.ChargedUSD)
	}
	var modelLocal, dayLocal, modelCharged, dayCharged float64
	for _, r := range report.ByModel {
		modelLocal += r.LocalUSD
		modelCharged += r.ChargedUSD
	}
	for _, r := range report.ByDay {
		dayLocal += r.LocalUSD
		dayCharged += r.ChargedUSD
	}
	if modelLocal != local || dayLocal != local || modelCharged != charged || dayCharged != charged {
		t.Fatal("grouped totals differ")
	}
	// Reconciliation is repeatable: derived matches cannot change either ledger.
	again, err := Reconcile(ctx, st, site, start.Add(-time.Hour), start.Add(100*time.Hour))
	if err != nil || again.ChargedUSD != report.ChargedUSD {
		t.Fatal(again, err)
	}
}
