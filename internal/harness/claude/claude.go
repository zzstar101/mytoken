// Package claude parses Claude Code transcripts.
//
// Roots: $CLAUDE_CONFIG_DIR/projects when CLAUDE_CONFIG_DIR is set, otherwise
// ~/.claude/projects (plus ~/.config/claude/projects when that directory
// exists). Layout:
//
//	<root>/<project-slug>/<session-id>.jsonl                  main transcript
//	<root>/<project-slug>/<session-id>/subagents/**/*.jsonl   subagent transcript
//
// Only the first real user message of a session is kept (as the session
// title); no other conversation content is read out of the log.
//
// Token normalization (docs/SPEC.md §4): the log's usage.input_tokens already
// excludes cache traffic, so Input is used as-is; cache_creation_input_tokens
// is reported as CacheWrite and cache_read_input_tokens as CacheRead.
// usage.output_tokens already contains usage.output_tokens_details.thinking_tokens,
// so that value is split out into Reasoning and subtracted from Output (Total is
// unchanged). Provider is left empty unless the log names one explicitly.
package claude

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

	"github.com/zzstar/mytoken/internal/harness"
	"github.com/zzstar/mytoken/internal/model"
)

const (
	kindJSONL     = "jsonl"
	fingerprintN  = 4096
	subagentsDir  = "subagents"
	configDirName = ".claude"
)

// Parser implements harness.Parser for Claude Code.
type Parser struct {
	roots []string
}

// New returns a parser for the standard Claude Code locations.
func New() *Parser { return &Parser{roots: DefaultRoots()} }

// NewWithRoots returns a parser reading the given project roots (tests).
func NewWithRoots(roots ...string) *Parser {
	return &Parser{roots: append([]string(nil), roots...)}
}

func init() { harness.Register(New()) }

// DefaultRoots returns the Claude Code project roots for this machine.
func DefaultRoots() []string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return []string{filepath.Join(dir, "projects")}
	}
	roots := []string{filepath.Join(harness.Home(), configDirName, "projects")}
	if alt := filepath.Join(harness.Home(), ".config", "claude", "projects"); isDir(alt) {
		roots = append(roots, alt)
	}
	return roots
}

// Harness implements harness.Parser.
func (p *Parser) Harness() model.Harness { return model.ClaudeCode }

