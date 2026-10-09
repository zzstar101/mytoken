// Package gemini parses Gemini CLI chat recordings (model.Harness "gemini").
//
// # On-disk layout
//
// gemini-cli keeps one file per conversation under the project's temp dir
// (packages/core/src/services/chatRecordingService.ts, storage.ts, paths.ts):
//
//	$GEMINI_CLI_HOME/.gemini/tmp/<projectId>/chats/session-<ts>-<shortId>.jsonl  main session
//	$GEMINI_CLI_HOME/.gemini/tmp/<projectId>/chats/<parentSessionId>/<id>.jsonl subagent
//	$GEMINI_CLI_HOME/.gemini/tmp/<projectId>/chats/session-<ts>-<shortId>.json   legacy main session
//	$GEMINI_CLI_HOME/.gemini/projects.json                                       path -> slug registry
//
// <projectId> is either the short slug registered in projects.json or, for
// recordings written before the slug migration, sha256(projectRoot) in hex.
// Subagent recordings are nested one directory deeper, under the complete
// parent session id ("subagents are nested under the complete parent session
// id", chatRecordingService.ts), which is the only parent link Gemini CLI
// writes — it is used as ParentID.
//
// Newer recordings are JSON Lines and are read incrementally by byte offset.
// A session that was resumed is migrated from the legacy single-object .json
// file to .jsonl (the .json is left in place), so Discover drops the .json when
// its migrated .jsonl sibling exists.
//
// # Records
//
// The first JSONL line is the ConversationRecord metadata
// {sessionId, projectHash, startTime, lastUpdated, summary?, directories?,
// kind?}; every later line is one of
//
//	MessageRecord        {id, timestamp, type:"user"|"gemini", content, tokens?, model?, ...}
//	{$set: {...}}        partial metadata update (frequently just lastUpdated)
//	{$patch: {...}}      message patch (tool results, content, thoughts)
//	{$rewindTo: "<id>"}  conversation rewound to a message id
//
// The legacy .json file is a single ConversationRecord with a `messages` array.
// Control records never change token usage, so they are folded into the
// incremental state (metadata fields) or ignored ($patch, $rewindTo); the file
// is still replayed from byte 0 whenever it shrinks or its fingerprint changes.
//
// # Token accounting
//
// A `gemini` message carries the usage metadata verbatim:
// {input=promptTokenCount, output=candidatesTokenCount, cached=cachedContentTokenCount,
// thoughts=thoughtsTokenCount, tool=toolUsePromptTokenCount, total=totalTokenCount}.
//
// promptTokenCount is cache-inclusive: the API counts the cached prefix inside
// the prompt. When the numbers add up inclusively (total == input+output+thoughts+tool
// and total != that plus cached) the cached part is subtracted from input so the
// input/cacheRead classes never double count; otherwise the values are already
// net. toolUsePromptTokenCount is folded into Input (same rule as tokscale).
// Reasoning is thoughtsTokenCount; Gemini CLI never reports a cache write.
//
// # Dedup and sessions
//
// DedupKey is "gemini:<sessionId>:<messageId>". gemini-cli re-appends a message
// with the same id whenever its usage metadata or tool calls arrive, so a key
// can appear several times in one file; the accumulator keeps the largest value
// per token class, and the store's last-write-wins upsert makes rescanning
// idempotent.
//
// Title is the first non-plumbing user message (message bodies are otherwise
// never decoded), falling back to the recording's own summary.
package gemini

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar/mytoken/internal/harness"
	"github.com/zzstar/mytoken/internal/model"
)

const (
	fingerprintBytes = 4096
	readBuffer       = 64 << 10

	kindJSON  = "json"
	kindJSONL = "jsonl"

	chatsDirName        = "chats"
	tmpDirName          = "tmp"
	projectRegistryFile = "projects.json"
)

// Parser reads Gemini CLI chat recordings.
type Parser struct {
	roots []string
}

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

// New returns a parser rooted at the default Gemini CLI directories.
func New() *Parser { return NewWithRoots() }

