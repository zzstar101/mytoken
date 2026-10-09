package clinetask

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzstar/mytoken/internal/harness"
	"github.com/zzstar/mytoken/internal/model"
)

func withGOOS(t *testing.T, goos string) {
	t.Helper()
	old := runtimeGOOS
	runtimeGOOS = func() string { return goos }
	t.Cleanup(func() { runtimeGOOS = old })
}

// TestVSCodeTaskDirs pins the per-OS root list: the VS Code family
// globalStorage tasks directory of the extension, for every app in the family.
func TestVSCodeTaskDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", "")

	withGOOS(t, "darwin")
	darwin := VSCodeTaskDirs(ExtensionIDRoo)
	wantDarwin := []string{
		filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage", ExtensionIDRoo, "tasks"),
		filepath.Join(home, "Library", "Application Support", "Code - Insiders", "User", "globalStorage", ExtensionIDRoo, "tasks"),
		filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", ExtensionIDRoo, "tasks"),
		filepath.Join(home, "Library", "Application Support", "Windsurf", "User", "globalStorage", ExtensionIDRoo, "tasks"),
		filepath.Join(home, "Library", "Application Support", "VSCodium", "User", "globalStorage", ExtensionIDRoo, "tasks"),
		filepath.Join(home, "Library", "Application Support", "Trae", "User", "globalStorage", ExtensionIDRoo, "tasks"),
	}
	if !reflect.DeepEqual(darwin, wantDarwin) {
		t.Errorf("darwin roots =\n%v\nwant\n%v", darwin, wantDarwin)
	}

	withGOOS(t, "linux")
	linux := VSCodeTaskDirs(ExtensionIDRoo)
	wantLinux := []string{
		filepath.Join(home, ".config", "Code", "User", "globalStorage", ExtensionIDRoo, "tasks"),
		filepath.Join(home, ".config", "Code - Insiders", "User", "globalStorage", ExtensionIDRoo, "tasks"),
		filepath.Join(home, ".config", "Cursor", "User", "globalStorage", ExtensionIDRoo, "tasks"),
		filepath.Join(home, ".config", "Windsurf", "User", "globalStorage", ExtensionIDRoo, "tasks"),
		filepath.Join(home, ".config", "VSCodium", "User", "globalStorage", ExtensionIDRoo, "tasks"),
		filepath.Join(home, ".config", "Trae", "User", "globalStorage", ExtensionIDRoo, "tasks"),
	}
	if !reflect.DeepEqual(linux, wantLinux) {
		t.Errorf("linux roots =\n%v\nwant\n%v", linux, wantLinux)
	}

	// XDG_CONFIG_HOME replaces ~/.config when set.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	withXDG := VSCodeTaskDirs(ExtensionIDRoo)
	if len(withXDG) != 12 {
		t.Fatalf("got %d roots with XDG_CONFIG_HOME set, want 12", len(withXDG))
	}
	if withXDG[6] != filepath.Join(xdg, "Code", "User", "globalStorage", ExtensionIDRoo, "tasks") {
		t.Errorf("first XDG root = %q", withXDG[6])
	}
	t.Setenv("XDG_CONFIG_HOME", "")

	appData := t.TempDir()
	t.Setenv("APPDATA", appData)
	withGOOS(t, "windows")
	windows := VSCodeTaskDirs(ExtensionIDRoo)
	if len(windows) != 6 {
		t.Fatalf("got %d windows roots, want 6", len(windows))
	}
	if windows[0] != filepath.Join(appData, "Code", "User", "globalStorage", ExtensionIDRoo, "tasks") {
		t.Errorf("first windows root = %q", windows[0])
	}
}

