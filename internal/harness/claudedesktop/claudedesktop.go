// Package claudedesktop parses Claude Desktop agent sessions.
//
// Claude Desktop keeps its agent ("Cowork") work under
// <base>/<appId>/<workspaceId>/ and stores three kinds of records there:
//
//   - <shortid>/.claude/projects/<sanitized-cwd>/*.jsonl — Claude Code
//     transcripts (same line format as the claude-code harness, but outside
//     ~/.claude/projects, so no record is counted twice);
//   - usage-ledger/*.ndjson — one Cowork/Code usage record per line, one file
//     per day;
//   - local_<sessionId>.json — workspace metadata (sessionId, cliSessionId,
//     spaceId, cwd, userSelectedFolders); spaces.json names the workspace
//     projects.
//
// <base> is $MYTOKEN_CLAUDEDESKTOP_DIRS when set, else the platform default
// ($HOME/Library/Application Support/Claude/local-agent-mode-sessions and its
// Claude-3p sibling on macOS, %APPDATA%\Claude\... on Windows,
// ~/.config/Claude/... on Linux).
//
// Ledger records name their tokens directly (inputTokens, outputTokens,
// cacheReadTokens, cacheWriteTokens) and carry the recorded cost; transcripts
// use the Claude Code fields (input_tokens already excludes cache hits,
// cache_creation_input_tokens is the cache write) and a compact boundary is
// reported through Boundary. Ledger sessions resolve their project from the
// workspace's spaces.json / local_<sessionId>.json (spaceId, then
// userSelectedFolders, then cwd), falling back to "Claude Cowork" (or
// "Claude Code" for surface "code").
//
// Known limitation: the ledger/transcript dedup happens while a transcript is
// parsed, so it can only suppress a transcript record whose ledger line is
// already on disk. If the transcript is scanned and stored first and its
// ledger line is written afterwards, both rows stay in the store: the store
// never deletes rows, and the later ledger record cannot reclaim the
// transcript row that was already counted. On real data exactly 1 of 27
// records is such an overlap, and the two files are normally written within
// the same window because scanning debounces by at least two seconds, so no
// cross-time reclamation is attempted.
//
// Only metadata is read: no message or tool text ever leaves this package
// except the first user message, which becomes the <=60 rune title.
package claudedesktop

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	kindJSONL     = "jsonl"
	sessionsDir   = "local-agent-mode-sessions"
	projectsName  = "projects"
	ledgerName    = "usage-ledger"
	subagentsName = "subagents"
	ledgerExt     = ".ndjson"
	fingerprintN  = 4096
	maxWalkDepth  = 8

	projectCowork = "Claude Cowork"
	projectCode   = "Claude Code"

	ledgerPrefix     = "claude-desktop:ledger:"
	transcriptPrefix = "claude-desktop:"

	// ledgerMatchWindow is how far a transcript request may sit from the ledger
	// record of the same request. Desktop timestamps the transcript when the
	// turn is written and the ledger when the turn is billed; the observed skew
	// is in the tens of milliseconds.
	ledgerMatchWindow = 5 * time.Second
)

// ledgerRecord is one model's usage from a Cowork usage-ledger line, used to
// recognise requests the transcript repeats.
type ledgerRecord struct {
	At     time.Time
	Tokens model.Tokens
}

// Parser implements harness.Parser for Claude Desktop.
type Parser struct {
	roots []string
}

// New returns a parser for the standard Claude Desktop locations.
func New() *Parser { return &Parser{roots: DefaultRoots()} }

// NewWithRoots returns a parser reading the given sessions roots (tests).
func NewWithRoots(roots ...string) *Parser {
	return &Parser{roots: append([]string(nil), roots...)}
}

func init() { harness.Register(New()) }

