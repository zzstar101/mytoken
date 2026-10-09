package cline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// fixtureRoot points at testdata/cline/sessions, a sanitized tree that mirrors
// Cline's real on-disk layout:
//
//	saoudrizwan.claude-dev/tasks/<taskId>/ui_messages.json   (+ siblings)
//	cli/sessions/<sessionId>/<sessionId>.messages.json       (+ manifest)
//	broken-task/ui_messages.json                             (not an array)
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..", "..", "testdata", "cline", "sessions")
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

func eventByDedupKey(events []model.UsageEvent, key string) (model.UsageEvent, bool) {
	for _, e := range events {
		if e.DedupKey == key {
			return e, true
		}
	}
	return model.UsageEvent{}, false
}

func TestDiscover(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// Five ui_messages.json task files (one of them malformed) plus one Cline
	// CLI session file. _index.json, task_metadata.json,
	// api_conversation_history.json and history_item.json are not sources.
	want := map[string]string{
		filepath.Join(fixtureRoot(t), "broken-task", "ui_messages.json"): "json",
		filepath.Join(fixtureRoot(t), "cli", "sessions", "9f1c2b3a-1111-4222-8333-444455556666",
			"9f1c2b3a-1111-4222-8333-444455556666.messages.json"): "json",
		filepath.Join(fixtureRoot(t), "saoudrizwan.claude-dev", "tasks", "1767225600000", "ui_messages.json"): "json",
		filepath.Join(fixtureRoot(t), "saoudrizwan.claude-dev", "tasks", "1767225700000", "ui_messages.json"): "json",
		filepath.Join(fixtureRoot(t), "saoudrizwan.claude-dev", "tasks", "1767225800000", "ui_messages.json"): "json",
		filepath.Join(fixtureRoot(t), "saoudrizwan.claude-dev", "tasks", "1767225900000", "ui_messages.json"): "json",
	}
	if len(srcs) != len(want) {
		var got []string
		for _, s := range srcs {
			got = append(got, s.Path+" ["+s.Kind+"]")
		}
		t.Fatalf("Discover returned %d sources, want %d:\n%v", len(srcs), len(want), got)
	}
	for _, s := range srcs {
		w, ok := want[s.Path]
		if !ok {
			t.Errorf("unexpected source %s", s.Path)
			continue
		}
		if s.Kind != w {
			t.Errorf("%s: kind %q, want %q", s.Path, s.Kind, w)
		}
	}
}

func TestRegistration(t *testing.T) {
	p, ok := harness.Get(model.Cline)
	if !ok {
		t.Fatal("cline parser is not registered")
	}
	if p.Harness() != model.Cline {
		t.Fatalf("Harness() = %v, want %v", p.Harness(), model.Cline)
	}
	if len(harness.All()) == 0 {
		t.Fatal("harness.All() is empty")
	}
}

func TestMissingRoot(t *testing.T) {
	p := NewWithRoot(filepath.Join(t.TempDir(), "nope"))
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover over a missing root: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("Discover returned %d sources for a missing root", len(srcs))
	}
}

// TestParseTotals pins the aggregate token accounting over the whole fixture
// tree. Cline's own SDK writes a DISJOINT tokensIn (cache already removed) for
// Anthropic-style providers, so those requests keep tokensIn verbatim; the
// OpenAI-style task subtracts the cache buckets from tokensIn.
//
// Task 1767225900000 is the subagent/deleted-API case: its totals are exactly
// what Cline's getApiMetrics reports for the same array — the api_req_started
// requests plus the subagent_usage aggregates plus the deleted_api_reqs
// aggregate, with the say:"subagent" status message and the malformed payloads
// contributing nothing.
func TestParseTotals(t *testing.T) {
	events, sessions := parseAll(t, NewWithRoot(fixtureRoot(t)))
	if len(events) != 13 {
		var keys []string
		for _, e := range events {
			keys = append(keys, e.DedupKey)
		}
		t.Fatalf("got %d events, want 13: %v", len(events), keys)
	}
	if len(sessions) != 7 {
		t.Fatalf("got %d sessions, want 7", len(sessions))
	}
	got := sumDedup(events)
	want := model.Tokens{Input: 37257, Output: 4311, CacheRead: 6350, CacheWrite: 900}
	if got != want {
		t.Errorf("dedup totals = %+v, want %+v", got, want)
	}
	if sum(events) != want {
		t.Errorf("raw totals = %+v, want %+v (fixture must not contain duplicates)", sum(events), want)
	}
}

