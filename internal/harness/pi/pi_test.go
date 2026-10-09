package pi

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// fixtureRoot points at testdata/pi/sessions, a sanitized copy of a real
// ~/.pi/agent/sessions tree: two cwd slugs, a top-level log, a subagent run log,
// a fork log, an async-run artifact mirror and a second project. Every message
// text is a PLACEHOLDER_* string.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..", "..", "testdata", "pi", "sessions")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("fixture tree missing: %v", err)
	}
	return root
}

func parseAll(t *testing.T, p *Parser) ([]model.UsageEvent, []model.SessionMeta) {
	t.Helper()
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var events []model.UsageEvent
	var sessions []model.SessionMeta
	for _, src := range srcs {
		b, err := p.Parse(context.Background(), src, harness.Cursor{})
		if err != nil {
			t.Fatalf("Parse(%s): %v", src.Path, err)
		}
		events = append(events, b.Events...)
		sessions = append(sessions, b.Sessions...)
	}
	return events, sessions
}

func sum(events []model.UsageEvent) model.Tokens {
	var t model.Tokens
	for _, e := range events {
		t = t.Add(e.Tokens)
	}
	return t
}

// sumDedup mirrors what the store does with repeated dedup keys: the last copy
// wins. The parser deliberately emits every copy it sees.
func sumDedup(events []model.UsageEvent) model.Tokens {
	last := map[string]model.Tokens{}
	order := make([]string, 0, len(events))
	for _, e := range events {
		if _, ok := last[e.DedupKey]; !ok {
			order = append(order, e.DedupKey)
		}
		last[e.DedupKey] = e.Tokens
	}
	var t model.Tokens
	for _, k := range order {
		t = t.Add(last[k])
	}
	return t
}

func sessionByID(sessions []model.SessionMeta, id string) (model.SessionMeta, bool) {
	for _, s := range sessions {
		if s.SessionID == id {
			return s, true
		}
	}
	return model.SessionMeta{}, false
}

// TestDiscover checks that every session log is found and that files that are
// not logs (a non-message artifact JSONL) are not.
func TestDiscover(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := map[string]bool{
		filepath.Join("--home-user-proj-alpha--", "2026-01-01T00-00-00-000Z_01a00000-0000-7000-8000-000000000001.jsonl"): true,
		filepath.Join("--home-user-proj-alpha--", "2026-01-01T00-00-00-000Z_01a00000-0000-7000-8000-000000000001",
			"01b00000-0000-7000-8000-00000000000f", "run-0", "session.jsonl"): true,
		filepath.Join("--home-user-proj-alpha--", "2026-01-01T00-00-00-000Z_01a00000-0000-7000-8000-000000000001",
			"forks", "2026-01-01T01-00-00-000Z_01a00000-0000-7000-8000-00000000000a.jsonl"): true,
		filepath.Join("--home-user-proj-alpha--", "subagent-artifacts",
			"01c00000-0000-7000-8000-00000000000c_reviewer_transcript.jsonl"): true,
		filepath.Join("--home-user-proj-beta--", "2026-01-02T09-00-00-000Z_01a00000-0000-7000-8000-000000000002.jsonl"): true,
	}
	if len(srcs) != len(want) {
		var got []string
		for _, s := range srcs {
			got = append(got, s.Path)
		}
		t.Fatalf("Discover returned %d sources, want %d: %v", len(srcs), len(want), got)
	}
	for _, s := range srcs {
		rel, err := filepath.Rel(fixtureRoot(t), s.Path)
		if err != nil {
			t.Fatal(err)
		}
		if !want[rel] {
			t.Errorf("unexpected source %s", s.Path)
		}
		if s.Kind != kindJSONL {
			t.Errorf("%s: Kind = %q, want %q", rel, s.Kind, kindJSONL)
		}
	}
	// An artifact JSONL whose records are not messages is not a log.
	for _, s := range srcs {
		if strings.Contains(s.Path, "_reviewer_notes.jsonl") {
			t.Errorf("non-message artifact discovered: %s", s.Path)
		}
	}
}