// DefaultRoots returns the Claude Desktop sessions roots for this machine.
func DefaultRoots() []string {
	if v := os.Getenv("MYTOKEN_CLAUDEDESKTOP_DIRS"); v != "" {
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
	switch runtime.GOOS {
	case "windows":
		base := firstNonEmpty(os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA"))
		if base == "" {
			return nil
		}
		return []string{filepath.Join(base, "Claude", sessionsDir)}
	case "darwin":
		home := harness.Home()
		return []string{
			filepath.Join(home, "Library", "Application Support", "Claude", sessionsDir),
			filepath.Join(home, "Library", "Application Support", "Claude-3p", sessionsDir),
		}
	default:
		dir, err := os.UserConfigDir()
		if err != nil {
			dir = filepath.Join(harness.Home(), ".config")
		}
		return []string{filepath.Join(dir, "Claude", sessionsDir)}
	}
}

// Harness implements harness.Parser.
func (p *Parser) Harness() model.Harness { return model.ClaudeDesktop }

// Roots implements harness.Parser.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover implements harness.Parser.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	seen := map[string]bool{}
	var out []harness.Source
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
		st, err := os.Stat(root)
		if err != nil || !st.IsDir() {
			continue // missing root is not an error
		}
		p.collect(ctx, root, 0, add)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// collect walks a Desktop session tree, harvesting transcripts under every
// "projects" directory and ledgers under every "usage-ledger" directory.
func (p *Parser) collect(ctx context.Context, dir string, depth int, add func(string)) {
	if depth > maxWalkDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return
		}
		if !e.IsDir() {
			continue
		}
		child := filepath.Join(dir, e.Name())
		switch e.Name() {
		case projectsName:
			collectProjects(child, add)
		case ledgerName:
			collectLedgers(child, add)
		case "node_modules", ".git":
			// never walk dependency or VCS trees
		default:
			p.collect(ctx, child, depth+1, add)
		}
	}
}

func collectProjects(dir string, add func(string)) {
	projects, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, proj := range projects {
		if !proj.IsDir() {
			continue
		}
		_ = filepath.Walk(filepath.Join(dir, proj.Name()), func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if strings.EqualFold(filepath.Ext(path), ".jsonl") {
				add(path)
			}
			return nil
		})
	}
}

func collectLedgers(dir string, add func(string)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ledgerExt) {
			add(filepath.Join(dir, e.Name()))
		}
	}
}

// Parse implements harness.Parser.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if strings.EqualFold(filepath.Ext(src.Path), ledgerExt) {
		return p.parseLedger(ctx, src, cur)
	}
	return p.parseTranscript(ctx, src, cur)
}

// state is the incremental parse state persisted in Cursor.Extra.
type state struct {
	SessionID    string `json:"id,omitempty"`
	Title        string `json:"t,omitempty"`
	Project      string `json:"p,omitempty"`
	StartedAt    string `json:"s,omitempty"`
	UpdatedAt    string `json:"u,omitempty"`
	Index        int    `json:"i,omitempty"`
	Pending      string `json:"bp,omitempty"`
	LastKey      string `json:"bl,omitempty"`
	LastBoundary string `json:"bb,omitempty"`
}

// rawLine is one Claude Code transcript line as Claude Desktop writes it.
type rawLine struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	Timestamp string          `json:"timestamp"`
	SessionID string          `json:"sessionId"`
	Session2  string          `json:"session_id"`
	Cwd       string          `json:"cwd"`
	UUID      string          `json:"uuid"`
	RequestID string          `json:"requestId"`
	IsMeta    bool            `json:"isMeta"`
	Provider  string          `json:"provider"`
	Message   json.RawMessage `json:"message"`
}

type rawMessage struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Usage   *rawUsage       `json:"usage"`
}

type rawUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokensDetails      *struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

func (p *Parser) parseTranscript(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
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
	if cur.Fingerprint != "" && cur.Fingerprint == fp && cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime()) {
		return harness.Batch{Next: cur}, nil
	}

	start := cur.Offset
	s := state{}
	if cur.Fingerprint != "" && cur.Fingerprint != fp {
		start = 0 // rewritten: reread from 0 and rely on DedupKey
	} else if cur.Extra != "" {
		_ = json.Unmarshal([]byte(cur.Extra), &s)
	}
	if start < 0 || start > st.Size() {
		start = 0
		s = state{}
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return b, err
	}

	acc := &accumulator{
		primary:  firstNonEmpty(s.SessionID, sessionIDFromPath(src.Path)),
		parent:   parentSessionOf(src.Path),
		project:  s.Project,
		title:    s.Title,
		fallback: projectSlug(src.Path),
		index:    s.Index,
		events:   map[string]int{},
		boundary: &harness.BoundaryState{Pending: model.Boundary(s.Pending), LastKey: s.LastKey, LastBoundary: model.Boundary(s.LastBoundary)},
	}
	if t, ok := parseTimestamp(s.StartedAt); ok {
		acc.started = t
	}
	if t, ok := parseTimestamp(s.UpdatedAt); ok {
		acc.updated = t
	}
	acc.ledger = linkedLedgerUsage(src.Path, acc.primary)

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
			continue
		}
		acc.handleLine(&b, &rl)
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