// NewWithRoot returns a parser reading only root.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser reading the given roots. With no roots the
// default (GEMINI_CLI_HOME or ~/.gemini) is used.
func NewWithRoots(roots ...string) *Parser {
	if len(roots) == 0 {
		roots = defaultRoots()
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if r == "" {
			continue
		}
		r = filepath.Clean(r)
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return &Parser{roots: out}
}

// defaultRoots returns the tmp directories of the Gemini CLI home. gemini-cli's
// homedir() returns $GEMINI_CLI_HOME, so the data dir is $GEMINI_CLI_HOME/.gemini;
// tokscale documents GEMINI_CLI_HOME as the data dir itself, so both are read.
func defaultRoots() []string {
	if v := os.Getenv("MYTOKEN_GEMINI_DIRS"); v != "" {
		return filepath.SplitList(v)
	}
	if h := os.Getenv("GEMINI_CLI_HOME"); h != "" {
		return []string{
			filepath.Join(h, ".gemini", tmpDirName),
			filepath.Join(h, tmpDirName),
		}
	}
	return []string{filepath.Join(harness.EnvOr("GEMINI_CLI_HOME", ".gemini"), tmpDirName)}
}

// Harness implements harness.Parser.
func (p *Parser) Harness() model.Harness { return model.Gemini }

// Roots implements harness.Parser.
func (p *Parser) Roots() []string { return p.roots }

// ---------------------------------------------------------------------------
// discovery
// ---------------------------------------------------------------------------

// Discover walks every root for chat recordings. Only files directly inside a
// "chats" directory (main sessions) or one level below it (subagents) are read.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	var out []harness.Source
	seen := map[string]bool{}
	for _, root := range p.roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if e := ctx.Err(); e != nil {
				return e
			}
			if d.IsDir() || !isChatFile(path) {
				return nil
			}
			if !seen[path] {
				seen[path] = true
				out = append(out, harness.Source{Path: path, Kind: kindOf(path)})
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	// A resumed session is migrated to .jsonl while the legacy .json stays
	// behind; the .jsonl is a superset, so keep only it.
	jsonl := map[string]bool{}
	for _, s := range out {
		if s.Kind == kindJSONL {
			jsonl[s.Path] = true
		}
	}
	kept := out[:0]
	for _, s := range out {
		if s.Kind == kindJSON && jsonl[s.Path+"l"] {
			continue
		}
		kept = append(kept, s)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Path < kept[j].Path })
	return kept, nil
}

// isChatFile reports whether path is a recording: <...>/chats/<file> or
// <...>/chats/<parentSessionId>/<file>.
func isChatFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".json" && ext != ".jsonl" {
		return false
	}
	dir := filepath.Dir(path)
	if filepath.Base(dir) == chatsDirName {
		return true
	}
	return filepath.Base(filepath.Dir(dir)) == chatsDirName
}

func kindOf(path string) string {
	if strings.HasSuffix(path, ".jsonl") {
		return kindJSONL
	}
	return kindJSON
}

// ---------------------------------------------------------------------------
// parsing
// ---------------------------------------------------------------------------

// Parse reads src incrementally from cur.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	st, err := os.Stat(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	fp, err := fingerprint(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	if unchanged(cur, st, fp) {
		return harness.Batch{Next: cur}, nil
	}
	if src.Kind == kindJSONL {
		return p.parseJSONL(ctx, src, cur, st, fp)
	}
	return p.parseJSON(src, st, fp)
}

// parseJSON reads a legacy single-object recording in full. The whole file is
// decoded on every change; DedupKey makes the replayed events idempotent.
func (p *Parser) parseJSON(src harness.Source, st os.FileInfo, fp string) (harness.Batch, error) {
	raw, err := os.ReadFile(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	acc := newAccumulator(src, st.ModTime())
	var rec conversationRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		// A corrupt or partially written recording is retried next scan: the
		// cursor still moves to the current size, and any further write
		// changes size/mtime/fingerprint again.
		return harness.Batch{Next: cursorFor(st, fp, st.Size(), "")}, nil
	}
	acc.metadata(&rec)
	for _, m := range rec.Messages {
		if err := acc.message(&m); err != nil {
			return harness.Batch{}, err
		}
	}
	next := cursorFor(st, fp, st.Size(), "")
	return harness.Batch{Events: acc.events(), Sessions: acc.sessions(p), Next: next}, nil
}