// TestParseTotals asserts the exact token totals of the fixture tree.
func TestParseTotals(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	events, sessions := parseAll(t, p)

	if len(events) != 9 { // 3 in alpha (one streamed twice), 1 subagent, 1 fork, 2 in beta, 2 async
		t.Errorf("got %d events, want 9", len(events))
	}
	keys := map[string]int{}
	for _, e := range events {
		keys[e.DedupKey]++
	}
	if keys["gen_0001"] != 2 {
		t.Errorf("streaming duplicate gen_0001 emitted %d times, want 2", keys["gen_0001"])
	}
	if len(keys) != 8 {
		t.Errorf("got %d distinct dedup keys, want 8: %v", len(keys), keys)
	}

	// Raw totals count both copies of the streamed message; the deduplicated
	// totals are what the store ends up with.
	raw := sum(events)
	if want := (model.Tokens{Input: 18852, Output: 1348, CacheRead: 103200, CacheWrite: 2098, Reasoning: 70}); raw != want {
		t.Errorf("raw totals = %+v, want %+v", raw, want)
	}
	got := sumDedup(events)
	if want := (model.Tokens{Input: 17852, Output: 1148, CacheRead: 103200, CacheWrite: 2098, Reasoning: 70}); got != want {
		t.Errorf("deduplicated totals = %+v, want %+v", got, want)
	}

	if len(sessions) != 5 {
		t.Fatalf("got %d sessions, want 5", len(sessions))
	}

	cases := []struct {
		id      string
		tokens  model.Tokens // raw, streaming duplicates included
		title   string
		project string
		parent  string
		started time.Time
		updated time.Time
	}{
		{
			id:      "01a00000-0000-7000-8000-000000000001",
			tokens:  model.Tokens{Input: 2800, Output: 570, CacheRead: 300, CacheWrite: 50, Reasoning: 40},
			title:   "PLACEHOLDER_PI_REQUEST",
			project: "/home/user/proj-alpha",
			started: time.UnixMilli(1767225600000).UTC(),
			updated: time.UnixMilli(1767225608000).UTC(),
		},
		{
			id:      "01b00000-0000-7000-8000-00000000000f",
			tokens:  model.Tokens{Input: 400, Output: 80},
			title:   "PLACEHOLDER_SUBAGENT_TASK",
			project: "/home/user/proj-alpha",
			parent:  "01a00000-0000-7000-8000-000000000001",
			started: time.UnixMilli(1767225660000).UTC(),
			updated: time.UnixMilli(1767225662000).UTC(),
		},
		{
			id:      "01a00000-0000-7000-8000-00000000000a",
			tokens:  model.Tokens{Input: 300, Output: 60},
			title:   "PLACEHOLDER_FORK_REQUEST",
			project: "/home/user/proj-alpha",
			parent:  "01a00000-0000-7000-8000-000000000001",
			started: time.UnixMilli(1767229200000).UTC(),
			updated: time.UnixMilli(1767229202000).UTC(),
		},
		{
			id:      "01a00000-0000-7000-8000-000000000002",
			tokens:  model.Tokens{Input: 1000, Output: 170, CacheRead: 500, Reasoning: 30},
			title:   "PLACEHOLDER_BETA_REQUEST",
			project: "/home/user/proj-beta",
			started: time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC),
			updated: time.Date(2026, 1, 2, 9, 0, 3, 0, time.UTC),
		},
		{
			// Async subagent run: the session id is the runId, the project is
			// the record cwd, and no parent is recorded.
			id:      "01c00000-0000-7000-8000-00000000000c",
			tokens:  model.Tokens{Input: 14352, Output: 468, CacheRead: 102400, CacheWrite: 2048},
			title:   "PLACEHOLDER_ASYNC_RUN_TASK",
			project: "/home/user/proj-alpha",
			parent:  "",
			started: time.UnixMilli(1767225700000).UTC(),
			updated: time.UnixMilli(1767225704000).UTC(),
		},
	}
	for _, c := range cases {
		s, ok := sessionByID(sessions, c.id)
		if !ok {
			t.Errorf("session %s missing", c.id)
			continue
		}
		if s.Harness != model.Pi {
			t.Errorf("%s: Harness = %q", c.id, s.Harness)
		}
		if s.ParentID != c.parent {
			t.Errorf("%s: ParentID = %q, want %q", c.id, s.ParentID, c.parent)
		}
		if s.Title != c.title {
			t.Errorf("%s: Title = %q, want %q", c.id, s.Title, c.title)
		}
		if s.Project != c.project {
			t.Errorf("%s: Project = %q, want %q", c.id, s.Project, c.project)
		}
		if !s.StartedAt.Equal(c.started) {
			t.Errorf("%s: StartedAt = %s, want %s", c.id, s.StartedAt, c.started)
		}
		if !s.UpdatedAt.Equal(c.updated) {
			t.Errorf("%s: UpdatedAt = %s, want %s", c.id, s.UpdatedAt, c.updated)
		}
		var st model.Tokens
		for _, e := range events {
			if e.SessionID == c.id {
				st = st.Add(e.Tokens)
			}
		}
		if st != c.tokens {
			t.Errorf("%s: raw tokens = %+v, want %+v", c.id, st, c.tokens)
		}
	}
}