// TestTaskTotals pins the per-task split of the aggregate above, which is how
// the session tree rolls the subagent usage up into its task.
func TestTaskTotals(t *testing.T) {
	events, _ := parseAll(t, NewWithRoot(fixtureRoot(t)))
	perSession := map[string]model.Tokens{}
	for _, e := range events {
		t := perSession[e.SessionID]
		perSession[e.SessionID] = t.Add(e.Tokens)
	}
	cases := []struct {
		session string
		want    model.Tokens
	}{
		// Two api_req_started requests plus the deleted_api_reqs aggregate,
		// which is the task's own session because the requests it stands for
		// belonged to the task.
		{"1767225900000", model.Tokens{Input: 5300, Output: 670, CacheRead: 500, CacheWrite: 200}},
		// Two subagent_usage aggregates; the say:"subagent" status message
		// carries the same numbers and must not be counted on top of them.
		{"1767225900000:subagents", model.Tokens{Input: 8000, Output: 1200, CacheRead: 0, CacheWrite: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.session, func(t *testing.T) {
			if got := perSession[tc.session]; got != tc.want {
				t.Errorf("%s totals = %+v, want %+v", tc.session, got, tc.want)
			}
		})
	}
	// Task + subagent session is exactly what Cline's getApiMetrics reports for
	// this task's array.
	rollup := perSession["1767225900000"].Add(perSession["1767225900000:subagents"])
	if want := (model.Tokens{Input: 13300, Output: 1870, CacheRead: 500, CacheWrite: 200}); rollup != want {
		t.Errorf("task rollup = %+v, want %+v", rollup, want)
	}
}

// TestTokenClasses walks every request and pins the exact bucket split,
// including the two places where the format's input already excludes cache.
func TestTokenClasses(t *testing.T) {
	events, _ := parseAll(t, NewWithRoot(fixtureRoot(t)))
	cases := []struct {
		name string
		key  string
		want model.Tokens
	}{
		{
			// Anthropic-style provider: Cline's normalizeUsageEvent already
			// removed the cache, so tokensIn is the input bucket as-is.
			name: "anthropic disjoint input",
			key:  "cline:1767225600000:1767225610000",
			want: model.Tokens{Input: 1200, Output: 300, CacheRead: 400, CacheWrite: 100},
		},
		{
			name: "anthropic disjoint input, no cache writes",
			key:  "cline:1767225600000:1767225620000",
			want: model.Tokens{Input: 800, Output: 210, CacheRead: 900, CacheWrite: 0},
		},
		{
			// OpenAI-style provider: legacy Cline wrote the raw prompt_tokens,
			// which already contains the cached tokens.
			name: "openai inclusive input",
			key:  "cline:1767225700000:1767225710000",
			want: model.Tokens{Input: 3000, Output: 700, CacheRead: 2000, CacheWrite: 0},
		},
		{
			name: "openai inclusive input with cache writes",
			key:  "cline:1767225700000:1767225720000",
			want: model.Tokens{Input: 1950, Output: 640, CacheRead: 2100, CacheWrite: 150},
		},
		{
			// task_metadata.json supplies both the provider (→ Anthropic-style)
			// and the model, because this task has no modelInfo at all.
			name: "task_metadata provider fallback",
			key:  "cline:1767225800000:1767225810000",
			want: model.Tokens{Input: 900, Output: 120, CacheRead: 0, CacheWrite: 50},
		},
		{
			name: "cli session metrics",
			key:  "cline-cli:9f1c2b3a-1111-4222-8333-444455556666:m2",
			want: model.Tokens{Input: 7457, Output: 131, CacheRead: 50, CacheWrite: 0},
		},
		{
			name: "cli session manifest model fallback",
			key:  "cline-cli:9f1c2b3a-1111-4222-8333-444455556666:m3",
			want: model.Tokens{Input: 7700, Output: 200, CacheRead: 100, CacheWrite: 400},
		},
		{
			// spawn_agent aggregate, attributed to the task's subagent session.
			// Cline writes cacheWrites/cacheReads as 0 for it, and the
			// Anthropic-style task model leaves tokensIn untouched.
			name: "subagent usage aggregate",
			key:  "cline:1767225900000:subagents:1767225902000",
			want: model.Tokens{Input: 5000, Output: 800, CacheRead: 0, CacheWrite: 0},
		},
		{
			name: "second subagent usage aggregate",
			key:  "cline:1767225900000:subagents:1767225905000",
			want: model.Tokens{Input: 3000, Output: 400, CacheRead: 0, CacheWrite: 0},
		},
		{
			// Checkpoint-restore aggregate of requests that were deleted from
			// the array: billed, and no longer present anywhere else.
			name: "deleted api reqs aggregate",
			key:  "cline:1767225900000:1767225908000",
			want: model.Tokens{Input: 4000, Output: 500, CacheRead: 300, CacheWrite: 200},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := eventByDedupKey(events, tc.key)
			if !ok {
				t.Fatalf("no event with DedupKey %q", tc.key)
			}
			if ev.Tokens != tc.want {
				t.Errorf("tokens = %+v, want %+v", ev.Tokens, tc.want)
			}
			if ev.Tokens.Reasoning != 0 {
				t.Errorf("Reasoning = %d, want 0 (the task format has no reasoning field)", ev.Tokens.Reasoning)
			}
			if !ev.Timestamp.Equal(ev.Timestamp.UTC()) {
				t.Errorf("Timestamp %v is not UTC", ev.Timestamp)
			}
			if ev.SessionID == "" || ev.DedupKey == "" {
				t.Errorf("event is missing SessionID/DedupKey: %+v", ev)
			}
		})
	}
}