type accumulator struct {
	primary  string
	parent   string
	project  string
	title    string
	fallback string
	started  time.Time
	updated  time.Time
	index    int
	events   map[string]int
	boundary *harness.BoundaryState
	// ledger holds the usage already recorded by the Cowork usage ledger for
	// the session this transcript belongs to, so the two are not counted twice.
	ledger []ledgerRecord
}

func (a *accumulator) handleLine(b *harness.Batch, rl *rawLine) {
	sid := firstNonEmpty(rl.SessionID, rl.Session2)
	if a.primary == "" && sid != "" {
		a.primary = sid
	}
	if sid == "" {
		sid = a.primary
	}
	if rl.Cwd != "" {
		a.project = rl.Cwd
	}
	ts, _ := parseTimestamp(rl.Timestamp)
	a.observe(ts)

	switch rl.Type {
	case "system":
		if rl.Subtype == "compact_boundary" {
			a.boundary.Pending = model.BoundaryCompact
		}
	case "assistant":
		a.handleAssistant(b, rl, sid, ts)
	case "user":
		if !rl.IsMeta && a.title == "" {
			if text := userText(rl.Message); text != "" {
				a.title = harness.Title(text)
			}
		}
	}
}

func (a *accumulator) handleAssistant(b *harness.Batch, rl *rawLine, sid string, ts time.Time) {
	var msg rawMessage
	if len(rl.Message) == 0 || json.Unmarshal(rl.Message, &msg) != nil || msg.Usage == nil {
		return
	}
	if msg.Model == "" || strings.HasPrefix(msg.Model, "<") {
		return // synthetic or unusable model name
	}
	reasoning := int64(0)
	if msg.Usage.OutputTokensDetails != nil {
		reasoning = msg.Usage.OutputTokensDetails.ThinkingTokens
	}
	if reasoning > msg.Usage.OutputTokens {
		reasoning = msg.Usage.OutputTokens
	}
	tk := model.Tokens{
		Input:      msg.Usage.InputTokens, // already excludes cache hits
		Output:     msg.Usage.OutputTokens - reasoning,
		CacheRead:  msg.Usage.CacheReadInputTokens,
		CacheWrite: msg.Usage.CacheCreationInputTokens,
		Reasoning:  reasoning,
	}
	if tk.IsZero() {
		return
	}
	if a.coveredByLedger(ts, msg.Usage, tk) {
		return // the Cowork ledger already bills this request
	}
	requestID := firstNonEmpty(rl.RequestID, msg.ID)
	key := transcriptPrefix + sid + ":" + firstNonEmpty(msg.ID, rl.UUID)
	if requestID != "" {
		key += ":" + requestID
	}
	if ts.IsZero() {
		ts = a.started
	}
	ev := model.UsageEvent{
		Harness:     model.ClaudeDesktop,
		DedupKey:    key,
		RequestID:   requestID,
		SessionID:   sid,
		ParentID:    a.parent,
		ProjectPath: firstNonEmpty(a.project, a.fallback),
		Timestamp:   ts,
		Model:       msg.Model,
		Provider:    firstNonEmpty(rl.Provider, "claude"),
		Tokens:      tk,
		Boundary:    a.boundary.Apply(key),
	}
	if i, ok := a.events[key]; ok {
		prev := &b.Events[i]
		prev.Tokens = maxTokens(prev.Tokens, ev.Tokens)
		if ev.Timestamp.After(prev.Timestamp) {
			prev.Timestamp = ev.Timestamp
		}
		return
	}
	a.events[key] = len(b.Events)
	b.Events = append(b.Events, ev)
}

func (a *accumulator) observe(ts time.Time) {
	if ts.IsZero() {
		return
	}
	if a.started.IsZero() || ts.Before(a.started) {
		a.started = ts
	}
	if ts.After(a.updated) {
		a.updated = ts
	}
}

func (a *accumulator) sessions() []model.SessionMeta {
	if a.primary == "" {
		return nil
	}
	started := a.started
	if started.IsZero() {
		started = a.updated
	}
	updated := a.updated
	if updated.IsZero() {
		updated = started
	}
	if started.IsZero() && a.title == "" {
		return nil
	}
	return []model.SessionMeta{{
		Harness:   model.ClaudeDesktop,
		SessionID: a.primary,
		ParentID:  a.parent,
		Title:     a.title,
		Project:   firstNonEmpty(a.project, a.fallback),
		StartedAt: started,
		UpdatedAt: updated,
	}}
}