// TestEventFields checks the per-event mapping of provider, model, cost, the
// parent link and the UTC timestamp.
func TestEventFields(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	events, _ := parseAll(t, p)

	byKey := map[string]model.UsageEvent{}
	for _, e := range events {
		byKey[e.DedupKey] = e // last wins, matching the store
	}

	ev, ok := byKey["gen_0006"]
	if !ok {
		t.Fatal("gen_0006 missing")
	}
	if ev.Provider != "rn" || ev.Model != "gpt-6-sol" {
		t.Errorf("provider/model = %q/%q, want rn/gpt-6-sol", ev.Provider, ev.Model)
	}
	if ev.ProjectPath != "/home/user/proj-beta" {
		t.Errorf("ProjectPath = %q", ev.ProjectPath)
	}
	if want := time.UnixMilli(1767328803000).UTC(); !ev.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %s, want %s", ev.Timestamp, want)
	}
	if ev.Timestamp.Location() != time.UTC {
		t.Errorf("Timestamp is not UTC: %s", ev.Timestamp.Location())
	}
	if ev.CostUSD == nil || *ev.CostUSD != 0.05 {
		t.Errorf("CostUSD = %v, want 0.05", ev.CostUSD)
	}
	if ev.SessionID != "01a00000-0000-7000-8000-000000000002" {
		t.Errorf("SessionID = %q", ev.SessionID)
	}
	if ev.ParentID != "" {
		t.Errorf("ParentID = %q, want empty for a top-level session", ev.ParentID)
	}
	// Reasoning is separate from output, not a subset of it.
	if ev.Tokens != (model.Tokens{Input: 100, Output: 20, CacheRead: 500, Reasoning: 30}) {
		t.Errorf("tokens = %+v", ev.Tokens)
	}

	// A zero cost is reported as absent, not as 0.
	if z, ok := byKey["gen_0005"]; !ok {
		t.Error("gen_0005 missing")
	} else if z.CostUSD != nil {
		t.Errorf("gen_0005 CostUSD = %v, want nil", *z.CostUSD)
	}

	// Subagent events inherit the parent session id.
	if r, ok := byKey["gen_0004"]; !ok {
		t.Error("gen_0004 missing")
	} else if r.ParentID != "01a00000-0000-7000-8000-000000000001" {
		t.Errorf("gen_0004 ParentID = %q, want the parent session id", r.ParentID)
	}

	// Records without usage are never counted.
	for _, e := range events {
		if strings.HasPrefix(e.DedupKey, "a1") || strings.HasPrefix(e.DedupKey, "a4") ||
			strings.HasPrefix(e.DedupKey, "c1") || strings.HasPrefix(e.DedupKey, "b1") {
			t.Errorf("non-usage record leaked into events: %+v", e)
		}
	}
}

