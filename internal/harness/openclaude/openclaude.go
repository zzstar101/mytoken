// Package openclaude parses OpenClaude (npm @gitlawb/openclaude) transcripts.
//
// OpenClaude is a Claude Code fork, so a session is a Claude Code schema JSONL
// transcript under a project slug:
//
//	<root>/projects/<project-slug>/<uuid>.jsonl
//
// Every transcript has a sibling `<uuid>.replay.json` that only carries replay
// state; discovery only picks `.jsonl` files, so replay files are skipped.
// Subagent (sidechain) traffic is interleaved in the same transcript with
// `isSidechain: true`; when such a line also carries an `agentId` it is
// attributed to that child session, with the transcript's own session as its
// parent.
//
// Roots: $MYTOKEN_OPENCLAUDE_DIRS when set, else $CODEBURN_OPENCLAUDE_DIR
// (the `<root>/projects` layer), else ~/.openclaude/projects.
//
// Token normalization (docs/HARNESS.md §4): Claude-family usage reports
// input_tokens excluding cache hits, cache_creation_input_tokens as CacheWrite,
// cache_read_input_tokens as CacheRead, and output_tokens already including
// thinking tokens, so thinking tokens are split out into Reasoning instead of
// being added on top. Only UsageEvent metadata is read: no message text, tool
// arguments or tool output ever leaves this package, except the first user
// message, which becomes the <=60 rune session title.
package openclaude

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	kindJSONL       = "jsonl"
	fingerprintN    = 4096
	projectsDirName = "projects"
)

// Parser implements harness.Parser for OpenClaude.
type Parser struct {
	roots []string
}

// New returns a parser for the standard OpenClaude locations.
func New() *Parser { return &Parser{roots: DefaultRoots()} }

// NewWithRoots returns a parser reading the given projects roots (tests).
func NewWithRoots(roots ...string) *Parser {
	return &Parser{roots: append([]string(nil), roots...)}
}

func init() { harness.Register(New()) }

// DefaultRoots returns the OpenClaude projects roots for this machine.
func DefaultRoots() []string {
	if v := os.Getenv("MYTOKEN_OPENCLAUDE_DIRS"); v != "" {
		var out []string
		for _, p := range filepath.SplitList(v) {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, filepath.Clean(p))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if v := strings.TrimSpace(os.Getenv("CODEBURN_OPENCLAUDE_DIR")); v != "" {
		return []string{filepath.Join(filepath.Clean(v), projectsDirName)}
	}
	return []string{filepath.Join(harness.Home(), ".openclaude", projectsDirName)}
}

// Harness implements harness.Parser.
func (p *Parser) Harness() model.Harness { return model.OpenClaude }

// Roots implements harness.Parser.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover implements harness.Parser.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	var out []harness.Source
	seen := map[string]bool{}
	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		projects, err := os.ReadDir(root)
		if err != nil {
			continue // missing root is not an error
		}
		for _, proj := range projects {
			if !proj.IsDir() {
				continue
			}
			dir := filepath.Join(root, proj.Name())
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() || !isJSONL(e.Name()) {
					continue
				}
				path := filepath.Join(dir, e.Name())
				if seen[path] {
					continue
				}
				seen[path] = true
				out = append(out, harness.Source{Path: path, Kind: kindJSONL})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// state is the incremental parse state persisted in Cursor.Extra: the facts
// that live in already-consumed bytes, so a resumed parse still reports a
// complete SessionMeta and the same compact boundaries.
type state struct {
	Title      string                            `json:"t,omitempty"`
	TitleSID   string                            `json:"ts,omitempty"`
	Project    string                            `json:"p,omitempty"`
	StartedAt  string                            `json:"s,omitempty"`
	UpdatedAt  string                            `json:"u,omitempty"`
	Boundaries map[string]*harness.BoundaryState `json:"b,omitempty"`
}

type rawLine struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype"`
	Timestamp   json.RawMessage `json:"timestamp"`
	SessionID   string          `json:"sessionId"`
	Cwd         string          `json:"cwd"`
	UUID        string          `json:"uuid"`
	IsSidechain bool            `json:"isSidechain"`
	IsMeta      bool            `json:"isMeta"`
	RequestID   string          `json:"requestId"`
	AgentID     string          `json:"agentId"`
	Message     *rawMessage     `json:"message"`
}

type rawMessage struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Usage   *rawUsage       `json:"usage"`
}

type rawUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	WebSearchRequests   int64 `json:"web_search_requests"`
	OutputTokensDetails *struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
	ServerToolUse *struct {
		WebSearchRequests int64 `json:"web_search_requests"`
	} `json:"server_tool_use"`
}

// Parse implements harness.Parser.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	b := harness.Batch{}
	f, err := os.Open(src.Path)
	if err != nil {
		return b, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return b, err
	}
	fp, err := fingerprint(f)
	if err != nil {
		return b, err
	}
	// Unchanged source: hand the cursor back untouched.
	if cur.Fingerprint != "" && cur.Fingerprint == fp && cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime()) {
		return harness.Batch{Next: cur}, nil
	}

	fileSession := strings.TrimSuffix(filepath.Base(src.Path), filepath.Ext(src.Path))
	slug := filepath.Base(filepath.Dir(src.Path))
	start := cur.Offset
	st8 := state{}
	if cur.Fingerprint != "" && cur.Fingerprint != fp {
		start = 0 // content rewritten => reread from 0, rely on DedupKey
	} else if cur.Extra != "" {
		_ = json.Unmarshal([]byte(cur.Extra), &st8)
	}
	if start < 0 || start > st.Size() {
		start = 0
		st8 = state{}
	}

	acc := newAccumulator(fileSession, slug)
	acc.project = st8.Project
	acc.boundaries = st8.Boundaries
	if st8.Title != "" {
		// The title belongs to whichever session produced it; fall back to the
		// transcript's own session for cursors written before that was tracked.
		owner := firstNonEmpty(st8.TitleSID, fileSession)
		acc.titleSID = owner
		acc.titles[owner] = st8.Title
	}
	if t, ok := parseTime(st8.StartedAt); ok {
		acc.started = t
	}
	if t, ok := parseTime(st8.UpdatedAt); ok {
		acc.updated = t
	}

	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return b, err
	}
	br := bufio.NewReaderSize(f, 1<<20)
	off := start
	for {
		if err := ctx.Err(); err != nil {
			return b, err
		}
		line, rerr := br.ReadBytes('\n')
		if rerr != nil {
			break // trailing bytes without '\n' are not consumed
		}
		off += int64(len(line))
		line = trimEOL(line)
		if len(line) == 0 {
			continue
		}
		var rl rawLine
		if err := json.Unmarshal(line, &rl); err != nil {
			continue // tolerate unknown/partial records
		}
		acc.handle(&b, &rl)
	}

	b.Sessions = acc.sessions()
	b.Next = harness.Cursor{
		Offset:      off,
		Size:        st.Size(),
		ModTime:     st.ModTime().UTC(),
		Fingerprint: fp,
		Extra:       acc.encodeState(),
	}
	return b, nil
}

// accumulator collects the sessions of one batch and merges streaming
// duplicates, like the claude-code parser does for the same schema.
type accumulator struct {
	primary    string
	slug       string
	project    string
	started    time.Time
	updated    time.Time
	fallbackTS time.Time
	titleSID   string
	boundaries map[string]*harness.BoundaryState
	titles     map[string]string
	metas      map[string]*model.SessionMeta
	order      []string
	index      map[string]int
}

func newAccumulator(primary, slug string) *accumulator {
	return &accumulator{
		primary: primary,
		slug:    slug,
		titles:  map[string]string{},
		metas:   map[string]*model.SessionMeta{},
		index:   map[string]int{},
	}
}

func (a *accumulator) boundary(sid string) *harness.BoundaryState {
	if a.boundaries == nil {
		a.boundaries = map[string]*harness.BoundaryState{}
	}
	if a.boundaries[sid] == nil {
		a.boundaries[sid] = &harness.BoundaryState{}
	}
	return a.boundaries[sid]
}