// TestClineDataDirs mirrors Cline's own resolution order for the standalone
// build: $CLINE_DATA_DIR, else $CLINE_DIR/data, else ~/.cline/data.
func TestClineDataDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Setenv("CLINE_DATA_DIR", "/explicit/data")
	t.Setenv("CLINE_DIR", "/explicit/cline")
	if got, want := ClineDataDirs(), []string{"/explicit/data"}; !reflect.DeepEqual(got, want) {
		t.Errorf("CLINE_DATA_DIR: got %v, want %v", got, want)
	}
	if got, want := ClineSessionDirs(), []string{"/explicit/data/sessions"}; !reflect.DeepEqual(got, want) {
		t.Errorf("CLINE_DATA_DIR sessions: got %v, want %v", got, want)
	}

	t.Setenv("CLINE_DATA_DIR", "")
	if got, want := ClineDataDirs(), []string{"/explicit/cline/data"}; !reflect.DeepEqual(got, want) {
		t.Errorf("CLINE_DIR: got %v, want %v", got, want)
	}

	t.Setenv("CLINE_DIR", "")
	if got, want := ClineDataDirs(), []string{filepath.Join(home, ".cline", "data")}; !reflect.DeepEqual(got, want) {
		t.Errorf("default: got %v, want %v", got, want)
	}
}

// TestNumFnumTSVal is the table test for the tolerant scalar decoders.
func TestNumFnumTSVal(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantNum int64
		wantF   float64
		wantTS  time.Time
	}{
		{name: "plain integers", payload: `{"n":12,"f":1.5,"t":1767225600000}`, wantNum: 12, wantF: 1.5, wantTS: time.UnixMilli(1767225600000).UTC()},
		{name: "floats", payload: `{"n":12.9,"f":0.0110232,"t":1767225600.5}`, wantNum: 12, wantF: 0.0110232, wantTS: time.Unix(1767225600, 0).UTC()},
		{name: "fractional millis", payload: `{"n":1,"f":2,"t":1767225600000.5}`, wantNum: 1, wantF: 2, wantTS: time.UnixMilli(1767225600000).Add(500 * time.Microsecond).UTC()},
		{name: "quoted strings", payload: `{"n":"42","f":"0.5","t":"1767225600000"}`, wantNum: 42, wantF: 0.5, wantTS: time.UnixMilli(1767225600000).UTC()},
		{name: "nulls", payload: `{"n":null,"f":null,"t":null}`, wantNum: 0, wantF: 0},
		{name: "rfc3339 string", payload: `{"n":1,"f":2,"t":"2026-01-01T00:00:00Z"}`, wantNum: 1, wantF: 2, wantTS: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "epoch seconds", payload: `{"n":1,"f":2,"t":1767225600}`, wantNum: 1, wantF: 2, wantTS: time.Unix(1767225600, 0).UTC()},
		// A value that is not a number at all must not abort the document.
		{name: "garbage numbers", payload: `{"n":"not-a-number","f":"NaN","t":"not-a-time"}`, wantNum: 0, wantF: 0},
		{name: "garbage after a good value", payload: `{"n":7,"f":8,"t":"not-a-time"}`, wantNum: 7, wantF: 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var doc struct {
				N num   `json:"n"`
				F fnum  `json:"f"`
				T tsVal `json:"t"`
			}
			if err := json.Unmarshal([]byte(tt.payload), &doc); err != nil {
				t.Fatalf("Unmarshal(%s): %v", tt.payload, err)
			}
			if int64(doc.N) != tt.wantNum {
				t.Errorf("num = %d, want %d", int64(doc.N), tt.wantNum)
			}
			if float64(doc.F) != tt.wantF {
				t.Errorf("fnum = %v, want %v", float64(doc.F), tt.wantF)
			}
			if got := doc.T.Time(); !got.Equal(tt.wantTS) {
				t.Errorf("tsVal = %v, want %v", got, tt.wantTS)
			}
		})
	}
}