func (a *accumulator) encodeState() string {
	s := state{
		SessionID: a.primary,
		Title:     a.title,
		Project:   a.project,
		Index:     a.index,
		Pending:   string(a.boundary.Pending),
		LastKey:   a.boundary.LastKey,
	}
	if !a.started.IsZero() {
		s.StartedAt = a.started.UTC().Format(time.RFC3339Nano)
	}
	if !a.updated.IsZero() {
		s.UpdatedAt = a.updated.UTC().Format(time.RFC3339Nano)
	}
	s.LastBoundary = string(a.boundary.LastBoundary)
	if s == (state{}) {
		return ""
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(raw)
}

// coveredByLedger reports whether the Cowork usage ledger already recorded this
// exact request. Claude Desktop writes one request to two places when a session
// is a Cowork session: the Claude Code transcript under the workspace and the
// Cowork usage ledger beside it. The ledger is the billing record, so the
// transcript copy is dropped rather than counted a second time. The comparison
// is deliberately strict (identical input/output/cache classes within a few
// seconds) so only a real duplicate matches.
func (a *accumulator) coveredByLedger(ts time.Time, u *rawUsage, tk model.Tokens) bool {
	if len(a.ledger) == 0 || ts.IsZero() {
		return false
	}
	raw := model.Tokens{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: u.CacheCreationInputTokens,
	}
	// The ledger records no thinking tokens, so only the billed classes take
	// part in the comparison, for both the raw and the emitted usage.
	emitted := model.Tokens{Input: tk.Input, Output: tk.Output, CacheRead: tk.CacheRead, CacheWrite: tk.CacheWrite}
	for _, rec := range a.ledger {
		delta := rec.At.Sub(ts)
		if delta < 0 {
			delta = -delta
		}
		if delta > ledgerMatchWindow {
			continue
		}
		if rec.Tokens == raw || rec.Tokens == emitted {
			return true
		}
	}
	return false
}

// linkedLedgerUsage returns the usage records the Cowork usage ledger holds for
// the session that owns this transcript. The two files of a Cowork session are
// related only by the session metadata Claude Desktop writes beside the ledger
// (<workspace>/local_<id>.json), whose cliSessionId names the transcript
// session. Without that link nothing is suppressed.
func linkedLedgerUsage(transcriptPath, sessionID string) []ledgerRecord {
	if sessionID == "" {
		return nil
	}
	workspaceDir := workspaceOf(transcriptPath)
	if workspaceDir == "" {
		return nil
	}
	linked := linkedLedgerSessions(workspaceDir, sessionID)
	if len(linked) == 0 {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(workspaceDir, ledgerName))
	if err != nil {
		return nil
	}
	var out []ledgerRecord
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ledgerExt) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(workspaceDir, ledgerName, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var rec ledgerLine
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue // torn tail line
			}
			if !linked[rec.SessionID] || (rec.Surface != "cowork" && rec.Surface != "code") {
				continue
			}
			ts, ok := parseTimestamp(rawTime(rec.TS))
			if !ok {
				continue
			}
			for _, usage := range rec.Models {
				tk := usage.tokens()
				if tk.IsZero() {
					continue
				}
				out = append(out, ledgerRecord{At: ts, Tokens: tk})
			}
		}
	}
	return out
}

// workspaceOf walks up from a transcript to the workspace directory that holds
// the Cowork usage ledger.
func workspaceOf(path string) string {
	dir := filepath.Dir(path)
	for i := 0; i < maxWalkDepth; i++ {
		if st, err := os.Stat(filepath.Join(dir, ledgerName)); err == nil && st.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// linkedLedgerSessions returns the ledger session ids tied to a transcript
// session by the workspace metadata files (<workspace>/local_<id>.json).
func linkedLedgerSessions(workspaceDir, sessionID string) map[string]bool {
	entries, err := os.ReadDir(workspaceDir)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "local_") || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(workspaceDir, name))
		if err != nil {
			continue
		}
		var meta sessionMeta
		if json.Unmarshal(raw, &meta) != nil || meta.CliSessionID != sessionID {
			continue
		}
		out[strings.TrimSuffix(name, ".json")] = true
		if meta.SessionID != "" {
			out[meta.SessionID] = true
		}
	}
	return out
}