// TestEventFields pins model / provider / cost / session metadata.
func TestEventFields(t *testing.T) {
	events, sessions := parseAll(t, NewWithRoot(fixtureRoot(t)))

	// modelInfo wins over every fallback.
	if ev, _ := eventByDedupKey(events, "cline:1767225600000:1767225610000"); ev.Model != "claude-sonnet-4-5-20250929" ||
		ev.Provider != "anthropic" || ev.CostUSD == nil || *ev.CostUSD != 0.031 {
		t.Errorf("anthropic request = %+v (cost %v)", ev, ev.CostUSD)
	}
	// OpenAI-style request keeps its own model/provider.
	if ev, _ := eventByDedupKey(events, "cline:1767225700000:1767225710000"); ev.Model != "gpt-5.1" ||
		ev.Provider != "openai" || ev.CostUSD == nil || *ev.CostUSD != 0.05 {
		t.Errorf("openai request = %+v (cost %v)", ev, ev.CostUSD)
	}
	// No modelInfo and no environment_details model → task_metadata's LAST
	// model_usage entry.
	if ev, _ := eventByDedupKey(events, "cline:1767225800000:1767225810000"); ev.Model != "claude-haiku-4-5-20251001" ||
		ev.Provider != "anthropic" {
		t.Errorf("task_metadata fallback request = %+v", ev)
	}
	// CLI session: per-message modelInfo, then the manifest.
	if ev, _ := eventByDedupKey(events, "cline-cli:9f1c2b3a-1111-4222-8333-444455556666:m2"); ev.Model != "cline/glm-5" ||
		ev.Provider != "cline-pass" || ev.CostUSD == nil || *ev.CostUSD != 0.0110232 {
		t.Errorf("cli request = %+v (cost %v)", ev, ev.CostUSD)
	}
	if ev, _ := eventByDedupKey(events, "cline-cli:9f1c2b3a-1111-4222-8333-444455556666:m3"); ev.Model != "cline-pass/glm-5.2" ||
		ev.Provider != "cline-pass" {
		t.Errorf("cli manifest fallback request = %+v", ev)
	}

	// Session metadata: title, project, parent, timestamps.
	s1, ok := sessionByID(sessions, "1767225600000")
	if !ok {
		t.Fatal("session 1767225600000 missing")
	}
	if s1.Title != "Refactor the widget cache" {
		t.Errorf("title = %q", s1.Title)
	}
	if s1.Project != "/home/user/proj-alpha" {
		t.Errorf("project = %q", s1.Project)
	}
	if s1.ParentID != "" {
		t.Errorf("parentID = %q, want empty", s1.ParentID)
	}
	if s1.StartedAt.UnixMilli() != 1767225600000 || s1.UpdatedAt.UnixMilli() != 1767225640000 {
		t.Errorf("window = %v..%v", s1.StartedAt, s1.UpdatedAt)
	}
	if s1.Harness != model.Cline {
		t.Errorf("harness = %v", s1.Harness)
	}

	// Subtask: history_item.json carries the parent link.
	s3, ok := sessionByID(sessions, "1767225800000")
	if !ok {
		t.Fatal("session 1767225800000 missing")
	}
	if s3.ParentID != "1767225600000" {
		t.Errorf("parentID = %q, want 1767225600000", s3.ParentID)
	}
	if s3.Title != "Add the retry wrapper" || s3.Project != "/home/user/proj-alpha" {
		t.Errorf("subtask session = %+v", s3)
	}

	// CLI session: manifest metadata title.
	sc, ok := sessionByID(sessions, "9f1c2b3a-1111-4222-8333-444455556666")
	if !ok {
		t.Fatal("cli session missing")
	}
	if sc.Title != "CLI cache summary" || sc.Project != "/home/user/proj-alpha" {
		t.Errorf("cli session = %+v", sc)
	}
	if sc.ParentID != "" {
		t.Errorf("cli parentID = %q, want empty", sc.ParentID)
	}

	// A malformed file still yields a session, but no events and no title.
	sb, ok := sessionByID(sessions, "broken-task")
	if !ok {
		t.Fatal("broken-task session missing")
	}
	if sb.Title != "" {
		t.Errorf("broken-task title = %q, want empty", sb.Title)
	}
}

