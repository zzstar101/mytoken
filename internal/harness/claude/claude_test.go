package claude

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const fixtureRoot = "../../../testdata/claude"

func parseFile(t *testing.T, p *Parser, path string, cur harness.Cursor) harness.Batch {
	t.Helper()
	b, err := p.Parse(context.Background(), harness.Source{Path: path, Kind: "jsonl"}, cur)
	if err != nil {
		t.Fatalf("Parse(%s): %v", path, err)
	}
	return b
}

func sum(events []model.UsageEvent) model.Tokens {
	var total model.Tokens
	for _, e := range events {
		total = total.Add(e.Tokens)
	}
	return total
}

func eventByKey(t *testing.T, b harness.Batch, key string) model.UsageEvent {
	t.Helper()
	for _, e := range b.Events {
		if e.DedupKey == key {
			return e
		}
	}
	t.Fatalf("no event with DedupKey %q (have %v)", key, keys(b))
	return model.UsageEvent{}
}

func keys(b harness.Batch) []string {
	var out []string
	for _, e := range b.Events {
		out = append(out, e.DedupKey)
	}
	return out
}

func metaByID(t *testing.T, b harness.Batch, id string) model.SessionMeta {
	t.Helper()
	for _, m := range b.Sessions {
		if m.SessionID == id {
			return m
		}
	}
	t.Fatalf("no session meta %q (have %v)", id, b.Sessions)
	return model.SessionMeta{}
}

// TestParseFixtures is the table-driven core: exact token totals, session
// count, dedup and parent linkage per fixture file.
func TestParseFixtures(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		wantEvents int
		want       model.Tokens
		wantSess   []string
	}{
		{
			name:       "main transcript",
			path:       filepath.Join(fixtureRoot, "-demo-proj", "sess-1.jsonl"),
			wantEvents: 3, // msg_demo_1 (2 streamed lines merged), msg_demo_2, inline sidechain
			want:       model.Tokens{Input: 307, Output: 51, CacheRead: 1000, CacheWrite: 30, Reasoning: 12},
			wantSess:   []string{"sess-1", "agent-inline"},
		},
		{
			name:       "subagent transcript",
			path:       filepath.Join(fixtureRoot, "-demo-proj", "sess-1", "subagents", "agent-abc.jsonl"),
			wantEvents: 1,
			want:       model.Tokens{Input: 11, Output: 4},
			wantSess:   []string{"agent-abc"},
		},
		{
			name:       "second session",
			path:       filepath.Join(fixtureRoot, "-demo-proj", "sess-2.jsonl"),
			wantEvents: 1,
			want:       model.Tokens{Input: 5, Output: 5},
			wantSess:   []string{"sess-2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewWithRoots(fixtureRoot)
			b := parseFile(t, p, tc.path, harness.Cursor{})
			if len(b.Events) != tc.wantEvents {
				t.Fatalf("events = %d, want %d (%v)", len(b.Events), tc.wantEvents, keys(b))
			}
			if got := sum(b.Events); got != tc.want {
				t.Errorf("tokens = %+v, want %+v", got, tc.want)
			}
			if len(b.Sessions) != len(tc.wantSess) {
				t.Fatalf("sessions = %d, want %d", len(b.Sessions), len(tc.wantSess))
			}
			for _, id := range tc.wantSess {
				metaByID(t, b, id)
			}
			if b.Next.Offset != b.Next.Size {
				t.Errorf("Next.Offset = %d, want size %d", b.Next.Offset, b.Next.Size)
			}
			if b.Next.Fingerprint == "" {
				t.Error("Next.Fingerprint is empty")
			}
		})
	}
}

func TestStreamedDuplicateMergesAndSkipsSynthetic(t *testing.T) {
	p := NewWithRoots(fixtureRoot)
	b := parseFile(t, p, filepath.Join(fixtureRoot, "-demo-proj", "sess-1.jsonl"), harness.Cursor{})

	ev := eventByKey(t, b, "claude:msg_demo_1:req_demo_1")
	// Streaming lines 20/thinking 5 then 50/thinking 12 => max per field, and
	// thinking is split out of output so Total is unchanged.
	if want := (model.Tokens{Input: 100, Output: 38, CacheRead: 1000, CacheWrite: 30, Reasoning: 12}); ev.Tokens != want {
		t.Errorf("streamed merge = %+v, want %+v", ev.Tokens, want)
	}
	if !ev.Timestamp.Equal(time.Date(2026, 1, 2, 3, 4, 9, 0, time.UTC)) {
		t.Errorf("timestamp = %s, want the later streamed line", ev.Timestamp)
	}
	for _, e := range b.Events {
		if e.Model == "<synthetic>" || e.Tokens.Input == 9999 {
			t.Fatalf("synthetic model was counted: %+v", e)
		}
	}
	// Inline sidechain entry becomes a child session of the transcript.
	child := eventByKey(t, b, "claude:msg_demo_4:req_demo_4")
	if child.SessionID != "agent-inline" || child.ParentID != "sess-1" {
		t.Errorf("sidechain event = %s/%s, want agent-inline/sess-1", child.SessionID, child.ParentID)
	}
}

