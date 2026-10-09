// Package clinetask parses the "Cline task" on-disk format shared by the Cline,
// Roo Code and Kilo Code VS Code extensions, plus the standalone Cline CLI
// session format.
//
// # VS Code task format
//
// Every task lives in its own directory:
//
//	<globalStorage>/<extension-id>/tasks/<taskId>/
//	    ui_messages.json             JSON array of ClineMessage (the usage log)
//	    api_conversation_history.json JSON array of the API conversation
//	    task_metadata.json            context + model_usage bookkeeping
//	    history_item.json             Roo/Kilo task history record (optional)
//
// <globalStorage> is VS Code's context.globalStorageUri.fsPath, i.e.
// ~/Library/Application Support/<app>/User/globalStorage/<ext-id> on macOS,
// ~/.config/<app>/User/globalStorage/<ext-id> on Linux and
// %APPDATA%\<app>\User\globalStorage\<ext-id> on Windows, for every app in the
// VS Code family (Code, Code - Insiders, Cursor, Windsurf, VSCodium, Trae).
//
// ui_messages.json is a JSON array rewritten in place (Roo writes it through
// safeWriteJson: lockfile + temp file + rename, Cline through its own atomic
// writer), so the cursor is the per-file ModTime/Size/Fingerprint plus an Extra
// counter recording how many usage entries were already emitted.
//
// Usage comes exclusively from entries of the shape
//
//	{"ts":<epoch ms>,"type":"say","say":"api_req_started",
//	 "text":"{\"tokensIn\":..,\"tokensOut\":..,\"cacheWrites\":..,
//	          \"cacheReads\":..,\"cost\":..,\"apiProtocol\":..}",
//	 "modelInfo":{"providerId":..,"modelId":..,"mode":..}}
//
// Two more say kinds carry the same token fields and are counted alongside it,
// mirroring Cline's own accounting (apps/vscode/src/shared/getApiMetrics.ts:
// "It includes 'api_req_started' messages …, 'deleted_api_reqs' messages, which
// are aggregated from deleted messages, 'subagent_usage' messages, which are
// aggregated usage snapshots emitted by subagent batches"):
//
//	{"ts":..,"type":"say","say":"subagent_usage",
//	 "text":"{\"source\":\"subagents\",\"tokensIn\":..,\"tokensOut\":..,
//	          \"cacheWrites\":0,\"cacheReads\":0,\"cost\":..}"}
//	{"ts":..,"type":"say","say":"deleted_api_reqs",
//	 "text":"{\"tokensIn\":..,\"tokensOut\":..,\"cacheWrites\":..,
//	          \"cacheReads\":..,\"cost\":..}"}
//
// deleted_api_reqs is written when a checkpoint restore truncates the array:
// src/core/task/index.ts keeps the messages before the restore point, drops the
// rest and appends the aggregate of what it dropped — "aggregate deleted api
// reqs info so we don't lose costs/tokens". Those requests were billed and are
// gone from the file, so counting the aggregate is what keeps our totals equal
// to the totals Cline itself reports for the live array.
//
// subagent_usage is written once per iteration when every spawn_agent call of
// that iteration has finished (apps/vscode/src/sdk/message-translator.ts:
// "When all done, emit subagent_usage for cost accounting"). The subagents
// themselves are never persisted as sessions of the task — their SDK ids are
// "<rootSessionId>__<agentId>" and only the aggregate reaches ui_messages.json
// — so the usage is attributed to one synthetic child session per task, named
// "<taskId>:subagents", whose ParentID is the task. That way it nests under the
// task as a child session and its tokens roll up into the task's totals.
//
// Roo Code additionally stores apiProtocol on the message itself; neither
// current Cline nor current Roo writes modelInfo, so the model usually comes
// from the last <environment_details><model>…</model></environment_details>
// block of api_conversation_history.json or from task_metadata.json's
// model_usage list.
//
// # Standalone Cline CLI session format
//
//	~/.cline/data/sessions/<sessionId>/
//	    <sessionId>.messages.json   JSON object {version, updated_at, agent,
//	                                sessionId, origin{…}, messages[…]}
//	    <sessionId>.json            JSON manifest (provider, model, cwd, …)
//
// Only assistant messages carrying metrics are counted. The same
// whole-file-rewrite cursor rules apply.
package clinetask

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar/mytoken/internal/harness"
	"github.com/zzstar/mytoken/internal/model"
)