// TestSubagentSession pins how spawn_agent usage nests: one synthetic child
// session per task, whose ParentID is the task and whose window is the span of
// the subagent_usage entries rather than the whole task.
func TestSubagentSession(t *testing.T) {
	_, sessions := parseAll(t, NewWithRoot(fixtureRoot(t)))

	// The event carries the child session id and the task as its parent, which
	// is what makes the store create the task's session row and roll the
	// subagent tokens up into it.
	events, _ := parseAll(t, NewWithRoot(fixtureRoot(t)))
	for _, key := range []string{"cline:1767225900000:subagents:1767225902000", "cline:1767225900000:subagents:1767225905000"} {
		ev, ok := eventByDedupKey(events, key)
		if !ok {
			t.Fatalf("no event with DedupKey %q", key)
		}
		if ev.SessionID != "1767225900000:subagents" {
			t.Errorf("%s: SessionID = %q, want %q", key, ev.SessionID, "1767225900000:subagents")
		}
		if ev.ParentID != "1767225900000" {
			t.Errorf("%s: ParentID = %q, want the task id", key, ev.ParentID)
		}
		if ev.ProjectPath != "/home/user/proj-gamma" {
			t.Errorf("%s: ProjectPath = %q, want /home/user/proj-gamma", key, ev.ProjectPath)
		}
		if ev.CostUSD == nil || *ev.CostUSD == 0 {
			t.Errorf("%s: CostUSD = %v, want the aggregate's cost", key, ev.CostUSD)
		}
	}
	// A plain request of the same task stays on the task session.
	if ev, _ := eventByDedupKey(events, "cline:1767225900000:1767225908000"); ev.SessionID != "1767225900000" ||
		ev.ParentID != "" {
		t.Errorf("deleted_api_reqs event = %+v, want it on the task session", ev)
	}

	sub, ok := sessionByID(sessions, "1767225900000:subagents")
	if !ok {
		t.Fatal("subagent session missing")
	}
	if sub.ParentID != "1767225900000" {
		t.Errorf("parentID = %q, want 1767225900000", sub.ParentID)
	}
	if sub.Title != "Subagents" {
		t.Errorf("title = %q, want Subagents", sub.Title)
	}
	if sub.Project != "/home/user/proj-gamma" {
		t.Errorf("project = %q, want /home/user/proj-gamma", sub.Project)
	}
	if sub.StartedAt.UnixMilli() != 1767225902000 || sub.UpdatedAt.UnixMilli() != 1767225905000 {
		t.Errorf("window = %v..%v, want the subagent_usage span", sub.StartedAt, sub.UpdatedAt)
	}
	if sub.Harness != model.Cline {
		t.Errorf("harness = %v", sub.Harness)
	}

	// The task's own window still covers every entry of the file.
	task, ok := sessionByID(sessions, "1767225900000")
	if !ok {
		t.Fatal("task session missing")
	}
	if task.Title != "Orchestrate the migration" {
		t.Errorf("title = %q", task.Title)
	}
	if task.StartedAt.UnixMilli() != 1767225900000 || task.UpdatedAt.UnixMilli() != 1767225910000 {
		t.Errorf("window = %v..%v", task.StartedAt, task.UpdatedAt)
	}
}

