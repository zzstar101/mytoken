package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	fixtureRoot = "../../../testdata/codex"
	mainRollout = fixtureRoot + "/sessions/2026/01/02/rollout-2026-01-02T03-04-05-019aaa00-1111-7222-8333-444455556666.jsonl"
	subRollout  = fixtureRoot + "/sessions/2026/01/02/rollout-2026-01-02T04-00-00-019bbb00-2222-7333-8444-555566667777.jsonl"
	archRollout = fixtureRoot + "/archived_sessions/rollout-2026-01-01T00-00-00-019ccc00-3333-7444-8555-666677778888.jsonl"
	fillerLine  = `{"timestamp":"2026-01-02T05:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"filler"}}` + "\n"
)

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

func keys(b harness.Batch) []string {
	var out []string
	for _, e := range b.Events {
		out = append(out, e.DedupKey)
	}
	return out
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

// TestParseFixtures is the table-driven core over the sanitized rollouts.
func TestParseFixtures(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		wantEvents int
		want       model.Tokens
		wantSess   string
		wantParent string
		wantModel  string
		wantTitle  string
	}{
		{
			// The token_usage_record / token_count pair, a repeated
			// token_count, and a replayed token_usage_record all describe
			// requests already counted: 3 records => 3 events.
			name:       "main rollout with duplicated records",
			path:       mainRollout,
			wantEvents: 3,
			want:       model.Tokens{Input: 1000, Output: 56, CacheRead: 500, Reasoning: 34},
			wantSess:   "019aaa00-1111-7222-8333-444455556666",
			wantModel:  "gpt-6.1-sol",
			wantTitle:  "Summarize the demo rollout",
		},
		{
			name:       "subagent rollout links to parent thread",
			path:       subRollout,
			wantEvents: 1,
			want:       model.Tokens{Input: 400, Output: 30, CacheRead: 100, Reasoning: 10},
			wantSess:   "019bbb00-2222-7333-8444-555566667777",
			wantParent: "019aaa00-1111-7222-8333-444455556666",
			wantModel:  "gpt-6.1-sol",
			wantTitle:  "Sub rollout prompt",
		},
		{
			name:       "archived rollout without token_count events",
			path:       archRollout,
			wantEvents: 2,
			want:       model.Tokens{Input: 130, Output: 14, CacheRead: 20, Reasoning: 1},
			wantSess:   "019ccc00-3333-7444-8555-666677778888",
			wantModel:  "gpt-5.9-mini",
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
			if len(b.Sessions) != 1 {
				t.Fatalf("sessions = %d, want 1", len(b.Sessions))
			}
			m := b.Sessions[0]
			if m.SessionID != tc.wantSess {
				t.Errorf("sessionId = %q, want %q", m.SessionID, tc.wantSess)
			}
			if m.ParentID != tc.wantParent {
				t.Errorf("parentId = %q, want %q", m.ParentID, tc.wantParent)
			}
			if m.Title != tc.wantTitle {
				t.Errorf("title = %q, want %q", m.Title, tc.wantTitle)
			}
			for _, e := range b.Events {
				if e.Model != tc.wantModel {
					t.Errorf("model = %q, want %q", e.Model, tc.wantModel)
				}
				if e.SessionID != tc.wantSess || e.ParentID != tc.wantParent {
					t.Errorf("event session/parent = %q/%q", e.SessionID, e.ParentID)
				}
				if e.Timestamp.Location() != time.UTC {
					t.Errorf("timestamp not UTC: %s", e.Timestamp)
				}
			}
			if b.Next.Offset != b.Next.Size {
				t.Errorf("Next.Offset = %d, want size %d", b.Next.Offset, b.Next.Size)
			}
		})
	}
}