// TestAsyncRunTotals checks the async subagent transcript: top-level usage, the
// provider read from the sibling meta file, the cost, and the fact that the
// meta's aggregate usage is never counted.
func TestAsyncRunTotals(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	events, sessions := parseAll(t, p)

	const runID = "01c00000-0000-7000-8000-00000000000c"
	var got []model.UsageEvent
	for _, e := range events {
		if e.SessionID == runID {
			got = append(got, e)
		}
	}
	if len(got) != 2 { // the all-zero usage record is not an event
		t.Fatalf("got %d async events, want 2", len(got))
	}
	var total model.Tokens
	var cost float64
	for _, e := range got {
		total = total.Add(e.Tokens)
		if e.CostUSD != nil {
			cost += *e.CostUSD
		}
		if e.Provider != "rn" || e.Model != "gpt-6.1-sol" {
			t.Errorf("provider/model = %q/%q, want rn/gpt-6.1-sol (from the meta file)", e.Provider, e.Model)
		}
		if e.ProjectPath != "/home/user/proj-alpha" {
			t.Errorf("ProjectPath = %q", e.ProjectPath)
		}
		if e.ParentID != "" {
			t.Errorf("ParentID = %q, want empty: an async run records no parent", e.ParentID)
		}
		if e.Timestamp.Location() != time.UTC {
			t.Errorf("Timestamp is not UTC: %s", e.Timestamp.Location())
		}
	}
	if want := (model.Tokens{Input: 14352, Output: 468, CacheRead: 102400, CacheWrite: 2048}); total != want {
		t.Errorf("async totals = %+v, want %+v", total, want)
	}
	// The meta file's aggregate (14352/468/102400/2048, cost 0.25, turns 3) is
	// the sum of these two records, so counting it would double everything.
	if cost != 0.25 {
		t.Errorf("async cost = %v, want 0.25", cost)
	}
	s, ok := sessionByID(sessions, runID)
	if !ok {
		t.Fatalf("session %s missing", runID)
	}
	if s.Harness != model.Pi {
		t.Errorf("Harness = %q", s.Harness)
	}
	if s.Title != "PLACEHOLDER_ASYNC_RUN_TASK" {
		t.Errorf("Title = %q, want the initial_prompt text", s.Title)
	}

	// An unchanged transcript is skipped by the cursor, so nothing is counted
	// twice on the next scan.
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var src harness.Source
	for _, s := range srcs {
		if strings.Contains(s.Path, "_reviewer_transcript.jsonl") {
			src = s
		}
	}
	if src.Path == "" {
		t.Fatal("transcript source not discovered")
	}
	first, err := p.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	again, err := p.Parse(context.Background(), src, first.Next)
	if err != nil {
		t.Fatalf("second Parse: %v", err)
	}
	if len(again.Events) != 0 || len(again.Sessions) != 0 {
		t.Errorf("unchanged transcript re-emitted %d events", len(again.Events))
	}
	if again.Next != first.Next {
		t.Errorf("cursor moved on an unchanged file: %+v vs %+v", again.Next, first.Next)
	}
}

// TestDedupLastWins checks that the final copy of a streamed message wins,
// including its cost.
func TestDedupLastWins(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	events, _ := parseAll(t, p)
	var copies []model.UsageEvent
	for _, e := range events {
		if e.DedupKey == "gen_0001" {
			copies = append(copies, e)
		}
	}
	if len(copies) != 2 {
		t.Fatalf("got %d copies of gen_0001, want 2", len(copies))
	}
	first, last := copies[0], copies[1]
	if first.Tokens != (model.Tokens{Input: 1000, Output: 200}) {
		t.Errorf("first copy = %+v", first.Tokens)
	}
	if last.Tokens != (model.Tokens{Input: 1100, Output: 250}) {
		t.Errorf("last copy = %+v", last.Tokens)
	}
	if !last.Timestamp.After(first.Timestamp) {
		t.Errorf("last copy is not newer: %s vs %s", last.Timestamp, first.Timestamp)
	}
	// The cost of the first copy is 0 and of the last one 0.5.
	if first.CostUSD != nil {
		t.Errorf("first copy CostUSD = %v, want nil", *first.CostUSD)
	}
	if last.CostUSD == nil || *last.CostUSD != 0.5 {
		t.Errorf("last copy CostUSD = %v, want 0.5", last.CostUSD)
	}
}