// Extension ids whose globalStorage/tasks directories hold the task format.
const (
	ExtensionIDCline = "saoudrizwan.claude-dev"
	ExtensionIDRoo   = "rooveterinaryinc.roo-cline"
	ExtensionIDKilo  = "kilocode.kilo-code"
)

// File names inside a task directory.
const (
	FileUIMessages   = "ui_messages.json"
	FileHistory      = "api_conversation_history.json"
	FileTaskMetadata = "task_metadata.json"
	FileHistoryItem  = "history_item.json"
)

// Source kind produced by Discover. Both formats are whole-file JSON documents,
// so the kind is harness.Source's "json"; which of the two a Source is can be
// read off the file name, and the cursor's Extra field records it alongside the
// emitted-entry counter.
const (
	KindJSON = "json"

	// cliMessagesSuffix marks a Cline standalone CLI session messages file.
	cliMessagesSuffix = ".messages.json"
)

// vscodeApps are the VS Code family application directories that share the
// <app>/User/globalStorage layout.
var vscodeApps = []string{"Code", "Code - Insiders", "Cursor", "Windsurf", "VSCodium", "Trae"}

// clineFamilyExtensions are the extension ids that share the Cline task format.
// Discover uses them to keep a root that holds more than one of them (a whole
// globalStorage directory, say) from being attributed to the wrong harness.
var clineFamilyExtensions = []string{
	ExtensionIDCline,
	ExtensionIDRoo,
	ExtensionIDKilo,
}

// Options configures Parse for one harness.
type Options struct {
	Harness model.Harness

	// ExtensionID is the VS Code extension id this parser belongs to. Discover
	// uses it to skip the task directories of the other Cline-family
	// extensions when a root holds more than one of them.
	ExtensionID string

	// InputIncludesCache reports whether the tokensIn field of an
	// api_req_started payload already contains the cached tokens reported in
	// its cacheReads/cacheWrites fields, in which case they are subtracted
	// (saturating at zero) so the input bucket never double counts.
	//
	// Cline writes a disjoint tokensIn — see apps/vscode/src/sdk/
	// message-translator.ts normalizeUsageEvent: "Classic Cline/webview metrics
	// expect tokensIn, cacheReads, and cacheWrites to be disjoint buckets" —
	// except for OpenAI-style providers, whose raw prompt_tokens already
	// include the cached tokens (src/api/providers/openai-native.ts: "Keep
	// inputTokens as TOTAL input to preserve correct context length").
	//
	// Roo Code and Kilo Code write the total input for both protocols — see
	// Roo's packages/core/src/message-utils/consolidateTokenUsage.ts: "Since
	// tokensIn now stores TOTAL input tokens (including cache tokens), we no
	// longer need to add cacheWrites and cacheReads separately. This applies to
	// both Anthropic and OpenAI protocols."
	InputIncludesCache func(protocol, providerID, modelID string) bool

	// Sessions also discovers standalone Cline CLI session files
	// (<sessionId>.messages.json) under the walked root.
	Sessions bool
}

// VSCodeTaskDirs returns the tasks directory of extID for every VS Code family
// application installed on this operating system.
func VSCodeTaskDirs(extID string) []string {
	var out []string
	add := func(globalStorage string) {
		if globalStorage == "" {
			return
		}
		out = append(out, filepath.Join(globalStorage, extID, "tasks"))
	}
	switch goos := runtimeGOOS(); goos {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			for _, app := range vscodeApps {
				add(filepath.Join(appData, app, "User", "globalStorage"))
			}
		}
	case "darwin":
		if home := homeDir(); home != "" {
			for _, app := range vscodeApps {
				add(filepath.Join(home, "Library", "Application Support", app, "User", "globalStorage"))
			}
		}
	default:
		if home := homeDir(); home != "" {
			for _, app := range vscodeApps {
				add(filepath.Join(home, ".config", app, "User", "globalStorage"))
			}
		}
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			for _, app := range vscodeApps {
				add(filepath.Join(xdg, app, "User", "globalStorage"))
			}
		}
	}
	return out
}

