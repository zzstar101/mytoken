package gemini

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// Fixtures live at the repository root (testdata/gemini) like the other harness
// packages; a package-local testdata directory is preferred when one exists.
const fixtureRoot = "../../../testdata/gemini"

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
	t.Fatalf("no session %q (have %d)", id, len(metas))
	return model.SessionMeta{}
}

func wantTokens(t *testing.T, label string, got, want model.Tokens) {
	t.Helper()
	if got != want {
		t.Errorf("%s tokens = %+v, want %+v", label, got, want)
	}
}

func sumTokens(events []model.UsageEvent) model.Tokens {
	var total model.Tokens
	for _, ev := range events {
		total = total.Add(ev.Tokens)
	}
	return total
}

func utc(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// token class mapping (unit table, mirrors the upstream normalisation rules)
// ---------------------------------------------------------------------------

func TestTokenClassMapping(t *testing.T) {
	tests := []struct {
		name string
		json string
		want model.Tokens
	}{
		{
			// input is INCLUSIVE of cached: 15+20+2+0 == total 37, so the
			// cached tokens are subtracted from input exactly once.
			name: "inclusive input subtracts cached",
			json: `{"input":15,"output":20,"cached":5,"thoughts":2,"tool":0,"total":37}`,
			want: model.Tokens{Input: 10, Output: 20, CacheRead: 5, Reasoning: 2},
		},
		{
			// already exclusive: 10+20+2 == 32 != 37, +cached == 37, so input
			// must be left alone.
			name: "exclusive input is left alone",
			json: `{"input":10,"output":20,"cached":5,"thoughts":2,"total":37}`,
			want: model.Tokens{Input: 10, Output: 20, CacheRead: 5, Reasoning: 2},
		},
		{
			name: "no total passes through",
			json: `{"input":10,"output":5,"cached":3}`,
			want: model.Tokens{Input: 10, Output: 5, CacheRead: 3},
		},
		{
			name: "tool tokens count as input",
			json: `{"input":10,"output":5,"tool":7}`,
			want: model.Tokens{Input: 17, Output: 5},
		},
		{
			name: "cached larger than input",
			json: `{"input":3,"output":1,"cached":10,"total":4}`,
			want: model.Tokens{Input: 0, Output: 1, CacheRead: 10},
		},
		{
			name: "zero total does not trigger subtraction",
			json: `{"input":7,"output":2,"total":0}`,
			want: model.Tokens{Input: 7, Output: 2},
		},
		{
			name: "negative values clamp to zero",
			json: `{"input":-5,"output":-1,"cached":-2,"thoughts":-3}`,
			want: model.Tokens{},
		},
		{
			name: "camelCase aliases",
			json: `{"promptTokenCount":10,"candidatesTokenCount":20,"cachedContentTokenCount":5,"thoughtsTokenCount":2,"totalTokenCount":37}`,
			want: model.Tokens{Input: 10, Output: 20, CacheRead: 5, Reasoning: 2},
		},
		{
			name: "snake_case aliases",
			json: `{"prompt_tokens":15,"output_tokens":20,"cached_tokens":5,"thoughts_tokens":2,"tool_tokens":0,"total_tokens":37}`,
			want: model.Tokens{Input: 10, Output: 20, CacheRead: 5, Reasoning: 2},
		},
		{
			name: "quoted numbers from the log",
			json: `{"input":"12","output":"4","cached":"2"}`,
			want: model.Tokens{Input: 12, Output: 4, CacheRead: 2},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ts tokensSummary
			if err := json.Unmarshal([]byte(tc.json), &ts); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := ts.classes(); got != tc.want {
				t.Errorf("classes() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// discovery
// ---------------------------------------------------------------------------

func TestDiscover(t *testing.T) {
	p := NewWithRoots(fixture(t, tmpDirName))
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := map[string]string{
		fixture(t, "tmp", "myproj", "chats", "session-2025-03-18T10-00-00-abc12345.json"):  kindJSON,
		fixture(t, "tmp", "myproj", "chats", "session-2025-03-18T11-00-00-def67890.jsonl"): kindJSONL,
		fixture(t, "tmp", "myproj", "chats", "gem-sess-jsonl", "gem-sess-sub.jsonl"):       kindJSONL,
		fixture(t, "tmp", "02aa0b5a228aa7ad2dede71eace495f60f28fc1cd42e1ad3853902311038f9a5",
			"chats", "session-2025-03-18T12-00-00-aaa11111.json"): kindJSON,
	}
	if len(srcs) != len(want) {
		t.Fatalf("got %d sources, want %d: %v", len(srcs), len(want), srcs)
	}
	for i, src := range srcs {
		kind, ok := want[src.Path]
		if !ok {
			t.Errorf("unexpected source %s", src.Path)
			continue
		}
		if src.Kind != kind {
			t.Errorf("%s kind = %q, want %q", src.Path, src.Kind, kind)
		}
		if i > 0 && srcs[i-1].Path >= src.Path {
			t.Errorf("sources not sorted: %q >= %q", srcs[i-1].Path, src.Path)
		}
	}
	// The stale .json leftover next to the migrated .jsonl must be ignored.
	stale := fixture(t, "tmp", "myproj", "chats", "session-2025-03-18T11-00-00-def67890.json")
	for _, src := range srcs {
		if src.Path == stale {
			t.Errorf("stale %s must not be discovered", stale)
		}
	}
}

func TestDiscoverMissingRoot(t *testing.T) {
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
// full fixture parse
// ---------------------------------------------------------------------------

func TestParseFixtures(t *testing.T) {
	p := NewWithRoots(fixture(t, tmpDirName))
	res := parseAll(t, p)

	if len(res.Events) != 6 {
		t.Fatalf("got %d events, want 6: %v", len(res.Events), keysOf(res.Events))
	}
	wantTotals := model.Tokens{Input: 217, Output: 107, CacheRead: 22, Reasoning: 8}
	wantTokens(t, "total", sumTokens(res.Events), wantTotals)

	t.Run("legacy json session", func(t *testing.T) {
		first := eventByKey(t, res.Events, "gemini:gem-sess-main:g-msg-2")
		wantTokens(t, "g-msg-2", first.Tokens, model.Tokens{Input: 10, Output: 20, CacheRead: 5, Reasoning: 2})
		if first.Model != "gemini-2.5-pro" {
			t.Errorf("model = %q", first.Model)
		}
		if first.SessionID != "gem-sess-main" || first.ProjectPath != "/Users/dev/myproj" {
			t.Errorf("session/project = %q/%q", first.SessionID, first.ProjectPath)
		}
		if !first.Timestamp.Equal(utc(2025, 3, 18, 10, 0, 20)) {
			t.Errorf("timestamp = %v", first.Timestamp)
		}
		if first.Timestamp.Location() != time.UTC {
			t.Errorf("timestamp location = %v", first.Timestamp.Location())
		}
		// Gemini CLI logs carry no provider: it must stay empty rather than
		// being invented by the parser.
		if first.Provider != "" || first.BaseURL != "" {
			t.Errorf("provider/baseURL = %q/%q, want empty", first.Provider, first.BaseURL)
		}
		if first.CostUSD != nil {
			t.Errorf("CostUSD = %v, want nil", *first.CostUSD)
		}
		// The second turn's totals are already exclusive.
		second := eventByKey(t, res.Events, "gemini:gem-sess-main:g-msg-3")
		wantTokens(t, "g-msg-3", second.Tokens, model.Tokens{Input: 10, Output: 20, CacheRead: 5, Reasoning: 2})
	})

	t.Run("jsonl session merges repeated records", func(t *testing.T) {
		merged := eventByKey(t, res.Events, "gemini:gem-sess-jsonl:g-j-2")
		wantTokens(t, "g-j-2", merged.Tokens, model.Tokens{Input: 120, Output: 50})
		after := eventByKey(t, res.Events, "gemini:gem-sess-jsonl:g-j-4")
		wantTokens(t, "g-j-4", after.Tokens, model.Tokens{Input: 7, Output: 3})
		if !after.Timestamp.Equal(utc(2025, 3, 18, 11, 12, 0)) {
			t.Errorf("timestamp = %v", after.Timestamp)
		}
		// $set/$patch/$rewindTo lines are not messages and change nothing.
		if got := len(res.Events); got != 6 {
			t.Errorf("control records produced extra events (%d total)", got)
		}
	})

	t.Run("subagent linkage", func(t *testing.T) {
		sub := eventByKey(t, res.Events, "gemini:gem-sess-sub:g-sub-2")
		if sub.ParentID != "gem-sess-jsonl" {
			t.Errorf("ParentID = %q, want gem-sess-jsonl", sub.ParentID)
		}
		wantTokens(t, "g-sub-2", sub.Tokens, model.Tokens{Input: 40, Output: 8, CacheRead: 12, Reasoning: 4})
	})

	t.Run("project from hash directory", func(t *testing.T) {
		hashed := eventByKey(t, res.Events, "gemini:gem-sess-hash:g-hash-1")
		if hashed.ProjectPath != "/Users/dev/hashed" {
			t.Errorf("ProjectPath = %q, want /Users/dev/hashed", hashed.ProjectPath)
		}
	})

	t.Run("sessions", func(t *testing.T) {
		if len(res.Sessions) != 4 {
			t.Fatalf("got %d sessions, want 4", len(res.Sessions))
		}
		main := sessionByID(t, res.Sessions, "gem-sess-main")
		if main.Title != "Explain the gemini token accounting" {
			t.Errorf("title = %q (the <session_context> turn must be ignored)", main.Title)
		}
		if main.Project != "/Users/dev/myproj" {
			t.Errorf("project = %q", main.Project)
		}
		if !main.StartedAt.Equal(utc(2025, 3, 18, 10, 0, 0)) || !main.UpdatedAt.Equal(utc(2025, 3, 18, 10, 5, 0)) {
			t.Errorf("started/updated = %v/%v", main.StartedAt, main.UpdatedAt)
		}
		if main.ParentID != "" {
			t.Errorf("ParentID = %q, want empty", main.ParentID)
		}
		// No directories in the jsonl record: the project comes from
		// projects.json (slug -> path).
		jsonl := sessionByID(t, res.Sessions, "gem-sess-jsonl")
		if jsonl.Project != "/Users/dev/myproj" {
			t.Errorf("jsonl project = %q, want /Users/dev/myproj", jsonl.Project)
		}
		if jsonl.Title != "Stream the jsonl records" {
			t.Errorf("jsonl title = %q", jsonl.Title)
		}
		sub := sessionByID(t, res.Sessions, "gem-sess-sub")
		if sub.ParentID != "gem-sess-jsonl" {
			t.Errorf("subagent ParentID = %q", sub.ParentID)
		}
		if sub.Title != "Look up the accounting rules" {
			t.Errorf("subagent title = %q", sub.Title)
		}
		hashed := sessionByID(t, res.Sessions, "gem-sess-hash")
		if hashed.Project != "/Users/dev/hashed" {
			t.Errorf("hashed project = %q", hashed.Project)
		}
		for _, m := range res.Sessions {
			if m.Harness != model.Gemini {
				t.Errorf("%s harness = %q", m.SessionID, m.Harness)
			}
			if m.UpdatedAt.Before(m.StartedAt) {
				t.Errorf("%s updated %v before started %v", m.SessionID, m.UpdatedAt, m.StartedAt)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// incremental behaviour
// ---------------------------------------------------------------------------

func TestIncrementalJSONLAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proj", "chats", "session-2025-03-18T11-00-00-def67890.jsonl")
	// Pad the head past the 4KB fingerprint window (the same trick pi's tests
	// use): a resume is only attempted while the fingerprint is stable, so a
	// tiny file would replay from byte 0 on every append.
	padding := strings.Repeat("x", 5000)
	writeFile(t, path,
		`{"sessionId":"gem-inc","projectHash":"h","startTime":"2025-03-18T11:00:00.000Z",`+
			`"lastUpdated":"2025-03-18T11:05:00.000Z","summary":"`+padding+`","kind":"main"}`+"\n"+
			`{"id":"g-i-1","timestamp":"2025-03-18T11:00:05.000Z","type":"user","content":"Stream it"}`+"\n"+
			`{"id":"g-i-2","timestamp":"2025-03-18T11:00:20.000Z","type":"gemini","content":[{"text":"ok"}],`+
			`"tokens":{"input":100,"output":50,"cached":0,"thoughts":0,"tool":0,"total":150},"model":"gemini-2.5-flash"}`+"\n")
	p := NewWithRoots(dir)

	first := parseAll(t, p)
	if len(first.Events) != 1 {
		t.Fatalf("first pass: got %d events, want 1: %v", len(first.Events), keysOf(first.Events))
	}
	wantTokens(t, "g-i-2", first.Events[0].Tokens, model.Tokens{Input: 100, Output: 50})
	if idle := parseFrom(t, p, first.Cursors); len(idle.Events) != 0 {
		t.Fatalf("idle re-parse emitted %v", keysOf(idle.Events))
	}

	// A half-written trailing line must not be consumed.
	appendFile(t, path, `{"id":"g-i-9","timestamp":"2025-03-18T11:20:00.000Z","type":"gemini","content":[{"text":"partial"}],"tokens":{"input":5,"output":1}`)
	partial := parseFrom(t, p, first.Cursors)
	if len(partial.Events) != 0 {
		t.Fatalf("half-written line emitted %v", keysOf(partial.Events))
	}

	// Completing the line releases exactly that one event.
	appendFile(t, path, `,"model":"gemini-2.5-flash"}`+"\n")
	second := parseFrom(t, p, partial.Cursors)
	if len(second.Events) != 1 {
		t.Fatalf("second pass: got %d events, want 1: %v", len(second.Events), keysOf(second.Events))
	}
	wantTokens(t, "g-i-9", second.Events[0].Tokens, model.Tokens{Input: 5, Output: 1})

	// And a further turn releases only its own record.
	appendFile(t, path, `{"id":"g-i-10","timestamp":"2025-03-18T11:21:00.000Z","type":"gemini","content":[{"text":"next"}],"tokens":{"input":9,"output":2},"model":"gemini-2.5-flash"}`+"\n")
	third := parseFrom(t, p, second.Cursors)
	if len(third.Events) != 1 {
		t.Fatalf("third pass: got %d events, want 1: %v", len(third.Events), keysOf(third.Events))
	}
	wantTokens(t, "g-i-10", third.Events[0].Tokens, model.Tokens{Input: 9, Output: 2})
}

func TestJSONLRewriteReparses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proj", "chats", "session-x.jsonl")
	head := `{"sessionId":"gem-rewrite","startTime":"2025-03-18T09:00:00.000Z","lastUpdated":"2025-03-18T09:00:00.000Z","kind":"main"}` + "\n"
	writeFile(t, path, head+
		`{"id":"g-r-1","timestamp":"2025-03-18T09:00:01.000Z","type":"user","content":"hi"}`+"\n"+
		`{"id":"g-r-2","timestamp":"2025-03-18T09:00:02.000Z","type":"gemini","content":"a","tokens":{"input":100,"output":10,"total":110},"model":"gemini-2.5-pro"}`+"\n")
	p := NewWithRoots(dir)
	first := parseAll(t, p)
	if len(first.Events) != 1 {
		t.Fatalf("first pass: got %d events, want 1", len(first.Events))
	}
	wantTokens(t, "g-r-2", first.Events[0].Tokens, model.Tokens{Input: 100, Output: 10})

	// Rewriting the whole file (same length prefix changes) keeps the same
	// DedupKey but carries the new usage.
	writeFile(t, path, head+
		`{"id":"g-r-1","timestamp":"2025-03-18T09:00:01.000Z","type":"user","content":"hi"}`+"\n"+
		`{"id":"g-r-2","timestamp":"2025-03-18T09:00:02.000Z","type":"gemini","content":"a","tokens":{"input":200,"output":20,"total":220},"model":"gemini-2.5-pro"}`+"\n")
	second := parseFrom(t, p, first.Cursors)
	if len(second.Events) != 1 {
		t.Fatalf("second pass: got %d events, want 1: %v", len(second.Events), keysOf(second.Events))
	}
	if second.Events[0].DedupKey != first.Events[0].DedupKey {
		t.Errorf("DedupKey changed on rewrite: %q -> %q", first.Events[0].DedupKey, second.Events[0].DedupKey)
	}
	wantTokens(t, "rewritten", second.Events[0].Tokens, model.Tokens{Input: 200, Output: 20})
}

func TestLegacyJSONReparseWithoutCursor(t *testing.T) {
	// Whole-file (.json) parses are never incremental: replaying from a zero
	// cursor must reproduce the same DedupKeys.
	p := NewWithRoots(fixture(t, tmpDirName))
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range srcs {
		if src.Kind != kindJSON {
			continue
		}
		first, err := p.Parse(context.Background(), src, harness.Cursor{})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		second, err := p.Parse(context.Background(), src, harness.Cursor{})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(first.Events) != len(second.Events) {
			t.Fatalf("%s: %d vs %d events", src.Path, len(first.Events), len(second.Events))
		}
		for i := range first.Events {
			if first.Events[i].DedupKey != second.Events[i].DedupKey {
				t.Errorf("%s: key %q != %q", src.Path, first.Events[i].DedupKey, second.Events[i].DedupKey)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// title rules
// ---------------------------------------------------------------------------

func TestTitleRules(t *testing.T) {
	long := "  line one\nsecond\tline  " + strings.Repeat("padding text ", 10)
	records := map[string]any{
		"sessionId":   "gem-title",
		"startTime":   "2025-03-18T08:00:00.000Z",
		"lastUpdated": "2025-03-18T08:00:05.000Z",
		"summary":     "Summary fallback",
		"kind":        "main",
		"messages": []any{
			map[string]any{"id": "t-1", "timestamp": "2025-03-18T08:00:01.000Z", "type": "user", "content": "/help"},
			map[string]any{"id": "t-2", "timestamp": "2025-03-18T08:00:02.000Z", "type": "user", "content": "<hook_context>ignored"},
			map[string]any{"id": "t-3", "timestamp": "2025-03-18T08:00:03.000Z", "type": "user", "content": long},
		},
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "proj", "chats", "session-title.json")
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(raw))

	p := NewWithRoots(dir)
	res := parseAll(t, p)
	if len(res.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(res.Sessions))
	}
	got := res.Sessions[0].Title
	if want := harness.Title(long); got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("title still contains whitespace: %q", got)
	}
	if utf8.RuneCountInString(got) > 60 {
		t.Errorf("title = %d runes, want <= 60: %q", utf8.RuneCountInString(got), got)
	}
	if len(res.Events) != 0 {
		t.Errorf("expected no usage events, got %v", keysOf(res.Events))
	}
}

func TestTitleFallsBackToSummary(t *testing.T) {
	records := map[string]any{
		"sessionId":   "gem-nouser",
		"startTime":   "2025-03-18T08:00:00.000Z",
		"lastUpdated": "2025-03-18T08:00:05.000Z",
		"summary":     "Only plumbing here",
		"messages": []any{
			map[string]any{"id": "n-1", "timestamp": "2025-03-18T08:00:01.000Z", "type": "user", "content": "<session_context>x"},
			map[string]any{"id": "n-2", "timestamp": "2025-03-18T08:00:02.000Z", "type": "gemini", "content": "hi", "model": "m"},
		},
	}
	dir := t.TempDir()
	raw, _ := json.Marshal(records)
	writeFile(t, filepath.Join(dir, "proj", "chats", "session-nouser.json"), string(raw))

	res := parseAll(t, NewWithRoots(dir))
	if len(res.Sessions) != 1 {
		t.Fatalf("got %d sessions", len(res.Sessions))
	}
	if got := res.Sessions[0].Title; got != "Only plumbing here" {
		t.Errorf("title = %q, want the summary fallback", got)
	}
}

// ---------------------------------------------------------------------------
// registration & roots
// ---------------------------------------------------------------------------

func TestRegistration(t *testing.T) {
	p, ok := harness.Get(model.Gemini)
	if !ok {
		t.Fatal("gemini parser is not registered")
	}
	if _, ok := p.(*Parser); !ok {
		t.Fatalf("registered parser has type %T", p)
	}
	if got := p.Harness(); got != model.Gemini {
		t.Errorf("Harness() = %q", got)
	}
	if len(p.Roots()) == 0 {
		t.Error("Roots() is empty")
	}
}

func TestRootsEnvOverride(t *testing.T) {
	t.Setenv("MYTOKEN_GEMINI_DIRS", "/tmp/g1"+string(os.PathListSeparator)+"/tmp/g2")
	p := New()
	roots := p.Roots()
	if len(roots) != 2 || roots[0] != "/tmp/g1" || roots[1] != "/tmp/g2" {
		t.Fatalf("Roots() = %v", roots)
	}
}

func TestRootsGeminiCliHome(t *testing.T) {
	t.Setenv("MYTOKEN_GEMINI_DIRS", "")
	t.Setenv("GEMINI_CLI_HOME", "/tmp/gem-home")
	p := New()
	want := []string{
		filepath.Join("/tmp/gem-home", ".gemini", tmpDirName),
		filepath.Join("/tmp/gem-home", tmpDirName),
	}
	got := p.Roots()
	if len(got) != len(want) {
		t.Fatalf("Roots() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Roots()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
