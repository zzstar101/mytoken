package kilo

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zzstar/mytoken/internal/harness"
	"github.com/zzstar/mytoken/internal/model"
)

func itoa(v int) string { return strconv.Itoa(v) }

// fixtureRoot points at testdata/kilo/sessions, a sanitized tree that mirrors
// Kilo Code's legacy (Cline-compatible) store:
//
//	kilocode.kilo-code/tasks/<taskId>/ui_messages.json
//	kilocode.kilo-code/tasks/<taskId>/api_conversation_history.json
//	kilocode.kilo-code/tasks/<taskId>/history_item.json
//	kilocode.kilo-code/tasks/<taskId>/task_metadata.json
//	kilocode.kilo-code/tasks/_index.json        (ignored by Discover)
//	kilocode.kilo-code/tasks/broken-task/...     (not an array)
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..", "..", "testdata", "kilo", "sessions")
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

func TestDiscover(t *testing.T) {
	srcs, err := NewWithRoot(fixtureRoot(t)).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 7 {
		var paths []string
		for _, s := range srcs {
			paths = append(paths, filepath.Base(filepath.Dir(s.Path)))
		}
		t.Fatalf("got %d sources (%v), want 7", len(srcs), paths)
	}
	for _, s := range srcs {
		if filepath.Base(s.Path) != "ui_messages.json" {
			t.Errorf("unexpected source %s", s.Path)
		}
		if s.Kind != "json" {
			t.Errorf("source %s kind = %q, want json", s.Path, s.Kind)
		}
	}
	for i := 1; i < len(srcs); i++ {
		if srcs[i-1].Path >= srcs[i].Path {
			t.Errorf("sources not sorted at %d", i)
		}
	}
}