// parseJSONL appends to the byte-offset cursor. A file whose fingerprint
// changed, that shrank, or whose size is below the stored offset is replayed
// from byte 0.
func (p *Parser) parseJSONL(ctx context.Context, src harness.Source, cur harness.Cursor, st os.FileInfo, fp string) (harness.Batch, error) {
	acc := newAccumulator(src, st.ModTime())
	off := int64(0)
	if cur.Fingerprint == fp && cur.Offset <= st.Size() && cur.Size <= st.Size() {
		off = cur.Offset
		if cur.Extra != "" {
			_ = json.Unmarshal([]byte(cur.Extra), &acc.state)
		}
	}
	f, err := os.Open(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	defer f.Close()
	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return harness.Batch{}, err
		}
	}
	n, err := readCompleteLines(f, func(line []byte) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		return acc.line(line)
	})
	if err != nil {
		return harness.Batch{}, err
	}
	extra, err := json.Marshal(acc.state)
	if err != nil {
		return harness.Batch{}, err
	}
	next := cursorFor(st, fp, off+n, string(extra))
	return harness.Batch{Events: acc.events(), Sessions: acc.sessions(p), Next: next}, nil
}

func cursorFor(st os.FileInfo, fp string, off int64, extra string) harness.Cursor {
	return harness.Cursor{
		Offset:      off,
		Size:        st.Size(),
		ModTime:     st.ModTime(),
		Fingerprint: fp,
		Extra:       extra,
	}
}

// ---------------------------------------------------------------------------
// record shapes
// ---------------------------------------------------------------------------

// conversationRecord is the metadata envelope of one recording, and also the
// target of a `{$set: ...}` partial update.
type conversationRecord struct {
	SessionID   string          `json:"sessionId"`
	ProjectHash string          `json:"projectHash"`
	StartTime   string          `json:"startTime"`
	LastUpdated string          `json:"lastUpdated"`
	Summary     string          `json:"summary"`
	Directories []string        `json:"directories"`
	Kind        string          `json:"kind"`
	Messages    []messageRecord `json:"messages"`
}

// messageRecord is one `user` or `gemini` turn.
type messageRecord struct {
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Content   json.RawMessage `json:"content"`
	Tokens    *tokensSummary  `json:"tokens"`
	Model     string          `json:"model"`
}

// lineEnvelope classifies a JSONL line without decoding the payload.
type lineEnvelope struct {
	ID       *string          `json:"id"`
	Set      *json.RawMessage `json:"$set"`
	Patch    *json.RawMessage `json:"$patch"`
	RewindTo *string          `json:"$rewindTo"`
}

// tokensSummary is the `tokens` object gemini-cli writes verbatim from
// GenerateContentResponseUsageMetadata. The camelCase aliases are the raw API
// names accepted by tokscale's shared deserializer.
type tokensSummary struct {
	Input    num  `json:"input"`
	Output   num  `json:"output"`
	Cached   num  `json:"cached"`
	Thoughts num  `json:"thoughts"`
	Tool     num  `json:"tool"`
	Total    *num `json:"total"`

	Prompt                  num  `json:"prompt"`
	PromptTokens            num  `json:"prompt_tokens"`
	PromptTokenCount        num  `json:"promptTokenCount"`
	Candidates              num  `json:"candidates"`
	OutputTokens            num  `json:"output_tokens"`
	CandidatesTokenCount    num  `json:"candidatesTokenCount"`
	CachedTokens            num  `json:"cached_tokens"`
	CachedContentTokenCount num  `json:"cachedContentTokenCount"`
	Reasoning               num  `json:"reasoning"`
	ThoughtsTokens          num  `json:"thoughts_tokens"`
	ThoughtsTokenCount      num  `json:"thoughtsTokenCount"`
	ToolTokens              num  `json:"tool_tokens"`
	ToolUsePromptTokenCount num  `json:"toolUsePromptTokenCount"`
	TotalTokens             *num `json:"total_tokens"`
	TotalTokenCount         *num `json:"totalTokenCount"`
}

// classes maps the raw usage metadata onto the five token classes.
func (t *tokensSummary) classes() model.Tokens {
	in := clamp(pick(t.Input, t.Prompt, t.PromptTokens, t.PromptTokenCount))
	cached := clamp(pick(t.Cached, t.CachedTokens, t.CachedContentTokenCount))
	out := clamp(pick(t.Output, t.Candidates, t.OutputTokens, t.CandidatesTokenCount))
	reasoning := clamp(pick(t.Thoughts, t.Reasoning, t.ThoughtsTokens, t.ThoughtsTokenCount))
	tool := clamp(pick(t.Tool, t.ToolTokens, t.ToolUsePromptTokenCount))
	in, cached = normalizeSessionInputAndCache(in, cached, out, reasoning, tool,
		pickTotal(t.Total, t.TotalTokens, t.TotalTokenCount))
	return model.Tokens{
		Input:     in + tool,
		Output:    out,
		CacheRead: cached,
		Reasoning: reasoning,
	}
}