// TestTitleRules checks that the title comes from the first real user message
// and that slash commands are skipped.
func TestTitleRules(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	_, sessions := parseAll(t, p)

	// The alpha session's first user message is a real request; a "/compact"
	// command appears later and must not replace it.
	s, ok := sessionByID(sessions, "01a00000-0000-7000-8000-000000000001")
	if !ok {
		t.Fatal("alpha session missing")
	}
	if s.Title != "PLACEHOLDER_PI_REQUEST" {
		t.Errorf("Title = %q, want PLACEHOLDER_PI_REQUEST", s.Title)
	}

	// A session whose only user message is a command has no title at all.
	dir := t.TempDir()
	path := filepath.Join(dir, "cmd.jsonl")
	body := `{"type":"session","version":3,"id":"01a00000-0000-7000-8000-000000000009","timestamp":"2026-01-03T00:00:00.000Z","cwd":"/home/user/proj-cmd"}
{"type":"message","id":"x1","parentId":null,"timestamp":"2026-01-03T00:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"/grill-me PLACEHOLDER_COMMAND"}]}}
{"type":"message","id":"x2","parentId":null,"timestamp":"2026-01-03T00:00:02.000Z","message":{"role":"assistant","provider":"rn","model":"gpt-6-sol","responseId":"gen_0009","usage":{"input":10,"output":2,"totalTokens":12,"cost":{"total":0}}}}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := p.Parse(context.Background(), harness.Source{Path: path, Kind: kindJSONL}, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(b.Sessions))
	}
	if b.Sessions[0].Title != "" {
		t.Errorf("Title = %q, want empty (only a slash command was sent)", b.Sessions[0].Title)
	}
	if len(b.Events) != 1 {
		t.Errorf("got %d events, want 1", len(b.Events))
	}
}

// TestZeroUsageSkipped checks that a failed request whose usage object is all
// zeroes (the real corpus has these: HTTP 429/400 with a cost object attached)
// is not counted as a request.
func TestZeroUsageSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zero.jsonl")
	body := `{"type":"session","version":3,"id":"01a00000-0000-7000-8000-00000000000b","timestamp":"2026-01-04T00:00:00.000Z","cwd":"/home/user/proj-zero"}
{"type":"message","id":"z1","parentId":null,"timestamp":"2026-01-04T00:00:01.000Z","message":{"role":"assistant","provider":"rn","model":"gpt-6-sol","responseId":"gen_z1","stopReason":"error","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}}
{"type":"message","id":"z2","parentId":null,"timestamp":"2026-01-04T00:00:02.000Z","message":{"role":"assistant","provider":"rn","model":"gpt-6-sol","responseId":"gen_z2","usage":{"input":5,"output":1,"totalTokens":6,"cost":{"total":0}}}}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := NewWithRoots(dir).Parse(context.Background(), harness.Source{Path: path, Kind: kindJSONL}, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Events) != 1 {
		t.Fatalf("got %d events, want 1 (the all-zero usage must be skipped)", len(b.Events))
	}
	if b.Events[0].DedupKey != "gen_z2" {
		t.Errorf("DedupKey = %q, want gen_z2", b.Events[0].DedupKey)
	}
	if got := sum(b.Events); got != (model.Tokens{Input: 5, Output: 1}) {
		t.Errorf("totals = %+v", got)
	}
	if len(b.Sessions) != 1 {
		t.Errorf("got %d sessions, want 1", len(b.Sessions))
	}
}

// TestIncrementalResume parses a log in two halves and requires the resumed
// totals to equal a single full parse.
func TestIncrementalResume(t *testing.T) {
	full, half := paddedLog(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, full[:half], 0o644); err != nil { // only the first half exists
		t.Fatal(err)
	}
	src := harness.Source{Path: path, Kind: kindJSONL}
	p := NewWithRoots(dir)

	first, err := p.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatalf("first Parse: %v", err)
	}
	if first.Next.Offset != half {
		t.Errorf("Next.Offset = %d, want %d", first.Next.Offset, half)
	}
	if first.Next.Size != half {
		t.Errorf("Next.Size = %d, want %d", first.Next.Size, half)
	}
	if first.Next.Fingerprint == "" {
		t.Error("Next.Fingerprint is empty")
	}
	if len(first.Events) != 1 {
		t.Errorf("first half produced %d events, want 1", len(first.Events))
	}

	if err := os.WriteFile(path, full, 0o644); err != nil { // the rest of the log arrives
		t.Fatal(err)
	}
	second, err := p.Parse(context.Background(), src, first.Next)
	if err != nil {
		t.Fatalf("resumed Parse: %v", err)
	}
	if second.Next.Offset != int64(len(full)) {
		t.Errorf("resumed Next.Offset = %d, want %d", second.Next.Offset, len(full))
	}
	combined := sum(append(append([]model.UsageEvent{}, first.Events...), second.Events...))

	whole, err := p.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatalf("full Parse: %v", err)
	}
	if got := sum(whole.Events); got != combined {
		t.Errorf("resumed totals %+v != full totals %+v", combined, got)
	}
	if want := (model.Tokens{Input: 1000, Output: 170, CacheRead: 500, Reasoning: 30}); combined != want {
		t.Errorf("totals = %+v, want %+v", combined, want)
	}
	seen := map[string]int{}
	for _, e := range append(append([]model.UsageEvent{}, first.Events...), second.Events...) {
		seen[e.DedupKey]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("dedup key %s produced %d times across halves", k, n)
		}
	}
	if first.Sessions[0].SessionID != whole.Sessions[0].SessionID {
		t.Errorf("session id changed between halves: %q vs %q",
			first.Sessions[0].SessionID, whole.Sessions[0].SessionID)
	}
}