func TestSessionMeta(t *testing.T) {
	p := NewWithRoots(fixtureRoot)
	b := parseFile(t, p, filepath.Join(fixtureRoot, "-demo-proj", "sess-1.jsonl"), harness.Cursor{})

	m := metaByID(t, b, "sess-1")
	if m.Title != "Design the demo landing page" {
		t.Errorf("title = %q, want the first real user message", m.Title)
	}
	if m.Project != "/Users/demo/demo-proj" {
		t.Errorf("project = %q", m.Project)
	}
	if !m.StartedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("startedAt = %s", m.StartedAt)
	}
	if !m.UpdatedAt.Equal(time.Date(2026, 1, 2, 3, 4, 13, 0, time.UTC)) {
		t.Errorf("updatedAt = %s", m.UpdatedAt)
	}
	if m.ParentID != "" {
		t.Errorf("parentId = %q, want empty", m.ParentID)
	}

	sub := parseFile(t, p, filepath.Join(fixtureRoot, "-demo-proj", "sess-1", "subagents", "agent-abc.jsonl"), harness.Cursor{})
	sm := metaByID(t, sub, "agent-abc")
	if sm.ParentID != "sess-1" {
		t.Errorf("subagent parentId = %q, want sess-1", sm.ParentID)
	}
	if sm.Title != "Subagent task prompt" {
		t.Errorf("subagent title = %q", sm.Title)
	}
	if sub.Events[0].ParentID != "sess-1" {
		t.Errorf("subagent event parentId = %q", sub.Events[0].ParentID)
	}
}

func TestDiscover(t *testing.T) {
	p := NewWithRoots(fixtureRoot)
	got, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range got {
		rel, _ := filepath.Rel(fixtureRoot, s.Path)
		names = append(names, filepath.ToSlash(rel))
		if s.Kind != "jsonl" {
			t.Errorf("%s: kind = %q", s.Path, s.Kind)
		}
	}
	want := []string{"-demo-proj/sess-1.jsonl", "-demo-proj/sess-1/subagents/agent-abc.jsonl", "-demo-proj/sess-2.jsonl"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("discovered %v, want %v (custom-title.json must be skipped)", names, want)
	}
}

// collapse mirrors the downstream store: duplicate DedupKeys are allowed and
// the last one wins.
func collapse(events []model.UsageEvent) (model.Tokens, int) {
	index := map[string]int{}
	var out []model.UsageEvent
	for _, e := range events {
		if e.DedupKey == "" {
			out = append(out, e)
			continue
		}
		if i, ok := index[e.DedupKey]; ok {
			out[i] = e
			continue
		}
		index[e.DedupKey] = len(out)
		out = append(out, e)
	}
	return sum(out), len(out)
}

