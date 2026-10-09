// Package workbuddy parses WorkBuddy session logs.
//
// # Log layout
//
//	$WORKBUDDY_HOME/projects/<project-slug>/<session-id>.jsonl
//	$WORKBUDDY_HOME/projects/<project-slug>/<session-id>/subagents/agent-<hex>.jsonl
//
// # Record schema
//
// Every line is one JSON object. The fields this parser reads:
//
//	{"id":"<record id>","timestamp":<ms epoch>,"type":"message"|"function_call"|
//	 "function_call_result"|"reasoning"|"ai-title"|"file-history-snapshot",
//	 "cwd":"/path","sessionId":"<uuid>","parentId":"<record id>",
//	 "role":"user"|"assistant","content":[{"type":"input_text","text":"..."}],
//	 "aiTitle":"...","message":{"usage":{...}},"providerData":{...}}
//
// Usage only appears on "function_call" and assistant "message" records. The
// normalized message.usage carries input/output/total plus cache reads; the raw
// provider numbers in providerData.rawUsage additionally carry cache creation
// and thinking tokens and the tool's own credit charge:
//
//	providerData = {"agent","messageId","model","requestModelId",
//	 "requestModelName","conversationRequestId","traceId","isSubAgent",
//	 "isMeta","isCompactInternal","compactType","isCompacted","isSummary",
//	 "rawUsage":{"prompt_tokens","completion_tokens","total_tokens",
//	   "cache_read_input_tokens","cache_creation_input_tokens",
//	   "prompt_cache_hit_tokens","prompt_cache_write_tokens",
//	   "completion_thinking_tokens","cached_tokens","credit"}}
//
// message.usage is OpenAI shaped: input_tokens already includes the cached
// tokens and output_tokens already includes the thinking tokens, so the parser
// subtracts them (model.Tokens keeps the five classes disjoint).
//
// # Sessions
//
// A log file is one session; its id comes from the records' sessionId, and a
// file below a <session-id>/subagents/ directory is a subagent session whose
// parent is that directory's session id. providerData.messageId is the stable
// per-request id used as DedupKey, providerData.conversationRequestId is the
// turn-level RequestID, and providerData.rawUsage.credit becomes
// model.Bill{Amount: credit, Unit: "credit"}.
package workbuddy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	kindJSONL    = "jsonl"
	fingerprintN = 4096
	subagentsDir = "subagents"
	homeDirName  = ".workbuddy"
	envHome      = "WORKBUDDY_HOME"
	creditUnit   = "credit"
)

// Parser implements harness.Parser for WorkBuddy. It is parameterized by
// harness id so CodeBuddy, which writes the same records under a different
// home directory, can reuse it.
type Parser struct {
	harnessID model.Harness
	roots     []string
}

// New returns a parser for the standard WorkBuddy locations.
func New() *Parser { return NewForHarness(model.WorkBuddy, DefaultRoots()...) }

// NewForHarness returns a parser reading roots for the given harness id.
func NewForHarness(h model.Harness, roots ...string) *Parser {
	return &Parser{harnessID: h, roots: append([]string(nil), roots...)}
}

// NewWithRoots returns a parser reading the given project roots (tests).
func NewWithRoots(roots ...string) *Parser {
	return NewForHarness(model.WorkBuddy, roots...)
}

func init() { harness.Register(New()) }

// DefaultRoots returns the WorkBuddy project roots for this machine.
func DefaultRoots() []string {
	if dir := os.Getenv(envHome); dir != "" {
		return []string{filepath.Join(dir, "projects")}
	}
	return []string{filepath.Join(harness.Home(), homeDirName, "projects")}
}

// Harness implements harness.Parser.
func (p *Parser) Harness() model.Harness { return p.harnessID }

// Roots implements harness.Parser.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover implements harness.Parser. It walks each project root for session
// logs, including the subagent logs nested below a session directory.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	var out []harness.Source
	seen := map[string]bool{}
	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !isDir(root) {
			continue // missing root is not an error
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
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
			if !isJSONL(d.Name()) || seen[path] {
				return nil
			}
			seen[path] = true
			out = append(out, harness.Source{Path: path, Kind: kindJSONL})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// record is one JSON line of a WorkBuddy session log.
type record struct {
	ID           string          `json:"id"`
	Timestamp    int64           `json:"timestamp"`
	Type         string          `json:"type"`
	Cwd          string          `json:"cwd"`
	SessionID    string          `json:"sessionId"`
	ParentID     string          `json:"parentId"`
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	AiTitle      string          `json:"aiTitle"`
	Message      *recordMessage  `json:"message"`
	ProviderData *providerData   `json:"providerData"`
}

type recordMessage struct {
	Usage *messageUsage `json:"usage"`
}

// messageUsage is the normalized OpenAI-shaped usage object.
type messageUsage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
	CacheReadTokens int64 `json:"cache_read_input_tokens"`
}