// ClineDataDirs mirrors Cline's own resolution order for the standalone build:
// $CLINE_DATA_DIR, else $CLINE_DIR/data, else ~/.cline/data.
func ClineDataDirs() []string {
	if v := strings.TrimSpace(os.Getenv("CLINE_DATA_DIR")); v != "" {
		return []string{v}
	}
	base := strings.TrimSpace(os.Getenv("CLINE_DIR"))
	if base == "" {
		if home := homeDir(); home != "" {
			base = filepath.Join(home, ".cline")
		}
	}
	if base == "" {
		return nil
	}
	return []string{filepath.Join(base, "data")}
}

// ClineSessionDirs mirrors resolveSessionDataDir(): $CLINE_SESSION_DATA_DIR,
// else $CLINE_DATA_DIR/sessions, else $CLINE_DIR/data/sessions, else
// ~/.cline/data/sessions.
func ClineSessionDirs() []string {
	if v := strings.TrimSpace(os.Getenv("CLINE_SESSION_DATA_DIR")); v != "" {
		return []string{v}
	}
	dirs := ClineDataDirs()
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, filepath.Join(d, "sessions"))
	}
	return out
}

// Discover walks root and returns every task file it contains, sorted by path.
// Unreadable directories are skipped rather than reported.
func Discover(root string, opts Options) []harness.Source {
	return DiscoverWithContext(context.Background(), root, opts)
}