// TestIncrementalResume parses an append-only file in two passes and requires
// the collapsed result to equal one full parse.
func TestIncrementalResume(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtureRoot, "-demo-proj", "sess-1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// Split at a line boundary, halfway through the file.
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	cut, seen := 0, 0
	for seen < lines/2 {
		i := strings.IndexByte(string(data[cut:]), '\n')
		if i < 0 {
			break
		}
		cut += i + 1
		seen++
	}
	// The filler keeps the first 4KB (and therefore the fingerprint) stable
	// while the file is appended to, like a real append-only transcript.
	filler := strings.Repeat(`{"type":"file-history-snapshot","messageId":"snap","timestamp":"2026-01-02T03:04:14.000Z"}`+"\n", 60)
	path := filepath.Join(t.TempDir(), "sess-1.jsonl")
	if err := os.WriteFile(path, []byte(filler+string(data[:cut])), 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewWithRoots(filepath.Dir(path))
	b1 := parseFile(t, p, path, harness.Cursor{})
	if b1.Next.Offset != b1.Next.Size {
		t.Fatalf("first pass offset = %d, want size %d", b1.Next.Offset, b1.Next.Size)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data[cut:]); err != nil {
		t.Fatal(err)
	}
	f.Close()

	b2 := parseFile(t, p, path, b1.Next)
	if b2.Next.Offset != b2.Next.Size {
		t.Errorf("resumed offset = %d, want size %d", b2.Next.Offset, b2.Next.Size)
	}
	if b2.Next.Fingerprint != b1.Next.Fingerprint {
		t.Errorf("fingerprint changed on append: %s -> %s", b1.Next.Fingerprint, b2.Next.Fingerprint)
	}
	oneShot := parseFile(t, p, path, harness.Cursor{})
	got, gotN := collapse(append(append([]model.UsageEvent{}, b1.Events...), b2.Events...))
	want, wantN := collapse(oneShot.Events)
	if got != want {
		t.Errorf("resumed tokens = %+v, want %+v", got, want)
	}
	if gotN != wantN {
		t.Errorf("resumed events = %d, want %d", gotN, wantN)
	}
}

// TestTruncatedTrailingLine requires that a partially written last line is not
// consumed, and that completing it does not lose or duplicate anything.
func TestTruncatedTrailingLine(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtureRoot, "-demo-proj", "sess-1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lastStart := bytes.LastIndexByte(data[:len(data)-1], '\n') + 1 // custom-title line
	body := data[:lastStart]
	partial := data[lastStart : lastStart+(len(data)-lastStart)/2]
	filler := strings.Repeat(`{"type":"file-history-snapshot","messageId":"snap","timestamp":"2026-01-02T03:04:14.000Z"}`+"\n", 60)
	path := filepath.Join(t.TempDir(), "sess-1.jsonl")
	if err := os.WriteFile(path, append([]byte(filler), append(body, partial...)...), 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewWithRoots(filepath.Dir(path))
	b := parseFile(t, p, path, harness.Cursor{})
	wantOffset := int64(len(filler) + len(body))
	if b.Next.Offset != wantOffset {
		t.Errorf("offset = %d, want %d (end of last complete line)", b.Next.Offset, wantOffset)
	}
	if b.Next.Offset >= b.Next.Size {
		t.Error("parser consumed the incomplete trailing line")
	}
	wantTokens := model.Tokens{Input: 307, Output: 51, CacheRead: 1000, CacheWrite: 30, Reasoning: 12}
	if got := sum(b.Events); got != wantTokens {
		t.Errorf("tokens = %+v, want %+v", got, wantTokens)
	}

	// Complete the line: the record must be consumed exactly once.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data[lastStart+len(partial):]); err != nil {
		t.Fatal(err)
	}
	f.Close()
	b2 := parseFile(t, p, path, b.Next)
	if b2.Next.Offset != b2.Next.Size {
		t.Errorf("offset = %d, want size %d", b2.Next.Offset, b2.Next.Size)
	}
	if len(b2.Events) != 0 {
		t.Errorf("custom-title line produced events: %v", keys(b2))
	}
}

// TestFingerprintChangeReparsesFromZero covers a rewritten transcript.
func TestFingerprintChangeReparsesFromZero(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtureRoot, "-demo-proj", "sess-2.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sess-2.jsonl")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoots(filepath.Dir(path))
	b1 := parseFile(t, p, path, harness.Cursor{})

	// Rewrite the head: same parser state, different fingerprint.
	rewritten := strings.Replace(string(data), `"sess-2"`, `"sess-2b"`, 1)
	if err := os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}
	b2 := parseFile(t, p, path, b1.Next)
	if len(b2.Events) != len(b1.Events) {
		t.Fatalf("after fingerprint change events = %d, want a full reparse (%d)", len(b2.Events), len(b1.Events))
	}
	if got := sum(b2.Events); got != sum(b1.Events) {
		t.Errorf("reparse tokens = %+v, want %+v", got, sum(b1.Events))
	}
}

// TestTitleFiltering is table-driven over the kinds of user lines that must not
// become a session title.
func TestTitleFiltering(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{
			name: "tool result and wrapper are skipped",
			lines: []string{
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"x"}]},"sessionId":"s","timestamp":"2026-01-02T03:04:05.000Z"}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>be nice</system-reminder>"}]},"sessionId":"s","timestamp":"2026-01-02T03:04:06.000Z"}`,
				`{"type":"user","isMeta":true,"message":{"role":"user","content":"meta note"},"sessionId":"s","timestamp":"2026-01-02T03:04:07.000Z"}`,
				`{"type":"user","message":{"role":"user","content":"  Real   prompt  here "},"sessionId":"s","timestamp":"2026-01-02T03:04:08.000Z"}`,
			},
			want: "Real prompt here",
		},
		{
			name: "custom title is the fallback",
			lines: []string{
				`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>"},"sessionId":"s","timestamp":"2026-01-02T03:04:05.000Z"}`,
				`{"type":"custom-title","customTitle":"Fallback title","sessionId":"s","timestamp":"2026-01-02T03:04:06.000Z"}`,
			},
			want: "Fallback title",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.jsonl")
			if err := os.WriteFile(path, []byte(strings.Join(tc.lines, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			p := NewWithRoots(filepath.Dir(path))
			b := parseFile(t, p, path, harness.Cursor{})
			if len(b.Sessions) != 1 {
				t.Fatalf("sessions = %d, want 1", len(b.Sessions))
			}
			if b.Sessions[0].Title != tc.want {
				t.Errorf("title = %q, want %q", b.Sessions[0].Title, tc.want)
			}
		})
	}
}