// providerData carries the request identity, the model and the raw provider
// usage of a record.
type providerData struct {
	Agent             string    `json:"agent"`
	MessageID         string    `json:"messageId"`
	Model             string    `json:"model"`
	RequestModelID    string    `json:"requestModelId"`
	RequestModelName  string    `json:"requestModelName"`
	ConversationReqID string    `json:"conversationRequestId"`
	TraceID           string    `json:"traceId"`
	IsSubAgent        bool      `json:"isSubAgent"`
	IsMeta            bool      `json:"isMeta"`
	IsCompactInternal bool      `json:"isCompactInternal"`
	CompactType       string    `json:"compactType"`
	IsCompacted       bool      `json:"isCompacted"`
	IsSummary         bool      `json:"isSummary"`
	RawUsage          *rawUsage `json:"rawUsage"`
}

// rawUsage is the provider's own usage object; it is a superset of
// message.usage and carries cache creation, thinking tokens and the credit.
type rawUsage struct {
	PromptTokens             int64   `json:"prompt_tokens"`
	CompletionTokens         int64   `json:"completion_tokens"`
	TotalTokens              int64   `json:"total_tokens"`
	CacheReadInputTokens     int64   `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64   `json:"cache_creation_input_tokens"`
	PromptCacheHitTokens     int64   `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens    int64   `json:"prompt_cache_miss_tokens"`
	PromptCacheWriteTokens   int64   `json:"prompt_cache_write_tokens"`
	CompletionThinkingTokens int64   `json:"completion_thinking_tokens"`
	CachedTokens             int64   `json:"cached_tokens"`
	Credit                   float64 `json:"credit"`
}

func (r *record) time() time.Time {
	if r.Timestamp <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(r.Timestamp).UTC()
}

// compact reports whether the record announces a context compaction, which
// belongs to the next distinct usage request rather than this record.
func (r *record) compact() bool {
	pd := r.ProviderData
	if pd == nil {
		return false
	}
	return pd.IsCompactInternal || pd.CompactType != "" || pd.IsCompacted || pd.IsSummary
}

// meta reports records that are bookkeeping rather than a real user turn: the
// injected context block, tool results and compaction summaries.
func (r *record) meta() bool {
	pd := r.ProviderData
	if pd != nil && (pd.IsMeta || pd.IsCompactInternal || pd.IsSummary) {
		return true
	}
	return r.Type != "message" || r.Role != "user"
}

// state is the incremental parse state persisted in Cursor.Extra: the facts
// that live in already-consumed bytes, so a resumed parse can still emit a
// complete SessionMeta and carry a pending compaction boundary forward.
type state struct {
	Title      string                 `json:"t,omitempty"`
	Fallback   string                 `json:"f,omitempty"`
	Project    string                 `json:"p,omitempty"`
	SessionID  string                 `json:"s,omitempty"`
	ParentID   string                 `json:"pa,omitempty"`
	StartedAt  string                 `json:"sa,omitempty"`
	UpdatedAt  string                 `json:"ua,omitempty"`
	Boundary   *harness.BoundaryState `json:"b,omitempty"`
	LastOffset int64                  `json:"o,omitempty"`
}

// Parse implements harness.Parser.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	f, err := os.Open(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return harness.Batch{}, err
	}
	fp, err := fingerprint(f)
	if err != nil {
		return harness.Batch{}, err
	}
	if unchanged(cur, st, fp) {
		return harness.Batch{Next: cur}, nil
	}

	next := harness.Cursor{Size: st.Size(), ModTime: st.ModTime(), Fingerprint: fp, Extra: cur.Extra}
	off := cur.Offset
	if off < 0 || off > st.Size() || cur.Fingerprint != fp {
		off = 0
		next.Extra = ""
	}
	acc := newAccumulator(p.harnessID, src.Path, next.Extra)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return harness.Batch{}, err
	}

	var b harness.Batch
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return b, err
		}
		line, err := r.ReadBytes('\n')
		if n := len(line); n > 0 && line[n-1] == '\n' {
			off += int64(n)
			acc.consume(&b, line[:n-1])
		} else {
			// A trailing partial line is left for the next parse.
			break
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return b, err
		}
	}

	next.Offset = off
	next.Extra = acc.encode(off)
	b.Sessions = acc.sessions()
	b.Next = next
	return b, nil
}

// accumulator builds one parse's batch for a single log file.
type accumulator struct {
	harnessID model.Harness
	sid       string
	pid       string
	project   string
	title     string
	fallback  string
	started   time.Time
	updated   time.Time
	saw       bool
	boundary  harness.BoundaryState
	index     map[string]int
}