// DiscoverWithContext is Discover with cancellation. A cancelled context stops
// the walk and returns whatever was collected so far, which the caller treats as
// a partial scan rather than an error.
func DiscoverWithContext(ctx context.Context, root string, opts Options) []harness.Source {
	if root == "" {
		return nil
	}
	var out []harness.Source
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case d.Name() == FileUIMessages:
			if sameFamily(path, opts.ExtensionID) {
				out = append(out, harness.Source{Path: path, Kind: KindJSON})
			}
		case opts.Sessions && strings.HasSuffix(d.Name(), cliMessagesSuffix):
			if sameFamily(path, opts.ExtensionID) {
				out = append(out, harness.Source{Path: path, Kind: KindJSON})
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// sameFamily reports whether path may belong to the extension identified by
// want. It returns false only when the path positively names a DIFFERENT
// Cline-family extension, so a root that holds all three (a whole
// globalStorage directory, or a tasks directory shared by a fork) does not leak
// another harness's tasks into this one, while a path that names none of them
// (a bare tasks directory, or the standalone CLI's sessions directory) is still
// accepted because there is nothing to disambiguate.
func sameFamily(path, want string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == want {
			continue
		}
		for _, ext := range clineFamilyExtensions {
			if part == ext {
				return false
			}
		}
	}
	return true
}

// Parse reads one task file. It re-parses the whole file whenever its
// fingerprint, size or mtime changed and skips the api_req_started entries the
// cursor already recorded as emitted, which is what makes the incremental
// rescan of a rewritten-in-place JSON file emit only new events.
func Parse(src harness.Source, cur harness.Cursor, opts Options) (harness.Batch, error) {
	st, err := os.Stat(src.Path)
	if err != nil {
		// A task file that vanished between Discover and Parse is not an error.
		return harness.Batch{Next: cur}, nil
	}
	fp := fingerprint(src.Path)
	if unchanged(cur, st, fp) {
		return harness.Batch{Next: cur}, nil
	}
	data, err := os.ReadFile(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}

	var (
		events   []model.UsageEvent
		sessions []model.SessionMeta
		emitted  int
	)
	if strings.HasSuffix(src.Path, cliMessagesSuffix) {
		events, sessions, emitted = parseCLISession(src.Path, data, emittedCount(cur.Extra), opts)
	} else {
		events, sessions, emitted = parseTask(src.Path, data, emittedCount(cur.Extra), opts)
	}

	next := harness.Cursor{
		Offset:      st.Size(),
		Size:        st.Size(),
		ModTime:     st.ModTime(),
		Fingerprint: fp,
		Extra:       fmt.Sprintf("%s:%d", src.Kind, emitted),
	}
	return harness.Batch{Events: events, Sessions: sessions, Next: next}, nil
}

// ---------------------------------------------------------------------------
// VS Code task format
// ---------------------------------------------------------------------------

// uiEntry is one element of ui_messages.json (Cline's ClineMessage).
type uiEntry struct {
	TS        tsVal  `json:"ts"`
	Type      string `json:"type"`
	Say       string `json:"say"`
	Ask       string `json:"ask"`
	Text      string `json:"text"`
	APIProto  string `json:"apiProtocol"`
	ModelInfo *struct {
		ProviderID string `json:"providerId"`
		ModelID    string `json:"modelId"`
	} `json:"modelInfo"`
}

func (e uiEntry) modelID() string {
	if e.ModelInfo == nil {
		return ""
	}
	return strings.TrimSpace(e.ModelInfo.ModelID)
}

func (e uiEntry) providerID() string {
	if e.ModelInfo == nil {
		return ""
	}
	return strings.TrimSpace(e.ModelInfo.ProviderID)
}

func (e uiEntry) isAPIReq() bool { return e.Type == "say" && e.Say == "api_req_started" }

// isSubagentUsage reports whether the entry is the aggregate usage snapshot of
// a task's spawn_agent subagents.
func (e uiEntry) isSubagentUsage() bool { return e.Type == "say" && e.Say == "subagent_usage" }

// isDeletedAPIReqs reports whether the entry is the aggregate of the requests a
// checkpoint restore removed from this task.
func (e uiEntry) isDeletedAPIReqs() bool { return e.Type == "say" && e.Say == "deleted_api_reqs" }

// isUsage reports whether the entry carries request usage, which is the set of
// say kinds Cline's own getApiMetrics sums.
func (e uiEntry) isUsage() bool {
	return e.isAPIReq() || e.isSubagentUsage() || e.isDeletedAPIReqs()
}

// isTaskStart reports whether the entry carries the task prompt, which Cline
// writes as say:"task" for the first user message of a task (see
// apps/vscode/src/sdk/message-translator.ts: `say: clineMessages.length === 0
// ? "task" : "user_feedback"`).
func (e uiEntry) isTaskStart() bool { return e.Say == "task" || e.Ask == "task" }

// apiReqPayload is the JSON-encoded text of an api_req_started entry (Cline's
// ClineApiReqInfo).
type apiReqPayload struct {
	TokensIn    *num   `json:"tokensIn"`
	TokensOut   *num   `json:"tokensOut"`
	CacheWrites *num   `json:"cacheWrites"`
	CacheReads  *num   `json:"cacheReads"`
	Cost        *fnum  `json:"cost"`
	APIProtocol string `json:"apiProtocol"`
}

// historyItem is Roo's / Kilo's history_item.json record. Cline has no such
// file, so every field is optional there.
type historyItem struct {
	ID           string `json:"id"`
	RootTaskID   string `json:"rootTaskId"`
	ParentTaskID string `json:"parentTaskId"`
	TS           *tsVal `json:"ts"`
	Task         string `json:"task"`
	Workspace    string `json:"workspace"`
	Mode         string `json:"mode"`
}

// modelUsageEntry is one element of Cline's task_metadata.json model_usage.
type modelUsageEntry struct {
	TS              *num   `json:"ts"`
	ModelID         string `json:"model_id"`
	ModelProviderID string `json:"model_provider_id"`
	Mode            string `json:"mode"`
}

func parseTask(path string, data []byte, emitted int, opts Options) ([]model.UsageEvent, []model.SessionMeta, int) {
	taskID := filepath.Base(filepath.Dir(path))
	entries := decodeUIMessages(data)
	dir := filepath.Dir(path)

	// Sibling files supply the model / provider fallbacks and the session
	// metadata. They are read best-effort: a task directory that is still being
	// written may not have them yet.
	histModel, histWorkspace := readHistoryMeta(filepath.Join(dir, FileHistory))
	metaModel, metaProvider := readTaskMetadata(filepath.Join(dir, FileTaskMetadata))
	item := readHistoryItem(filepath.Join(dir, FileHistoryItem))
	parentID := taskParentID(taskID, item.ParentTaskID, item.RootTaskID)

	// Cline writes an api_req_started entry as a bare {apiProtocol} placeholder
	// and later rewrites the very same array element with the usage payload
	// (src/core/task/Task.ts: `JSON.stringify({apiProtocol})` then the update in
	// the same message). The counter therefore only counts entries that
	// actually carried usage, so a placeholder that is later filled in is still
	// emitted on the next scan. The same rule covers subagent_usage and
	// deleted_api_reqs: both are written once, in final form.
	var (
		events  []model.UsageEvent
		title   string
		first   time.Time
		last    time.Time
		seen    int
		subFrom time.Time
		subTo   time.Time
	)
	for _, e := range entries {
		if ts := e.TS.Time(); !ts.IsZero() {
			if first.IsZero() || ts.Before(first) {
				first = ts
			}
			if ts.After(last) {
				last = ts
			}
		}
		if title == "" && e.isTaskStart() {
			title = harness.Title(e.Text)
		}
		if !e.isUsage() {
			continue
		}
		ev, ok := buildEvent(opts, taskID, parentID, e, histModel, metaModel, metaProvider)
		if !ok {
			continue
		}
		if e.isSubagentUsage() {
			if subFrom.IsZero() || ev.Timestamp.Before(subFrom) {
				subFrom = ev.Timestamp
			}
			if ev.Timestamp.After(subTo) {
				subTo = ev.Timestamp
			}
		}
		seen++
		if seen <= emitted {
			continue
		}
		events = append(events, ev)
	}

	if first.IsZero() && item.TS != nil {
		first = item.TS.Time()
	}
	if last.IsZero() {
		last = first
	}

	project := firstNonEmpty(item.Workspace, histWorkspace)
	session := model.SessionMeta{
		Harness:   opts.Harness,
		SessionID: taskID,
		ParentID:  parentID,
		Title:     firstNonEmpty(title, harness.Title(item.Task)),
		Project:   project,
		StartedAt: first,
		UpdatedAt: last,
	}
	// buildEvent already stamped the session identity — the task itself for
	// api_req_started and deleted_api_reqs, the synthetic subagent session for
	// subagent_usage — so only the project is filled in here. A consumer that
	// only looks at UsageEvent still sees which session, subagent tree and
	// project a request belongs to.
	for i := range events {
		events[i].ProjectPath = project
	}
	sessions := []model.SessionMeta{session}
	if !subFrom.IsZero() {
		sessions = append(sessions, model.SessionMeta{
			Harness:   opts.Harness,
			SessionID: subagentSessionID(taskID),
			ParentID:  taskID,
			Title:     subagentSessionTitle,
			Project:   project,
			StartedAt: subFrom,
			UpdatedAt: subTo,
		})
	}
	return events, sessions, seen
}

// subagentSessionSuffix names the synthetic session that carries a task's
// subagent usage. Cline's spawn_agent subagents run inside the task's own
// process and are never persisted as sessions of their own — their SDK ids are
// "<rootSessionId>__<agentId>" (sdk/packages/core/src/session/models/
// session-graph.ts, makeSubSessionId) and only the aggregate say:"subagent_usage"
// message reaches ui_messages.json — so the task is the only stable identity
// left. One child session per task keeps the ids stable across scans and the
// session tree shallow.
const subagentSessionSuffix = ":subagents"

// subagentSessionTitle is the title of that synthetic session.
const subagentSessionTitle = "Subagents"

// subagentSessionID is the session id a task's subagent usage belongs to.
func subagentSessionID(taskID string) string { return taskID + subagentSessionSuffix }

// buildEvent turns one usage-carrying entry into an event, stamping the session
// it belongs to. api_req_started and deleted_api_reqs belong to the task
// itself; subagent_usage belongs to the task's synthetic subagent session, so
// the usage nests under the task in the session tree instead of inflating the
// task's own request count.
func buildEvent(opts Options, taskID, parentID string, e uiEntry, histModel, metaModel, metaProvider string) (model.UsageEvent, bool) {
	var p apiReqPayload
	if err := json.Unmarshal([]byte(e.Text), &p); err != nil {
		// Malformed payload: skip this request, the next one still parses.
		return model.UsageEvent{}, false
	}
	if p.TokensIn == nil && p.TokensOut == nil && p.CacheReads == nil && p.CacheWrites == nil {
		// Placeholder written before the request streamed anything, or a
		// request that was cancelled with no usage at all.
		return model.UsageEvent{}, false
	}
	ts := e.TS.Time()
	if ts.IsZero() {
		return model.UsageEvent{}, false
	}

	sessionID, parent := taskID, parentID
	if e.isSubagentUsage() {
		sessionID, parent = subagentSessionID(taskID), taskID
	}

	protocol := strings.TrimSpace(p.APIProtocol)
	modelID := firstNonEmpty(e.modelID(), histModel, metaModel)
	// The provider the request actually ran on: the payload's apiProtocol, else
	// the entry's modelInfo, else task_metadata.json's model_usage. The
	// input-token rule has to see this resolved value, because a task with no
	// modelInfo at all still knows its provider from task_metadata.json.
	providerID := firstNonEmpty(e.providerID(), metaProvider)

	tokensIn, tokensOut := int64(0), int64(0)
	var cacheReads, cacheWrites int64
	if p.TokensIn != nil {
		tokensIn = int64(*p.TokensIn)
	}
	if p.TokensOut != nil {
		tokensOut = int64(*p.TokensOut)
	}
	if p.CacheReads != nil {
		cacheReads = int64(*p.CacheReads)
	}
	if p.CacheWrites != nil {
		cacheWrites = int64(*p.CacheWrites)
	}
	if opts.InputIncludesCache != nil && opts.InputIncludesCache(protocol, providerID, modelID) {
		tokensIn -= cacheReads + cacheWrites
		if tokensIn < 0 {
			tokensIn = 0
		}
	}

	ev := model.UsageEvent{
		Harness:   opts.Harness,
		DedupKey:  fmt.Sprintf("%s:%s:%d", opts.Harness, sessionID, ts.UnixMilli()),
		SessionID: sessionID,
		ParentID:  parent,
		Timestamp: ts.UTC(),
		Model:     modelID,
		Provider:  firstNonEmpty(protocol, providerID),
		Tokens: model.Tokens{
			Input:      tokensIn,
			Output:     tokensOut,
			CacheRead:  cacheReads,
			CacheWrite: cacheWrites,
		},
	}
	if p.Cost != nil {
		c := float64(*p.Cost)
		ev.CostUSD = &c
	}
	return ev, true
}

func decodeUIMessages(data []byte) []uiEntry {
	var entries []uiEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		// Not an array (truncated write, legacy object shape): nothing to read.
		return nil
	}
	return entries
}