// Roots implements harness.Parser.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover implements harness.Parser.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	var out []harness.Source
	seen := map[string]bool{}
	add := func(path string) {
		if !seen[path] {
			seen[path] = true
			out = append(out, harness.Source{Path: path, Kind: kindJSONL})
		}
	}
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
				if isJSONL(proj.Name()) {
					add(filepath.Join(root, proj.Name()))
				}
				continue
			}
			projDir := filepath.Join(root, proj.Name())
			entries, err := os.ReadDir(projDir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() {
					if isJSONL(e.Name()) {
						add(filepath.Join(projDir, e.Name()))
					}
					continue
				}
				// <session-id>/subagents/**/*.jsonl
				sub := filepath.Join(projDir, e.Name(), subagentsDir)
				if !isDir(sub) {
					continue
				}
				_ = filepath.WalkDir(sub, func(path string, d os.DirEntry, err error) error {
					if err != nil {
						return nil
					}
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if !d.IsDir() && isJSONL(d.Name()) {
						add(path)
					}
					return nil
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// state is the incremental parse state persisted in Cursor.Extra. It carries
// the facts that live in already-consumed bytes (title, first project) so a
// resumed parse can still emit a complete SessionMeta.
type state struct {
	Title     string `json:"t,omitempty"`
	Project   string `json:"p,omitempty"`
	StartedAt string `json:"s,omitempty"`
	UpdatedAt string `json:"u,omitempty"`
}

type rawLine struct {
	Type        string      `json:"type"`
	Timestamp   string      `json:"timestamp"`
	SessionID   string      `json:"sessionId"`
	Cwd         string      `json:"cwd"`
	UUID        string      `json:"uuid"`
	IsSidechain bool        `json:"isSidechain"`
	IsMeta      bool        `json:"isMeta"`
	RequestID   string      `json:"requestId"`
	AgentID     string      `json:"agentId"`
	Provider    string      `json:"providerId"`
	ProviderAlt string      `json:"provider"`
	ProviderSnk string      `json:"provider_id"`
	CustomTitle string      `json:"customTitle"`
	Message     *rawMessage `json:"message"`
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
	OutputTokensDetails *struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// decodeLine decodes a transcript line leniently. Claude Code renames and
// retypes fields between versions (providerId/provider_id/provider, a null
// requestId, message content as a plain string or a block array), and one
// unexpected type must not discard an assistant record that carries usage.
func decodeLine(raw []byte) (rawLine, bool) {
	var rl rawLine
	if err := json.Unmarshal(raw, &rl); err == nil {
		return rl, true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return rawLine{}, false
	}
	rl.Type = fieldString(fields, "type")
	rl.Timestamp = fieldString(fields, "timestamp")
	rl.SessionID = fieldString(fields, "sessionId")
	rl.Cwd = fieldString(fields, "cwd")
	rl.UUID = fieldString(fields, "uuid")
	rl.RequestID = fieldString(fields, "requestId")
	rl.AgentID = fieldString(fields, "agentId")
	rl.Provider = fieldString(fields, "providerId")
	rl.ProviderAlt = fieldString(fields, "provider")
	rl.ProviderSnk = fieldString(fields, "provider_id")
	rl.CustomTitle = fieldString(fields, "customTitle")
	rl.IsSidechain = fieldBool(fields["isSidechain"])
	rl.IsMeta = fieldBool(fields["isMeta"])
	if r, ok := fields["message"]; ok && string(r) != "null" {
		msg := decodeMessage(r)
		rl.Message = &msg
	}
	return rl, true
}

func decodeMessage(raw json.RawMessage) rawMessage {
	var msg rawMessage
	if err := json.Unmarshal(raw, &msg); err == nil {
		return msg
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return rawMessage{}
	}
	msg.ID = fieldString(fields, "id")
	msg.Model = fieldString(fields, "model")
	msg.Role = fieldString(fields, "role")
	msg.Content = fields["content"]
	if u, ok := fields["usage"]; ok && string(u) != "null" {
		var usage rawUsage
		if json.Unmarshal(u, &usage) == nil {
			msg.Usage = &usage
		}
	}
	return msg
}

func fieldString(fields map[string]json.RawMessage, key string) string {
	r, ok := fields[key]
	if !ok || string(r) == "null" {
		return ""
	}
	var v string
	if json.Unmarshal(r, &v) != nil {
		return ""
	}
	return v
}

func fieldBool(raw json.RawMessage) bool {
	var v bool
	if json.Unmarshal(raw, &v) == nil {
		return v
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == "true"
	}
	return false
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

	fileSession := strings.TrimSuffix(filepath.Base(src.Path), filepath.Ext(src.Path))
	parentID := parentSessionOf(src.Path)

	acc := newAccumulator(fileSession, parentID)
	if st8.Project != "" {
		acc.project = st8.Project
	}
	if st8.StartedAt != "" {
		if t, err := time.Parse(time.RFC3339, st8.StartedAt); err == nil {
			acc.started = t.UTC()
		}
	}
	if st8.Title != "" {
		acc.titles[acc.primary] = st8.Title
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
		rl, ok := decodeLine(line)
		if !ok {
			continue // tolerate unknown/partial records
		}
		p.handleLine(&b, &rl, acc, fileSession, parentID)
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

func (p *Parser) handleLine(b *harness.Batch, rl *rawLine, acc *accumulator, fileSession, parentID string) {
	ts, _ := parseTime(rl.Timestamp)

	sessionID := fileSession
	if parentID != "" {
		// Subagent transcript: its own identity comes from agentId, else the file.
		if rl.AgentID != "" {
			sessionID = rl.AgentID
		}
	} else if rl.IsSidechain && rl.AgentID != "" && rl.AgentID != fileSession {
		// Inline sidechain entry inside a main transcript.
		sessionID = rl.AgentID
	}
	if sessionID == "" {
		sessionID = fileSession
	}
	if rl.SessionID != "" && parentID == "" && !rl.IsSidechain {
		sessionID = rl.SessionID
	}
	sid := sessionID
	pid := ""
	if parentID != "" {
		pid = parentID
	} else if sid != fileSession {
		pid = fileSession
	}

	if rl.Cwd != "" {
		acc.project = rl.Cwd
	}
	acc.observe(sid, pid, ts, rl.Cwd)

	switch rl.Type {
	case "assistant":
		p.handleAssistant(b, rl, sid, pid, ts, acc)
	case "user":
		if t := userText(rl); t != "" {
			acc.title(sid, t)
		}
	case "custom-title":
		if rl.CustomTitle != "" {
			acc.fallbackTitle(sid, rl.CustomTitle)
		}
	}
}

func (p *Parser) handleAssistant(b *harness.Batch, rl *rawLine, sid, pid string, ts time.Time, acc *accumulator) {
	m := rl.Message
	if m == nil || m.Usage == nil {
		return
	}
	modelName := m.Model
	if modelName == "" || isSynthetic(modelName) {
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
	provider := firstNonEmpty(rl.Provider, rl.ProviderAlt, rl.ProviderSnk)
	ev := model.UsageEvent{
		Harness:     model.ClaudeCode,
		DedupKey:    key,
		SessionID:   sid,
		ParentID:    pid,
		ProjectPath: acc.project,
		Timestamp:   ts,
		Model:       modelName,
		Provider:    provider,
		Tokens:      tk,
	}
	acc.addEvent(b, ev)
}

// dedupKey is the stable identity of one assistant message. Claude Code streams
// a single message as several lines; all of them share message.id (+ requestId
// when present), so the last/most complete snapshot wins.
func dedupKey(rl *rawLine, m *rawMessage) string {
	if m.ID == "" {
		if rl.UUID != "" {
			return "claude:uuid:" + rl.UUID
		}
		return ""
	}
	if rl.RequestID != "" {
		return "claude:" + m.ID + ":" + rl.RequestID
	}
	return "claude:message:" + m.ID
}

// userText returns the real user text of a line, or "" when the line is a
// tool result, a meta entry, or a command/system wrapper.
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
	case strings.HasPrefix(t, "<"): // <command-name>, <system-reminder>, <local-command-stdout>, ...
		return true
	case strings.HasPrefix(t, "Caveat:"):
		return true
	case strings.HasPrefix(t, "# AGENTS.md"):
		return true
	}
	return false
}

func isSynthetic(m string) bool { return strings.HasPrefix(m, "<") }

// parentSessionOf returns the parent session id for a transcript that lives in
// a "<session-id>/subagents/..." tree, else "".
func parentSessionOf(path string) string {
	dir := filepath.Dir(path)
	for i := 0; i < 8; i++ {
		if filepath.Base(dir) == subagentsDir {
			return filepath.Base(filepath.Dir(dir))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// accumulator collects the per-batch sessions and merges streaming duplicates.
type accumulator struct {
	primary  string
	parent   string
	project  string
	started  time.Time
	titles   map[string]string
	fallback map[string]string
	metas    map[string]*model.SessionMeta
	order    []string
	index    map[string]int
}

func newAccumulator(primary, parent string) *accumulator {
	return &accumulator{
		primary:  primary,
		parent:   parent,
		titles:   map[string]string{},
		fallback: map[string]string{},
		metas:    map[string]*model.SessionMeta{},
		index:    map[string]int{},
	}
}

func (a *accumulator) observe(sid, pid string, ts time.Time, project string) {
	m, ok := a.metas[sid]
	if !ok {
		m = &model.SessionMeta{Harness: model.ClaudeCode, SessionID: sid, ParentID: pid}
		if project != "" {
			m.Project = project
		}
		a.metas[sid] = m
		a.order = append(a.order, sid)
	}
	if m.ParentID == "" {
		m.ParentID = pid
	}
	if m.Project == "" && project != "" {
		m.Project = project
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
	if _, ok := a.fallback[sid]; ok {
		return
	}
	a.titles[sid] = harness.Title(text)
}

// fallbackTitle is used only when a session never produced a user message.
func (a *accumulator) fallbackTitle(sid, text string) {
	if _, ok := a.titles[sid]; ok {
		return
	}
	if _, ok := a.fallback[sid]; ok {
		return
	}
	a.fallback[sid] = harness.Title(text)
}

// addEvent appends ev, merging into an earlier event with the same DedupKey
// (per-field max, later timestamp wins) instead of emitting a duplicate.
func (a *accumulator) addEvent(b *harness.Batch, ev model.UsageEvent) {
	if ev.DedupKey != "" {
		if i, ok := a.index[ev.DedupKey]; ok {
			prev := &b.Events[i]
			prev.Tokens = maxTokens(prev.Tokens, ev.Tokens)
			if ev.Timestamp.After(prev.Timestamp) {
				prev.Timestamp = ev.Timestamp
			}
			if prev.Model == "" {
				prev.Model = ev.Model
			}
			if prev.Provider == "" {
				prev.Provider = ev.Provider
			}
			return
		}
		a.index[ev.DedupKey] = len(b.Events)
	}
	b.Events = append(b.Events, ev)
}

func (a *accumulator) sessions() []model.SessionMeta {
	if len(a.order) == 0 {
		return nil
	}
	out := make([]model.SessionMeta, 0, len(a.order))
	for _, sid := range a.order {
		m := *a.metas[sid]
		if m.Project == "" {
			m.Project = a.project
		}
		if t, ok := a.titles[sid]; ok {
			m.Title = t
		} else if t, ok := a.fallback[sid]; ok {
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
	s := state{Project: a.project}
	if t, ok := a.titles[a.primary]; ok {
		s.Title = t
	} else if t, ok := a.fallback[a.primary]; ok {
		s.Title = t
	}
	if m, ok := a.metas[a.primary]; ok {
		if !m.StartedAt.IsZero() {
			s.StartedAt = m.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		if !m.UpdatedAt.IsZero() {
			s.UpdatedAt = m.UpdatedAt.UTC().Format(time.RFC3339Nano)
		}
	}
	if s == (state{}) {
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
	line = trimRightByte(line, '\n')
	return trimRightByte(line, '\r')
}

func trimRightByte(b []byte, c byte) []byte {
	if n := len(b); n > 0 && b[n-1] == c {
		return b[:n-1]
	}
	return b
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
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