// normalizeSessionInputAndCache removes the cached prefix from an inclusive
// prompt count. It only subtracts when the reported total proves the prompt is
// cache-inclusive, so an already-net recording is left untouched.
func normalizeSessionInputAndCache(in, cached, out, reasoning, tool int64, total *int64) (int64, int64) {
	in, cached = clamp(in), clamp(cached)
	if total == nil {
		return in, cached
	}
	t := clamp(*total)
	inclusive := in + clamp(out) + clamp(reasoning) + clamp(tool)
	exclusive := inclusive + cached
	if cached > 0 && t == inclusive && t != exclusive {
		return in - min64(cached, in), cached
	}
	return in, cached
}

// ---------------------------------------------------------------------------
// incremental state
// ---------------------------------------------------------------------------

// jsonlState is the Cursor.Extra payload needed to keep parsing an append-only
// recording after its metadata line scrolls out of the window.
type jsonlState struct {
	SessionID   string   `json:"sid,omitempty"`
	ProjectHash string   `json:"ph,omitempty"`
	Kind        string   `json:"k,omitempty"`
	StartTime   string   `json:"st,omitempty"`
	LastUpdated string   `json:"lu,omitempty"`
	Summary     string   `json:"sm,omitempty"`
	Directories []string `json:"dirs,omitempty"`
	Title       string   `json:"t,omitempty"`
}

// ---------------------------------------------------------------------------
// session accumulation (one file == one session)
// ---------------------------------------------------------------------------

type accumulator struct {
	src      harness.Source
	parentID string
	modTime  time.Time
	state    jsonlState
	evs      map[string]model.UsageEvent
	order    []string
	firstTS  time.Time
	lastTS   time.Time

	registry       map[string]string
	registryLoaded bool
}

func newAccumulator(src harness.Source, modTime time.Time) *accumulator {
	return &accumulator{
		src:      src,
		parentID: parentFromPath(src.Path),
		modTime:  modTime,
		evs:      map[string]model.UsageEvent{},
		state:    jsonlState{SessionID: sessionIDFromPath(src.Path)},
	}
}

// line consumes one complete JSONL record.
func (a *accumulator) line(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	var env lineEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil // skip malformed records, keep the file moving
	}
	switch {
	case env.RewindTo != nil:
		// The conversation was rewound; the usage already happened, so the
		// events stay. The rewind can however truncate the file, which the
		// size/fingerprint guard in parseJSONL turns into a full replay.
		return nil
	case env.Patch != nil:
		// Patches only carry tool results and message text.
		return nil
	case env.Set != nil:
		var rec conversationRecord
		if err := json.Unmarshal(*env.Set, &rec); err != nil {
			return nil
		}
		a.metadata(&rec)
		return nil
	case env.ID != nil:
		var m messageRecord
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil
		}
		return a.message(&m)
	default:
		var rec conversationRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil
		}
		a.metadata(&rec)
		return nil
	}
}

// metadata folds a conversation record (or a `$set` partial update) into state.
func (a *accumulator) metadata(rec *conversationRecord) {
	if rec.SessionID != "" {
		a.state.SessionID = rec.SessionID
	}
	if rec.ProjectHash != "" {
		a.state.ProjectHash = rec.ProjectHash
	}
	if rec.Kind != "" {
		a.state.Kind = rec.Kind
	}
	if rec.StartTime != "" {
		a.state.StartTime = rec.StartTime
	}
	if rec.LastUpdated != "" {
		a.state.LastUpdated = rec.LastUpdated
	}
	if rec.Summary != "" {
		a.state.Summary = rec.Summary
	}
	if len(rec.Directories) > 0 {
		a.state.Directories = rec.Directories
	}
	// rec.Messages (a `$set` full replacement) is intentionally not replayed:
	// usage events are append-only and already keyed by message id.
}

