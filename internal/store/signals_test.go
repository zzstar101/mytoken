package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

func TestEventSignalsRoundTripAndReplay(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := model.UsageEvent{DedupKey: "signal", SessionID: "s", Tokens: model.Tokens{Input: 10}, RequestID: "req-exact", Boundary: model.BoundaryCompact, Bill: &model.Bill{Amount: 1.25, Unit: "credits"}}
	write := func() {
		t.Helper()
		if err := st.Commit(context.Background(), model.Codex, "source", harness.Batch{Events: []model.UsageEvent{e}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		t.Helper()
		var request, boundary, unit string
		var amount float64
		var cost sql.NullFloat64
		if err := st.DB().QueryRow(`SELECT request_id,boundary,bill_amount,bill_unit,log_cost FROM events`).Scan(&request, &boundary, &amount, &unit, &cost); err != nil {
			t.Fatal(err)
		}
		if request != "req-exact" || boundary != "compact" || amount != 1.25 || unit != "credits" || cost.Valid {
			t.Fatalf("signals %q %q %g %q cost=%v", request, boundary, amount, unit, cost)
		}
	}
	write()
	check()
	// Streaming replays with absent optional signals must not erase observations.
	e.RequestID = ""
	e.Boundary = ""
	e.Bill = nil
	e.Tokens.Output = 3
	write()
	check()
	if err := st.Exec(context.Background(), `UPDATE events SET resolved_provider='updated'`); err != nil {
		t.Fatal(err)
	}
	check()
	assertDailyRows(t, st)
}

func TestSignalsUpgradeExistingCompactDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Commit(context.Background(), model.Codex, "old", harness.Batch{Events: []model.UsageEvent{{DedupKey: "old", SessionID: "s", Tokens: model.Tokens{Input: 7}}}}, nil); err != nil {
		t.Fatal(err)
	}
	// Recreate the actual pre-signal compact shape while retaining facts and rollups.
	if err = st.Exec(context.Background(), `DROP VIEW events; ALTER TABLE event_data DROP COLUMN request_id; ALTER TABLE event_data DROP COLUMN boundary; ALTER TABLE event_data DROP COLUMN bill_amount; ALTER TABLE event_data DROP COLUMN bill_unit; CREATE VIEW events AS SELECT e.rowid AS rowid,e.harness,e.dedup_key,d.session_id,d.parent_id,d.project,e.timestamp,d.model,d.provider,d.base_url,e.input,e.output,e.cache_read,e.cache_write,e.reasoning,e.log_cost,d.resolved_provider,d.attrib,e.cost,e.priced,d.raw_project FROM event_data e JOIN event_dimensions d ON d.id=e.dimension_id;`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var input int
	var request, boundary, unit string
	var amount sql.NullFloat64
	if err = st.DB().QueryRow(`SELECT input,request_id,boundary,bill_amount,bill_unit FROM events`).Scan(&input, &request, &boundary, &amount, &unit); err != nil {
		t.Fatal(err)
	}
	if input != 7 || request != "" || boundary != "" || amount.Valid || unit != "" {
		t.Fatalf("migration changed signals: %d %q %q %v %q", input, request, boundary, amount, unit)
	}
	assertDailyRows(t, st)
}