var (
	envDetailsRE = regexp.MustCompile(`(?s)<environment_details>(.*?)</environment_details>`)
	modelTagRE   = regexp.MustCompile(`(?s)<model>(.*?)</model>`)
	// The workspace banner only carries line anchors in the source string; in
	// api_conversation_history.json the newlines are JSON escapes, so the match
	// deliberately does not depend on them.
	workspaceRE = regexp.MustCompile(`# Current Workspace Directory \(([^)]*)\) Files`)
)

// readHistoryMeta scans api_conversation_history.json for the last
// <environment_details> block and returns its <model> value plus the workspace
// directory advertised by that block.
func readHistoryMeta(path string) (modelName, workspace string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	text := string(data)
	blocks := envDetailsRE.FindAllStringSubmatch(text, -1)
	for i := len(blocks) - 1; i >= 0; i-- {
		if m := modelTagRE.FindStringSubmatch(blocks[i][1]); m != nil {
			modelName = strings.TrimSpace(m[1])
			break
		}
	}
	ws := workspaceRE.FindAllStringSubmatch(text, -1)
	for i := len(ws) - 1; i >= 0; i-- {
		if v := strings.TrimSpace(ws[i][1]); v != "" {
			workspace = v
			break
		}
	}
	return modelName, workspace
}