// message turns one recording message into an event, and records the first
// user message as the session title.
func (a *accumulator) message(m *messageRecord) error {
	if m.Type == "user" && a.state.Title == "" {
		if txt := firstText(m.Content); txt != "" && !ignoredUserText(txt) {
			a.state.Title = harness.Title(txt)
		}
	}
	if m.Tokens == nil || m.Model == "" {
		return nil
	}
	tk := m.Tokens.classes()
	if tk.IsZero() {
		return nil
	}
	id := m.ID
	if id == "" {
		return nil // no stable identity => no stable DedupKey
	}
	sid := a.state.SessionID
	if sid == "" {
		sid = sessionIDFromPath(a.src.Path)
	}
	ts := parseTSString(m.Timestamp)
	if ts.IsZero() {
		ts = parseTSString(a.state.StartTime)
	}
	if ts.IsZero() {
		ts = a.modTime.UTC()
	}
	if a.firstTS.IsZero() || ts.Before(a.firstTS) {
		a.firstTS = ts
	}
	if a.lastTS.IsZero() || ts.After(a.lastTS) {
		a.lastTS = ts
	}
	ev := model.UsageEvent{
		Harness:     model.Gemini,
		DedupKey:    "gemini:" + sid + ":" + id,
		SessionID:   sid,
		ParentID:    a.parentID,
		ProjectPath: a.project(),
		Timestamp:   ts.UTC(),
		Model:       m.Model,
		Tokens:      tk,
	}
	a.add(ev)
	return nil
}

// add keeps the largest value per token class for a repeated DedupKey.
func (a *accumulator) add(ev model.UsageEvent) {
	if old, ok := a.evs[ev.DedupKey]; ok {
		ev.Tokens = maxTokens(old.Tokens, ev.Tokens)
		a.evs[ev.DedupKey] = ev
		return
	}
	a.evs[ev.DedupKey] = ev
	a.order = append(a.order, ev.DedupKey)
}

func (a *accumulator) events() []model.UsageEvent {
	out := make([]model.UsageEvent, 0, len(a.order))
	for _, k := range a.order {
		out = append(out, a.evs[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Timestamp.Equal(out[j].Timestamp) {
			return out[i].Timestamp.Before(out[j].Timestamp)
		}
		return out[i].DedupKey < out[j].DedupKey
	})
	return out
}

// sessions returns the metadata of this file's session, refreshed on every scan
// so that a late title or summary still lands.
func (a *accumulator) sessions(p *Parser) []model.SessionMeta {
	sid := a.state.SessionID
	if sid == "" {
		sid = sessionIDFromPath(a.src.Path)
	}
	started := parseTSString(a.state.StartTime)
	if started.IsZero() {
		started = a.firstTS
	}
	updated := parseTSString(a.state.LastUpdated)
	if updated.IsZero() {
		updated = a.lastTS
	}
	if started.IsZero() {
		started = updated
	}
	if updated.Before(started) {
		updated = started
	}
	title := a.state.Title
	if title == "" {
		title = harness.Title(a.state.Summary)
	}
	return []model.SessionMeta{{
		Harness:   model.Gemini,
		SessionID: sid,
		ParentID:  a.parentID,
		Title:     title,
		Project:   a.project(),
		StartedAt: started.UTC(),
		UpdatedAt: updated.UTC(),
	}}
}

// project resolves the workspace path: the recording's own `directories` first
// (only subagents record them), else the projects.json registry entry for this
// project id (a slug, or sha256(path) for pre-migration directories).
func (a *accumulator) project() string {
	for _, d := range a.state.Directories {
		if d != "" {
			return d
		}
	}
	if !a.registryLoaded {
		a.registryLoaded = true
		if base := geminiBase(a.src.Path); base != "" {
			a.registry = projectRegistry(base)
		}
	}
	return a.registry[projectIDFromPath(a.src.Path)]
}

// ---------------------------------------------------------------------------
// paths and the project registry
// ---------------------------------------------------------------------------

// parentFromPath returns the parent session id of a subagent recording, which
// gemini-cli encodes as the directory between "chats" and the file.
func parentFromPath(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(filepath.Dir(dir)) != chatsDirName {
		return ""
	}
	if filepath.Base(dir) == chatsDirName {
		return ""
	}
	return filepath.Base(dir)
}

func sessionIDFromPath(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// geminiBase returns the directory holding projects.json for a recording: the
// parent of the "tmp" directory the recording lives under.
func geminiBase(path string) string {
	dir := filepath.Dir(path)
	for {
		parent := filepath.Dir(dir)
		if parent == dir || parent == "." || parent == string(filepath.Separator) {
			return ""
		}
		if filepath.Base(dir) == tmpDirName {
			return parent
		}
		dir = parent
	}
}

// projectIDFromPath returns the tmp directory name of a recording: the path
// component directly below "tmp".
func projectIDFromPath(path string) string {
	dir := filepath.Dir(path)
	below := ""
	for {
		base := filepath.Base(dir)
		if base == tmpDirName {
			return below
		}
		parent := filepath.Dir(dir)
		if parent == dir || parent == "." || parent == string(filepath.Separator) {
			return ""
		}
		below = base
		dir = parent
	}
}

// projectRegistry maps tmp directory names onto workspace paths: gemini-cli's
// projects.json is {projects: {"<absolute path>": "<slug>"}} and pre-migration
// directories are named sha256(projectRoot) (storage.ts getFilePathHash).
func projectRegistry(base string) map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(base, projectRegistryFile))
	if err != nil {
		return out
	}
	var reg struct {
		Projects map[string]string `json:"projects"`
	}
	if json.Unmarshal(raw, &reg) != nil {
		return out
	}
	for path, slug := range reg.Projects {
		if path == "" {
			continue
		}
		if slug != "" {
			out[slug] = path
		}
		sum := sha256.Sum256([]byte(path))
		out[hex.EncodeToString(sum[:])] = path
	}
	return out
}