func (a *accumulator) handle(b *harness.Batch, rl *rawLine) {
	ts, ok := parseTime(rawTime(rl.Timestamp))
	if !ok {
		// A line without a usable timestamp lands inside the session window:
		// the first valid timestamp of the file.
		ts = a.fallbackTS
	} else if a.fallbackTS.IsZero() {
		a.fallbackTS = ts
	}

	sid := a.primary
	pid := ""
	if rl.IsSidechain && rl.AgentID != "" && rl.AgentID != a.primary {
		// Inline sidechain entry: it belongs to the child session, and the
		// transcript's own session is its parent.
		sid = rl.AgentID
		pid = a.primary
	} else if rl.SessionID != "" {
		sid = rl.SessionID
	}

	if rl.Cwd != "" && a.project == "" {
		a.project = rl.Cwd
	}
	a.observe(sid, pid, ts)

	switch rl.Type {
	case "system":
		if rl.Subtype == "compact_boundary" {
			a.boundary(sid).Pending = model.BoundaryCompact
		}
	case "assistant":
		a.handleAssistant(b, rl, sid, pid, ts)
	case "user":
		if t := userText(rl); t != "" {
			a.title(sid, t)
		}
	}
}

func (a *accumulator) handleAssistant(b *harness.Batch, rl *rawLine, sid, pid string, ts time.Time) {
	m := rl.Message
	if m == nil || m.Usage == nil {
		return
	}
	if m.Model == "" || strings.HasPrefix(m.Model, "<") {
		return
	}
	u := m.Usage
	reasoning := int64(0)
	if u.OutputTokensDetails != nil {
		reasoning = u.OutputTokensDetails.ThinkingTokens
	}
	if reasoning > u.OutputTokens {
		reasoning = u.OutputTokens
	}
	tk := model.Tokens{
		Input:      u.InputTokens,
		Output:     u.OutputTokens - reasoning,
		CacheRead:  u.CacheReadTokens,
		CacheWrite: u.CacheCreationTokens,
		Reasoning:  reasoning,
	}
	if tk.IsZero() {
		return
	}
	key := dedupKey(rl, m)
	if key == "" {
		return
	}
	ev := model.UsageEvent{
		Harness:     model.OpenClaude,
		DedupKey:    key,
		RequestID:   firstNonEmpty(rl.RequestID, m.ID),
		Boundary:    a.boundary(sid).Apply(key),
		SessionID:   sid,
		ParentID:    pid,
		ProjectPath: a.projectPath(),
		Timestamp:   ts,
		Model:       m.Model,
		Provider:    "openclaude",
		Tokens:      tk,
	}
	a.addEvent(b, ev)
}

// dedupKey is the stable identity of one assistant message. Claude Code schema
// transcripts stream a single message as several lines that share message.id,
// so the last/most complete snapshot wins instead of emitting duplicates.
func dedupKey(rl *rawLine, m *rawMessage) string {
	sid := firstNonEmpty(rl.SessionID, rl.AgentID, "session")
	switch {
	case m.ID != "" && rl.RequestID != "":
		return "openclaude:" + sid + ":" + m.ID + ":" + rl.RequestID
	case m.ID != "":
		return "openclaude:message:" + m.ID
	case rl.UUID != "":
		return "openclaude:uuid:" + rl.UUID
	}
	return ""
}

// userText returns the real user text of a line, or "" for tool results, meta
// entries and command/system wrappers.
func userText(rl *rawLine) string {
	if rl.IsMeta || rl.Message == nil {
		return ""
	}
	if rl.Message.Role != "" && rl.Message.Role != "user" {
		return ""
	}
	raw := rl.Message.Content
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if isWrapperText(s) {
			return ""
		}
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, blk := range blocks {
		if blk.Type != "" && blk.Type != "text" {
			continue // tool_result, image, ...
		}
		if strings.TrimSpace(blk.Text) != "" {
			parts = append(parts, blk.Text)
		}
	}
	text := strings.TrimSpace(strings.Join(parts, " "))
	if isWrapperText(text) {
		return ""
	}
	return text
}

// isWrapperText reports whether text is harness plumbing rather than a prompt.
func isWrapperText(s string) bool {
	t := strings.TrimSpace(s)
	switch {
	case t == "":
		return true
	case strings.HasPrefix(t, "<"): // <command-name>, <system-reminder>, ...
		return true
	case strings.HasPrefix(t, "Caveat:"):
		return true
	case strings.HasPrefix(t, "# AGENTS.md"):
		return true
	}
	return false
}