// TestEpochToTime covers the millisecond/second threshold and the zero guard.
func TestEpochToTime(t *testing.T) {
	if got := epochToTime(0); !got.IsZero() {
		t.Errorf("0 -> %v, want zero", got)
	}
	if got := epochToTime(-5); !got.IsZero() {
		t.Errorf("-5 -> %v, want zero", got)
	}
	if got, want := epochToTime(1767225600), time.Unix(1767225600, 0).UTC(); !got.Equal(want) {
		t.Errorf("seconds: got %v, want %v", got, want)
	}
	if got, want := epochToTime(1767225600000), time.UnixMilli(1767225600000).UTC(); !got.Equal(want) {
		t.Errorf("millis: got %v, want %v", got, want)
	}
}

// TestSameFamily checks that a root holding more than one Cline-family
// extension does not leak another harness's tasks.
func TestSameFamily(t *testing.T) {
	tests := []struct {
		path  string
		extID string
		want  bool
	}{
		{path: "/gs/kilocode.kilo-code/tasks/1/ui_messages.json", extID: ExtensionIDKilo, want: true},
		{path: "/gs/kilocode.kilo-code/tasks/1/ui_messages.json", extID: ExtensionIDRoo, want: false},
		{path: "/gs/kilocode.kilo-code/tasks/1/ui_messages.json", extID: ExtensionIDCline, want: false},
		// A path that names no extension at all cannot be disambiguated, so it is
		// accepted: a bare tasks directory, or the CLI's sessions directory.
		{path: "/anywhere/tasks/1/ui_messages.json", extID: ExtensionIDRoo, want: true},
		{path: "/home/u/.cline/data/sessions/s/s.messages.json", extID: ExtensionIDCline, want: true},
		// The CLI sessions directory names no extension, so Roo cannot tell it
		// apart from its own; it is accepted rather than guessed at.
		{path: "/home/u/.cline/data/sessions/s/s.messages.json", extID: ExtensionIDRoo, want: true},
	}
	for _, tt := range tests {
		if got := sameFamily(tt.path, tt.extID); got != tt.want {
			t.Errorf("sameFamily(%q, %q) = %v, want %v", tt.path, tt.extID, got, tt.want)
		}
	}
}

// TestTaskParentID checks the history_item.json parent resolution, including the
// self-reference a root task records as its own rootTaskId.
func TestTaskParentID(t *testing.T) {
	tests := []struct {
		name   string
		taskID string
		parent string
		root   string
		want   string
	}{
		{name: "no links", taskID: "a", want: ""},
		{name: "direct parent wins", taskID: "a", parent: "p", root: "r", want: "p"},
		{name: "root fallback", taskID: "a", root: "r", want: "r"},
		{name: "self root is not a parent", taskID: "a", root: "a", want: ""},
		{name: "self parent is not a parent", taskID: "a", parent: "a", root: "r", want: "r"},
		{name: "blank links", taskID: "a", parent: "  ", root: "", want: ""},
	}
	for _, tt := range tests {
		if got := taskParentID(tt.taskID, tt.parent, tt.root); got != tt.want {
			t.Errorf("%s: taskParentID(%q, %q, %q) = %q, want %q", tt.name, tt.taskID, tt.parent, tt.root, got, tt.want)
		}
	}
}

