package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zzstar/mytoken/internal/harness"
	"github.com/zzstar/mytoken/internal/model"
)

// Fixtures live at the repository root (testdata/opencode) like the other
// harness packages; a package-local testdata directory is preferred when one
// exists.
const fixtureRoot = "../../../testdata/opencode"

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

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type parseResult struct {
	Events   []model.UsageEvent
	Sessions []model.SessionMeta
	Cursors  map[string]harness.Cursor
}

// parseFrom discovers every source under the parser roots and parses each one
// starting from the supplied cursor (nil = cold start).
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

// dedupLastWins collapses events by DedupKey keeping the last copy, the way
// the store folds a re-emitted row.
func dedupLastWins(events []model.UsageEvent) map[string]model.UsageEvent {
	out := make(map[string]model.UsageEvent, len(events))
	for _, ev := range events {
		out[ev.DedupKey] = ev
	}
	return out
}

func sumTokens(events []model.UsageEvent) model.Tokens {
	var total model.Tokens
	for _, ev := range events {
		total = total.Add(ev.Tokens)
	}
	return total
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

func keysOf(events []model.UsageEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.DedupKey)
	}
	return out
}

func sessionByID(t *testing.T, metas []model.SessionMeta, id string) model.SessionMeta {
	t.Helper()
	for _, m := range metas {
		if m.SessionID == id {
			return m
		}
	}
	t.Fatalf("no session %q (have %d sessions)", id, len(metas))
	return model.SessionMeta{}
}

func wantTokens(t *testing.T, label string, got, want model.Tokens) {
	t.Helper()
	if got != want {
		t.Errorf("%s tokens = %+v, want %+v", label, got, want)
	}
}

func timeFromMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

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

func TestDiscover(t *testing.T) {
	tests := []struct {
		name  string
		roots []string
		want  map[string]string // base name -> kind
	}{
		{
			name:  "sqlite only",
			roots: []string{fixture(t, "v1")},
			want:  map[string]string{"opencode.db": kindSQLite},
		},
		{
			name:  "channel build db",
			roots: []string{fixture(t, "v2")},
			want:  map[string]string{"opencode-stable.db": kindSQLite},
		},
		{
			name:  "legacy json tree",
			roots: []string{fixture(t, "legacy")},
			want: map[string]string{
				"msg_leg1.json":      kindJSON,
				"msg_leg2.json":      kindJSON,
				"msg_leg3.json":      kindJSON,
				"msg_leg4.json":      kindJSON,
				"ses_legacy001.json": kindJSON,
			},
		},
		{
			name:  "all roots",
			roots: []string{fixture(t, "v1"), fixture(t, "v2"), fixture(t, "legacy")},
			want: map[string]string{
				"opencode.db":        kindSQLite,
				"opencode-stable.db": kindSQLite,
				"msg_leg1.json":      kindJSON,
				"msg_leg2.json":      kindJSON,
				"msg_leg3.json":      kindJSON,
				"msg_leg4.json":      kindJSON,
				"ses_legacy001.json": kindJSON,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewWithRoots(tc.roots...)
			srcs, err := p.Discover(context.Background())
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(srcs) != len(tc.want) {
				t.Fatalf("got %d sources, want %d: %v", len(srcs), len(tc.want), srcs)
			}
			// Discover must be sorted by path.
			for i := 1; i < len(srcs); i++ {
				if srcs[i-1].Path >= srcs[i].Path {
					t.Errorf("sources not sorted: %q >= %q", srcs[i-1].Path, srcs[i].Path)
				}
			}
			for _, src := range srcs {
				want, ok := tc.want[filepath.Base(src.Path)]
				if !ok {
					t.Errorf("unexpected source %s", src.Path)
					continue
				}
				if src.Kind != want {
					t.Errorf("%s kind = %q, want %q", src.Path, src.Kind, want)
				}
			}
		})
	}
}

func TestDiscoverMissingRootIsEmpty(t *testing.T) {
	p := NewWithRoots(filepath.Join(t.TempDir(), "nope"))
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("got %d sources, want 0", len(srcs))
	}
}

// ---------------------------------------------------------------------------
// v1 (legacy sqlite) token mapping, dedupe, parents, titles
// ---------------------------------------------------------------------------