// TestDiscoverIgnoresSiblingExtensions checks that a shared globalStorage root
// holding all three Cline-family extensions only yields Kilo Code sources.
func TestDiscoverIgnoresSiblingExtensions(t *testing.T) {
	root := t.TempDir()
	for _, ext := range []string{"saoudrizwan.claude-dev", "rooveterinaryinc.roo-cline", "kilocode.kilo-code"} {
		dir := filepath.Join(root, ext, "tasks", "1767331200000")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := `[{"ts":1767331210000,"type":"say","say":"api_req_started","text":"{\"tokensIn\":10,\"tokensOut\":1}"}]`
		if err := os.WriteFile(filepath.Join(dir, "ui_messages.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srcs, err := NewWithRoot(root).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("got %d sources, want 1", len(srcs))
	}
	if !strings.HasPrefix(srcs[0].Path, filepath.Join(root, "kilocode.kilo-code")) {
		t.Errorf("picked the wrong extension: %s", srcs[0].Path)
	}
}

// TestParseTotals pins the token accounting. Kilo Code stores the TOTAL input
// tokens with cache tokens included for both protocols, so the input bucket is
// what remains after subtracting cache reads and writes.
func TestParseTotals(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	events, sessions := parseAll(t, p)
	if len(events) != 9 {
		t.Errorf("got %d events, want 9", len(events))
	}
	if len(sessions) != 7 {
		t.Errorf("got %d sessions, want 7", len(sessions))
	}
	want := model.Tokens{Input: 18740, Output: 4185, CacheRead: 18840, CacheWrite: 1320}
	if got := sum(events); got != want {
		t.Errorf("sum = %+v, want %+v", got, want)
	}
	if got := sumDedup(events); got != want {
		t.Errorf("dedup sum = %+v, want %+v", got, want)
	}
}

// TestTokenAccounting is the table test for the token class mapping. Each case
// writes a real task directory and parses it, so the assertions cover the whole
// path from ui_messages.json to the emitted buckets.
func TestTokenAccounting(t *testing.T) {
	tests := []struct {
		name         string
		history      string
		metadata     string
		payload      string
		rawEntry     string
		modelInfo    string
		want         model.Tokens
		wantCost     float64
		wantModel    string
		wantProvider string
		wantNoEvent  bool
	}{
		{
			name:         "azure protocol is kept verbatim",
			history:      "<environment_details><model>azure/gpt-5</model></environment_details>",
			payload:      `{"apiProtocol":"azure/openai","tokensIn":5200,"tokensOut":710,"cacheWrites":240,"cacheReads":3900,"cost":0.0398}`,
			want:         model.Tokens{Input: 1060, Output: 710, CacheRead: 3900, CacheWrite: 240},
			wantCost:     0.0398,
			wantModel:    "azure/gpt-5",
			wantProvider: "azure/openai",
		},
		{
			name:         "anthropic protocol subtracts cache from the total input",
			history:      "<environment_details><model>anthropic/claude-sonnet-4-5-20250929</model></environment_details>",
			payload:      `{"apiProtocol":"anthropic","tokensIn":1800,"tokensOut":260,"cacheWrites":90,"cacheReads":1200,"cost":0.0132}`,
			want:         model.Tokens{Input: 510, Output: 260, CacheRead: 1200, CacheWrite: 90},
			wantCost:     0.0132,
			wantModel:    "anthropic/claude-sonnet-4-5-20250929",
			wantProvider: "anthropic",
		},
		{
			name:         "openai protocol subtracts cache from the total input",
			history:      "<environment_details><model>openai/gpt-5.1</model></environment_details>",
			payload:      `{"apiProtocol":"openai","tokensIn":7200,"tokensOut":520,"cacheWrites":0,"cacheReads":1800,"cost":0.024}`,
			want:         model.Tokens{Input: 5400, Output: 520, CacheRead: 1800},
			wantCost:     0.024,
			wantModel:    "openai/gpt-5.1",
			wantProvider: "openai",
		},
		{
			name:      "modelInfo outranks the history model tag",
			history:   "<environment_details><model>openai/gpt-5.1</model></environment_details>",
			modelInfo: `{"providerId":"anthropic","modelId":"claude-sonnet-4-5-20250929","mode":"code"}`,
			payload:   `{"apiProtocol":"anthropic","tokensIn":5000,"tokensOut":600,"cacheWrites":300,"cacheReads":1000,"cost":0.05}`,
			want:      model.Tokens{Input: 3700, Output: 600, CacheRead: 1000, CacheWrite: 300},
			wantCost:  0.05, wantModel: "claude-sonnet-4-5-20250929", wantProvider: "anthropic",
		},
		{
			name:     "no protocol falls back to task_metadata model_usage",
			metadata: `{"model_usage":[{"ts":1,"model_id":"glm-5","model_provider_id":"zai","mode":"code"},{"ts":2,"model_id":"glm-6","model_provider_id":"zai","mode":"code"}]}`,
			payload:  `{"tokensIn":2500,"tokensOut":300,"cacheWrites":0,"cacheReads":900,"cost":0.03}`,
			want:     model.Tokens{Input: 1600, Output: 300, CacheRead: 900},
			wantCost: 0.03, wantModel: "glm-6", wantProvider: "zai",
		},
		{
			name: "the last model tag in the history wins",
			history: "<environment_details><model>azure/gpt-5</model></environment_details>" +
				"<environment_details><model>azure/gpt-5.1</model></environment_details>",
			payload:  `{"apiProtocol":"azure/openai","tokensIn":900,"tokensOut":70,"cacheWrites":20,"cacheReads":600,"cost":0.006}`,
			want:     model.Tokens{Input: 280, Output: 70, CacheRead: 600, CacheWrite: 20},
			wantCost: 0.006, wantModel: "azure/gpt-5.1", wantProvider: "azure/openai",
		},
		{
			name:        "placeholder payload is not a request",
			history:     "<environment_details><model>openai/gpt-5.1</model></environment_details>",
			payload:     `{"apiProtocol":"openai"}`,
			wantNoEvent: true,
		},
		{
			name:        "malformed payload is not a request",
			history:     "<environment_details><model>openai/gpt-5.1</model></environment_details>",
			payload:     `not-json`,
			wantNoEvent: true,
		},
		{
			name:        "missing timestamp is not a request",
			history:     "<environment_details><model>openai/gpt-5.1</model></environment_details>",
			rawEntry:    `{"type":"say","say":"api_req_started","text":"{\"apiProtocol\":\"openai\",\"tokensIn\":10,\"tokensOut\":1}"}`,
			wantNoEvent: true,
		},
		{
			name:        "unparseable timestamp is not a request",
			history:     "<environment_details><model>openai/gpt-5.1</model></environment_details>",
			rawEntry:    `{"ts":"not-a-time","type":"say","say":"api_req_started","text":"{\"apiProtocol\":\"openai\",\"tokensIn\":10,\"tokensOut\":1}"}`,
			wantNoEvent: true,
		},
		{
			name:    "all zero usage is still a request",
			history: "<environment_details><model>openai/gpt-5.1</model></environment_details>",
			payload: `{"apiProtocol":"openai","tokensIn":0,"tokensOut":0,"cacheWrites":0,"cacheReads":0,"cost":0}`,
			want:    model.Tokens{}, wantModel: "openai/gpt-5.1", wantProvider: "openai",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			taskID := "1767331200000"
			root := t.TempDir()
			task := filepath.Join(root, "kilocode.kilo-code", "tasks", taskID)
			if err := os.MkdirAll(task, 0o755); err != nil {
				t.Fatal(err)
			}
			entry := tt.rawEntry
			if entry == "" {
				entry = `{"ts":1767331210000,"type":"say","say":"api_req_started","text":` + jsonQuote(tt.payload)
				if tt.modelInfo != "" {
					entry += `,"modelInfo":` + tt.modelInfo
				}
			}
			body := `[{"ts":1767331200000,"type":"say","say":"task","text":"Table test task"},` + entry + `}]`
			if err := os.WriteFile(filepath.Join(task, "ui_messages.json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if tt.history != "" {
				hist := `[{"role":"user","content":[{"type":"text","text":` + jsonQuote(tt.history) + `}]}]`
				if err := os.WriteFile(filepath.Join(task, "api_conversation_history.json"), []byte(hist), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.metadata != "" {
				if err := os.WriteFile(filepath.Join(task, "task_metadata.json"), []byte(tt.metadata), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			events, sessions := parseAll(t, NewWithRoot(root))
			if len(sessions) != 1 {
				t.Fatalf("sessions = %+v", sessions)
			}
			if tt.rawEntry == "" && sessions[0].Title != "Table test task" {
				t.Fatalf("title = %q, want \"Table test task\"", sessions[0].Title)
			}
			if tt.wantNoEvent {
				if len(events) != 0 {
					t.Fatalf("got %d events, want 0: %+v", len(events), events)
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("got %d events, want 1: %+v", len(events), events)
			}
			got := events[0]
			if got.Tokens != tt.want {
				t.Errorf("tokens = %+v, want %+v", got.Tokens, tt.want)
			}
			if got.Model != tt.wantModel {
				t.Errorf("model = %q, want %q", got.Model, tt.wantModel)
			}
			if got.Provider != tt.wantProvider {
				t.Errorf("provider = %q, want %q", got.Provider, tt.wantProvider)
			}
			if tt.wantCost != 0 && (got.CostUSD == nil || *got.CostUSD != tt.wantCost) {
				t.Errorf("cost = %v, want %v", got.CostUSD, tt.wantCost)
			}
		})
	}
}

// TestEventFields checks the per-event identity fields, including the subtask
// links that Roo Code and Kilo Code record in history_item.json.
func TestEventFields(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	events, _ := parseAll(t, p)
	byTask := map[string][]model.UsageEvent{}
	for _, e := range events {
		byTask[e.SessionID] = append(byTask[e.SessionID], e)
	}

	for _, e := range events {
		if e.Harness != model.Kilo {
			t.Errorf("harness = %q, want kilo", e.Harness)
		}
		if e.SessionID == "" || e.DedupKey == "" {
			t.Fatalf("event missing ids: %+v", e)
		}
		if e.Timestamp.IsZero() || e.Timestamp.Location().String() != "UTC" {
			t.Errorf("timestamp = %v, want UTC", e.Timestamp)
		}
		if e.BaseURL != "" {
			t.Errorf("base URL fabricated: %q", e.BaseURL)
		}
	}

	// The root task: no parent, provider from apiProtocol, model from the last
	// <environment_details> block of the sibling history file.
	for _, e := range byTask["1767331200000"] {
		if e.Provider != "azure/openai" {
			t.Errorf("provider = %q, want azure/openai", e.Provider)
		}
		if e.Model != "azure/gpt-5" {
			t.Errorf("model = %q, want azure/gpt-5", e.Model)
		}
		if e.ParentID != "" {
			t.Errorf("parent = %q, want empty for a root task", e.ParentID)
		}
		if e.ProjectPath != "/home/dev/kilo-proj" {
			t.Errorf("project = %q, want /home/dev/kilo-proj", e.ProjectPath)
		}
		want := "kilo:1767331200000:" + strconv.FormatInt(e.Timestamp.UnixMilli(), 10)
		if e.DedupKey != want {
			t.Errorf("dedup key = %q, want %q", e.DedupKey, want)
		}
	}

	// A delegated subtask links back to its direct parent.
	for _, e := range byTask["1767331300000"] {
		if e.ParentID != "1767331200000" {
			t.Errorf("parent = %q, want the direct parent 1767331200000", e.ParentID)
		}
	}
	// A task that only records rootTaskId falls back to the root.
	for _, e := range byTask["1767331400000"] {
		if e.ParentID != "1767331200000" {
			t.Errorf("parent = %q, want the root 1767331200000", e.ParentID)
		}
	}
	// A root task that records itself as its own rootTaskId must not become its
	// own parent.
	for _, e := range byTask["1781613537275"] {
		if e.ParentID == e.SessionID {
			t.Errorf("task %s is its own parent", e.SessionID)
		}
		if e.ParentID != "" {
			t.Errorf("parent = %q, want empty", e.ParentID)
		}
	}
	// modelInfo wins over the history model tag.
	for _, e := range byTask["1781613538300"] {
		if e.Model != "claude-sonnet-4-5-20250929" {
			t.Errorf("model = %q, want the modelInfo model", e.Model)
		}
		if e.Provider != "anthropic" {
			t.Errorf("provider = %q, want anthropic", e.Provider)
		}
		if e.ParentID != "1781613537275" {
			t.Errorf("parent = %q, want 1781613537275", e.ParentID)
		}
	}
	// task_metadata.json's LAST model_usage entry is the model in use.
	for _, e := range byTask["1781613539450"] {
		if e.Model != "glm-5" {
			t.Errorf("model = %q, want glm-5", e.Model)
		}
		if e.Provider != "zai" {
			t.Errorf("provider = %q, want zai", e.Provider)
		}
	}
}

// TestSessionMeta checks the session side of the batch.
func TestSessionMeta(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	_, sessions := parseAll(t, p)

	main, ok := sessionByID(sessions, "1767331200000")
	if !ok {
		t.Fatal("main session missing")
	}
	if main.Title != "Set up the CI matrix" {
		t.Errorf("title = %q", main.Title)
	}
	if main.Project != "/home/dev/kilo-proj" {
		t.Errorf("project = %q", main.Project)
	}
	if main.ParentID != "" {
		t.Errorf("parent = %q, want empty", main.ParentID)
	}
	if main.StartedAt.UnixMilli() != 1767331200000 {
		t.Errorf("started = %v", main.StartedAt)
	}
	if main.UpdatedAt.UnixMilli() != 1767331230000 {
		t.Errorf("updated = %v", main.UpdatedAt)
	}
	if main.UpdatedAt.Before(main.StartedAt) {
		t.Error("updated before started")
	}

	sub, ok := sessionByID(sessions, "1781613538300")
	if !ok {
		t.Fatal("subtask session missing")
	}
	if sub.Title != "Add the backfill step" {
		t.Errorf("subtask title = %q", sub.Title)
	}
	if sub.ParentID != "1781613537275" {
		t.Errorf("subtask parent = %q", sub.ParentID)
	}

	// A task directory whose ui_messages.json is not an array still yields a
	// session; its window comes from history_item.json.
	broken, ok := sessionByID(sessions, "broken-task")
	if !ok {
		t.Fatal("broken session missing")
	}
	if broken.Title != "" {
		t.Errorf("broken title = %q, want empty", broken.Title)
	}
	if broken.StartedAt.UnixMilli() != 1767331500000 {
		t.Errorf("broken started = %v", broken.StartedAt)
	}
}

// TestDedupLastWins checks that a rescanned file emits the same events again
// with the same keys, so the store's last-copy-wins rule keeps one copy.
func TestDedupLastWins(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	first, _ := parseAll(t, p)
	second, _ := parseAll(t, p)
	if len(first) != len(second) {
		t.Fatalf("rescan changed the event count: %d -> %d", len(first), len(second))
	}
	seen := map[string]int{}
	for _, e := range first {
		seen[e.DedupKey]++
	}
	for _, e := range second {
		if seen[e.DedupKey] != 1 {
			t.Errorf("dedup key %s seen %d times in one pass, want 1", e.DedupKey, seen[e.DedupKey])
		}
	}
	if got, want := sumDedup(append(first, second...)), sum(first); got != want {
		t.Errorf("dedup sum = %+v, want %+v", got, want)
	}
}

// TestIncrementalResume checks that appending requests to a rewritten
// ui_messages.json emits only the new ones.
func TestIncrementalResume(t *testing.T) {
	root := t.TempDir()
	task := filepath.Join(root, "kilocode.kilo-code", "tasks", "1767331200000")
	if err := os.MkdirAll(task, 0o755); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoot(root)
	req := func(ts int, in, out, cr, cw int) string {
		return `{"ts":` + itoa(ts) + `,"type":"say","say":"api_req_started","text":"{\"apiProtocol\":\"anthropic\",\"tokensIn\":` + itoa(in) +
			`,\"tokensOut\":` + itoa(out) + `,\"cacheWrites\":` + itoa(cw) + `,\"cacheReads\":` + itoa(cr) + `}"}`
	}
	steps := []string{
		`[` + req(1767331210000, 1000, 100, 0, 0) + `]`,
		`[` + req(1767331210000, 1000, 100, 0, 0) + `,` + req(1767331220000, 2000, 200, 0, 0) + `]`,
		`[` + req(1767331210000, 1000, 100, 0, 0) + `,` + req(1767331220000, 2000, 200, 0, 0) + `,` + req(1767331230000, 3000, 300, 500, 0) + `]`,
	}
	if err := os.WriteFile(filepath.Join(task, "ui_messages.json"), []byte(steps[0]), 0o644); err != nil {
		t.Fatal(err)
	}
	srcs, err := p.Discover(context.Background())
	if err != nil || len(srcs) != 1 {
		t.Fatalf("Discover = %v, %v", srcs, err)
	}
	s := srcs[0]
	var cur harness.Cursor
	var emitted int64
	for i, body := range steps {
		if err := os.WriteFile(filepath.Join(task, "ui_messages.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		b, err := p.Parse(context.Background(), s, cur)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(b.Events) != 1 {
			t.Errorf("step %d emitted %d events, want 1", i, len(b.Events))
		}
		for _, e := range b.Events {
			emitted += e.Tokens.Input + e.Tokens.Output + e.Tokens.CacheRead + e.Tokens.CacheWrite
		}
		cur = b.Next
	}
	if want := int64(1000 + 100 + 2000 + 200 + 2500 + 300 + 500); emitted != want {
		t.Errorf("emitted %d tokens, want %d", emitted, want)
	}
}

// TestFingerprintChangeReparses checks that a file rewritten with different
// content (a different fingerprint) is re-read from the start instead of being
// resumed by byte offset, which is meaningless for a whole-file JSON document.
func TestFingerprintChangeReparses(t *testing.T) {
	root := t.TempDir()
	task := filepath.Join(root, "kilocode.kilo-code", "tasks", "1767331200000")
	if err := os.MkdirAll(task, 0o755); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoot(root)
	req := func(ts int, in, out int) string {
		return `{"ts":` + itoa(ts) + `,"type":"say","say":"api_req_started","text":"{\"apiProtocol\":\"anthropic\",\"tokensIn\":` + itoa(in) +
			`,\"tokensOut\":` + itoa(out) + `}"}`
	}

	first := "[" + req(1767331210000, 100, 10) + "," + req(1767331220000, 200, 20) + "]"
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
	second := "[" + req(1767331210000, 100, 10) + "," + req(1767331220000, 200, 20) + "," +
		req(1767331230000, 300, 30) + "]"
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
// cannot see: the whole array is rewritten on every message, and an edit to a
// request that was already reported is not re-emitted.
func TestInPlaceEditKeepsFirstValue(t *testing.T) {
	root := t.TempDir()
	task := filepath.Join(root, "kilocode.kilo-code", "tasks", "1767331200000")
	if err := os.MkdirAll(task, 0o755); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoot(root)
	body := func(in int) string {
		return `[{"ts":1767331210000,"type":"say","say":"api_req_started","text":"{\"apiProtocol\":\"anthropic\",\"tokensIn\":` + itoa(in) +
			`,\"tokensOut\":10}"}]`
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
	if b2.Next.Extra != b1.Next.Extra {
		t.Errorf("cursor Extra moved from %q to %q", b1.Next.Extra, b2.Next.Extra)
	}
}

// TestUnchangedFileIsSkipped checks the fast path: an unchanged file produces an
// empty batch and keeps the cursor.
func TestUnchangedFileIsSkipped(t *testing.T) {
	p := NewWithRoot(fixtureRoot(t))
	srcs, err := p.Discover(context.Background())
	if err != nil || len(srcs) == 0 {
		t.Fatalf("Discover = %v, %v", srcs, err)
	}
	b1, err := p.Parse(context.Background(), srcs[0], harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b2, err := p.Parse(context.Background(), srcs[0], b1.Next)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(b2.Events) != 0 || len(b2.Sessions) != 0 {
		t.Errorf("unchanged file emitted %d events, %d sessions", len(b2.Events), len(b2.Sessions))
	}
	if b2.Next != b1.Next {
		t.Errorf("cursor changed on an unchanged file: %+v -> %+v", b1.Next, b2.Next)
	}
}

func TestRegistration(t *testing.T) {
	got, ok := harness.Get(model.Kilo)
	if !ok {
		t.Fatal("kilo not registered")
	}
	if got.Harness() != model.Kilo {
		t.Errorf("registered parser reports %q", got.Harness())
	}
	roots := got.Roots()
	if len(roots) == 0 {
		t.Error("no default roots")
	}
	for _, r := range roots {
		if !filepath.IsAbs(r) {
			t.Errorf("default root %q is not absolute", r)
		}
	}
}

func TestMissingRoot(t *testing.T) {
	p := NewWithRoot(filepath.Join(t.TempDir(), "nope"))
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover on a missing root: %v", err)
	}
	if len(srcs) != 0 {
		t.Errorf("got %d sources, want 0", len(srcs))
	}
	if len(p.Roots()) != 1 {
		t.Errorf("Roots() = %v", p.Roots())
	}
}

// jsonQuote renders s as a JSON string literal. Go's json.Marshal HTML-escapes
// < > & into \u003c \u003e \u0026, which real Cline-family files never contain
// (JavaScript's JSON.stringify does not escape them), and the escaped form would
// hide the <environment_details> markers from the parser's regexes.
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	out := string(b)
	for _, r := range [][2]string{{`\u003c`, "<"}, {`\u003e`, ">"}, {`\u0026`, "&"}} {
		out = strings.ReplaceAll(out, r[0], r[1])
	}
	return out
}