// TestDiscoverFiltersExtensions checks the Discover-side extension filter end to
// end, and that _index.json and the sibling files are never sources.
func TestDiscoverFiltersExtensions(t *testing.T) {
	root := t.TempDir()
	body := `[{"ts":1767225600000,"type":"say","say":"task","text":"t"},` +
		`{"ts":1767225610000,"type":"say","say":"api_req_started","text":"{\"tokensIn\":1,\"tokensOut\":1}"}]`
	for _, ext := range clineFamilyExtensions {
		dir := filepath.Join(root, ext, "tasks", "1")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, FileUIMessages), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, FileHistory), []byte("[]"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A stray ui_messages.json under a dot directory must be skipped.
	if err := os.MkdirAll(filepath.Join(root, ".Trash", "tasks", "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".Trash", "tasks", "1", FileUIMessages), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, ext := range clineFamilyExtensions {
		srcs := Discover(root, Options{Harness: model.Cline, ExtensionID: ext})
		if len(srcs) != 1 {
			t.Fatalf("ext %s: got %d sources, want 1", ext, len(srcs))
		}
		if !strings.Contains(srcs[0].Path, ext) {
			t.Errorf("ext %s: picked %s", ext, srcs[0].Path)
		}
		if srcs[0].Kind != KindJSON {
			t.Errorf("ext %s: kind = %q, want json", ext, srcs[0].Kind)
		}
	}
}

// TestParseIgnoresSiblingFormats checks that a Source for a .messages.json file
// is only parsed when the harness asked for the CLI format.
func TestParseIgnoresSiblingFormats(t *testing.T) {
	root := t.TempDir()
	session := filepath.Join(root, "sessions", "abc")
	if err := os.MkdirAll(session, 0o755); err != nil {
		t.Fatal(err)
	}
	msgs := `{"sessionId":"abc","agent":"lead","messages":[` +
		`{"id":"m1","role":"user","content":[{"type":"text","text":"hi"}]},` +
		`{"id":"m2","role":"assistant","createdAt":1767225600000,"content":[{"type":"text","text":"ok"}],` +
		`"metrics":{"inputTokens":100,"outputTokens":10,"cacheReadTokens":5,"cacheWriteTokens":0,"cost":0.01}}]}`
	if err := os.WriteFile(filepath.Join(session, "abc.messages.json"), []byte(msgs), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := Options{Harness: model.Cline, ExtensionID: ExtensionIDCline}
	if srcs := Discover(root, opts); len(srcs) != 0 {
		t.Fatalf("Discover without Sessions returned %d sources, want 0", len(srcs))
	}
	opts.Sessions = true
	srcs := Discover(root, opts)
	if len(srcs) != 1 {
		t.Fatalf("Discover with Sessions returned %d sources, want 1", len(srcs))
	}
	b, err := Parse(srcs[0], harness.Cursor{}, opts)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(b.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(b.Events))
	}
	// metrics.inputTokens is the full request size, so the input bucket is what
	// remains after the cache buckets.
	if b.Events[0].Tokens != (model.Tokens{Input: 95, Output: 10, CacheRead: 5}) {
		t.Errorf("tokens = %+v", b.Events[0].Tokens)
	}
	if b.Events[0].DedupKey != "cline-cli:abc:m2" {
		t.Errorf("dedup key = %q", b.Events[0].DedupKey)
	}
	if b.Next.Extra != "json:1" {
		t.Errorf("cursor Extra = %q, want json:1", b.Next.Extra)
	}
}

// TestParseSubagentAndDeletedUsage checks the two extra say kinds Cline's own
// getApiMetrics sums — subagent_usage and deleted_api_reqs — against
// api_req_started: they share one positional counter, follow the same cache
// rule, and subagent_usage becomes a child session of the task.
func TestParseSubagentAndDeletedUsage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ExtensionIDCline, "tasks", "9001")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, FileUIMessages), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// One of each kind, plus the say:"subagent" status message that carries the
	// same numbers as the aggregate and must not be counted twice, plus a
	// malformed payload of each new kind.
	body := `[{"ts":1767225900000,"type":"say","say":"task","text":"Orchestrate"},` +
		`{"ts":1767225901000,"type":"say","say":"api_req_started","text":"{\"tokensIn\":100,\"tokensOut\":10,\"cacheWrites\":5,\"cacheReads\":7,\"cost\":0.01}"},` +
		`{"ts":1767225902000,"type":"say","say":"subagent_usage","text":"{\"source\":\"subagents\",\"tokensIn\":900,\"tokensOut\":90,\"cacheWrites\":0,\"cacheReads\":0,\"cost\":0.09}"},` +
		`{"ts":1767225903000,"type":"say","say":"subagent","text":"{\"status\":\"running\",\"inputTokens\":900,\"outputTokens\":90}"},` +
		`{"ts":1767225904000,"type":"say","say":"subagent_usage","text":"not-json"},` +
		`{"ts":1767225905000,"type":"say","say":"deleted_api_reqs","text":"{\"tokensIn\":400,\"tokensOut\":40,\"cacheWrites\":4,\"cacheReads\":3,\"cost\":0.04}"},` +
		`{"ts":1767225906000,"type":"say","say":"deleted_api_reqs","text":"{}"},` +
		`{"ts":1767225907000,"type":"say","say":"completion_result","text":"done"}]`
	write(body)
	src := harness.Source{Path: filepath.Join(dir, FileUIMessages), Kind: KindJSON}
	opts := Options{
		Harness:            model.Cline,
		ExtensionID:        ExtensionIDCline,
		InputIncludesCache: func(string, string, string) bool { return true },
	}

	b, err := Parse(src, harness.Cursor{}, opts)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Three usage entries survived; the two malformed ones did not count
	// towards the cursor either, so a later repair still gets parsed.
	if len(b.Events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(b.Events), b.Events)
	}
	if b.Next.Extra != "json:3" {
		t.Errorf("cursor Extra = %q, want json:3", b.Next.Extra)
	}
	want := []model.Tokens{
		{Input: 88, Output: 10, CacheRead: 7, CacheWrite: 5},  // api_req_started
		{Input: 900, Output: 90},                              // subagent_usage
		{Input: 393, Output: 40, CacheRead: 3, CacheWrite: 4}, // deleted_api_reqs
	}
	for i, tc := range want {
		if b.Events[i].Tokens != tc {
			t.Errorf("event %d tokens = %+v, want %+v", i, b.Events[i].Tokens, tc)
		}
	}
	// No modelInfo and no siblings → provider-less, so the input bucket is the
	// prompt size minus the cache buckets, exactly as for api_req_started.
	if got := b.Events[0].DedupKey; got != "cline:9001:1767225901000" {
		t.Errorf("api dedup key = %q", got)
	}
	if got := b.Events[1].DedupKey; got != "cline:9001:subagents:1767225902000" {
		t.Errorf("subagent dedup key = %q", got)
	}
	if got := b.Events[2].DedupKey; got != "cline:9001:1767225905000" {
		t.Errorf("deleted dedup key = %q", got)
	}

	// The subagent session is a child of the task, and only it exists when the
	// task actually ran subagents.
	if len(b.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2: %+v", len(b.Sessions), b.Sessions)
	}
	sub := b.Sessions[1]
	if sub.SessionID != "9001:subagents" || sub.ParentID != "9001" || sub.Title != "Subagents" {
		t.Errorf("subagent session = %+v", sub)
	}
	if sub.StartedAt.UnixMilli() != 1767225902000 || sub.UpdatedAt.UnixMilli() != 1767225902000 {
		t.Errorf("subagent window = %v..%v, want the subagent_usage span", sub.StartedAt, sub.UpdatedAt)
	}
	task := b.Sessions[0]
	if task.SessionID != "9001" || task.ParentID != "" || task.Title != "Orchestrate" {
		t.Errorf("task session = %+v", task)
	}
	if task.StartedAt.UnixMilli() != 1767225900000 || task.UpdatedAt.UnixMilli() != 1767225907000 {
		t.Errorf("task window = %v..%v", task.StartedAt, task.UpdatedAt)
	}

	// Incremental: appending one more of each kind emits exactly those two.
	write(body[:len(body)-1] + `,` +
		`{"ts":1767225908000,"type":"say","say":"api_req_started","text":"{\"tokensIn\":50,\"tokensOut\":5}"},` +
		`{"ts":1767225909000,"type":"say","say":"subagent_usage","text":"{\"tokensIn\":10,\"tokensOut\":1}"}]`)
	b2, err := Parse(src, b.Next, opts)
	if err != nil {
		t.Fatalf("Parse (incremental): %v", err)
	}
	if len(b2.Events) != 2 {
		t.Fatalf("incremental: got %d events, want 2: %+v", len(b2.Events), b2.Events)
	}
	if b2.Next.Extra != "json:5" {
		t.Errorf("incremental cursor Extra = %q, want json:5", b2.Next.Extra)
	}
	if b2.Sessions[1].UpdatedAt.UnixMilli() != 1767225909000 {
		t.Errorf("subagent window after append = %v..%v", b2.Sessions[1].StartedAt, b2.Sessions[1].UpdatedAt)
	}

	// Resuming with a counter that already covers the file emits nothing, which
	// is the documented behaviour for an in-place edit.
	b3, err := Parse(src, b2.Next, opts)
	if err != nil {
		t.Fatalf("Parse (no-op): %v", err)
	}
	if len(b3.Events) != 0 {
		t.Errorf("no-op rescan emitted %d events", len(b3.Events))
	}
	if b3.Next.Extra != "json:5" {
		t.Errorf("no-op cursor Extra = %q, want json:5", b3.Next.Extra)
	}
}