func TestParseV1(t *testing.T) {
	p := NewWithRoots(fixture(t, "v1"))
	res := parseAll(t, p)

	if len(res.Events) != 4 {
		t.Fatalf("got %d events, want 4: %v", len(res.Events), keysOf(res.Events))
	}
	// Every key must be unique: the user row, the token-less assistant row,
	// the all-zero aborted row and the agent-switched row are all skipped.
	if got := len(dedupLastWins(res.Events)); got != 4 {
		t.Errorf("got %d distinct dedup keys, want 4", got)
	}
	wantTotals := model.Tokens{Input: 1950, Output: 340, CacheRead: 500, CacheWrite: 100, Reasoning: 60}
	wantTokens(t, "v1 total", sumTokens(res.Events), wantTotals)

	t.Run("input excludes cache", func(t *testing.T) {
		// msg_a1 reports input 1000 AND cache.read 400; opencode's input
		// already excludes the cached tokens, so nothing may be subtracted.
		ev := eventByKey(t, res.Events, "opencode:msg_a1")
		wantTokens(t, "msg_a1", ev.Tokens, model.Tokens{
			Input: 1000, Output: 200, CacheRead: 400, CacheWrite: 100, Reasoning: 50,
		})
		if ev.SessionID != "ses_main001" {
			t.Errorf("SessionID = %q, want ses_main001", ev.SessionID)
		}
		if ev.ProjectPath != "/Users/dev/proj" {
			t.Errorf("ProjectPath = %q, want /Users/dev/proj", ev.ProjectPath)
		}
		if ev.Model != "claude-sonnet-4-5" || ev.Provider != "anthropic" {
			t.Errorf("model/provider = %q/%q, want claude-sonnet-4-5/anthropic", ev.Model, ev.Provider)
		}
		if ev.CostUSD == nil || *ev.CostUSD != 0.0123 {
			t.Errorf("CostUSD = %v, want 0.0123", ev.CostUSD)
		}
		if !ev.Timestamp.Equal(timeFromMS(1742300010000)) {
			t.Errorf("Timestamp = %v, want %v", ev.Timestamp, timeFromMS(1742300010000))
		}
		if ev.Timestamp.Location() != time.UTC {
			t.Errorf("Timestamp location = %v, want UTC", ev.Timestamp.Location())
		}
	})

	t.Run("zero cost is unknown", func(t *testing.T) {
		ev := eventByKey(t, res.Events, "opencode:msg_a2")
		wantTokens(t, "msg_a2", ev.Tokens, model.Tokens{Input: 500, Output: 80})
		if ev.CostUSD != nil {
			t.Errorf("CostUSD = %v, want nil (cost 0 means unknown, not free)", *ev.CostUSD)
		}
	})

	t.Run("subagent parent link", func(t *testing.T) {
		ev := eventByKey(t, res.Events, "opencode:msg_sub1")
		if ev.ParentID != "ses_main001" {
			t.Errorf("ParentID = %q, want ses_main001", ev.ParentID)
		}
		wantTokens(t, "msg_sub1", ev.Tokens, model.Tokens{
			Input: 300, Output: 40, CacheRead: 100, Reasoning: 10,
		})
		if ev.Model != "gpt-5" || ev.Provider != "openai" {
			t.Errorf("model/provider = %q/%q, want gpt-5/openai", ev.Model, ev.Provider)
		}
	})

	t.Run("sessions", func(t *testing.T) {
		if len(res.Sessions) != 4 {
			t.Fatalf("got %d sessions, want 4", len(res.Sessions))
		}
		main := sessionByID(t, res.Sessions, "ses_main001")
		if main.Title != "Refactor the parser pipeline" {
			t.Errorf("title = %q", main.Title)
		}
		if main.Project != "/Users/dev/proj" {
			t.Errorf("project = %q", main.Project)
		}
		if main.ParentID != "" {
			t.Errorf("ParentID = %q, want empty", main.ParentID)
		}
		if !main.StartedAt.Equal(timeFromMS(1742300000000)) || !main.UpdatedAt.Equal(timeFromMS(1742300600000)) {
			t.Errorf("started/updated = %v/%v", main.StartedAt, main.UpdatedAt)
		}
		sub := sessionByID(t, res.Sessions, "ses_sub001")
		if sub.ParentID != "ses_main001" {
			t.Errorf("subagent ParentID = %q, want ses_main001", sub.ParentID)
		}
		if sub.Title != "Explore token mapping" {
			t.Errorf("subagent title = %q", sub.Title)
		}
		empty := sessionByID(t, res.Sessions, "ses_empty001")
		if empty.Title != "Never used" {
			t.Errorf("empty session title = %q", empty.Title)
		}
		for _, m := range res.Sessions {
			if m.Harness != model.OpenCode {
				t.Errorf("%s harness = %q", m.SessionID, m.Harness)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// v2 (session_message) degradation path
// ---------------------------------------------------------------------------

func TestParseV2(t *testing.T) {
	p := NewWithRoots(fixture(t, "v2"))
	res := parseAll(t, p)

	if len(res.Events) != 2 {
		t.Fatalf("got %d events, want 2: %v", len(res.Events), keysOf(res.Events))
	}
	wantTokens(t, "v2 total", sumTokens(res.Events), model.Tokens{
		Input: 940, Output: 140, Reasoning: 30, CacheWrite: 50,
	})
	main := eventByKey(t, res.Events, "opencode:msg_v2a")
	if main.SessionID != "ses_v2main" || main.ProjectPath != "/Users/dev/proj3" {
		t.Errorf("session/project = %q/%q", main.SessionID, main.ProjectPath)
	}
	if main.Model != "kimi-k2" || main.Provider != "moonshot" {
		t.Errorf("model/provider = %q/%q", main.Model, main.Provider)
	}
	if main.CostUSD == nil || *main.CostUSD != 0.007 {
		t.Errorf("CostUSD = %v", main.CostUSD)
	}
	sub := eventByKey(t, res.Events, "opencode:msg_v2b")
	if sub.ParentID != "ses_v2main" {
		t.Errorf("ParentID = %q, want ses_v2main", sub.ParentID)
	}
	if len(res.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(res.Sessions))
	}
	if got := sessionByID(t, res.Sessions, "ses_v2main").Title; got != "Wire up the sqlite reader" {
		t.Errorf("v2 title = %q", got)
	}
	// No session table at all: the parser must still emit the usage.
	names := map[string]bool{}
	for _, src := range mustDiscover(t, p) {
		names[filepath.Base(src.Path)] = true
	}
	if !names["opencode-stable.db"] {
		t.Errorf("channel db not discovered: %v", names)
	}
}

func mustDiscover(t *testing.T, p *Parser) []harness.Source {
	t.Helper()
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return srcs
}

// ---------------------------------------------------------------------------
// legacy JSON tree
// ---------------------------------------------------------------------------

func TestParseLegacyJSON(t *testing.T) {
	p := NewWithRoots(fixture(t, "legacy"))
	res := parseAll(t, p)

	if len(res.Events) != 2 {
		t.Fatalf("got %d events, want 2: %v", len(res.Events), keysOf(res.Events))
	}
	wantTokens(t, "legacy total", sumTokens(res.Events), model.Tokens{
		Input: 2060, Output: 155, CacheRead: 800, CacheWrite: 200, Reasoning: 25,
	})

	first := eventByKey(t, res.Events, "opencode:msg_leg1")
	if first.SessionID != "ses_legacy001" {
		t.Errorf("SessionID = %q, want ses_legacy001", first.SessionID)
	}
	if first.ProjectPath != "/Users/dev/legacy" {
		t.Errorf("ProjectPath = %q", first.ProjectPath)
	}
	if first.Model != "claude-opus-4-1" || first.Provider != "anthropic" {
		t.Errorf("model/provider = %q/%q", first.Model, first.Provider)
	}
	if first.CostUSD == nil || *first.CostUSD != 0.02 {
		t.Errorf("CostUSD = %v", first.CostUSD)
	}

	// msg_leg4 has no embedded id: the file stem becomes the key, the session
	// id falls back to the parent directory name, and the incomplete
	// tokens.cache is irrelevant because it is absent, not partial.
	stem := eventByKey(t, res.Events, "opencode:msg_leg4")
	if stem.SessionID != "ses_legacy001" {
		t.Errorf("stem SessionID = %q", stem.SessionID)
	}
	wantTokens(t, "msg_leg4", stem.Tokens, model.Tokens{Input: 60, Output: 5})

	// msg_leg2 (assistant, no tokens.cache) and msg_leg3 (user) are dropped.
	for _, key := range []string{"opencode:msg_leg2", "opencode:msg_leg3"} {
		for _, ev := range res.Events {
			if ev.DedupKey == key {
				t.Errorf("%s must be dropped", key)
			}
		}
	}

	if len(res.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(res.Sessions))
	}
	meta := res.Sessions[0]
	if meta.SessionID != "ses_legacy001" || meta.Title != "Legacy JSON session" {
		t.Errorf("session = %+v", meta)
	}
	if meta.Project != "/Users/dev/legacy" {
		t.Errorf("session project = %q", meta.Project)
	}
	if !meta.StartedAt.Equal(timeFromMS(1742302000000)) || !meta.UpdatedAt.Equal(timeFromMS(1742302600000)) {
		t.Errorf("session times = %v/%v", meta.StartedAt, meta.UpdatedAt)
	}
}

// ---------------------------------------------------------------------------
// dedupe across generations
// ---------------------------------------------------------------------------

func TestDedupeAcrossGenerations(t *testing.T) {
	p := NewWithRoots(fixture(t, "v1"))
	res := parseAll(t, p)
	// msg_dual exists in both the `message` and the `session_message` table.
	ev := eventByKey(t, res.Events, "opencode:msg_dual")
	wantTokens(t, "msg_dual", ev.Tokens, model.Tokens{Input: 150, Output: 20})
	n := 0
	for _, e := range res.Events {
		if e.DedupKey == "opencode:msg_dual" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("got %d msg_dual events, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// incremental re-parse
// ---------------------------------------------------------------------------

func TestIncrementalAppendAndBackfill(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	copyFile(t, fixture(t, "v1", "opencode.db"), dbPath)
	p := NewWithRoots(dir)

	first := parseAll(t, p)
	if len(first.Events) != 4 {
		t.Fatalf("first pass: got %d events, want 4", len(first.Events))
	}

	// Re-parsing from the returned cursor with no changes must emit nothing.
	idle := parseFrom(t, p, first.Cursors)
	if len(idle.Events) != 0 {
		t.Fatalf("idle re-parse emitted %v", keysOf(idle.Events))
	}

	// Give the fixture database a usable row shape for a new turn.
	execSQL(t, dbPath,
		`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES
		 ('msg_new','ses_main001',1742302000000,1742302000000,
		  '{"role":"assistant","agent":"build","cost":0.5,"tokens":{"total":90,"input":70,"output":20,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"gemini-3-pro","providerID":"google","time":{"created":1742302000000}}')`,
		`UPDATE message SET time_updated = 1742302010000,
		  data = '{"role":"assistant","agent":"build","cost":0.7,"tokens":{"total":900,"input":800,"output":100,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"claude-sonnet-4-5","providerID":"anthropic","time":{"created":1742300011000}}'
		 WHERE id = 'msg_a2'`,
	)

	second := parseFrom(t, p, first.Cursors)
	if len(second.Events) != 2 {
		t.Fatalf("second pass: got %d events, want 2 (new row + backfilled row): %v",
			len(second.Events), keysOf(second.Events))
	}
	fresh := eventByKey(t, second.Events, "opencode:msg_new")
	wantTokens(t, "msg_new", fresh.Tokens, model.Tokens{Input: 70, Output: 20})
	if fresh.Model != "gemini-3-pro" || fresh.Provider != "google" {
		t.Errorf("msg_new model/provider = %q/%q", fresh.Model, fresh.Provider)
	}
	// The backfilled row keeps its DedupKey and arrives with the new usage;
	// the store folds it by last-write-wins.
	back := eventByKey(t, second.Events, "opencode:msg_a2")
	wantTokens(t, "msg_a2 backfill", back.Tokens, model.Tokens{Input: 800, Output: 100})
	if back.CostUSD == nil || *back.CostUSD != 0.7 {
		t.Errorf("backfill CostUSD = %v", back.CostUSD)
	}

	third := parseFrom(t, p, second.Cursors)
	if len(third.Events) != 0 {
		t.Fatalf("third pass emitted %v", keysOf(third.Events))
	}
}

func TestIncrementalResetWhenDatabaseReplaced(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	copyFile(t, fixture(t, "v1", "opencode.db"), dbPath)
	p := NewWithRoots(dir)
	first := parseAll(t, p)
	if len(first.Events) != 4 {
		t.Fatalf("first pass: got %d events, want 4", len(first.Events))
	}
	// Replace the database with the (older-dated) v2 fixture: the watermark is
	// past every row, so the parser must reset to a cold read.
	copyFile(t, fixture(t, "v2", "opencode-stable.db"), dbPath)
	second := parseFrom(t, p, first.Cursors)
	if len(second.Events) != 2 {
		t.Fatalf("got %d events after replacement, want 2: %v", len(second.Events), keysOf(second.Events))
	}
}

// ---------------------------------------------------------------------------
// robustness
// ---------------------------------------------------------------------------

func TestParseUnreadableDatabaseReturnsCursor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")
	if err := os.WriteFile(path, []byte("not a database at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoots(dir)
	cur := harness.Cursor{Offset: 7, Size: 3, Extra: `{"u":1,"i":"x"}`}
	b, err := p.Parse(context.Background(), harness.Source{Path: path, Kind: kindSQLite}, cur)
	if err != nil {
		t.Fatalf("Parse returned error for corrupt db: %v", err)
	}
	if len(b.Events) != 0 || len(b.Sessions) != 0 {
		t.Fatalf("got %d events/%d sessions from a corrupt db", len(b.Events), len(b.Sessions))
	}
	if b.Next.Offset != cur.Offset || b.Next.Extra != cur.Extra {
		t.Errorf("Next = %+v, want the input cursor back", b.Next)
	}
}

func TestParseMissingDatabase(t *testing.T) {
	p := NewWithRoots(t.TempDir())
	path := filepath.Join(t.TempDir(), "gone.db")
	b, err := p.Parse(context.Background(), harness.Source{Path: path, Kind: kindSQLite}, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(b.Events) != 0 {
		t.Fatalf("got %d events from a missing db", len(b.Events))
	}
}

// A well-formed database without any usage tables (e.g. a crush.db) must be
// tolerated rather than treated as an error.
func TestParseDatabaseWithoutUsageTables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")
	execSQL(t, path, `CREATE TABLE unrelated (id TEXT PRIMARY KEY)`)
	p := NewWithRoots(dir)
	b, err := p.Parse(context.Background(), harness.Source{Path: path, Kind: kindSQLite}, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(b.Events) != 0 || len(b.Sessions) != 0 {
		t.Fatalf("got %d events/%d sessions", len(b.Events), len(b.Sessions))
	}
}

// ---------------------------------------------------------------------------
// title rules
// ---------------------------------------------------------------------------

const titleDDL = `
CREATE TABLE session (
  id text PRIMARY KEY, parent_id text, directory text NOT NULL,
  title text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE message (
  id text PRIMARY KEY, session_id text NOT NULL,
  time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
`

func TestTitleRules(t *testing.T) {
	raw := "  first\tline\nsecond line " + strings.Repeat("very long title ", 8)
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")
	execSQL(t, path, titleDDL,
		`INSERT INTO session VALUES ('ses_title', NULL, '/tmp/p', `+sqlQuote(raw)+`, 1742304000000, 1742304000000)`,
		`INSERT INTO message VALUES ('msg_t','ses_title',1742304000000,1742304000000,
		  '{"role":"assistant","cost":0,"tokens":{"input":11,"output":2,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"m","providerID":"p","time":{"created":1742304000000}}')`,
	)
	p := NewWithRoots(dir)
	res := parseAll(t, p)
	if len(res.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(res.Sessions))
	}
	got := res.Sessions[0].Title
	if want := harness.Title(raw); got != want {
		t.Errorf("title = %q, want harness.Title output %q", got, want)
	}
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("title still holds whitespace: %q", got)
	}
	if utf8.RuneCountInString(got) > 60 {
		t.Errorf("title is %d runes, want <= 60: %q", utf8.RuneCountInString(got), got)
	}
}

// sqlQuote renders s as a SQLite string literal (SQLite has no backslash
// escapes, so a JSON-quoted literal would store the escapes verbatim).
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ---------------------------------------------------------------------------
// registration & roots
// ---------------------------------------------------------------------------

func TestRegistration(t *testing.T) {
	p, ok := harness.Get(model.OpenCode)
	if !ok {
		t.Fatal("opencode parser is not registered")
	}
	if _, ok := p.(*Parser); !ok {
		t.Fatalf("registered parser has type %T", p)
	}
	if got := p.Harness(); got != model.OpenCode {
		t.Errorf("Harness() = %q", got)
	}
	if len(p.Roots()) == 0 {
		t.Error("Roots() is empty")
	}
}

func TestRootsEnvOverride(t *testing.T) {
	t.Setenv("MYTOKEN_OPENCODE_DIRS", "/tmp/oc-one"+string(os.PathListSeparator)+"/tmp/oc-two")
	p := New()
	roots := p.Roots()
	if len(roots) != 2 || roots[0] != "/tmp/oc-one" || roots[1] != "/tmp/oc-two" {
		t.Fatalf("Roots() = %v", roots)
	}
}

func TestRootsDefault(t *testing.T) {
	t.Setenv("MYTOKEN_OPENCODE_DIRS", "")
	t.Setenv("OPENCODE_DATA_HOME", "")
	t.Setenv("XDG_DATA_HOME", "/tmp/xdg")
	p := New()
	if got := p.Roots(); len(got) != 1 || got[0] != filepath.Join("/tmp/xdg", "opencode") {
		t.Fatalf("Roots() = %v", got)
	}
}

// ---------------------------------------------------------------------------
// token class mapping
// ---------------------------------------------------------------------------

// TestTokenClassMapping pins opencode's token accounting: `input` already
// excludes cached tokens, so no class is subtracted, cache read/write map
// straight through, a missing cache block is zero, and negatives are clamped.
func TestTokenClassMapping(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want model.Tokens
	}{
		{
			name: "input is not reduced by cache read or write",
			raw:  `{"input":1000,"output":200,"reasoning":50,"cache":{"write":100,"read":400}}`,
			want: model.Tokens{Input: 1000, Output: 200, CacheRead: 400, CacheWrite: 100, Reasoning: 50},
		},
		{
			name: "no cache block leaves cache classes at zero",
			raw:  `{"input":10,"output":5}`,
			want: model.Tokens{Input: 10, Output: 5},
		},
		{
			name: "empty cache object leaves cache classes at zero",
			raw:  `{"input":7,"output":3,"cache":{}}`,
			want: model.Tokens{Input: 7, Output: 3},
		},
		{
			name: "a cache read alone still does not reduce input",
			raw:  `{"input":900,"output":10,"cache":{"read":5000}}`,
			want: model.Tokens{Input: 900, Output: 10, CacheRead: 5000},
		},
		{
			name: "negative classes clamp to zero",
			raw:  `{"input":-5,"output":-1,"reasoning":-3,"cache":{"read":-4,"write":-2}}`,
			want: model.Tokens{},
		},
		{
			name: "quoted numbers decode like JSON numbers",
			raw:  `{"input":"12","output":"4","reasoning":"2","cache":{"read":"1","write":"0"}}`,
			want: model.Tokens{Input: 12, Output: 4, CacheRead: 1, Reasoning: 2},
		},
		{
			name: "fractional counters truncate toward zero",
			raw:  `{"input":1.9,"output":2.7}`,
			want: model.Tokens{Input: 1, Output: 2},
		},
		{
			name: "null reasoning is treated as absent",
			raw:  `{"input":3,"output":4,"reasoning":null,"cache":null}`,
			want: model.Tokens{Input: 3, Output: 4},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p tokenPayload
			if err := json.Unmarshal([]byte(tt.raw), &p); err != nil {
				t.Fatalf("unmarshal %s: %v", tt.raw, err)
			}
			wantTokens(t, "classes", p.classes(), tt.want)
			if got, want := p.classes().Total(), tt.want.Total(); got != want {
				t.Errorf("Total() = %d, want %d", got, want)
			}
		})
	}
}