func newAccumulator(h model.Harness, path, extra string) *accumulator {
	a := &accumulator{
		harnessID: h,
		sid:       sessionIDFromPath(path),
		pid:       parentFromPath(path),
		index:     map[string]int{},
	}
	if extra != "" {
		var s state
		if json.Unmarshal([]byte(extra), &s) == nil {
			a.title, a.fallback, a.project = s.Title, s.Fallback, s.Project
			a.sid, a.pid = firstNonEmpty(s.SessionID, a.sid), firstNonEmpty(s.ParentID, a.pid)
			a.started = parseTime(s.StartedAt)
			a.updated = parseTime(s.UpdatedAt)
			if s.Boundary != nil {
				a.boundary = *s.Boundary
			}
		}
	}
	return a
}

func (a *accumulator) consume(b *harness.Batch, line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var rec record
	if err := json.Unmarshal(line, &rec); err != nil {
		return
	}
	a.saw = true
	if rec.SessionID != "" {
		a.sid = rec.SessionID
	}
	if rec.Cwd != "" && a.project == "" {
		a.project = rec.Cwd
	}
	if ts := rec.time(); !ts.IsZero() {
		if a.started.IsZero() || ts.Before(a.started) {
			a.started = ts
		}
		if ts.After(a.updated) {
			a.updated = ts
		}
	}
	switch rec.Type {
	case "ai-title":
		if rec.AiTitle != "" {
			a.title = harness.Title(rec.AiTitle)
		}
	case "message":
		if rec.Role == "user" && !rec.meta() {
			if t := textOf(rec.Content); a.fallback == "" && titleCandidate(t) {
				a.fallback = harness.Title(t)
			}
		}
	}
	if rec.compact() {
		a.boundary.Pending = model.BoundaryCompact
	}
	a.event(b, &rec)
}

// event turns a usage-bearing record into one UsageEvent. Records without
// usage (reasoning, tool results, snapshots, compaction markers) add no event.
func (a *accumulator) event(b *harness.Batch, rec *record) {
	u := usageOf(rec)
	if u == nil {
		return
	}
	tokens := mapTokens(u, rec.ProviderData)
	if tokens.IsZero() {
		return
	}
	key := dedupKey(rec)
	if key == "" {
		return
	}
	if i, ok := a.index[key]; ok {
		// A repeated key is the same request streamed twice: keep the peak.
		prev := &b.Events[i]
		prev.Tokens = maxTokens(prev.Tokens, tokens)
		if ev := rec.time(); ev.After(prev.Timestamp) {
			prev.Timestamp = ev
		}
		return
	}
	ev := model.UsageEvent{
		Harness:     a.harnessID,
		DedupKey:    key,
		SessionID:   a.sid,
		ParentID:    a.pid,
		ProjectPath: firstNonEmpty(rec.Cwd, a.project),
		Timestamp:   rec.time(),
		Model:       modelOf(rec.ProviderData),
		Tokens:      tokens,
	}
	if pd := rec.ProviderData; pd != nil {
		ev.RequestID = pd.ConversationReqID
		if pd.RawUsage != nil && pd.RawUsage.Credit != 0 {
			ev.Bill = &model.Bill{Amount: pd.RawUsage.Credit, Unit: creditUnit}
		}
	}
	ev.Boundary = a.boundary.Apply(key)
	a.index[key] = len(b.Events)
	b.Events = append(b.Events, ev)
}

func (a *accumulator) sessions() []model.SessionMeta {
	if !a.saw || a.sid == "" {
		return nil
	}
	title := a.title
	if title == "" {
		title = a.fallback
	}
	m := model.SessionMeta{
		Harness:   a.harnessID,
		SessionID: a.sid,
		ParentID:  a.pid,
		Title:     title,
		Project:   a.project,
		StartedAt: a.started,
		UpdatedAt: a.updated,
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = m.StartedAt
	}
	return []model.SessionMeta{m}
}