// TestDedupLastWins rewrites an already-parsed task file with a changed
// request and checks that the dedup key is stable, so the store keeps one
// copy of the request rather than two.
func TestDedupLastWins(t *testing.T) {
	root := t.TempDir()
	task := filepath.Join(root, "saoudrizwan.claude-dev", "tasks", "1767225600000")
	if err := os.MkdirAll(task, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(fixtureRoot(t), "saoudrizwan.claude-dev", "tasks", "1767225600000")
	write := func(name, data string) {
		if err := os.WriteFile(filepath.Join(task, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	copyFile := func(name string) {
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		write(name, string(b))
	}
	copyFile("api_conversation_history.json")
	copyFile("task_metadata.json")

	p := NewWithRoot(root)
	write("ui_messages.json", `[
	  {"ts":1767225610000,"type":"say","say":"api_req_started","text":"{\"tokensIn\":1200,\"tokensOut\":300,\"cacheWrites\":100,\"cacheReads\":400,\"cost\":0.031}","modelInfo":{"modelId":"claude-sonnet-4-5-20250929","providerId":"anthropic","mode":"act"}}
	]`)
	srcs, err := p.Discover(context.Background())
	if err != nil || len(srcs) != 1 {
		t.Fatalf("Discover = %v, %v", srcs, err)
	}
	s := srcs[0]
	b1, err := p.Parse(context.Background(), s, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(b1.Events) != 1 {
		t.Fatalf("first parse emitted %d events", len(b1.Events))
	}
	key := b1.Events[0].DedupKey
	if want := "cline:1767225600000:1767225610000"; key != want {
		t.Fatalf("DedupKey = %q, want %q", key, want)
	}

	// The task file is rewritten in place with the same request but different
	// numbers, plus a second request. The dedup key must not move.
	write("ui_messages.json", `[
	  {"ts":1767225610000,"type":"say","say":"api_req_started","text":"{\"tokensIn\":1300,\"tokensOut\":320,\"cacheWrites\":100,\"cacheReads\":400,\"cost\":0.032}","modelInfo":{"modelId":"claude-sonnet-4-5-20250929","providerId":"anthropic","mode":"act"}},
	  {"ts":1767225620000,"type":"say","say":"api_req_started","text":"{\"tokensIn\":900,\"tokensOut\":200,\"cacheWrites\":0,\"cacheReads\":900,\"cost\":0.02}","modelInfo":{"modelId":"claude-sonnet-4-5-20250929","providerId":"anthropic","mode":"act"}}
	]`)
	b2, err := p.Parse(context.Background(), s, b1.Next)
	if err != nil {
		t.Fatalf("Parse after rewrite: %v", err)
	}
	// Only the request that was not emitted before shows up.
	if len(b2.Events) != 1 {
		t.Fatalf("second parse emitted %d events, want 1", len(b2.Events))
	}
	if got := b2.Events[0].DedupKey; got == key {
		t.Errorf("second parse re-emitted %q", got)
	}
	// Feeding both batches through the store's last-copy-wins rule keeps the
	// request count at two.
	all := append(append([]model.UsageEvent{}, b1.Events...), b2.Events...)
	seen := map[string]bool{}
	for _, e := range all {
		seen[e.DedupKey] = true
	}
	if len(seen) != 2 {
		t.Errorf("%d distinct dedup keys, want 2", len(seen))
	}
	if got := sumDedup(all); got != (model.Tokens{Input: 2100, Output: 500, CacheRead: 1300, CacheWrite: 100}) {
		t.Errorf("dedup totals = %+v", got)
	}
}

// TestIncrementalResume appends requests to a task file the way Cline does (a
// whole-file rewrite) and checks that each scan emits only the new requests.
func TestIncrementalResume(t *testing.T) {
	root := t.TempDir()
	task := filepath.Join(root, "saoudrizwan.claude-dev", "tasks", "1767225600000")
	if err := os.MkdirAll(task, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(fixtureRoot(t), "saoudrizwan.claude-dev", "tasks", "1767225600000")
	for _, name := range []string{"api_conversation_history.json", "task_metadata.json"} {
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(task, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := NewWithRoot(root)
	req := func(ts int, in, out, cr, cw int) string {
		return `{"ts":` + itoa(ts) + `,"type":"say","say":"api_req_started","text":"{\"tokensIn\":` + itoa(in) +
			`,\"tokensOut\":` + itoa(out) + `,\"cacheWrites\":` + itoa(cw) + `,\"cacheReads\":` + itoa(cr) +
			`,\"cost\":0.01}","modelInfo":{"modelId":"claude-sonnet-4-5-20250929","providerId":"anthropic","mode":"act"}}`
	}
	steps := []string{
		`[` + req(1767225610000, 1000, 100, 0, 0) + `]`,
		`[` + req(1767225610000, 1000, 100, 0, 0) + `,` + req(1767225620000, 2000, 200, 0, 0) + `]`,
		`[` + req(1767225610000, 1000, 100, 0, 0) + `,` + req(1767225620000, 2000, 200, 0, 0) + `,` +
			req(1767225630000, 3000, 300, 500, 0) + `]`,
	}
	if err := os.WriteFile(filepath.Join(task, "ui_messages.json"), []byte(steps[0]), 0o644); err != nil {
		t.Fatal(err)
	}
	srcs, err := p.Discover(context.Background())
	if err != nil || len(srcs) != 1 {
		t.Fatalf("Discover = %v, %v", srcs, err)
	}
	s := srcs[0]

	wantEvents := []int{1, 1, 1}
	wantTotals := []model.Tokens{
		{Input: 1000, Output: 100},
		{Input: 2000, Output: 200},
		{Input: 3000, Output: 300, CacheRead: 500},
	}
	var cur harness.Cursor
	var all []model.UsageEvent
	for i, body := range steps {
		if err := os.WriteFile(filepath.Join(task, "ui_messages.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		b, err := p.Parse(context.Background(), s, cur)
		if err != nil {
			t.Fatalf("Parse step %d: %v", i, err)
		}
		if len(b.Events) != wantEvents[i] {
			t.Fatalf("step %d emitted %d events, want %d", i, len(b.Events), wantEvents[i])
		}
		if got := sum(b.Events); got != wantTotals[i] {
			t.Errorf("step %d totals = %+v, want %+v", i, got, wantTotals[i])
		}
		all = append(all, b.Events...)
		cur = b.Next

		// An immediate re-scan with no change must be a no-op.
		again, err := p.Parse(context.Background(), s, cur)
		if err != nil {
			t.Fatalf("Parse no-op %d: %v", i, err)
		}
		if len(again.Events) != 0 || len(again.Sessions) != 0 {
			t.Errorf("step %d re-scan emitted %d events / %d sessions, want none", i, len(again.Events), len(again.Sessions))
		}
		if again.Next.Extra != cur.Extra {
			t.Errorf("step %d cursor Extra = %q, want %q", i, again.Next.Extra, cur.Extra)
		}
	}
	if got := sumDedup(all); got != (model.Tokens{Input: 6000, Output: 600, CacheRead: 500}) {
		t.Errorf("resume totals = %+v", got)
	}
	if cur.Offset != int64(len(steps[len(steps)-1])) {
		t.Errorf("cursor offset = %d, want %d", cur.Offset, len(steps[len(steps)-1]))
	}
}

// TestFingerprintChangeReparses checks that a file rewritten with different
// content (a different fingerprint) is re-read from the start instead of being
// resumed by byte offset, which is meaningless for a whole-file JSON document.
func TestFingerprintChangeReparses(t *testing.T) {
	root := t.TempDir()
	task := filepath.Join(root, "saoudrizwan.claude-dev", "tasks", "1767225600000")
	if err := os.MkdirAll(task, 0o755); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoot(root)
	req := func(ts int, in, out int) string {
		return `{"ts":` + itoa(ts) + `,"type":"say","say":"api_req_started","text":"{\"tokensIn\":` + itoa(in) +
			`,\"tokensOut\":` + itoa(out) + `}","modelInfo":{"modelId":"m","providerId":"anthropic"}}`
	}

	first := "[" + req(1767225610000, 100, 10) + "," + req(1767225620000, 200, 20) + "]"
	if err := os.WriteFile(filepath.Join(task, "ui_messages.json"), []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	srcs, err := p.Discover(context.Background())
	if err != nil || len(srcs) != 1 {
		t.Fatalf("Discover = %v, %v", srcs, err)
	}
	s := srcs[0]
	b1, err := p.Parse(context.Background(), s, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(b1.Events) != 2 || b1.Events[0].Tokens.Input != 100 {
		t.Fatalf("first parse = %+v", b1.Events)
	}
	if b1.Next.Extra != "json:2" {
		t.Fatalf("cursor Extra = %q, want json:2", b1.Next.Extra)
	}

	// A longer file with a different first 4KiB: the fingerprint changes, so the
	// whole document is parsed again and only the request past the counter is
	// emitted. The old byte offset is past the new content, so an offset-based
	// resume would have emitted nothing.
	second := "[" + req(1767225610000, 100, 10) + "," + req(1767225620000, 200, 20) + "," +
		req(1767225630000, 300, 30) + "]"
	if err := os.WriteFile(filepath.Join(task, "ui_messages.json"), []byte(second), 0o644); err != nil {
		t.Fatal(err)
	}
	b2, err := p.Parse(context.Background(), s, b1.Next)
	if err != nil {
		t.Fatalf("Parse after rewrite: %v", err)
	}
	if len(b2.Events) != 1 || b2.Events[0].Tokens.Input != 300 {
		t.Fatalf("second parse = %+v, want only the new request", b2.Events)
	}
	if b2.Next.Fingerprint == b1.Next.Fingerprint {
		t.Error("fingerprint did not change across a rewrite")
	}
	if b2.Next.Fingerprint == "" || b2.Next.Size == 0 || b2.Next.Offset != b2.Next.Size {
		t.Errorf("cursor is incomplete: %+v", b2.Next)
	}
	if b2.Next.Extra != "json:3" {
		t.Errorf("cursor Extra = %q, want json:3", b2.Next.Extra)
	}
}

// TestInPlaceEditKeepsFirstValue documents the one thing the emitted counter
// cannot see: Cline rewrites the whole array on every message, and an edit to a
// request that was already reported is not re-emitted. The store keeps the
// first-seen value for that dedup key, so the totals stay stable instead of
// oscillating while a request streams.
func TestInPlaceEditKeepsFirstValue(t *testing.T) {
	root := t.TempDir()
	task := filepath.Join(root, "saoudrizwan.claude-dev", "tasks", "1767225600000")
	if err := os.MkdirAll(task, 0o755); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoot(root)
	body := func(in int) string {
		return `[{"ts":1767225610000,"type":"say","say":"api_req_started","text":"{\"tokensIn\":` + itoa(in) +
			`,\"tokensOut\":10}","modelInfo":{"modelId":"m","providerId":"anthropic"}}]`
	}
	if err := os.WriteFile(filepath.Join(task, "ui_messages.json"), []byte(body(100)), 0o644); err != nil {
		t.Fatal(err)
	}
	srcs, err := p.Discover(context.Background())
	if err != nil || len(srcs) != 1 {
		t.Fatalf("Discover = %v, %v", srcs, err)
	}
	s := srcs[0]
	b1, err := p.Parse(context.Background(), s, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(b1.Events) != 1 || b1.Events[0].Tokens.Input != 100 {
		t.Fatalf("first parse = %+v", b1.Events)
	}
	if err := os.WriteFile(filepath.Join(task, "ui_messages.json"), []byte(body(999)), 0o644); err != nil {
		t.Fatal(err)
	}
	b2, err := p.Parse(context.Background(), s, b1.Next)
	if err != nil {
		t.Fatalf("Parse after edit: %v", err)
	}
	if len(b2.Events) != 0 {
		t.Errorf("edit re-emitted %d events, want none", len(b2.Events))
	}
	// The dedup key did not move, so the store keeps exactly one copy.
	if b2.Next.Extra != b1.Next.Extra {
		t.Errorf("cursor Extra moved from %q to %q", b1.Next.Extra, b2.Next.Extra)
	}
}

// TestRoots checks the default root list and the env override.
func TestRoots(t *testing.T) {
	p := New()
	roots := p.Roots()
	if len(roots) == 0 {
		t.Fatal("no default roots")
	}
	seen := map[string]bool{}
	for _, r := range roots {
		if seen[r] {
			t.Errorf("duplicate root %s", r)
		}
		seen[r] = true
	}
	// The VS Code family globalStorage tasks dir for the Cline extension.
	wantSuffix := filepath.Join("saoudrizwan.claude-dev", "tasks")
	found := false
	for _, r := range roots {
		if filepath.Base(r) == "tasks" && filepath.Base(filepath.Dir(r)) == "saoudrizwan.claude-dev" {
			found = true
		}
	}
	if !found {
		t.Errorf("no root ends in %s: %v", wantSuffix, roots)
	}

	t.Setenv("MYTOKEN_CLINE_DIRS", "/tmp/a"+string(filepath.ListSeparator)+"/tmp/b")
	q := New()
	if got := q.Roots(); len(got) != 2 || got[0] != "/tmp/a" || got[1] != "/tmp/b" {
		t.Errorf("env override roots = %v", got)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