// TestCumulativeInvariant checks that the summed increments equal the final
// cumulative snapshot, which only holds if reasoning/cached are normalized out
// of output/input and every record is counted exactly once.
func TestCumulativeInvariant(t *testing.T) {
	for _, path := range []string{mainRollout, subRollout, archRollout} {
		p := NewWithRoots(fixtureRoot)
		b := parseFile(t, p, path, harness.Cursor{})
		last := b.Events[len(b.Events)-1]
		// DedupKey embeds the cumulative snapshot; the last event's total is
		// the final cumulative total of the thread.
		var cumulative int64
		for _, e := range b.Events {
			cumulative += e.Tokens.Total()
		}
		if cumulative != 1590 && path == mainRollout {
			t.Errorf("%s: summed increments = %d, want the final cumulative 1590", path, cumulative)
		}
		if cumulative != 165 && path == archRollout {
			t.Errorf("%s: summed increments = %d, want the final cumulative 165", path, cumulative)
		}
		if cumulative != 540 && path == subRollout {
			t.Errorf("%s: summed increments = %d, want 540", path, cumulative)
		}
		if last.Timestamp.IsZero() {
			t.Errorf("%s: last event has no timestamp", path)
		}
	}
}

func TestSessionMetaAndProvider(t *testing.T) {
	p := NewWithRoots(fixtureRoot)
	b := parseFile(t, p, mainRollout, harness.Cursor{})
	m := b.Sessions[0]
	if m.Project != "/Users/demo/codex-proj" {
		t.Errorf("project = %q", m.Project)
	}
	if !m.StartedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("startedAt = %s", m.StartedAt)
	}
	if !m.UpdatedAt.Equal(time.Date(2026, 1, 2, 3, 4, 8, 0, time.UTC)) {
		t.Errorf("updatedAt = %s", m.UpdatedAt)
	}
	// model_provider is explicit in session_meta.
	if got := b.Events[0].Provider; got != "custom" {
		t.Errorf("provider = %q, want custom", got)
	}
	if b.Events[0].BaseURL != "" {
		t.Errorf("baseUrl = %q, want empty (not in the log)", b.Events[0].BaseURL)
	}
	// The AGENTS.md dump must not become the title.
	if strings.Contains(m.Title, "AGENTS.md") {
		t.Errorf("title = %q", m.Title)
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
	}
	want := []string{
		"archived_sessions/rollout-2026-01-01T00-00-00-019ccc00-3333-7444-8555-666677778888.jsonl",
		"sessions/2026/01/02/rollout-2026-01-02T03-04-05-019aaa00-1111-7222-8333-444455556666.jsonl",
		"sessions/2026/01/02/rollout-2026-01-02T04-00-00-019bbb00-2222-7333-8444-555566667777.jsonl",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("discovered %v, want %v", names, want)
	}
	if p.Harness() != model.Codex {
		t.Errorf("Harness = %q", p.Harness())
	}
	if _, ok := harness.Get(model.Codex); !ok {
		t.Error("parser was not registered by init()")
	}
	if roots := p.Roots(); len(roots) != 1 || roots[0] != fixtureRoot {
		t.Errorf("Roots = %v, want [%s]", roots, fixtureRoot)
	}
}

