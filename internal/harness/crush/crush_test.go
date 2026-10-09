package crush

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// Fixtures live at the repository root (testdata/crush) like the other harness
// packages; a package-local testdata directory is preferred when one exists.
const fixtureRoot = "../../../testdata/crush"

func fixture(t *testing.T, parts ...string) string {
	t.Helper()
	if local := filepath.Join(append([]string{"testdata"}, parts...)...); exists(local) {
		return local
	}
	return filepath.Join(append([]string{fixtureRoot}, parts...)...)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// isolate points the global registry lookups at throwaway directories so a
// developer machine's real ~/.local/share/crush cannot leak into the tests.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("CRUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type parseResult struct {
	Events   []model.UsageEvent
	Sessions []model.SessionMeta
	Cursors  map[string]harness.Cursor
}

func parseFrom(t *testing.T, p *Parser, cursors map[string]harness.Cursor) parseResult {
	t.Helper()
	ctx := context.Background()
	srcs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if cursors == nil {
		cursors = map[string]harness.Cursor{}
	}
	res := parseResult{Cursors: map[string]harness.Cursor{}}
	for _, src := range srcs {
		b, err := p.Parse(ctx, src, cursors[src.Path])
		if err != nil {
			t.Fatalf("Parse(%s): %v", src.Path, err)
		}
		res.Events = append(res.Events, b.Events...)
		res.Sessions = append(res.Sessions, b.Sessions...)
		res.Cursors[src.Path] = b.Next
	}
	return res
}

func parseAll(t *testing.T, p *Parser) parseResult { return parseFrom(t, p, nil) }

func keysOf(events []model.UsageEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.DedupKey)
	}
	return out
}

func eventByKey(t *testing.T, events []model.UsageEvent, key string) model.UsageEvent {
	t.Helper()
	for _, ev := range events {
		if ev.DedupKey == key {
			return ev
		}
	}
	t.Fatalf("no event with DedupKey %q (have %v)", key, keysOf(events))
	return model.UsageEvent{}
}

func sessionByID(t *testing.T, metas []model.SessionMeta, id string) model.SessionMeta {
	t.Helper()
	for _, m := range metas {
		if m.SessionID == id {
			return m
		}
	}
	t.Fatalf("no session %q", id)
	return model.SessionMeta{}
}

func wantTokens(t *testing.T, label string, got, want model.Tokens) {
	t.Helper()
	if got != want {
		t.Errorf("%s tokens = %+v, want %+v", label, got, want)
	}
}

func timeFromMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// wantCost compares costs with a tolerance: deltas are computed by
// subtraction, so 0.06-0.05 is 0.009999999999999995 in float64.
func wantCost(t *testing.T, label string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s CostUSD = nil, want %v", label, want)
		return
	}
	if math.Abs(*got-want) > 1e-9 {
		t.Errorf("%s CostUSD = %v, want %v", label, *got, want)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func execSQL(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %.60q: %v", stmt, err)
		}
	}
}

// ---------------------------------------------------------------------------
// discovery
// ---------------------------------------------------------------------------

func TestDiscoverViaRegistry(t *testing.T) {
	isolate(t)
	p := NewWithRoots(fixture(t))
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 2 {
		t.Fatalf("got %d sources, want 2: %v", len(srcs), srcs)
	}
	got := []string{srcs[0].Path, srcs[1].Path}
	want := []string{
		fixture(t, "proj-a", ".crush", "crush.db"),
		fixture(t, "proj-b", "data", "b", "crush.db"),
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("source[%d] = %q, want %q", i, got[i], want[i])
		}
		if srcs[i].Kind != kindSQLite {
			t.Errorf("source[%d] kind = %q, want %q", i, srcs[i].Kind, kindSQLite)
		}
	}
}

