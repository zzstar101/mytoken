package opencode

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// watermarkDDL is the minimal schema both message generations need.
const watermarkDDL = `
CREATE TABLE session (
  id text PRIMARY KEY, parent_id text, directory text NOT NULL,
  title text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE message (
  id text PRIMARY KEY, session_id text NOT NULL,
  time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
CREATE TABLE session_message (
  id text PRIMARY KEY, session_id text NOT NULL, type text NOT NULL, seq integer NOT NULL,
  time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
`

// watermarkRows writes a database whose `session_message` (v2) usage row is
// older than the newest `message` (v1) row. readMessages used to resume both
// tables from one shared watermark and scan v1 first, so msg_wm_v2a was skipped
// on every pass — including the cold one, which persisted the watermark.
func watermarkRows(t *testing.T, path string) {
	t.Helper()
	execSQL(t, path, watermarkDDL,
		`INSERT INTO session VALUES ('ses_wm', NULL, '/Users/dev/wmproj', 'Watermark regression', 1742200000000, 1742200090000)`,
		`INSERT INTO message VALUES ('msg_wm_v1a','ses_wm',1742200010000,1742200010000,
		  '{"role":"assistant","cost":0.01,"tokens":{"total":110,"input":100,"output":10,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"claude-sonnet-4-5","providerID":"anthropic","time":{"created":1742200010000}}')`,
		`INSERT INTO session_message VALUES ('msg_wm_v2a','ses_wm','assistant',0,1742200020000,1742200020000,
		  '{"role":"assistant","cost":0.02,"tokens":{"total":220,"input":200,"output":20,"reasoning":0,"cache":{"write":10,"read":0}},"modelID":"kimi-k2","providerID":"moonshot","time":{"created":1742200020000}}')`,
		`INSERT INTO message VALUES ('msg_wm_v1b','ses_wm',1742200030000,1742200030000,
		  '{"role":"assistant","cost":0.03,"tokens":{"total":330,"input":300,"output":30,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"gpt-5","providerID":"openai","time":{"created":1742200030000}}')`,
	)
}

func watermarkKeys(events []model.UsageEvent) []string {
	out := keysOf(events)
	slices.Sort(out)
	return out
}

// TestInterleavedWatermarksKeepOlderV2Row is the regression test for the shared
// watermark: an older v2 row must appear in the cold parse, survive an idle
// re-parse, and must not block (or be blocked by) later rows in either table.
func TestInterleavedWatermarksKeepOlderV2Row(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")
	watermarkRows(t, path)
	p := NewWithRoots(dir)

	first := parseAll(t, p)
	want := []string{"opencode:msg_wm_v1a", "opencode:msg_wm_v1b", "opencode:msg_wm_v2a"}
	if got := watermarkKeys(first.Events); !slices.Equal(got, want) {
		t.Fatalf("cold parse events = %v, want %v", got, want)
	}

	if idle := parseFrom(t, p, first.Cursors); len(idle.Events) != 0 {
		t.Fatalf("idle re-parse emitted %v", keysOf(idle.Events))
	}

	// Append to both tables at once, with the new v2 row older than the new v1
	// row: each table must advance on its own watermark.
	execSQL(t, path,
		`INSERT INTO message VALUES ('msg_wm_v1c','ses_wm',1742200060000,1742200060000,
		  '{"role":"assistant","cost":0.04,"tokens":{"total":440,"input":400,"output":40,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"claude-sonnet-4-5","providerID":"anthropic","time":{"created":1742200060000}}')`,
		`INSERT INTO session_message VALUES ('msg_wm_v2b','ses_wm','assistant',1,1742200040000,1742200040000,
		  '{"role":"assistant","cost":0.05,"tokens":{"total":550,"input":500,"output":50,"reasoning":0,"cache":{"write":0,"read":20}},"modelID":"kimi-k2","providerID":"moonshot","time":{"created":1742200040000}}')`,
	)
	second := parseFrom(t, p, first.Cursors)
	want = []string{"opencode:msg_wm_v1c", "opencode:msg_wm_v2b"}
	if got := watermarkKeys(second.Events); !slices.Equal(got, want) {
		t.Fatalf("incremental events = %v, want %v", got, want)
	}
	if third := parseFrom(t, p, second.Cursors); len(third.Events) != 0 {
		t.Fatalf("third pass emitted %v", keysOf(third.Events))
	}
}

// TestLegacySingleWatermarkCursorMigrates covers the upgrade path: a cursor
// written before the split holds one watermark that both tables had advanced.
// It must be honoured for the v1 table only, so v2 replays from the beginning
// exactly once (its DedupKeys keep that replay idempotent for the store) and
// the cursor returned by that pass is the new, stable form.
func TestLegacySingleWatermarkCursorMigrates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")
	watermarkRows(t, path)
	p := NewWithRoots(dir)

	first := parseAll(t, p)
	if len(first.Events) != 3 {
		t.Fatalf("cold parse got %d events, want 3", len(first.Events))
	}

	// The pre-split parser stored {"u":…,"i":…}: the newest v1 row, already
	// consumed. A later v2 row arrives too, so the scan is not short-circuited
	// by the unchanged-file fast path (opencode.go:365). v2 must then be
	// re-read from zero, while v1 stays behind the legacy watermark.
	legacy := first.Cursors[path]
	legacy.Extra = `{"u":1742200030000,"i":"msg_wm_v1b"}`
	execSQL(t, path,
		`INSERT INTO session_message VALUES ('msg_wm_v2c','ses_wm','assistant',1,1742200070000,1742200070000,
		  '{"role":"assistant","cost":0.06,"tokens":{"total":660,"input":600,"output":60,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"kimi-k2","providerID":"moonshot","time":{"created":1742200070000}}')`,
	)
	migrated := parseFrom(t, p, map[string]harness.Cursor{path: legacy})
	want := []string{"opencode:msg_wm_v2a", "opencode:msg_wm_v2c"}
	if got := watermarkKeys(migrated.Events); !slices.Equal(got, want) {
		t.Fatalf("legacy-cursor parse events = %v, want the replayed v2 rows %v", got, want)
	}
	if got := migrated.Cursors[path].Extra; got == legacy.Extra {
		t.Errorf("cursor Extra = %q, want the split watermark form", got)
	}

	// The migrated cursor must now be idle: the replay happens once.
	if idle := parseFrom(t, p, migrated.Cursors); len(idle.Events) != 0 {
		t.Fatalf("re-parse after migration emitted %v", keysOf(idle.Events))
	}
}
