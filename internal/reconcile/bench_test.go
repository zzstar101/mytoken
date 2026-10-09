package reconcile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/store"
)

func performanceFixture(tb testing.TB) (*store.Store, relay.Site) {
	tb.Helper()
	st, err := store.Open(filepath.Join(tb.TempDir(), "benchmark.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { st.Close() })
	site := relay.Site{Origin: "https://relay.example", KeyID: "123456789abc", Kind: relay.KindNewAPI, Layers: relay.LayerBills, Providers: []string{"gateway"}}
	ctx := context.Background()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var bills []relay.Bill
	for batch := 0; batch < 400; batch++ {
		events := make([]model.UsageEvent, 500)
		res := make([]store.Resolution, 500)
		for j := range events {
			i := batch*500 + j
			tm := at.Add(time.Duration(i) * 10 * time.Second)
			tok := model.Tokens{Input: int64(1000 + i%5000), Output: int64(50 + i%317), CacheRead: 500, CacheWrite: 10}
			events[j] = model.UsageEvent{SessionID: fmt.Sprintf("s-%03d", batch), DedupKey: fmt.Sprintf("event-%08d", i), RequestID: fmt.Sprintf("resp-local-%08d", i), Timestamp: tm, Model: "gpt-4o", Tokens: tok}
			res[j] = store.Resolution{Provider: "gateway", Cost: .01, Priced: true}
			if i%20 == 0 {
				bills = append(bills, relay.Bill{Origin: site.Origin, KeyID: site.KeyID, ID: int64(i + 1), At: tm.Add(17 * time.Second), Type: "consume", Model: "gpt-4o-20260101", Tokens: tok, ChargedUSD: .01, Ratios: relay.Ratios{Model: 1, Group: 1, Completion: 2, Cache: .1, CacheCreate: 1.25}})
			}
		}
		if err = st.Commit(ctx, model.Codex, fmt.Sprint(batch), harness.Batch{Events: events}, res); err != nil {
			tb.Fatal(err)
		}
	}
	if err = st.PutRelayBills(ctx, bills); err != nil {
		tb.Fatal(err)
	}
	return st, site
}
func BenchmarkReconcile(b *testing.B) {
	st, site := performanceFixture(b)
	if path := os.Getenv("MYTOKEN_RECONCILE_PROFILE"); path != "" {
		f, err := os.Create(path)
		if err != nil {
			b.Fatal(err)
		}
		defer f.Close()
		if err = pprof.StartCPUProfile(f); err != nil {
			b.Fatal(err)
		}
		defer pprof.StopCPUProfile()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := Reconcile(context.Background(), st, site, time.Time{}, time.Time{})
		if err != nil {
			b.Fatal(err)
		}
		if len(r.Lines) != 2 {
			b.Fatalf("unexpected lines %+v", r.Lines)
		}
	}
}
func TestReconcilePerformanceBudget(t *testing.T) {
	if os.Getenv("MYTOKEN_STRICT_RECONCILE") == "" {
		t.Skip("set MYTOKEN_STRICT_RECONCILE=1 for the 10K x 200K performance gate")
	}
	st, site := performanceFixture(t)
	at := time.Now()
	_, err := Reconcile(context.Background(), st, site, time.Time{}, time.Time{})
	elapsed := time.Since(at)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("10K bills x 200K events: %s", elapsed)
	if elapsed > 2*time.Second {
		t.Fatalf("initial matching exceeded 2s: %s", elapsed)
	}
	at = time.Now()
	if _, err = StoredReport(context.Background(), st, site, time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	elapsed = time.Since(at)
	t.Logf("persisted report: %s", elapsed)
	if elapsed > 200*time.Millisecond {
		t.Errorf("report exceeded 200ms: %s", elapsed)
	}
	addIncrementalBills(t, st, site)
	at = time.Now()
	if err = ReconcilePending(context.Background(), st, site); err != nil {
		t.Fatal(err)
	}
	elapsed = time.Since(at)
	t.Logf("100 new bills incremental: %s", elapsed)
	if elapsed > 50*time.Millisecond {
		t.Errorf("incremental matching exceeded 50ms: %s", elapsed)
	}
}

func addIncrementalBills(tb testing.TB, st *store.Store, site relay.Site) {
	tb.Helper()
	var bills []relay.Bill
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for n := 0; n < 100; n++ {
		i := n*20 + 1
		bills = append(bills, relay.Bill{Origin: site.Origin, KeyID: site.KeyID, ID: int64(200001 + n), RequestID: fmt.Sprintf("resp-local-%08d", i), At: at.Add(time.Duration(i)*10*time.Second + 17*time.Second), Type: "consume", Model: "gpt-4o", Tokens: model.Tokens{Input: int64(1000 + i%5000), Output: int64(50 + i%317), CacheRead: 500, CacheWrite: 10}, ChargedUSD: .01})
	}
	if err := st.PutRelayBills(context.Background(), bills); err != nil {
		tb.Fatal(err)
	}
}
func BenchmarkReconcileReport(b *testing.B) {
	st, site := performanceFixture(b)
	if _, err := Reconcile(context.Background(), st, site, time.Time{}, time.Time{}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := StoredReport(context.Background(), st, site, time.Time{}, time.Time{}); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkReconcileIncremental(b *testing.B) {
	st, site := performanceFixture(b)
	if _, err := Reconcile(context.Background(), st, site, time.Time{}, time.Time{}); err != nil {
		b.Fatal(err)
	}
	addIncrementalBills(b, st, site)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if err := st.Exec(context.Background(), `UPDATE relay_bill_data SET match_harness='',match_dedup_key='',match_kind='' WHERE id>=200001`); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := ReconcilePending(context.Background(), st, site); err != nil {
			b.Fatal(err)
		}
	}
}