func (a *accumulator) encode(offset int64) string {
	s := state{
		Title:      a.title,
		Fallback:   a.fallback,
		Project:    a.project,
		SessionID:  a.sid,
		ParentID:   a.pid,
		LastOffset: offset,
	}
	if !a.started.IsZero() {
		s.StartedAt = a.started.UTC().Format(time.RFC3339Nano)
	}
	if !a.updated.IsZero() {
		s.UpdatedAt = a.updated.UTC().Format(time.RFC3339Nano)
	}
	if a.boundary.Pending != "" || a.boundary.LastKey != "" {
		b := a.boundary
		s.Boundary = &b
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(raw)
}

// usageOf returns the usage object of a record, if any.
func usageOf(rec *record) *messageUsage {
	if rec.Message == nil {
		return nil
	}
	return rec.Message.Usage
}

// mapTokens converts OpenAI-shaped usage into the five disjoint classes:
// Input excludes cache read/write, Output excludes reasoning.
func mapTokens(u *messageUsage, pd *providerData) model.Tokens {
	in, out := u.InputTokens, u.OutputTokens
	var read, write, reasoning int64
	if pd != nil && pd.RawUsage != nil {
		raw := pd.RawUsage
		if raw.PromptTokens > 0 {
			in = raw.PromptTokens
		}
		if raw.CompletionTokens > 0 {
			out = raw.CompletionTokens
		}
		read = raw.CacheReadInputTokens
		if raw.PromptCacheHitTokens > read {
			read = raw.PromptCacheHitTokens
		}
		if raw.CachedTokens > read {
			read = raw.CachedTokens
		}
		write = raw.CacheCreationInputTokens
		if raw.PromptCacheWriteTokens > write {
			write = raw.PromptCacheWriteTokens
		}
		reasoning = raw.CompletionThinkingTokens
	}
	if u.CacheReadTokens > read {
		read = u.CacheReadTokens
	}
	read = clamp(read, in)
	write = clamp(write, in-read)
	reasoning = clamp(reasoning, out)
	return model.Tokens{
		Input:      in - read - write,
		Output:     out - reasoning,
		CacheRead:  read,
		CacheWrite: write,
		Reasoning:  reasoning,
	}
}

// dedupKey is the stable identity of one model request: the assistant message
// id, else the record id, else nothing (the record is then ignored).
func dedupKey(rec *record) string {
	if pd := rec.ProviderData; pd != nil && pd.MessageID != "" {
		return pd.MessageID
	}
	return rec.ID
}

// modelOf is the resolved model id; requestModelId is an alias and
// requestModelName a display label, so both are only fallbacks.
func modelOf(pd *providerData) string {
	if pd == nil {
		return ""
	}
	return firstNonEmpty(pd.Model, pd.RequestModelID, pd.RequestModelName)
}

var (
	// wrapperTagRE matches an injected wrapper block, e.g. <system-reminder
	// data-role="user-context"> or <conversation_history_summary>.
	wrapperTagRE = regexp.MustCompile(`^<[a-zA-Z][a-zA-Z0-9-]*(\s[^>]*)?>`)
	// slashCommandRE matches a slash command line.
	slashCommandRE = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9_-]*(\s|$)`)
)

// titleCandidate reports whether text may title a session: WorkBuddy's first
// user message is usually an injected wrapper block, which is not a title.
func titleCandidate(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	return !wrapperTagRE.MatchString(s) && !slashCommandRE.MatchString(s)
}

// textOf extracts the text of a message content field, which is either a
// string or a list of {"type":"input_text"|"output_text","text":...} blocks.
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, " ")
}

// sessionIDFromPath falls back to the file name when no record carried a
// sessionId (the file is named after its session).
func sessionIDFromPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if looksLikeSessionID(base) {
		return base
	}
	return ""
}

// parentFromPath reports the parent session of a subagent log:
// <project>/<session-id>/subagents/agent-<hex>.jsonl.
func parentFromPath(path string) string {
	dir := filepath.Dir(path)
	for i := 0; i < 4; i++ {
		if filepath.Base(dir) == subagentsDir {
			if parent := filepath.Base(filepath.Dir(dir)); looksLikeSessionID(parent) {
				return parent
			}
			return ""
		}
		up := filepath.Dir(dir)
		if up == dir {
			break
		}
		dir = up
	}
	return ""
}

// looksLikeSessionID reports a canonical 8-4-4-4-12 uuid, which is what
// WorkBuddy names its session directories after.
func looksLikeSessionID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}

func unchanged(cur harness.Cursor, st os.FileInfo, fp string) bool {
	return cur.Fingerprint != "" && cur.Fingerprint == fp &&
		cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime())
}

func fingerprint(f *os.File) (string, error) {
	buf := make([]byte, fingerprintN)
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return "", err
	}
	sum := sha1.Sum(buf[:n])
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum[:]), nil
}

func maxTokens(a, b model.Tokens) model.Tokens {
	return model.Tokens{
		Input:      max64(a.Input, b.Input),
		Output:     max64(a.Output, b.Output),
		CacheRead:  max64(a.CacheRead, b.CacheRead),
		CacheWrite: max64(a.CacheWrite, b.CacheWrite),
		Reasoning:  max64(a.Reasoning, b.Reasoning),
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func clamp(v, limit int64) int64 {
	if v < 0 {
		return 0
	}
	if limit >= 0 && v > limit {
		return limit
	}
	return v
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func isJSONL(name string) bool { return strings.HasSuffix(name, ".jsonl") }

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
