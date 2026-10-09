package reconcile

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/store"
)

func TestPersistedReportIsReadOnlyAndIncrementalReservesMatches(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local)
	site := relay.Site{Origin: "https://relay.example", KeyID: "key", Kind: relay.KindNewAPI, Providers: []string{"proxy"}}
	if err := st.PutRelaySite(ctx, site); err != nil {
		t.Fatal(err)
	}
	tok := model.Tokens{Input: 100, Output: 10}
	events := []model.UsageEvent{{DedupKey: "a", SessionID: "s", Timestamp: at, Model: "gpt-4o", Tokens: tok}, {DedupKey: "b", RequestID: "upstream-b", SessionID: "s", Timestamp: at.Add(10 * time.Second), Model: "gpt-4o", Tokens: tok}}
	if err := st.Commit(ctx, model.Codex, "source", harness.Batch{Events: events}, []store.Resolution{{Provider: "proxy", Cost: 1, Priced: true}, {Provider: "proxy", Cost: 2, Priced: true}}); err != nil {
		t.Fatal(err)
	}
	bill := relay.Bill{Origin: site.Origin, KeyID: site.KeyID, ID: 1, At: at.Add(9 * time.Second), Type: "consume", Model: "gpt-4o", Tokens: tok, ChargedUSD: 2}
	if err := st.PutRelayBills(ctx, []relay.Bill{bill}); err != nil {
		t.Fatal(err)
	}
	want, err := Reconcile(ctx, st, site, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, nil, nil, nil)
	var before, after int64
	if err = st.DB().QueryRow("SELECT total_changes()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err = st.Exec(ctx, "PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Report(ctx, site.Origin, site.KeyID, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal("read-only report", err)
	}
	if err = st.DB().QueryRow("SELECT total_changes()").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || after != before {
		t.Fatalf("report changed assignments or result: before=%d after=%d\ngot=%+v\nwant=%+v", before, after, got, want)
	}
	if err = st.Exec(ctx, "PRAGMA query_only=OFF"); err != nil {
		t.Fatal(err)
	}
	// Exact IDs cannot steal a previously persisted assignment during incremental
	// sync. The weaker unclaimed event is still available as a fallback.
	bill.ID = 2
	bill.RequestID = "upstream-b"
	bill.At = at.Add(10 * time.Second)
	bill.ChargedUSD = 1
	if err = st.PutRelayBills(ctx, []relay.Bill{bill}); err != nil {
		t.Fatal(err)
	}
	if err = ReconcilePending(ctx, st, site); err != nil {
		t.Fatal(err)
	}
	matches, err := st.RelayMatches(ctx, site.Origin, site.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if matches[1].DedupKey != "b" || matches[2].DedupKey != "a" {
		t.Fatal(matches)
	}
	// An identical replay keeps assignments, but corrected upstream facts are
	// invalidated so the next sync re-evaluates them.
	if err = st.PutRelayBills(ctx, []relay.Bill{bill}); err != nil {
		t.Fatal(err)
	}
	matches, _ = st.RelayMatches(ctx, site.Origin, site.KeyID)
	if matches[2].DedupKey != "a" {
		t.Fatal("unchanged replay lost match", matches)
	}
	bill.Tokens.Input = 999
	if err = st.PutRelayBills(ctx, []relay.Bill{bill}); err != nil {
		t.Fatal(err)
	}
	matches, _ = st.RelayMatches(ctx, site.Origin, site.KeyID)
	if _, ok := matches[2]; ok {
		t.Fatal("changed bill retained stale assignment", matches)
	}
	if err = ReconcilePending(ctx, st, site); err != nil {
		t.Fatal(err)
	}
	matches, _ = st.RelayMatches(ctx, site.Origin, site.KeyID)
	if matches[2].Kind != string(BillOnly) {
		t.Fatal(matches)
	}
}