// TestTruncatedTrailingLine checks that a partially written record is neither
// consumed nor counted, and is picked up once it is complete.
func TestTruncatedTrailingLine(t *testing.T) {
	full, cut := paddedLog(t)
	next := bytes.IndexByte(full[cut:], '\n')
	if next < 0 {
		t.Fatal("no record after the boundary")
	}
	partial := full[:cut+int64(next/2)]

	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, partial, 0o644); err != nil {
		t.Fatal(err)
	}
	src := harness.Source{Path: path, Kind: kindJSONL}
	p := NewWithRoots(dir)

	b, err := p.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if b.Next.Offset != cut {
		t.Errorf("Next.Offset = %d, want %d (end of the last complete line)", b.Next.Offset, cut)
	}
	if got := sum(b.Events); got != (model.Tokens{Input: 900, Output: 150}) {
		t.Errorf("totals with a truncated tail = %+v, want {900 150}", got)
	}
	if len(b.Events) != 1 {
		t.Errorf("got %d events, want 1 (the incomplete record must be ignored)", len(b.Events))
	}

	if err := os.WriteFile(path, full, 0o644); err != nil {
		t.Fatal(err)
	}
	b2, err := p.Parse(context.Background(), src, b.Next)
	if err != nil {
		t.Fatalf("resumed Parse: %v", err)
	}
	if got := sum(b2.Events); got != (model.Tokens{Input: 100, Output: 20, CacheRead: 500, Reasoning: 30}) {
		t.Errorf("resumed totals = %+v", got)
	}
	if b2.Next.Offset != int64(len(full)) {
		t.Errorf("Next.Offset = %d, want %d", b2.Next.Offset, len(full))
	}
	if got := sum(append(append([]model.UsageEvent{}, b.Events...), b2.Events...)); got !=
		(model.Tokens{Input: 1000, Output: 170, CacheRead: 500, Reasoning: 30}) {
		t.Errorf("cumulative totals = %+v", got)
	}
	// A third parse from the final cursor must add nothing.
	b3, err := p.Parse(context.Background(), src, b2.Next)
	if err != nil {
		t.Fatalf("final Parse: %v", err)
	}
	if len(b3.Events) != 0 {
		t.Errorf("parsing past the end produced %d events", len(b3.Events))
	}
}