func (a *accumulator) observe(sid, pid string, ts time.Time) {
	if !ts.IsZero() {
		// The transcript timeline spans every session it carries.
		if a.started.IsZero() || ts.Before(a.started) {
			a.started = ts
		}
		if ts.After(a.updated) {
			a.updated = ts
		}
	}
	m, ok := a.metas[sid]
	if !ok {
		m = &model.SessionMeta{Harness: model.OpenClaude, SessionID: sid, ParentID: pid}
		a.metas[sid] = m
		a.order = append(a.order, sid)
	}
	if m.ParentID == "" {
		m.ParentID = pid
	}
	if ts.IsZero() {
		return
	}
	if m.StartedAt.IsZero() || ts.Before(m.StartedAt) {
		m.StartedAt = ts
	}
	if ts.After(m.UpdatedAt) {
		m.UpdatedAt = ts
	}
}

// title records the first real user message of a session.
func (a *accumulator) title(sid, text string) {
	if _, ok := a.titles[sid]; ok {
		return
	}
	a.titles[sid] = harness.Title(text)
	if a.titleSID == "" {
		a.titleSID = sid
	}
}

// addEvent appends ev, merging into an earlier event with the same DedupKey
// (per-field max, later timestamp wins) instead of emitting a duplicate.
func (a *accumulator) addEvent(b *harness.Batch, ev model.UsageEvent) {
	if i, ok := a.index[ev.DedupKey]; ok {
		prev := &b.Events[i]
		prev.Tokens = maxTokens(prev.Tokens, ev.Tokens)
		if ev.Timestamp.After(prev.Timestamp) {
			prev.Timestamp = ev.Timestamp
		}
		return
	}
	a.index[ev.DedupKey] = len(b.Events)
	b.Events = append(b.Events, ev)
}

func (a *accumulator) projectPath() string {
	if a.project != "" {
		return a.project
	}
	return a.slug
}

func (a *accumulator) sessions() []model.SessionMeta {
	if len(a.order) == 0 {
		return nil
	}
	out := make([]model.SessionMeta, 0, len(a.order))
	for _, sid := range a.order {
		m := *a.metas[sid]
		if m.Project == "" {
			m.Project = a.projectPath()
		}
		if t, ok := a.titles[sid]; ok {
			m.Title = t
		}
		if m.StartedAt.IsZero() {
			m.StartedAt = a.started
		}
		if m.UpdatedAt.IsZero() {
			m.UpdatedAt = m.StartedAt
		}
		out = append(out, m)
	}
	return out
}

func (a *accumulator) encodeState() string {
	s := state{Project: a.project, Boundaries: a.boundaries}
	if a.titleSID != "" {
		if t, ok := a.titles[a.titleSID]; ok {
			s.Title = t
			s.TitleSID = a.titleSID
		}
	}
	if !a.started.IsZero() {
		s.StartedAt = a.started.UTC().Format(time.RFC3339Nano)
	}
	if !a.updated.IsZero() {
		s.UpdatedAt = a.updated.UTC().Format(time.RFC3339Nano)
	}
	if s.Title == "" && s.Project == "" && s.StartedAt == "" && s.UpdatedAt == "" && len(s.Boundaries) == 0 {
		return ""
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(raw)
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

func fingerprint(f *os.File) (string, error) {
	buf := make([]byte, fingerprintN)
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return "", err
	}
	sum := sha1.Sum(buf[:n])
	return hex.EncodeToString(sum[:]), nil
}

func trimEOL(line []byte) []byte {
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	return line
}

// rawTime renders a JSON timestamp field that may be an ISO string or an epoch
// number as a string for parseTime.
func rawTime(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return millisToRFC3339(n)
	}
	return ""
}

func millisToRFC3339(n float64) string {
	ms := int64(n)
	if ms > 0 && ms < 1_000_000_000_000 {
		ms *= 1000 // seconds-resolution value
	}
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999Z0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func isJSONL(name string) bool { return strings.HasSuffix(name, ".jsonl") }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