// TestParseNoSubagentSession checks that a task without subagent_usage gets no
// synthetic child session, so only real subagent usage shows up in the tree.
func TestParseNoSubagentSession(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ExtensionIDRoo, "tasks", "9002")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `[{"ts":1767225900000,"type":"say","say":"task","text":"t"},` +
		`{"ts":1767225910000,"type":"say","say":"api_req_started","text":"{\"tokensIn\":1,\"tokensOut\":1}"}]`
	if err := os.WriteFile(filepath.Join(dir, FileUIMessages), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := Parse(harness.Source{Path: filepath.Join(dir, FileUIMessages), Kind: KindJSON}, harness.Cursor{},
		Options{Harness: model.Roo, ExtensionID: ExtensionIDRoo})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(b.Events) != 1 || len(b.Sessions) != 1 {
		t.Fatalf("got %d events, %d sessions, want 1 and 1", len(b.Events), len(b.Sessions))
	}
	if b.Sessions[0].SessionID != "9002" {
		t.Errorf("session = %+v, want only the task", b.Sessions[0])
	}
}

// TestParseMissingFile checks that a file that vanished between Discover and
// Parse is not an error.
func TestParseMissingFile(t *testing.T) {
	src := harness.Source{Path: filepath.Join(t.TempDir(), "gone", "ui_messages.json"), Kind: KindJSON}
	b, err := Parse(src, harness.Cursor{}, Options{Harness: model.Cline, ExtensionID: ExtensionIDCline})
	if err != nil {
		t.Fatalf("Parse on a missing file: %v", err)
	}
	if len(b.Events) != 0 || len(b.Sessions) != 0 {
		t.Errorf("got %d events, %d sessions", len(b.Events), len(b.Sessions))
	}
	if b.Next != (harness.Cursor{}) {
		t.Errorf("cursor = %+v, want zero", b.Next)
	}
}

// TestContextCancelled checks that a cancelled context stops the walk.
func TestContextCancelled(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ExtensionIDRoo, "tasks", "1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileUIMessages), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Discover is the only entry point that takes a context; Parse is a pure
	// read of one file and cannot block.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A cancelled context stops the walk early instead of scanning the tree.
	got := DiscoverWithContext(ctx, root, Options{Harness: model.Roo, ExtensionID: ExtensionIDRoo})
	if len(got) != 0 {
		t.Errorf("cancelled Discover returned %d sources, want 0", len(got))
	}
	// Without a context the same tree is found.
	if srcs := Discover(root, Options{Harness: model.Roo, ExtensionID: ExtensionIDRoo}); len(srcs) != 1 {
		t.Errorf("Discover returned %d sources, want 1", len(srcs))
	}
}