// ledgerLine is one Cowork/Code usage-ledger record.
type ledgerLine struct {
	Surface   string                 `json:"surface"`
	SessionID string                 `json:"sessionId"`
	TS        json.RawMessage        `json:"ts"`
	Models    map[string]ledgerUsage `json:"models"`
}

type ledgerUsage struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	CacheReadTokens   int64 `json:"cacheReadTokens"`
	CacheWriteTokens  int64 `json:"cacheWriteTokens"`
	WebSearchRequests int64 `json:"webSearchRequests"`
	Cost              *struct {
		USD float64 `json:"usd"`
	} `json:"cost"`
}

// tokens converts a ledger usage record to the shared token shape.
func (u ledgerUsage) tokens() model.Tokens {
	return model.Tokens{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadTokens,
		CacheWrite: u.CacheWriteTokens,
	}
}

func (p *Parser) parseLedger(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
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
	if cur.Fingerprint != "" && cur.Fingerprint == fp && cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime()) {
		return harness.Batch{Next: cur}, nil
	}

	start := cur.Offset
	s := state{}
	if cur.Fingerprint != "" && cur.Fingerprint != fp {
		start = 0
	} else if cur.Extra != "" {
		_ = json.Unmarshal([]byte(cur.Extra), &s)
	}
	if start < 0 || start > st.Size() {
		start = 0
		s = state{}
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return b, err
	}

	primary := s.SessionID
	index := s.Index
	started, updated := time.Time{}, time.Time{}
	if t, ok := parseTimestamp(s.StartedAt); ok {
		started = t
	}
	if t, ok := parseTimestamp(s.UpdatedAt); ok {
		updated = t
	}
	project := ""
	seen := map[string]int{}

	br := bufio.NewReaderSize(f, 1<<20)
	off := start
	for {
		if err := ctx.Err(); err != nil {
			return b, err
		}
		line, rerr := br.ReadBytes('\n')
		if rerr != nil {
			break
		}
		off += int64(len(line))
		line = trimEOL(line)
		if len(line) == 0 {
			continue
		}
		var rec ledgerLine
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // torn tail line
		}
		lineIndex := index
		index++
		if rec.Surface != "cowork" && rec.Surface != "code" {
			continue
		}
		if rec.SessionID == "" || len(rec.Models) == 0 {
			continue
		}
		ts, ok := parseTimestamp(rawTime(rec.TS))
		if !ok {
			continue
		}
		if primary == "" {
			primary = rec.SessionID
		}
		if started.IsZero() || ts.Before(started) {
			started = ts
		}
		if ts.After(updated) {
			updated = ts
		}
		if project == "" {
			project = coworkProject(src.Path, rec.SessionID, rec.Surface)
		}
		stamp := ts.UTC().Format(time.RFC3339)
		turn := stamp + ":" + strconv.Itoa(lineIndex)
		names := make([]string, 0, len(rec.Models))
		for name := range rec.Models {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			usage := rec.Models[name]
			var cost *float64
			if usage.Cost != nil && usage.Cost.USD > 0 {
				v := usage.Cost.USD
				cost = &v
			}
			tk := usage.tokens()
			if tk.IsZero() && cost == nil {
				continue // nothing recorded for this model
			}
			key := ledgerPrefix + rec.SessionID + ":" + stamp + ":" + name
			ev := model.UsageEvent{
				Harness:     model.ClaudeDesktop,
				DedupKey:    key,
				RequestID:   turn,
				SessionID:   rec.SessionID,
				ProjectPath: project,
				Timestamp:   ts,
				Model:       name,
				Provider:    "claude",
				Tokens:      tk,
				CostUSD:     cost,
			}
			if i, ok := seen[key]; ok {
				prev := &b.Events[i]
				prev.Tokens = maxTokens(prev.Tokens, ev.Tokens)
				if ev.Timestamp.After(prev.Timestamp) {
					prev.Timestamp = ev.Timestamp
				}
				if prev.CostUSD == nil && ev.CostUSD != nil {
					prev.CostUSD = ev.CostUSD
				}
				continue
			}
			seen[key] = len(b.Events)
			b.Events = append(b.Events, ev)
		}
	}

	if primary != "" {
		if started.IsZero() {
			started = updated
		}
		if updated.IsZero() {
			updated = started
		}
		surface := "cowork"
		if strings.Contains(project, projectCode) {
			surface = "code"
		}
		if project == "" {
			project = fallbackProject(surface)
		}
		b.Sessions = append(b.Sessions, model.SessionMeta{
			Harness:   model.ClaudeDesktop,
			SessionID: primary,
			Project:   project,
			StartedAt: started,
			UpdatedAt: updated,
		})
	}

	next := state{SessionID: primary, Project: project, Index: index}
	if !started.IsZero() {
		next.StartedAt = started.UTC().Format(time.RFC3339Nano)
	}
	if !updated.IsZero() {
		next.UpdatedAt = updated.UTC().Format(time.RFC3339Nano)
	}
	b.Next = harness.Cursor{
		Offset:      off,
		Size:        st.Size(),
		ModTime:     st.ModTime().UTC(),
		Fingerprint: fp,
		Extra:       encodeJSON(next),
	}
	return b, nil
}