// TestEmptyAndMissingRoot covers Discover on a nonexistent root.
func TestEmptyAndMissingRoot(t *testing.T) {
	p := NewWithRoots(filepath.Join(t.TempDir(), "nope"))
	got, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("sources = %d, want 0", len(got))
	}
	if p.Harness() != model.ClaudeCode {
		t.Errorf("Harness = %q", p.Harness())
	}
	if _, ok := harness.Get(model.ClaudeCode); !ok {
		t.Error("parser was not registered by init()")
	}
	if len(p.Roots()) != 1 {
		t.Errorf("Roots = %v", p.Roots())
	}
}

// TestRealData parses the real Claude Code logs. Gated by MYTOKEN_REALDATA=1.
func TestRealData(t *testing.T) {
	if os.Getenv("MYTOKEN_REALDATA") != "1" {
		t.Skip("set MYTOKEN_REALDATA=1 to scan real log directories")
	}
	ctx := context.Background()
	start := time.Now()
	p := New()
	sources, err := p.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	perModel := map[string]model.Tokens{}
	sessions := map[string]bool{}
	var events int
	var parseErr int
	for _, src := range sources {
		b, err := p.Parse(ctx, src, harness.Cursor{})
		if err != nil {
			parseErr++
			t.Logf("parse error %s: %v", src.Path, err)
			continue
		}
		events += len(b.Events)
		for _, e := range b.Events {
			perModel[e.Model] = perModel[e.Model].Add(e.Tokens)
		}
		for _, m := range b.Sessions {
			sessions[m.SessionID] = true
		}
	}
	var total model.Tokens
	for _, tk := range perModel {
		total = total.Add(tk)
	}
	t.Logf("real data: %d files, %d events, %d sessions, %d parse errors in %s",
		len(sources), events, len(sessions), parseErr, time.Since(start).Round(time.Millisecond))
	for modelName, tk := range perModel {
		t.Logf("  %-24s in=%d out=%d cacheRead=%d cacheWrite=%d reasoning=%d total=%d",
			modelName, tk.Input, tk.Output, tk.CacheRead, tk.CacheWrite, tk.Reasoning, tk.Total())
	}
	t.Logf("  %-24s total=%d", "ALL MODELS", total.Total())
}

// TestDecodeLineTolerant locks the lenient decode: a retyped or unknown field
// must not discard an assistant record that carries usage.
func TestDecodeLineTolerant(t *testing.T) {
	line := `{"type":"assistant","timestamp":"2026-01-02T03:04:05.000Z","sessionId":"s-1","cwd":"/tmp/d","uuid":"u-1","requestId":null,"isSidechain":"true","message":{"id":"msg_1","model":"claude-opus-5-5","role":"assistant","usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":2}}}`
	rl, ok := decodeLine([]byte(line))
	if !ok {
		t.Fatal("decodeLine failed")
	}
	if rl.Type != "assistant" || rl.SessionID != "s-1" || rl.UUID != "u-1" || rl.Cwd != "/tmp/d" {
		t.Errorf("fields = %+v", rl)
	}
	if !rl.IsSidechain {
		t.Error("string \"true\" isSidechain should decode as true")
	}
	if rl.Message == nil || rl.Message.Usage == nil {
		t.Fatalf("message = %+v", rl.Message)
	}
	if got := rl.Message.Usage.InputTokens; got != 10 {
		t.Errorf("input = %d, want 10", got)
	}

	// A retyped unrelated field (type as a number) still yields the usage.
	rl2, ok := decodeLine([]byte(`{"type":7,"timestamp":"2026-01-02T03:04:05.000Z","message":{"id":"m","model":"x","usage":{"output_tokens":3}}}`))
	if !ok || rl2.Message == nil || rl2.Message.Usage == nil || rl2.Message.Usage.OutputTokens != 3 {
		t.Fatalf("lenient decode = %+v (ok=%v)", rl2, ok)
	}
}