// TestNormalization is table-driven over hand-written rollouts covering the
// normalization and dedup rules.
func TestNormalization(t *testing.T) {
	usage := func(in, cached, write, out, reasoning, total int64) string {
		return `{"input_tokens":` + itoa(in) + `,"cached_input_tokens":` + itoa(cached) +
			`,"cache_write_input_tokens":` + itoa(write) + `,"output_tokens":` + itoa(out) +
			`,"reasoning_output_tokens":` + itoa(reasoning) + `,"total_tokens":` + itoa(total) + `}`
	}
	tc := func(total, last string) string {
		return `{"timestamp":"2026-01-02T03:04:06.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":` + total + `,"last_token_usage":` + last + `}}}`
	}
	meta := `{"timestamp":"2026-01-02T03:04:05.000Z","type":"session_meta","payload":{"session_id":"s-1","id":"s-1","cwd":"/tmp/demo","model_provider":"custom"}}`

	cases := []struct {
		name       string
		lines      []string
		wantEvents int
		want       model.Tokens
	}{
		{
			name:       "reasoning and cached are subsets",
			lines:      []string{meta, tc(usage(1000, 400, 0, 50, 20, 1050), usage(1000, 400, 0, 50, 20, 1050))},
			wantEvents: 1,
			want:       model.Tokens{Input: 600, Output: 30, CacheRead: 400, Reasoning: 20},
		},
		{
			name:       "cache write is excluded from input",
			lines:      []string{meta, tc(usage(100, 0, 30, 10, 0, 110), usage(100, 0, 30, 10, 0, 110))},
			wantEvents: 1,
			want:       model.Tokens{Input: 70, Output: 10, CacheWrite: 30},
		},
		{
			name:       "repeated identical snapshots collapse",
			lines:      []string{meta, tc(usage(10, 0, 0, 5, 0, 15), usage(10, 0, 0, 5, 0, 15)), tc(usage(10, 0, 0, 5, 0, 15), usage(10, 0, 0, 5, 0, 15))},
			wantEvents: 1,
			want:       model.Tokens{Input: 10, Output: 5},
		},
		{
			name: "two requests accumulate",
			lines: []string{
				meta,
				tc(usage(10, 0, 0, 5, 0, 15), usage(10, 0, 0, 5, 0, 15)),
				tc(usage(30, 10, 0, 9, 4, 39), usage(20, 10, 0, 4, 4, 24)),
			},
			wantEvents: 2,
			want:       model.Tokens{Input: 20, Output: 5, CacheRead: 10, Reasoning: 4},
		},
		{
			name:       "no usage yields no events but a session",
			lines:      []string{meta},
			wantEvents: 0,
			want:       model.Tokens{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout-2026-01-02T03-04-05-019ddd00-4444-7555-8666-777788889999.jsonl")
			if err := os.WriteFile(path, []byte(strings.Join(tc.lines, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			p := NewWithRoots(filepath.Dir(path))
			b := parseFile(t, p, path, harness.Cursor{})
			if len(b.Events) != tc.wantEvents {
				t.Fatalf("events = %d, want %d (%v)", len(b.Events), tc.wantEvents, keys(b))
			}
			if got := sum(b.Events); got != tc.want {
				t.Errorf("tokens = %+v, want %+v", got, tc.want)
			}
			if len(b.Sessions) != 1 {
				t.Fatalf("sessions = %d, want 1", len(b.Sessions))
			}
			if b.Sessions[0].SessionID != "s-1" && tc.wantEvents >= 0 {
				t.Errorf("sessionId = %q, want s-1", b.Sessions[0].SessionID)
			}
		})
	}
}

// TestIncrementalResumeWithDuplicateTail appends a replay of the last counted
// snapshot plus a new request; only the new request may be emitted, which
// requires the previous snapshot to survive in Cursor.Extra.
func TestIncrementalResumeWithDuplicateTail(t *testing.T) {
	data, err := os.ReadFile(mainRollout)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	head := strings.Join(lines[:5], "\n") + "\n" // through the first token_count pair
	tail := strings.Join(lines[5:], "\n") + "\n"
	filler := strings.Repeat(fillerLine, 60)
	path := filepath.Join(t.TempDir(), "rollout-2026-01-02T03-04-05-019aaa00-1111-7222-8333-444455556666.jsonl")
	if err := os.WriteFile(path, []byte(filler+head), 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewWithRoots(filepath.Dir(path))
	b1 := parseFile(t, p, path, harness.Cursor{})
	if len(b1.Events) != 1 {
		t.Fatalf("first pass events = %d, want 1", len(b1.Events))
	}
	if b1.Next.Extra == "" {
		t.Error("Cursor.Extra is empty; incremental state was not persisted")
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(tail); err != nil {
		t.Fatal(err)
	}
	f.Close()

	b2 := parseFile(t, p, path, b1.Next)
	if b2.Next.Offset != b2.Next.Size {
		t.Errorf("resumed offset = %d, want size %d", b2.Next.Offset, b2.Next.Size)
	}
	if b2.Next.Fingerprint != b1.Next.Fingerprint {
		t.Errorf("fingerprint changed on append")
	}
	// The tail replays the first snapshot twice before the two new requests
	// (r2 = 100/6/100/4, r3 = 300/20/0/10).
	if got := sum(b2.Events); got != (model.Tokens{Input: 400, Output: 26, CacheRead: 100, Reasoning: 14}) {
		t.Errorf("resumed tokens = %+v, want only the two new requests", got)
	}
	if len(b2.Events) != 2 {
		t.Errorf("resumed events = %d, want 2 (%v)", len(b2.Events), keys(b2))
	}
	oneShot := parseFile(t, p, path, harness.Cursor{})
	got, gotN := collapse(append(append([]model.UsageEvent{}, b1.Events...), b2.Events...))
	want, wantN := collapse(oneShot.Events)
	if got != want || gotN != wantN {
		t.Errorf("resumed = %+v/%d events, want %+v/%d", got, gotN, want, wantN)
	}
}

// TestTruncatedTrailingLine requires that a partially written last line is not
// consumed and is counted exactly once after it is completed.
func TestTruncatedTrailingLine(t *testing.T) {
	data, err := os.ReadFile(mainRollout)
	if err != nil {
		t.Fatal(err)
	}
	lastStart := bytes.LastIndexByte(data[:len(data)-1], '\n') + 1
	body := data[:lastStart]
	partial := data[lastStart : lastStart+(len(data)-lastStart)/2]
	filler := strings.Repeat(fillerLine, 60)
	path := filepath.Join(t.TempDir(), "rollout-2026-01-02T03-04-05-019aaa00-1111-7222-8333-444455556666.jsonl")
	if err := os.WriteFile(path, append([]byte(filler), append(body, partial...)...), 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewWithRoots(filepath.Dir(path))
	b := parseFile(t, p, path, harness.Cursor{})
	if want := int64(len(filler) + len(body)); b.Next.Offset != want {
		t.Errorf("offset = %d, want %d (end of last complete line)", b.Next.Offset, want)
	}
	if b.Next.Offset >= b.Next.Size {
		t.Error("parser consumed the incomplete trailing line")
	}
	// The truncated record is the third request: only the first two are counted.
	if got := sum(b.Events); got != (model.Tokens{Input: 700, Output: 36, CacheRead: 500, Reasoning: 24}) {
		t.Errorf("tokens = %+v, want the first two requests only", got)
	}

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
	if got := sum(b2.Events); got != (model.Tokens{Input: 300, Output: 20, Reasoning: 10}) {
		t.Errorf("completed record tokens = %+v", got)
	}
}

// TestFingerprintChangeReparsesFromZero covers a rewritten rollout.
func TestFingerprintChangeReparsesFromZero(t *testing.T) {
	data, err := os.ReadFile(subRollout)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rollout-2026-01-02T04-00-00-019bbb00-2222-7333-8444-555566667777.jsonl")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoots(filepath.Dir(path))
	b1 := parseFile(t, p, path, harness.Cursor{})

	rewritten := strings.Replace(string(data), `"model":"gpt-6.1-sol"`, `"model":"gpt-6.1-sol-x"`, 1)
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

func TestSessionFromFilenameFallback(t *testing.T) {
	name := "rollout-2026-01-02T03-04-05-019ddd00-4444-7555-8666-777788889999.jsonl"
	path := filepath.Join(t.TempDir(), name)
	line := `{"timestamp":"2026-01-02T03:04:06.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15},"last_token_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoots(filepath.Dir(path))
	b := parseFile(t, p, path, harness.Cursor{})
	if len(b.Sessions) != 1 || b.Sessions[0].SessionID != "019ddd00-4444-7555-8666-777788889999" {
		t.Fatalf("sessions = %+v, want the uuid from the filename", b.Sessions)
	}
	if len(b.Events) != 1 || b.Events[0].SessionID != "019ddd00-4444-7555-8666-777788889999" {
		t.Fatalf("events = %+v", b.Events)
	}
	if got := sum(b.Events); got != (model.Tokens{Input: 10, Output: 5}) {
		t.Errorf("tokens = %+v", got)
	}
}

func TestEmptyAndMissingRoot(t *testing.T) {
	p := NewWithRoots(filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "nope2"))
	got, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("sources = %d, want 0", len(got))
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestRealData parses the real Codex rollouts. Gated by MYTOKEN_REALDATA=1.
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
	events, parseErr, unmodeled := 0, 0, 0
	var bytes int64
	for _, src := range sources {
		if st, err := os.Stat(src.Path); err == nil {
			bytes += st.Size()
		}
		b, err := p.Parse(ctx, src, harness.Cursor{})
		if err != nil {
			parseErr++
			t.Logf("parse error %s: %v", src.Path, err)
			continue
		}
		events += len(b.Events)
		for _, e := range b.Events {
			if e.Model == "" {
				unmodeled++
			}
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
	t.Logf("real data: %d files, %.1f MB, %d events (%d without a model), %d sessions, %d parse errors in %s",
		len(sources), float64(bytes)/(1<<20), events, unmodeled, len(sessions), parseErr,
		time.Since(start).Round(time.Millisecond))
	for name, tk := range perModel {
		t.Logf("  %-24s in=%d out=%d cacheRead=%d cacheWrite=%d reasoning=%d total=%d",
			name, tk.Input, tk.Output, tk.CacheRead, tk.CacheWrite, tk.Reasoning, tk.Total())
	}
	t.Logf("  %-24s total=%d", "ALL MODELS", total.Total())
}

// TestDecodePayloadTolerant locks the lenient decode: Codex retypes fields
// between versions (source is a string on user threads, an object on subagent
// threads) and one unexpected type must not discard the whole record.
func TestDecodePayloadTolerant(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantParent string
		wantID     string
		wantCwd    string
	}{
		{"string source", `{"type":"session_meta","id":"a","cwd":"/tmp/a","model_provider":"custom","source":"cli"}`, "", "a", "/tmp/a"},
		{"subagent source object", `{"type":"session_meta","id":"b","source":{"subagent":{"thread_spawn":{"parent_thread_id":"p-1"}}}}`, "p-1", "b", ""},
		{"top level parent_thread_id", `{"type":"session_meta","id":"c","parent_thread_id":"p-2"}`, "p-2", "c", ""},
		{"forked_from_id", `{"type":"session_meta","id":"e","forked_from_id":"p-3"}`, "p-3", "e", ""},
		{"retyped unrelated field", `{"type":123,"id":"d","cwd":"/tmp/d","model_provider":"custom","source":"cli"}`, "", "d", "/tmp/d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pl, ok := decodePayload(json.RawMessage(tc.raw))
			if !ok {
				t.Fatal("decodePayload failed")
			}
			if got := parentOf(&pl); got != tc.wantParent {
				t.Errorf("parent = %q, want %q", got, tc.wantParent)
			}
			if pl.ID != tc.wantID {
				t.Errorf("id = %q, want %q", pl.ID, tc.wantID)
			}
			if pl.Cwd != tc.wantCwd {
				t.Errorf("cwd = %q, want %q", pl.Cwd, tc.wantCwd)
			}
		})
	}
}

func TestModelOfFallback(t *testing.T) {
	pl, ok := decodePayload(json.RawMessage(`{"turn_id":"t","collaboration_mode":{"settings":{"model":"gpt-6.1-sol","reasoning_effort":"xhigh"}}}`))
	if !ok {
		t.Fatal("decodePayload failed")
	}
	if got := modelOf(&pl); got != "gpt-6.1-sol" {
		t.Errorf("model = %q, want gpt-6.1-sol", got)
	}
	if pl, _ := decodePayload(json.RawMessage(`{"model":"gpt-x","collaboration_mode":"none"}`)); modelOf(&pl) != "gpt-x" {
		t.Errorf("a retyped collaboration_mode must not hide payload.model")
	}
}

func TestSessionFromFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"rollout-2026-01-02T03-04-05-019aaa00-1111-7222-8333-444455556666.jsonl", "019aaa00-1111-7222-8333-444455556666"},
		{"rollout-2026-05-20T09-38-01-019e4308-5a3d-70d3-84e2-a5ce8ffe3e15_01a05ac8-a300-74d2-8be9-57b06013fe12.jsonl", "01a05ac8-a300-74d2-8be9-57b06013fe12"},
		{"not-a-rollout.jsonl", "not-a-rollout"},
	}
	for _, tc := range cases {
		if got := sessionFromFilename(tc.in); got != tc.want {
			t.Errorf("sessionFromFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