// coworkWorkspaceDir returns the workspace directory owning a ledger file. The
// layout observed on macOS is <workspace>/usage-ledger/*.ndjson, with the
// session metadata written beside it as <workspace>/local_<id>.json.
func coworkWorkspaceDir(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) == ledgerName {
		return filepath.Dir(dir)
	}
	return ""
}

// coworkProject resolves the project name of a Cowork session: the workspace
// spaces.json entry for the session's spaceId, else the first user-selected
// folder or working directory, else a product default. Session titles are
// deliberately not used: docs/HARNESS.md §5 allows no free text beyond the
// first user message.
func coworkProject(path, sessionID, surface string) string {
	workspaceDir := coworkWorkspaceDir(path)
	if workspaceDir == "" {
		return fallbackProject(surface)
	}
	if meta := readSessionMeta(workspaceDir, sessionID); meta != nil {
		if name := spaceName(workspaceDir, meta.SpaceID); name != "" {
			return name
		}
		folders := append(append([]string(nil), meta.UserSelectedFolders...), meta.Cwd)
		for _, folder := range folders {
			if base := filepath.Base(strings.TrimRight(folder, `/\`)); base != "" && base != "." {
				return base
			}
		}
	}
	return fallbackProject(surface)
}

func fallbackProject(surface string) string {
	if surface == "code" {
		return projectCode
	}
	return projectCowork
}

type sessionMeta struct {
	SessionID           string   `json:"sessionId"`
	CliSessionID        string   `json:"cliSessionId"`
	SpaceID             string   `json:"spaceId"`
	Cwd                 string   `json:"cwd"`
	UserSelectedFolders []string `json:"userSelectedFolders"`
}

func readSessionMeta(workspaceDir, sessionID string) *sessionMeta {
	if sessionID == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(workspaceDir, sessionID+".json"))
	if err != nil {
		return nil
	}
	var meta sessionMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil
	}
	return &meta
}

func spaceName(workspaceDir, spaceID string) string {
	if spaceID == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(workspaceDir, "spaces.json"))
	if err != nil {
		return ""
	}
	var doc struct {
		Spaces []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"spaces"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	for _, sp := range doc.Spaces {
		if sp.ID == spaceID && sp.Name != "" {
			return sp.Name
		}
	}
	return ""
}

// userText extracts the first text block of a user message.
func userText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var msg rawMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	var text string
	if err := json.Unmarshal(msg.Content, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return ""
	}
	var out []string
	for _, block := range blocks {
		if block.Type != "text" {
			continue
		}
		if t := strings.TrimSpace(block.Text); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, " ")
}

// sessionIDFromPath is the transcript basename without its extension.
func sessionIDFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// parentSessionOf returns the parent session of a subagent transcript.
func parentSessionOf(path string) string {
	dir := filepath.Dir(path)
	for i := 0; i <= maxWalkDepth; i++ {
		if filepath.Base(dir) == subagentsName {
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

// projectSlug returns the sanitized project directory of a transcript.
func projectSlug(path string) string {
	dir := filepath.Dir(path)
	for i := 0; i <= maxWalkDepth; i++ {
		if filepath.Base(filepath.Dir(dir)) == projectsName {
			return filepath.Base(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
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

func encodeJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	if string(raw) == "{}" {
		return ""
	}
	return string(raw)
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

// rawTime renders a timestamp field that may be an ISO string or an epoch
// number (seconds or milliseconds) as a string for parseTimestamp.
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
		ms := int64(n)
		if ms > 0 && ms < 1_000_000_000_000 {
			ms *= 1000
		}
		if ms <= 0 {
			return ""
		}
		return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
	}
	return ""
}

func parseTimestamp(s string) (time.Time, bool) {
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
