package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/relay"
)

func TestRelayPersistenceAndRebuild(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	site := relay.Site{Origin: "https://relay.example", KeyID: "deadbeef0123", Kind: relay.KindNewAPI, QuotaPerUnit: 500000, Layers: relay.LayerBills, DetectedAt: time.Unix(100, 0).UTC(), Providers: []string{"relay"}}
	if err = st.PutRelaySite(ctx, site); err != nil {
		t.Fatal(err)
	}
	bill := relay.Bill{Origin: site.Origin, KeyID: site.KeyID, ID: 7, At: time.Unix(120, 0).UTC(), Type: "consume", Model: "model", Tokens: model.Tokens{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40, Reasoning: 5}, CacheWrite1h: 25, RequestID: "req-test", UpstreamRequestID: "resp-test", ChargedUSD: .25, Ratios: relay.Ratios{Model: 2, Group: .5, CacheCreate1h: 2}, Stream: true, LatencyMS: 19}
	if err = st.PutRelayBills(ctx, []relay.Bill{bill, bill}); err != nil {
		t.Fatal(err)
	}
	if err = st.PutRelayCursor(ctx, RelayCursor{Origin: site.Origin, KeyID: site.KeyID, LastBillID: 7, LastSync: bill.At}); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	bal := relay.Balance{Origin: site.Origin, KeyID: site.KeyID, At: bill.At, RemainingUSD: &zero, Unlimited: true}
	if err = st.PutRelayBalance(ctx, bal); err != nil {
		t.Fatal(err)
	}
	day := relay.Daily{Origin: site.Origin, KeyID: site.KeyID, Day: "2026-01-01", Model: "model", Requests: 1, Tokens: bill.Tokens, ListUSD: 1, ChargedUSD: .25}
	if err = st.PutRelayDaily(ctx, []relay.Daily{day}); err != nil {
		t.Fatal(err)
	}
	if err = st.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sites, err := st.RelaySites(ctx)
	if err != nil || len(sites) != 1 || !reflect.DeepEqual(sites[0], site) {
		t.Fatalf("sites=%+v err=%v", sites, err)
	}
	bills, err := st.RelayBills(ctx, site.Origin, site.KeyID, time.Time{}, time.Time{})
	if err != nil || len(bills) != 1 || !reflect.DeepEqual(bills[0], bill) {
		t.Fatalf("bills=%+v err=%v", bills, err)
	}
	got, err := st.LatestRelayBalance(ctx, site.Origin, site.KeyID)
	if err != nil || !reflect.DeepEqual(got, &bal) {
		t.Fatalf("balance=%+v err=%v", got, err)
	}
	days, err := st.RelayDaily(ctx, site.Origin, site.KeyID, "", "")
	if err != nil || len(days) != 1 || !reflect.DeepEqual(days[0], day) {
		t.Fatalf("days=%+v err=%v", days, err)
	}
	cur, err := st.RelayCursor(ctx, site.Origin, site.KeyID)
	if err != nil || cur.LastBillID != 7 {
		t.Fatalf("cursor=%+v err=%v", cur, err)
	}
}

func TestRelayMigrationRetainsEvents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Commit(ctx, model.Codex, "source", harness.Batch{Events: []model.UsageEvent{{DedupKey: "keep", SessionID: "s", Timestamp: time.Unix(100, 0), Tokens: model.Tokens{Input: 123}}}}, []Resolution{{Cost: 1, Priced: true}}); err != nil {
		t.Fatal(err)
	}
	const snapshotSQL = `SELECT json_array(rowid,harness,dedup_key,session_id,parent_id,project,timestamp,model,provider,base_url,input,output,cache_read,cache_write,reasoning,log_cost,resolved_provider,attrib,cost,priced,raw_project,request_id,boundary,bill_amount,bill_unit) FROM events WHERE dedup_key='keep'`
	var before string
	if err = st.DB().QueryRow(snapshotSQL).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// An old index has no relay schema; migration must never rewrite its events.
	if err = st.Exec(ctx, `DROP VIEW relay_bills; DROP TABLE relay_bill_data; DROP TABLE relay_bill_dimensions; DROP TABLE relay_origins; DROP TABLE relay_sites; DROP TABLE relay_balances; DROP TABLE relay_daily; DROP TABLE relay_cursors;`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var input int64
	if err = st.DB().QueryRow("SELECT input FROM events WHERE dedup_key='keep'").Scan(&input); err != nil || input != 123 {
		t.Fatalf("input=%d err=%v", input, err)
	}
	if _, err = st.RelaySites(ctx); err != nil {
		t.Fatal(err)
	}
	var after string
	if err = st.DB().QueryRow(snapshotSQL).Scan(&after); err != nil || after != before {
		t.Fatalf("relay migration changed events: %s -> %s (%v)", before, after, err)
	}
}

