package reconcile

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
func TestExactIDsReserveBeforeFallbackAndNearestCacheVariant(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local)
	site := relay.Site{Origin: "https://relay.example", KeyID: "k", Kind: relay.KindNewAPI, Providers: []string{"other", "proxy"}}
	events := []model.UsageEvent{
		{DedupKey: "a", SessionID: "s", RequestID: "upstream-a", Timestamp: at, Model: "alias", Tokens: model.Tokens{Input: 100, Output: 20}},
		{DedupKey: "b", SessionID: "s", Timestamp: at.Add(time.Minute), Model: "alias", Tokens: model.Tokens{Input: 100, Output: 20}},
		{DedupKey: "c", SessionID: "s", Timestamp: at.Add(2 * time.Hour), Model: "alias", Tokens: model.Tokens{Input: 100, Output: 30}},
		{DedupKey: "d", SessionID: "s", Timestamp: at.Add(2*time.Hour + time.Minute), Model: "alias", Tokens: model.Tokens{Input: 50, CacheRead: 50, Output: 30}},
	}
	res := []store.Resolution{{Provider: "proxy", Cost: .1, Priced: true}, {Provider: "proxy", Cost: .2, Priced: true}, {Provider: "proxy", Cost: .3, Priced: true}, {Provider: "proxy", Cost: .4, Priced: true}}
	if err := st.Exec(ctx, `INSERT INTO settings VALUES('model-aliases','[{"from":"alias","to":"real-model","provider":"proxy"}]')`); err != nil {
		t.Fatal(err)
	}
	if err := st.Commit(ctx, model.Codex, "source", harness.Batch{Events: events}, res); err != nil {
		t.Fatal(err)
	}
	bills := []relay.Bill{
		{Origin: site.Origin, KeyID: site.KeyID, ID: 1, Type: "consume", At: at.Add(5 * time.Second), Model: "alias", Tokens: events[0].Tokens, ChargedUSD: .2},
		{Origin: site.Origin, KeyID: site.KeyID, ID: 2, Type: "consume", At: at.Add(30 * time.Second), UpstreamRequestID: "upstream-a", Model: "different", Tokens: model.Tokens{Input: 999}, ChargedUSD: .1},
		{Origin: site.Origin, KeyID: site.KeyID, ID: 3, Type: "consume", At: at.Add(2*time.Hour + 59*time.Second), Model: "real-model-20260101", Tokens: events[2].Tokens, ChargedUSD: .4},
	}
	if err := st.PutRelayBills(ctx, bills); err != nil {
		t.Fatal(err)
	}
	r, err := Reconcile(ctx, st, site, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(r.LocalUSD-1) > 1e-9 || math.Abs(r.ChargedUSD-.7) > 1e-9 {
		t.Fatal(r)
	}
	for id, want := range map[int]string{1: "b", 2: "a", 3: "d"} {
		var got string
		if err = st.DB().QueryRow("SELECT match_dedup_key FROM relay_bills WHERE id=?", id).Scan(&got); err != nil || got != want {
			t.Fatalf("bill %d matched %s want %s (%v)", id, got, want, err)
		}
	}
	var semantics bool
	for _, line := range r.Lines {
		semantics = semantics || line.Category == TokenSemantics && line.Count == 1
	}
	if !semantics {
		t.Fatal(r.Lines)
	}
}
func TestDailyDecompositionAndUnknownCatalog(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local)
	site := relay.Site{Origin: "https://relay.example", KeyID: "k", Kind: relay.KindSub2API, Providers: []string{"proxy"}}
	events := []model.UsageEvent{{DedupKey: "known", SessionID: "s", Timestamp: at, Model: "gpt-4o", Tokens: model.Tokens{Input: 1000000}}, {DedupKey: "unknown", SessionID: "s", Timestamp: at.AddDate(0, 0, 1), Model: "not-in-catalog", Tokens: model.Tokens{Input: 100}}, {DedupKey: "local-only", SessionID: "s", Timestamp: at.AddDate(0, 0, 2), Model: "gpt-4o", Tokens: model.Tokens{Input: 1000}}}
	res := []store.Resolution{{Provider: "proxy", Cost: 1, Priced: true}, {Provider: "proxy", Cost: 2, Priced: true}, {Provider: "proxy", Cost: 3, Priced: true}}
	if err := st.Commit(ctx, model.Codex, "source", harness.Batch{Events: events}, res); err != nil {
		t.Fatal(err)
	}
	days := []relay.Daily{{Origin: site.Origin, KeyID: site.KeyID, Day: "2026-01-01", Model: "gpt-4o", Requests: 1, ListUSD: 5, ChargedUSD: 2}, {Origin: site.Origin, KeyID: site.KeyID, Day: "2026-01-02", Model: "not-in-catalog", Requests: 1, ListUSD: 4, ChargedUSD: 3}}
	if err := st.PutRelayDaily(ctx, days); err != nil {
		t.Fatal(err)
	}
	r, err := Reconcile(ctx, st, site, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if r.LocalUSD != 6 || r.ChargedUSD != 5 || len(r.ByDay) != 3 {
		t.Fatal(r)
	}
	known, unknown := r.ByDay[0], r.ByDay[1]
	if known.MultiplierDiffUSD == nil || known.UsageDiffUSD == nil || known.Unpriced {
		t.Fatal(known)
	}
	if math.Abs(*known.MultiplierDiffUSD+*known.UsageDiffUSD-(known.ChargedUSD-known.LocalUSD)) > 1e-9 {
		t.Fatal("daily decomposition does not conserve")
	}
	if !unknown.Unpriced || unknown.MultiplierDiffUSD != nil || unknown.UsageDiffUSD != nil || r.ImpliedMultiplier != nil {
		t.Fatal("unknown catalog fabricated a decomposition", r)
	}
}
func TestMixedOriginRollupsCannotAttributeOtherSites(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local)
	site := relay.Site{Origin: "https://relay.example", KeyID: "k", Kind: relay.KindNewAPI}
	events := []model.UsageEvent{{DedupKey: "ours-a", SessionID: "s", Timestamp: at, Model: "m", BaseURL: site.Origin + "/v1", Tokens: model.Tokens{Input: 10}}, {DedupKey: "ours-b", SessionID: "s", Timestamp: at.Add(time.Minute), Model: "m", BaseURL: site.Origin + "/api", Tokens: model.Tokens{Input: 11}}, {DedupKey: "theirs", SessionID: "s", Timestamp: at, Model: "m", BaseURL: "https://other.example/v1", Tokens: model.Tokens{Input: 20}}}
	if err := st.Commit(ctx, model.Codex, "source", harness.Batch{Events: events}, []store.Resolution{{Cost: 1, Priced: true}, {Cost: 2, Priced: true}, {Cost: 30, Priced: true}}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRelayBills(ctx, []relay.Bill{{Origin: site.Origin, KeyID: site.KeyID, ID: 1, At: at, Type: "consume", Model: "m", Tokens: events[0].Tokens, ChargedUSD: 1}}); err != nil {
		t.Fatal(err)
	}
	r, err := Reconcile(ctx, st, site, time.Time{}, time.Time{})
	if err != nil || r.LocalUSD != 3 {
		t.Fatal("mixed-origin summary leaked or double-counted", r, err)
	}
}