// TestFingerprintChangeReparses checks that a rewritten file is reparsed from 0
// even when its size is unchanged.
func TestFingerprintChangeReparses(t *testing.T) {
	// A log smaller than the 4KB fingerprint window: the fingerprint covers the
	// whole file, so any same-length rewrite is detected.
	full, err := os.ReadFile(filepath.Join(fixtureRoot(t), "--home-user-proj-beta--",
		"2026-01-02T09-00-00-000Z_01a00000-0000-7000-8000-000000000002.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, full, 0o644); err != nil {
		t.Fatal(err)
	}
	src := harness.Source{Path: path, Kind: kindJSONL}
	p := NewWithRoots(dir)

	b1, err := p.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if got := sum(b1.Events); got != (model.Tokens{Input: 1000, Output: 170, CacheRead: 500, Reasoning: 30}) {
		t.Fatalf("totals = %+v", got)
	}

	// Same length, different bytes: only the fingerprint can detect this.
	rewritten := strings.Replace(string(full), `"input":900`, `"input":990`, 1)
	rewritten = strings.Replace(rewritten, `"totalTokens":1050`, `"totalTokens":1140`, 1)
	if len(rewritten) != len(full) {
		t.Fatalf("rewrite changed the length: %d vs %d", len(rewritten), len(full))
	}
	if err := os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}
	b2, err := p.Parse(context.Background(), src, b1.Next)
	if err != nil {
		t.Fatal(err)
	}
	if got := sum(b2.Events); got != (model.Tokens{Input: 990 + 100, Output: 170, CacheRead: 500, Reasoning: 30}) {
		t.Errorf("totals after rewrite = %+v, want the rewritten values", got)
	}
	if b2.Next.Fingerprint == b1.Next.Fingerprint {
		t.Error("fingerprint did not change")
	}
}

// TestRegistration checks the package registers itself.
func TestRegistration(t *testing.T) {
	var found bool
	for _, p := range harness.All() {
		if p.Harness() == model.Pi {
			found = true
		}
	}
	if !found {
		t.Error("pi parser is not registered")
	}
	p := New()
	if p.Harness() != model.Pi {
		t.Errorf("Harness() = %q, want %q", p.Harness(), model.Pi)
	}
	roots := p.Roots()
	if len(roots) != 1 || roots[0] == "" {
		t.Errorf("Roots() = %v", roots)
	}
	if got := NewWithRoot("/tmp/x").Roots(); len(got) != 1 || got[0] != "/tmp/x" {
		t.Errorf("NewWithRoot Roots() = %v", got)
	}
	if got := NewWithRoots("/a", "/b").Roots(); len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Errorf("NewWithRoots Roots() = %v", got)
	}
}

// TestMissingRoot checks that a non-existent root is not an error.
func TestMissingRoot(t *testing.T) {
	p := NewWithRoot(filepath.Join(t.TempDir(), "nope"))
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Errorf("got %d sources, want 0", len(srcs))
	}
}

// padLine is a usage-free record long enough to push a log past the 4KB
// fingerprint window, so that appending to it leaves the fingerprint intact and
// the cursor must advance by offset alone.
func padLine(seq int) string {
	return fmt.Sprintf(`{"type":"message","id":"p%d","parentId":null,"timestamp":"2026-01-02T09:00:%02d.000Z","message":{"role":"toolResult","timestamp":1767328800000,"content":[{"type":"text","text":%q}]}}`,
		seq, seq%60, strings.Repeat("P", 1200))
}

// paddedLog rebuilds the beta fixture with padding between the records,
// returning the bytes and the offset just after the first assistant message.
func paddedLog(t *testing.T) ([]byte, int64) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureRoot(t), "--home-user-proj-beta--",
		"2026-01-02T09-00-00-000Z_01a00000-0000-7000-8000-000000000002.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n") // header, user, assistant, assistant
	var b strings.Builder
	write := func(s string) {
		b.WriteString(s)
		b.WriteByte('\n')
	}
	// Enough padding precedes the first assistant message that the first half
	// already covers the whole 4KB fingerprint window: appending the rest then
	// leaves the fingerprint intact, so the cursor advances by offset alone.
	write(lines[0])
	write(lines[1])
	write(padLine(10))
	write(padLine(11))
	write(padLine(12))
	write(padLine(13))
	write(lines[2])
	cut := int64(b.Len()) // just after the first assistant message
	if cut <= 4096 {
		t.Fatalf("boundary at %d is inside the fingerprint window", cut)
	}
	write(padLine(14))
	write(padLine(15))
	write(lines[3])
	out := []byte(b.String())
	if len(out) < 5000 {
		t.Fatalf("padded log is only %d bytes, want >= 5000 (past the 4KB fingerprint window)", len(out))
	}
	return out, cut
}