func TestDiscoverRoots(t *testing.T) {
	isolate(t)
	// A data directory that holds crush.db directly.
	dataDir := t.TempDir()
	copyFile(t, fixture(t, "proj-a", ".crush", "crush.db"), filepath.Join(dataDir, dbName))
	// A project directory that holds .crush/crush.db.
	projDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projDir, crushDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, fixture(t, "proj-a", ".crush", "crush.db"), filepath.Join(projDir, crushDirName, dbName))
	// A database path used as a root.
	fileRoot := filepath.Join(t.TempDir(), dbName)
	copyFile(t, fixture(t, "proj-a", ".crush", "crush.db"), fileRoot)

	tests := []struct {
		name string
		root string
		want int
	}{
		{"data dir", dataDir, 1},
		{"project dir", projDir, 1},
		{"db file", fileRoot, 1},
		{"missing", filepath.Join(t.TempDir(), "nope"), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewWithRoots(tc.root)
			srcs, err := p.Discover(context.Background())
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(srcs) != tc.want {
				t.Fatalf("got %d sources, want %d: %v", len(srcs), tc.want, srcs)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// session totals become delta events
// ---------------------------------------------------------------------------

func TestParseSessionTotals(t *testing.T) {
	isolate(t)
	p := NewWithRoots(fixture(t))
	res := parseAll(t, p)

	if len(res.Events) != 3 {
		t.Fatalf("got %d events, want 3: %v", len(res.Events), keysOf(res.Events))
	}
	if len(res.Sessions) != 3 {
		t.Fatalf("got %d sessions, want 3", len(res.Sessions))
	}

	root := eventByKey(t, res.Events, "crush:s_root001:12000:800")
	wantTokens(t, "s_root001", root.Tokens, model.Tokens{Input: 12000, Output: 800})
	if root.SessionID != "s_root001" {
		t.Errorf("SessionID = %q", root.SessionID)
	}
	if root.ParentID != "" {
		t.Errorf("root ParentID = %q, want empty", root.ParentID)
	}
	if root.Model != "claude-sonnet-4-5" || root.Provider != "anthropic" {
		t.Errorf("model/provider = %q/%q", root.Model, root.Provider)
	}
	wantCost(t, "s_root001", root.CostUSD, 0.05)
	if want := fixture(t, "proj-a"); root.ProjectPath != want {
		t.Errorf("ProjectPath = %q, want %q", root.ProjectPath, want)
	}
	// created_at/updated_at are seconds: 1742303600 -> 1742303600000 ms.
	if !root.Timestamp.Equal(timeFromMS(1742303600000)) {
		t.Errorf("Timestamp = %v, want %v", root.Timestamp, timeFromMS(1742303600000))
	}
	if root.Timestamp.Location() != time.UTC {
		t.Errorf("Timestamp location = %v", root.Timestamp.Location())
	}

	child := eventByKey(t, res.Events, "crush:s_child001:3000:150")
	if child.ParentID != "s_root001" {
		t.Errorf("child ParentID = %q, want s_root001", child.ParentID)
	}
	wantTokens(t, "s_child001", child.Tokens, model.Tokens{Input: 3000, Output: 150})
	if child.Model != "gpt-5" || child.Provider != "openai" {
		t.Errorf("child model/provider = %q/%q", child.Model, child.Provider)
	}
	if !child.Timestamp.Equal(timeFromMS(1742303200000)) {
		t.Errorf("child Timestamp = %v", child.Timestamp)
	}

	// proj-b lives outside .crush/, so the project is the database directory.
	other := eventByKey(t, res.Events, "crush:s_b001:500:20")
	wantTokens(t, "s_b001", other.Tokens, model.Tokens{Input: 500, Output: 20})
	if other.CostUSD != nil {
		t.Errorf("zero cost must stay unknown, got %v", *other.CostUSD)
	}
	if want := fixture(t, "proj-b", "data", "b"); other.ProjectPath != want {
		t.Errorf("ProjectPath = %q, want %q", other.ProjectPath, want)
	}
	if other.Model != "qwen3-coder" || other.Provider != "alibaba" {
		t.Errorf("model/provider = %q/%q", other.Model, other.Provider)
	}

	t.Run("sessions", func(t *testing.T) {
		root := sessionByID(t, res.Sessions, "s_root001")
		if root.Title != "Build the CLI" {
			t.Errorf("title = %q", root.Title)
		}
		if !root.StartedAt.Equal(timeFromMS(1742303000000)) || !root.UpdatedAt.Equal(timeFromMS(1742303600000)) {
			t.Errorf("started/updated = %v/%v", root.StartedAt, root.UpdatedAt)
		}
		child := sessionByID(t, res.Sessions, "s_child001")
		if child.ParentID != "s_root001" || child.Title != "Inspect the schema" {
			t.Errorf("child = %+v", child)
		}
		empty := sessionByID(t, res.Sessions, "s_b001")
		if empty.Title != "" {
			t.Errorf("untitled session title = %q", empty.Title)
		}
		// The all-zero session is never reported.
		for _, m := range res.Sessions {
			if m.SessionID == "s_zero001" {
				t.Error("s_zero001 must be skipped")
			}
		}
	})
}

// ---------------------------------------------------------------------------
// incremental delta growth
// ---------------------------------------------------------------------------

func TestIncrementalDeltaGrowth(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, dbName)
	copyFile(t, fixture(t, "proj-a", ".crush", "crush.db"), dbPath)
	p := NewWithRoots(dbPath)

	first := parseAll(t, p)
	if len(first.Events) != 2 {
		t.Fatalf("first pass: got %d events, want 2: %v", len(first.Events), keysOf(first.Events))
	}
	if idle := parseFrom(t, p, first.Cursors); len(idle.Events) != 0 {
		t.Fatalf("idle re-parse emitted %v", keysOf(idle.Events))
	}

	// The session totals grow: one new delta event, keyed by the new totals.
	execSQL(t, dbPath,
		`UPDATE sessions SET prompt_tokens = 13000, completion_tokens = 900,
		  cost = 0.06, updated_at = 1742304000 WHERE id = 's_root001'`)
	second := parseFrom(t, p, first.Cursors)
	if len(second.Events) != 1 {
		t.Fatalf("second pass: got %d events, want 1: %v", len(second.Events), keysOf(second.Events))
	}
	delta := second.Events[0]
	if delta.DedupKey != "crush:s_root001:13000:900" {
		t.Errorf("DedupKey = %q", delta.DedupKey)
	}
	wantTokens(t, "delta", delta.Tokens, model.Tokens{Input: 1000, Output: 100})
	wantCost(t, "delta", delta.CostUSD, 0.01)
	if !delta.Timestamp.Equal(timeFromMS(1742304000000)) {
		t.Errorf("delta Timestamp = %v", delta.Timestamp)
	}

	// A counter that moves backwards emits nothing and does not lower the
	// high-water mark.
	execSQL(t, dbPath,
		`UPDATE sessions SET prompt_tokens = 12900, completion_tokens = 850,
		  cost = 0.055, updated_at = 1742304100 WHERE id = 's_root001'`)
	third := parseFrom(t, p, second.Cursors)
	if len(third.Events) != 0 {
		t.Fatalf("backwards counter emitted %v", keysOf(third.Events))
	}

	// Growing again measures from the high-water mark, not the last read.
	execSQL(t, dbPath,
		`UPDATE sessions SET prompt_tokens = 13200, completion_tokens = 950,
		  cost = 0.07, updated_at = 1742304200 WHERE id = 's_root001'`)
	fourth := parseFrom(t, p, third.Cursors)
	if len(fourth.Events) != 1 {
		t.Fatalf("fourth pass: got %d events, want 1: %v", len(fourth.Events), keysOf(fourth.Events))
	}
	last := fourth.Events[0]
	if last.DedupKey != "crush:s_root001:13200:950" {
		t.Errorf("DedupKey = %q", last.DedupKey)
	}
	wantTokens(t, "last delta", last.Tokens, model.Tokens{Input: 200, Output: 50})
	wantCost(t, "last delta", last.CostUSD, 0.01)
}

// ---------------------------------------------------------------------------
// robustness
// ---------------------------------------------------------------------------

func TestMissingSessionsTableTolerated(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	path := filepath.Join(dir, dbName)
	execSQL(t, path, `CREATE TABLE messages (id TEXT PRIMARY KEY)`)
	p := NewWithRoots(dir)
	b, err := p.Parse(context.Background(), harness.Source{Path: path, Kind: kindSQLite}, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(b.Events) != 0 || len(b.Sessions) != 0 {
		t.Fatalf("got %d events/%d sessions", len(b.Events), len(b.Sessions))
	}
}

func TestCorruptDatabaseReturnsCursor(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	path := filepath.Join(dir, dbName)
	if err := os.WriteFile(path, []byte("this is not sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoots(dir)
	cur := harness.Cursor{Offset: 3, Size: 9, Extra: `{"sessions":{"a":{"p":1}}}`}
	b, err := p.Parse(context.Background(), harness.Source{Path: path, Kind: kindSQLite}, cur)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(b.Events) != 0 || len(b.Sessions) != 0 {
		t.Fatalf("got %d events/%d sessions", len(b.Events), len(b.Sessions))
	}
	if b.Next.Offset != cur.Offset || b.Next.Extra != cur.Extra {
		t.Errorf("Next = %+v, want the input cursor", b.Next)
	}
}

func TestMissingDatabase(t *testing.T) {
	isolate(t)
	p := NewWithRoots(t.TempDir())
	b, err := p.Parse(context.Background(), harness.Source{
		Path: filepath.Join(t.TempDir(), dbName), Kind: kindSQLite,
	}, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(b.Events) != 0 {
		t.Fatalf("got %d events", len(b.Events))
	}
}

// ---------------------------------------------------------------------------
// registration & roots
// ---------------------------------------------------------------------------

func TestRegistration(t *testing.T) {
	p, ok := harness.Get(model.Crush)
	if !ok {
		t.Fatal("crush parser is not registered")
	}
	if _, ok := p.(*Parser); !ok {
		t.Fatalf("registered parser has type %T", p)
	}
	if got := p.Harness(); got != model.Crush {
		t.Errorf("Harness() = %q", got)
	}
	if len(p.Roots()) == 0 {
		t.Error("Roots() is empty")
	}
}

func TestRootsEnvOverride(t *testing.T) {
	t.Setenv("MYTOKEN_CRUSH_DIRS", "/tmp/crush-one"+string(os.PathListSeparator)+"/tmp/crush-two")
	p := New()
	roots := p.Roots()
	if len(roots) != 2 || roots[0] != "/tmp/crush-one" || roots[1] != "/tmp/crush-two" {
		t.Fatalf("Roots() = %v", roots)
	}
}

func TestRootsDefault(t *testing.T) {
	t.Setenv("MYTOKEN_CRUSH_DIRS", "")
	t.Setenv("CRUSH_GLOBAL_DATA", "")
	t.Setenv("XDG_DATA_HOME", "/tmp/xdg")
	p := New()
	if got := p.Roots(); len(got) != 1 || got[0] != filepath.Join("/tmp/xdg", crushDataDir) {
		t.Fatalf("Roots() = %v", got)
	}
}

func TestNewWithRootsDedupes(t *testing.T) {
	dir := t.TempDir()
	p := NewWithRoots(dir, dir+string(os.PathSeparator), filepath.Join(dir, "..", filepath.Base(dir)))
	roots := p.Roots()
	if len(roots) != 1 {
		t.Fatalf("Roots() = %v, want one entry", roots)
	}
	if !strings.HasSuffix(roots[0], filepath.Base(dir)) {
		t.Errorf("Roots() = %v", roots)
	}
}

// ---------------------------------------------------------------------------
// token/delta class mapping
// ---------------------------------------------------------------------------

func ptrCost(v float64) *float64 { return &v }

// TestDeltaMapping walks one session's cumulative counters through a sequence of
// observations and checks that every step emits exactly the monotone delta of the
// counters seen so far.
func TestDeltaMapping(t *testing.T) {
	const sid = "s_delta"
	steps := []struct {
		name           string
		prompt         int64
		completion     int64
		cost           float64
		wantEvents     int
		wantTokens     model.Tokens
		wantCost       *float64
		wantKey        string
		basePrompt     int64
		baseCompletion int64
	}{
		{
			name:   "first observation is the full cumulative value",
			prompt: 100, completion: 20, cost: 0.01,
			wantEvents: 1, wantTokens: model.Tokens{Input: 100, Output: 20},
			wantCost: ptrCost(0.01), wantKey: "crush:s_delta:100:20",
			basePrompt: 100, baseCompletion: 20,
		},
		{
			name:   "unchanged counters emit nothing",
			prompt: 100, completion: 20, cost: 0.01,
			wantEvents: 0,
			basePrompt: 100, baseCompletion: 20,
		},
		{
			name:   "growth emits only the difference",
			prompt: 150, completion: 30, cost: 0.02,
			wantEvents: 1, wantTokens: model.Tokens{Input: 50, Output: 10},
			wantCost: ptrCost(0.01), wantKey: "crush:s_delta:150:30",
			basePrompt: 150, baseCompletion: 30,
		},
		{
			// A cost-only update must not reuse the token key: the store folds a
			// repeated DedupKey last-write-wins, which would blank the tokens
			// carried by the previous event.
			name:   "cost-only growth takes a cost-suffixed key",
			prompt: 150, completion: 30, cost: 0.03,
			wantEvents: 1, wantTokens: model.Tokens{},
			wantCost: ptrCost(0.01), wantKey: "crush:s_delta:150:30:cost:0.03",
			basePrompt: 150, baseCompletion: 30,
		},
		{
			name:   "counters that moved backwards emit nothing and keep the high-water mark",
			prompt: 140, completion: 25, cost: 0.025,
			wantEvents: 0,
			basePrompt: 150, baseCompletion: 30,
		},
		{
			name:   "growth after a backwards move is measured from the high-water mark",
			prompt: 160, completion: 35, cost: 0.04,
			wantEvents: 1, wantTokens: model.Tokens{Input: 10, Output: 5},
			wantCost: ptrCost(0.01), wantKey: "crush:s_delta:160:35",
			basePrompt: 160, baseCompletion: 35,
		},
	}
	acc := newAccumulator("/proj", timeFromMS(1742300000000))
	state := map[string]baseline{}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			// Each parse starts with an empty batch, so reset the accumulator's
			// event set to observe exactly this step's output.
			acc.evs = map[string]model.UsageEvent{}
			acc.order = nil
			acc.usage(sessionRow{
				ID:         sid,
				Prompt:     st.prompt,
				Completion: st.completion,
				Cost:       st.cost,
				Created:    1742303000,
				Updated:    1742303600,
			}, modelRef{Model: "claude-sonnet-4-5", Provider: "anthropic"}, state)

			events := acc.batch(harness.Cursor{}).Events
			if len(events) != st.wantEvents {
				t.Fatalf("events = %v, want %d", keysOf(events), st.wantEvents)
			}
			if base := state[sid]; base.Prompt != st.basePrompt || base.Completion != st.baseCompletion {
				t.Errorf("baseline = %+v, want %d/%d", base, st.basePrompt, st.baseCompletion)
			}
			if st.wantEvents == 0 {
				return
			}
			ev := events[0]
			if ev.DedupKey != st.wantKey {
				t.Errorf("DedupKey = %q, want %q", ev.DedupKey, st.wantKey)
			}
			wantTokens(t, "delta", ev.Tokens, st.wantTokens)
			if st.wantCost == nil {
				if ev.CostUSD != nil {
					t.Errorf("CostUSD = %v, want nil", *ev.CostUSD)
				}
			} else {
				wantCost(t, "delta", ev.CostUSD, *st.wantCost)
			}
			if want := timeFromMS(1742303600000); !ev.Timestamp.Equal(want) {
				t.Errorf("Timestamp = %v, want %v", ev.Timestamp, want)
			}
			if ev.Timestamp.Location() != time.UTC {
				t.Errorf("Timestamp location = %v, want UTC", ev.Timestamp.Location())
			}
			if ev.SessionID != sid || ev.Harness != model.Crush || ev.ProjectPath != "/proj" {
				t.Errorf("event identity = %+v", ev)
			}
			if ev.Model != "claude-sonnet-4-5" || ev.Provider != "anthropic" {
				t.Errorf("model/provider = %q/%q", ev.Model, ev.Provider)
			}
		})
	}
}

func TestTimestampNormalization(t *testing.T) {
	tests := []struct {
		name string
		raw  int64
		want time.Time
	}{
		{"zero is unset", 0, time.Time{}},
		{"negative is unset", -1, time.Time{}},
		{"seconds are scaled to milliseconds", 1742303600, timeFromMS(1742303600000)},
		{"milliseconds are used as-is", 1742303100000, timeFromMS(1742303100000)},
		{"just below the millisecond threshold", 99999999999, timeFromMS(99999999999000)},
		{"at the millisecond threshold", 100000000000, timeFromMS(100000000000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := crushTime(tt.raw)
			if got.IsZero() != tt.want.IsZero() {
				t.Fatalf("crushTime(%d) = %v, want %v", tt.raw, got, tt.want)
			}
			if !tt.want.IsZero() && !got.Equal(tt.want) {
				t.Fatalf("crushTime(%d) = %v, want %v", tt.raw, got, tt.want)
			}
			if !got.IsZero() && got.Location() != time.UTC {
				t.Errorf("crushTime(%d) location = %v, want UTC", tt.raw, got.Location())
			}
		})
	}
}

// TestIncrementalCostOnlyGrowth is the end-to-end guard for the split key: a
// session whose cost grows while its token counters stand still must emit a cost
// event under its own DedupKey, so the store's last-write-wins fold cannot blank
// the tokens of the event that carried them.
func TestIncrementalCostOnlyGrowth(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "crush.db")
	copyFile(t, fixture(t, "proj-a", ".crush", "crush.db"), db)

	p := NewWithRoots(dir)
	first := parseAll(t, p)
	if got := len(first.Events); got != 2 {
		t.Fatalf("first parse events = %v, want 2", keysOf(first.Events))
	}
	if ev := eventByKey(t, first.Events, "crush:s_root001:12000:800"); ev.Tokens.Input != 12000 || ev.Tokens.Output != 800 {
		t.Fatalf("root event = %+v", ev)
	}

	// Cost grows, tokens do not.
	execSQL(t, db, "UPDATE sessions SET cost=0.09, updated_at=1742304000 WHERE id='s_root001'")
	second := parseFrom(t, p, first.Cursors)
	if got := len(second.Events); got != 1 {
		t.Fatalf("cost-only parse events = %v, want 1", keysOf(second.Events))
	}
	ev := second.Events[0]
	if ev.DedupKey != "crush:s_root001:12000:800:cost:0.09" {
		t.Errorf("DedupKey = %q, want the cost-suffixed key", ev.DedupKey)
	}
	if !ev.Tokens.IsZero() {
		t.Errorf("Tokens = %+v, want zero (the tokens live on the token key)", ev.Tokens)
	}
	wantCost(t, "cost-only delta", ev.CostUSD, 0.04)
	if want := timeFromMS(1742304000000); !ev.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", ev.Timestamp, want)
	}

	// The token-growth key is untouched by the cost-only update, and the next
	// token growth carries only the new token delta plus the remaining cost.
	execSQL(t, db, "UPDATE sessions SET prompt_tokens=12100, completion_tokens=810, cost=0.10, updated_at=1742304100 WHERE id='s_root001'")
	third := parseFrom(t, p, second.Cursors)
	if got := len(third.Events); got != 1 {
		t.Fatalf("growth parse events = %v, want 1", keysOf(third.Events))
	}
	grown := third.Events[0]
	if grown.DedupKey != "crush:s_root001:12100:810" {
		t.Errorf("DedupKey = %q, want crush:s_root001:12100:810", grown.DedupKey)
	}
	wantTokens(t, "growth", grown.Tokens, model.Tokens{Input: 100, Output: 10})
	wantCost(t, "growth delta", grown.CostUSD, 0.01)
}