func readTaskMetadata(path string) (modelName, providerID string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var meta struct {
		ModelUsage []modelUsageEntry `json:"model_usage"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", ""
	}
	// model_usage is append-ordered; the last entry is the model in use when
	// the task was last written.
	for i := len(meta.ModelUsage) - 1; i >= 0; i-- {
		if v := strings.TrimSpace(meta.ModelUsage[i].ModelID); v != "" {
			modelName = v
			providerID = strings.TrimSpace(meta.ModelUsage[i].ModelProviderID)
			break
		}
	}
	return modelName, providerID
}

// taskParentID resolves the parent of a task from history_item.json.
// parentTaskId is the DIRECT parent and rootTaskId the root of the subtree. A
// root task records itself as its own rootTaskId, so a link that points back at
// the task is not a parent at all and would put a cycle in the session tree.
func taskParentID(taskID, parentTaskID, rootTaskID string) string {
	if v := strings.TrimSpace(parentTaskID); v != "" && v != taskID {
		return v
	}
	if v := strings.TrimSpace(rootTaskID); v != "" && v != taskID {
		return v
	}
	return ""
}

func readHistoryItem(path string) historyItem {
	var item historyItem
	data, err := os.ReadFile(path)
	if err != nil {
		return item
	}
	_ = json.Unmarshal(data, &item)
	return item
}

// ---------------------------------------------------------------------------
// Standalone Cline CLI session format
// ---------------------------------------------------------------------------

type cliSessionFile struct {
	Version   *int         `json:"version"`
	UpdatedAt json.Number  `json:"updated_at"`
	Agent     string       `json:"agent"`
	SessionID string       `json:"sessionId"`
	TaskType  string       `json:"taskType"`
	Origin    *cliOrigin   `json:"origin"`
	Messages  []cliMessage `json:"messages"`
}

type cliOrigin struct {
	Source         string `json:"source"`
	Mode           string `json:"mode"`
	SessionID      string `json:"sessionId"`
	ParentThreadID string `json:"parentThreadId"`
	Subagent       string `json:"subagent"`
}

type cliMessage struct {
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CreatedAt *tsVal          `json:"createdAt"`
	TS        *tsVal          `json:"ts"`
	ModelInfo *cliModelInfo   `json:"modelInfo"`
	Metrics   *cliMetrics     `json:"metrics"`
}

type cliModelInfo struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Family   string `json:"family"`
}

type cliMetrics struct {
	InputTokens      *num  `json:"inputTokens"`
	OutputTokens     *num  `json:"outputTokens"`
	CacheReadTokens  *num  `json:"cacheReadTokens"`
	CacheWriteTokens *num  `json:"cacheWriteTokens"`
	Cost             *fnum `json:"cost"`
}

type cliManifest struct {
	SessionID     string         `json:"session_id"`
	Source        string         `json:"source"`
	StartedAt     string         `json:"started_at"`
	EndedAt       string         `json:"ended_at"`
	Status        string         `json:"status"`
	Provider      string         `json:"provider"`
	Model         string         `json:"model"`
	Cwd           string         `json:"cwd"`
	WorkspaceRoot string         `json:"workspace_root"`
	Prompt        string         `json:"prompt"`
	Metadata      map[string]any `json:"metadata"`
}

func parseCLISession(path string, data []byte, emitted int, opts Options) ([]model.UsageEvent, []model.SessionMeta, int) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	sessionID := strings.TrimSuffix(base, ".messages.json")

	var file cliSessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, nil, 0
	}
	manifest := readCLIManifest(filepath.Join(dir, sessionID+".json"))

	var (
		events []model.UsageEvent
		first  time.Time
		last   time.Time
		seen   int
	)
	for _, m := range file.Messages {
		ts := cliMessageTS(m)
		if !ts.IsZero() {
			if first.IsZero() || ts.Before(first) {
				first = ts
			}
			if ts.After(last) {
				last = ts
			}
		}
		if m.Role != "assistant" || m.Metrics == nil {
			continue
		}
		// Only assistant messages with a metrics block are requests.
		if m.Metrics.InputTokens == nil && m.Metrics.OutputTokens == nil &&
			m.Metrics.CacheReadTokens == nil && m.Metrics.CacheWriteTokens == nil {
			continue
		}
		if ts.IsZero() {
			continue
		}
		seen++
		if seen <= emitted {
			// Already reported on an earlier scan of this file.
			continue
		}
		modelID, providerID := "", ""
		if m.ModelInfo != nil {
			modelID = strings.TrimSpace(m.ModelInfo.ID)
			providerID = strings.TrimSpace(m.ModelInfo.Provider)
		}
		if modelID == "" {
			modelID = manifest.Model
		}
		if providerID == "" {
			providerID = manifest.Provider
		}

		var in, out, cacheRead, cacheWrite int64
		if m.Metrics.InputTokens != nil {
			in = int64(*m.Metrics.InputTokens)
		}
		if m.Metrics.OutputTokens != nil {
			out = int64(*m.Metrics.OutputTokens)
		}
		if m.Metrics.CacheReadTokens != nil {
			cacheRead = int64(*m.Metrics.CacheReadTokens)
		}
		if m.Metrics.CacheWriteTokens != nil {
			cacheWrite = int64(*m.Metrics.CacheWriteTokens)
		}
		// Cline's SDK documents metrics.inputTokens as the full request size
		// with cache reads/writes included, so the input bucket is what remains
		// after subtracting them (saturating at zero).
		in -= cacheRead + cacheWrite
		if in < 0 {
			in = 0
		}

		ev := model.UsageEvent{
			Harness:   opts.Harness,
			DedupKey:  fmt.Sprintf("cline-cli:%s:%s", sessionID, cliMessageKey(m, seen)),
			SessionID: sessionID,
			Timestamp: ts.UTC(),
			Model:     modelID,
			Provider:  providerID,
			Tokens: model.Tokens{
				Input:      in,
				Output:     out,
				CacheRead:  cacheRead,
				CacheWrite: cacheWrite,
			},
		}
		if m.Metrics.Cost != nil {
			c := float64(*m.Metrics.Cost)
			ev.CostUSD = &c
		}
		events = append(events, ev)
	}

	parentID := ""
	if file.Origin != nil {
		parentID = strings.TrimSpace(file.Origin.ParentThreadID)
	}
	project := firstNonEmpty(manifest.WorkspaceRoot, manifest.Cwd)
	session := model.SessionMeta{
		Harness:   opts.Harness,
		SessionID: sessionID,
		ParentID:  parentID,
		Title:     cliTitle(manifest, file.Messages),
		Project:   project,
		StartedAt: first,
		UpdatedAt: last,
	}
	for i := range events {
		events[i].SessionID = sessionID
		events[i].ParentID = parentID
		events[i].ProjectPath = project
	}
	return events, []model.SessionMeta{session}, seen
}

func cliMessageTS(m cliMessage) time.Time {
	if m.TS != nil {
		return m.TS.Time()
	}
	if m.CreatedAt != nil {
		return m.CreatedAt.Time()
	}
	return time.Time{}
}

// cliMessageKey is the stable identity of a request inside a session file: the
// message id when present, else the ordinal of the assistant message.
func cliMessageKey(m cliMessage, ordinal int) string {
	if id := strings.TrimSpace(m.ID); id != "" {
		return id
	}
	return "assistant-" + strconv.Itoa(ordinal)
}

func cliTitle(manifest cliManifest, messages []cliMessage) string {
	if t, ok := manifest.Metadata["title"].(string); ok {
		if s := harness.Title(t); s != "" {
			return s
		}
	}
	if s := harness.Title(manifest.Prompt); s != "" {
		return s
	}
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		if s := harness.Title(cliText(m.Content)); s != "" {
			return s
		}
	}
	return ""
}

func cliText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" && blk.Text != "" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

func readCLIManifest(path string) cliManifest {
	var m cliManifest
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// fingerprint hashes the first 4 KiB of a file, the same window the other
// parsers in this repository use to detect truncation or replacement.
func fingerprint(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.CopyN(h, f, 4096); err != nil && err != io.EOF {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func unchanged(cur harness.Cursor, st os.FileInfo, fp string) bool {
	return cur.Fingerprint != "" && cur.Fingerprint == fp &&
		cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime())
}

// emittedCount reads the api_req_started counter out of a cursor Extra value of
// the form "<kind>:<count>".
func emittedCount(extra string) int {
	i := strings.LastIndex(extra, ":")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(extra[i+1:])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