// ---------------------------------------------------------------------------
// text and timestamp helpers
// ---------------------------------------------------------------------------

// firstText returns the text of a PartListUnion: a bare string, a single part,
// or the concatenation of the text parts of a part array.
func firstText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	var one struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &one) == nil {
		return one.Text
	}
	return ""
}

// ignoredUserText mirrors gemini-cli's isIgnoredUserContent
// (packages/core/src/utils/sessionUtils.ts): slash commands, help prompts and
// injected context are not usable titles.
func ignoredUserText(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" ||
		strings.HasPrefix(s, "/") ||
		strings.HasPrefix(s, "?") ||
		strings.HasPrefix(s, "<session_context>") ||
		strings.HasPrefix(s, "<hook_context>")
}

func parseTSString(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return msTime(ms)
	}
	return time.Time{}
}

func msTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	if ms < 1e12 {
		ms *= 1000
	}
	return time.UnixMilli(ms).UTC()
}

func pick(vals ...num) int64 {
	for _, v := range vals {
		if v != 0 {
			return int64(v)
		}
	}
	return 0
}

func pickTotal(vals ...*num) *int64 {
	for _, v := range vals {
		if v != nil {
			n := int64(*v)
			return &n
		}
	}
	return nil
}

// num decodes a JSON number that may arrive as an integer, a float or a quoted
// string; null and absent values decode to 0.
type num int64

func (n *num) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err == nil {
		*n = num(f)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil
		}
		*n = num(v)
	}
	return nil
}

func clamp(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxTokens(a, b model.Tokens) model.Tokens {
	if b.Input > a.Input {
		a.Input = b.Input
	}
	if b.Output > a.Output {
		a.Output = b.Output
	}
	if b.CacheRead > a.CacheRead {
		a.CacheRead = b.CacheRead
	}
	if b.CacheWrite > a.CacheWrite {
		a.CacheWrite = b.CacheWrite
	}
	if b.Reasoning > a.Reasoning {
		a.Reasoning = b.Reasoning
	}
	return a
}

// ---------------------------------------------------------------------------
// file helpers
// ---------------------------------------------------------------------------

func unchanged(cur harness.Cursor, st os.FileInfo, fp string) bool {
	return cur.Fingerprint != "" && cur.Fingerprint == fp &&
		cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime())
}

// fingerprint returns the sha1 of the first fingerprintBytes bytes.
func fingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.CopyN(h, f, fingerprintBytes); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readCompleteLines feeds every '\n'-terminated line of r to fn and returns the
// number of bytes consumed. A trailing chunk without '\n' is never passed to fn
// and never counted, so a half-written record is left for the next scan.
func readCompleteLines(r io.Reader, fn func(line []byte) error) (int64, error) {
	br := bufio.NewReaderSize(r, readBuffer)
	var n int64
	for {
		line, err := br.ReadBytes('\n')
		if err == nil {
			if e := fn(line[:len(line)-1]); e != nil {
				return n, e
			}
			n += int64(len(line))
			continue
		}
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		return n, err
	}
}