func TestRelayBillFootprint(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bills.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	size := func() int64 {
		t.Helper()
		if _, err := st.DB().ExecContext(ctx, "VACUUM; PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	before := size()
	const n = 10000
	origin := "https://relay.example"
	keyID := "123456789abc"
	bills := make([]relay.Bill, n)
	matches := make([]RelayMatch, n)
	for i := range bills {
		bills[i] = relay.Bill{Origin: origin, KeyID: keyID, ID: int64(i + 1), At: time.Unix(1700000000+int64(i), 0), Type: "consume", Model: fmt.Sprintf("model-%d", i%4), Group: "default", RequestID: fmt.Sprintf("req-%032d", i), UpstreamRequestID: fmt.Sprintf("resp-%031d", i), Tokens: model.Tokens{Input: 2000, Output: 500, CacheRead: 10000, CacheWrite: 200}, ChargedUSD: .0123, Ratios: relay.Ratios{Model: 1, Group: .7, Completion: 3, Cache: .1, CacheCreate: 1.25}, Stream: true, LatencyMS: 1300}
		matches[i] = RelayMatch{ID: int64(i + 1), Harness: "codex", DedupKey: fmt.Sprintf("event-%08d", i), Kind: "matched"}
	}
	if err = st.PutRelayBills(ctx, bills); err != nil {
		t.Fatal(err)
	}
	if err = st.SaveRelayMatches(ctx, origin, keyID, matches); err != nil {
		t.Fatal(err)
	}
	delta := size() - before
	bpe := float64(delta) / n
	t.Logf("%d populated bills including both request IDs, matches, dictionaries and indexes: %d bytes, %.2f B/bill", n, delta, bpe)
	if bpe > 200 {
		t.Fatalf("bill footprint %.2f B exceeds 200B", bpe)
	}
}

// TestRelayBillCacheWrite1hMigration upgrades a bill table from before 1-hour
// cache writes were stored: the column is added and the bill cursors are reset
// so the next sync fetches the bills again with their 1-hour counts.
func TestRelayBillCacheWrite1hMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	site := relay.Site{Origin: "https://relay.example", KeyID: "deadbeef0123", Kind: relay.KindNewAPI, Layers: relay.LayerBills}
	bill := relay.Bill{Origin: site.Origin, KeyID: site.KeyID, ID: 3, At: time.Unix(120, 0).UTC(), Type: "consume", Model: "m", Tokens: model.Tokens{Input: 1, CacheWrite: 9}, ChargedUSD: .1}
	if err = st.PutRelaySite(ctx, site); err != nil {
		t.Fatal(err)
	}
	if err = st.PutRelayBills(ctx, []relay.Bill{bill}); err != nil {
		t.Fatal(err)
	}
	if err = st.PutRelayCursor(ctx, RelayCursor{Origin: site.Origin, KeyID: site.KeyID, LastBillID: 3, LastSync: bill.At}); err != nil {
		t.Fatal(err)
	}
	// Turn the database back into the previous layout.
	if err = st.Exec(ctx, `DROP VIEW relay_bills; ALTER TABLE relay_bill_data DROP COLUMN cache_write_1h;`); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bills, err := st.RelayBills(ctx, site.Origin, site.KeyID, time.Time{}, time.Time{})
	if err != nil || len(bills) != 1 || !reflect.DeepEqual(bills[0], bill) {
		t.Fatalf("bills=%+v err=%v", bills, err)
	}
	cur, err := st.RelayCursor(ctx, site.Origin, site.KeyID)
	if err != nil || cur.LastBillID != 0 {
		t.Fatalf("cursor after upgrade = %+v, %v; want bills refetched", cur, err)
	}
	bill.CacheWrite1h = 6
	if err = st.PutRelayBills(ctx, []relay.Bill{bill}); err != nil {
		t.Fatal(err)
	}
	if bills, err = st.RelayBills(ctx, site.Origin, site.KeyID, time.Time{}, time.Time{}); err != nil || len(bills) != 1 || bills[0].CacheWrite1h != 6 {
		t.Fatalf("refetched bills=%+v err=%v", bills, err)
	}
}
