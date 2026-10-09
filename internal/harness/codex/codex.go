// Package codex parses Codex CLI rollout logs.
//
// Roots: $CODEX_HOME/sessions or ~/.codex/sessions (plus archived_sessions),
// layout <root>/YYYY/MM/DD/rollout-*.jsonl. Only token accounting is read:
// every other payload (messages, reasoning, tool output) is ignored, except the
// first real user message which becomes the session title.
//
// Token normalization (verified against real logs): the usage objects satisfy
// total_tokens == input_tokens + output_tokens, i.e. cached_input_tokens is a
// subset of input_tokens and reasoning_output_tokens is a subset of
// output_tokens. Therefore
//
//	Input     = input_tokens - cached_input_tokens - cache_write_input_tokens
//	CacheRead = cached_input_tokens
//	CacheWrite= cache_write_input_tokens (0 in every real sample seen)
//	Output    = output_tokens - reasoning_output_tokens
//	Reasoning = reasoning_output_tokens
//
// Usage increments come from last_token_usage (token_count events) or usage
// (token_usage_record events); the session-cumulative snapshots
// (info.total_token_usage / thread_token_usage) are used only to detect
// duplicate replays. A record whose cumulative snapshot equals the previous
// emitted snapshot is skipped, which also collapses the token_usage_record /
// token_count pair that describes the same request. DedupKey embeds the
// cumulative snapshot, so a forked or subagent rollout that replays its
// parent's history produces the same keys and is deduplicated downstream.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zzstar/mytoken/internal/harness"
	"github.com/zzstar/mytoken/internal/model"
)

const (
	kindJSONL    = "jsonl"
	fingerprintN = 4096
	maxDepth     = 6
)

// Parser implements harness.Parser for Codex CLI.
type Parser struct {
	roots []string
}

// New returns a parser for the standard Codex locations.
func New() *Parser { return &Parser{roots: DefaultRoots()} }

// NewWithRoots returns a parser reading the given roots (tests).
func NewWithRoots(roots ...string) *Parser {
	return &Parser{roots: append([]string(nil), roots...)}
}

func init() { harness.Register(New()) }

// DefaultRoots returns $CODEX_HOME/sessions and .../archived_sessions.
func DefaultRoots() []string {
	base := harness.EnvOr("CODEX_HOME", ".codex")
	return []string{filepath.Join(base, "sessions"), filepath.Join(base, "archived_sessions")}
}

// Harness implements harness.Parser.
func (p *Parser) Harness() model.Harness { return model.Codex }

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
		if !isDir(root) {
			continue // missing root is not an error
		}
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				if depthWithin(root, path) > maxDepth {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}
			if !seen[path] {
				seen[path] = true
				out = append(out, harness.Source{Path: path, Kind: kindJSONL})
			}
			return nil
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// state is the incremental parse state persisted in Cursor.Extra: the facts
// that live in already-consumed bytes plus the last cumulative token snapshot.
type state struct {
	Session  string        `json:"s,omitempty"`
	Parent   string        `json:"p,omitempty"`
	Model    string        `json:"m,omitempty"`
	Provider string        `json:"pr,omitempty"`
	Cwd      string        `json:"c,omitempty"`
	Title    string        `json:"t,omitempty"`
	Started  string        `json:"st,omitempty"`
	Updated  string        `json:"u,omitempty"`
	Total    *model.Tokens `json:"tot,omitempty"`
}

type rawLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type rawPayload struct {
	Type          string          `json:"type"`
	ID            string          `json:"id"`
	SessionID     string          `json:"session_id"`
	ThreadID      string          `json:"thread_id"`
	Cwd           string          `json:"cwd"`
	ModelProvider string          `json:"model_provider"`
	Model         string          `json:"model"`
	ForkedFromID  string          `json:"forked_from_id"`
	ThreadSource  string          `json:"thread_source"`
	Message       string          `json:"message"`
	Role          string          `json:"role"`
	Content       json.RawMessage `json:"content"`

	ParentThreadID *string `json:"parent_thread_id"`
	// source is a plain string ("cli", "vscode") in most rollouts and an
	// object only for subagent threads, so it is kept raw.
	Source json.RawMessage `json:"source"`
	// collaboration_mode settings carry the model when payload.model is absent.
	CollaborationMode json.RawMessage `json:"collaboration_mode"`

	Info *infoPayload `json:"info"`

	Usage            *rawUsage `json:"usage"`
	ThreadTokenUsage *rawUsage `json:"thread_token_usage"`
}

type infoPayload struct {
	Total *rawUsage `json:"total_token_usage"`
	Last  *rawUsage `json:"last_token_usage"`
}

// decodePayload decodes a rollout payload leniently: Codex adds and retypes
// fields between versions (source is a string on user threads and an object on
// subagent threads, for instance) and a single unexpected type must not discard
// an entire session_meta or token_count record. The strict decode is tried
// first; on failure each field is decoded on its own.
func decodePayload(raw json.RawMessage) (rawPayload, bool) {
	var pl rawPayload
	if err := json.Unmarshal(raw, &pl); err == nil {
		return pl, true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return rawPayload{}, false
	}
	str := func(key string) string {
		var v string
		if r, ok := fields[key]; ok {
			_ = json.Unmarshal(r, &v)
		}
		return v
	}
	pl.Type = str("type")
	pl.ID = str("id")
	pl.SessionID = str("session_id")
	pl.ThreadID = str("thread_id")
	pl.Cwd = str("cwd")
	pl.ModelProvider = str("model_provider")
	pl.Model = str("model")
	pl.ForkedFromID = str("forked_from_id")
	pl.ThreadSource = str("thread_source")
	pl.Message = str("message")
	pl.Role = str("role")
	pl.Content = fields["content"]
	pl.Source = fields["source"]
	pl.CollaborationMode = fields["collaboration_mode"]
	if r, ok := fields["parent_thread_id"]; ok && string(r) != "null" {
		var v string
		if json.Unmarshal(r, &v) == nil && v != "" {
			pl.ParentThreadID = &v
		}
	}
	if r, ok := fields["info"]; ok {
		var info infoPayload
		if json.Unmarshal(r, &info) == nil {
			pl.Info = &info
		}
	}
	usage := func(key string) *rawUsage {
		r, ok := fields[key]
		if !ok || string(r) == "null" {
			return nil
		}
		var u rawUsage
		if json.Unmarshal(r, &u) != nil {
			return nil
		}
		return &u
	}
	pl.Usage = usage("usage")
	pl.ThreadTokenUsage = usage("thread_token_usage")
	return pl, true
}

type rawUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheReadInputTokens  int64 `json:"cache_read_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

var readers = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 1<<20) }}

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

	if cur.Fingerprint == fp && cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime()) {
		return harness.Batch{Next: cur}, nil
	}
	start := cur.Offset
	stt := state{}
	if cur.Fingerprint != "" && cur.Fingerprint != fp {
		start = 0 // rewritten file: reread, DedupKey keeps it idempotent
	} else if cur.Extra != "" {
		_ = json.Unmarshal([]byte(cur.Extra), &stt)
	}
	if start < 0 || start > st.Size() {
		start = 0
		stt = state{}
	}
	if stt.Session == "" {
		stt.Session = sessionFromFilename(src.Path)
	}

	acc := newAccumulator(&stt)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return b, err
	}
	br := readers.Get().(*bufio.Reader)
	br.Reset(f)
	defer func() {
		br.Reset(nil)
		readers.Put(br)
	}()
	var pending []byte
	var skipped int64
	var skippedTime time.Time
	off := start
	for {
		if err := ctx.Err(); err != nil {
			return b, err
		}
		line, rerr := br.ReadSlice('\n')
		if rerr == bufio.ErrBufferFull {
			// Tool output can span many MiB. Once the envelope identifies an
			// irrelevant record, count its chunks without retaining its body.
			if skipped > 0 {
				skipped += int64(len(line))
			} else if ts, ok := skippable(line); len(pending) == 0 && ok {
				skipped = int64(len(line))
				skippedTime = ts
			} else {
				pending = append(pending, line...)
			}
			continue
		}
		if rerr != nil {
			break // never consume a trailing incomplete line
		}
		if skipped > 0 {
			off += skipped + int64(len(line))
			acc.observe(skippedTime)
			skipped = 0
			continue
		}
		if len(pending) > 0 {
			line = append(pending, line...)
		}
		off += int64(len(line))
		pending = pending[:0]
		if cap(pending) > 1<<20 {
			pending = nil
		}
		line = trimEOL(line)
		if len(line) == 0 {
			continue
		}
		if ts, ok := skippable(line); ok {
			acc.observe(ts)
			continue
		}
		var cl combinedLine
		if err := json.Unmarshal(line, &cl); err == nil {
			ts, _ := parseTime(cl.Timestamp)
			handlePayload(&b, cl.Type, &cl.Payload, acc, ts)
			continue
		}
		var rl rawLine
		if err := json.Unmarshal(line, &rl); err != nil {
			continue
		}
		ts, _ := parseTime(rl.Timestamp)
		p.handleLine(&b, &rl, acc, ts)
	}

	b.Sessions = acc.sessionMeta()
	b.Next = harness.Cursor{
		Offset:      off,
		Size:        st.Size(),
		ModTime:     st.ModTime().UTC(),
		Fingerprint: fp,
		Extra:       acc.encodeState(),
	}
	return b, nil
}

var (
	tsPrefix      = []byte(`{"timestamp":"`)
	payloadPrefix = []byte(`"payload":{"type":"`)
)

// skippable recognises, without decoding, the rollout lines whose only effect is
// advancing the session's last-activity time: tool calls/outputs, reasoning and
// other non-message response items, and event_msg records other than
// token_count/user_message. They are the bulk of a rollout's bytes. Codex
// writes compact JSON with timestamp first and type before payload; any other
// layout is not recognised and takes the full decode path.
func skippable(line []byte) (time.Time, bool) {
	if !bytes.HasPrefix(line, tsPrefix) {
		return time.Time{}, false
	}
	rest := line[len(tsPrefix):]
	end := bytes.IndexByte(rest, '"')
	if end < 0 {
		return time.Time{}, false
	}
	stamp := rest[:end]
	rest = rest[end+1:]
	// Only cross the known numeric ordinal field. Searching for a type key
	// could mistake a nested object's type for the record's own type.
	if bytes.HasPrefix(rest, []byte(`,"ordinal":`)) {
		rest = rest[len(`,"ordinal":`):]
		n := 0
		for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
			n++
		}
		if n == 0 {
			return time.Time{}, false
		}
		rest = rest[n:]
	}
	if !bytes.HasPrefix(rest, []byte(`,"type":"`)) {
		return time.Time{}, false
	}
	head := rest[len(`,"type":"`):]
	end = bytes.IndexByte(head, '"')
	if end < 0 {
		return time.Time{}, false
	}
	typ := string(head[:end])
	head = head[end+1:]
	if !bytes.HasPrefix(head, []byte(`,`)) || !bytes.HasPrefix(head[1:], payloadPrefix) {
		return time.Time{}, false
	}
	head = head[1+len(payloadPrefix):]
	end = bytes.IndexByte(head, '"')
	if end < 0 {
		return time.Time{}, false
	}
	if bytes.IndexByte(head[:end], '\\') >= 0 {
		return time.Time{}, false
	}
	ptype := string(head[:end])
	switch typ {
	case "response_item":
		if ptype == "message" {
			return time.Time{}, false
		}
	case "event_msg":
		if ptype == "token_count" || ptype == "user_message" {
			return time.Time{}, false
		}
	default:
		return time.Time{}, false
	}
	ts, _ := parseTime(string(stamp))
	return ts, true
}

// combinedLine decodes a rollout line and its payload in a single JSON pass.
// Payload fields that Codex retypes between versions are raw here, so the fast
// path is type-safe; anything unexpected falls back to decodePayload.
type combinedLine struct {
	Timestamp string     `json:"timestamp"`
	Type      string     `json:"type"`
	Payload   rawPayload `json:"payload"`
}

// handleLine decodes a line the slow way: payload first, then the record.
func (p *Parser) handleLine(b *harness.Batch, rl *rawLine, acc *accumulator, ts time.Time) {
	pl, ok := decodePayload(rl.Payload)
	if !ok {
		acc.observe(ts)
		return
	}
	handlePayload(b, rl.Type, &pl, acc, ts)
}

func handlePayload(b *harness.Batch, typ string, pl *rawPayload, acc *accumulator, ts time.Time) {
	switch typ {
	case "session_meta":
		if pl.ID != "" {
			acc.session = pl.ID
		} else if pl.SessionID != "" {
			acc.session = pl.SessionID
		}
		if pl.Cwd != "" {
			acc.cwd = pl.Cwd
		}
		if pl.ModelProvider != "" {
			acc.provider = pl.ModelProvider
		}
		if pid := parentOf(pl); pid != "" {
			acc.parent = pid
		}
		acc.observe(ts)
		return
	case "turn_context":
		acc.setModel(b, modelOf(pl))
		if pl.Cwd != "" {
			acc.cwd = pl.Cwd
		}
		acc.observe(ts)
		return
	case "token_usage_record":
		if pl.SessionID != "" {
			acc.session = pl.SessionID
		}
		acc.observe(ts)
		acc.addUsage(b, pl.ThreadTokenUsage, pl.Usage, ts)
		return
	case "event_msg":
		switch pl.Type {
		case "token_count":
			if pl.Info == nil {
				return
			}
			acc.observe(ts)
			acc.addUsage(b, pl.Info.Total, pl.Info.Last, ts)
		case "user_message":
			if t := userText(pl.Message); t != "" {
				acc.setTitle(t)
			}
			acc.observe(ts)
		default:
			acc.observe(ts)
		}
		return
	case "response_item":
		if pl.Role == "user" && pl.Type == "message" {
			if t := contentText(pl.Content); t != "" {
				acc.setTitle(t)
			}
		}
		acc.observe(ts)
		return
	default:
		acc.observe(ts)
	}
}

func parentOf(pl *rawPayload) string {
	if p := sourceParent(pl.Source); p != "" {
		return p
	}
	if pl.ParentThreadID != nil && *pl.ParentThreadID != "" {
		return *pl.ParentThreadID
	}
	return pl.ForkedFromID
}

// sourceParent reads source.subagent.thread_spawn.parent_thread_id. A string
// source ("cli", "vscode") simply has no parent and decodes to "".
func sourceParent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s struct {
		Subagent *struct {
			ThreadSpawn *struct {
				ParentThreadID string `json:"parent_thread_id"`
			} `json:"thread_spawn"`
		} `json:"subagent"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	if s.Subagent != nil && s.Subagent.ThreadSpawn != nil {
		return s.Subagent.ThreadSpawn.ParentThreadID
	}
	return ""
}

// modelOf prefers payload.model and falls back to collaboration_mode.settings.model.
func modelOf(pl *rawPayload) string {
	if pl.Model != "" {
		return pl.Model
	}
	if len(pl.CollaborationMode) == 0 {
		return ""
	}
	var cm struct {
		Settings *struct {
			Model string `json:"model"`
		} `json:"settings"`
	}
	if json.Unmarshal(pl.CollaborationMode, &cm) != nil || cm.Settings == nil {
		return ""
	}
	return cm.Settings.Model
}

// accumulator carries the state that survives across incremental batches and
// merges the current batch.
type accumulator struct {
	session  string
	parent   string
	model    string
	provider string
	cwd      string
	title    string

	started time.Time
	updated time.Time

	prevTotal *model.Tokens

	index map[string]int
	// unmodeled indexes events of the current batch that were written before a
	// turn_context was seen; Codex writes usage records for replayed history
	// ahead of the first turn_context of a resumed rollout.
	unmodeled []int
}

func newAccumulator(st *state) *accumulator {
	a := &accumulator{
		session:   st.Session,
		parent:    st.Parent,
		model:     st.Model,
		provider:  st.Provider,
		cwd:       st.Cwd,
		title:     st.Title,
		prevTotal: st.Total,
		index:     map[string]int{},
	}
	if t, err := time.Parse(time.RFC3339, st.Started); err == nil {
		a.started = t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, st.Updated); err == nil {
		a.updated = t.UTC()
	}
	return a
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

// setModel records the current model and backfills events of this batch that
// were emitted before the model was known.
func (a *accumulator) setModel(b *harness.Batch, m string) {
	if m == "" || m == a.model {
		return
	}
	a.model = m
	for _, i := range a.unmodeled {
		b.Events[i].Model = m
	}
	a.unmodeled = a.unmodeled[:0]
}

func (a *accumulator) setTitle(text string) {
	if a.title != "" {
		return
	}
	a.title = harness.Title(text)
}

// addUsage turns one token record into a UsageEvent. total is the
// session-cumulative snapshot, last the per-request increment.
func (a *accumulator) addUsage(b *harness.Batch, total, last *rawUsage, ts time.Time) {
	if total == nil || last == nil {
		return
	}
	snap := snapshot(total)
	if a.prevTotal != nil && *a.prevTotal == snap {
		return // replay of an already-counted snapshot (or the paired record)
	}
	a.prevTotal = &snap

	tk := toTokens(last)
	if tk.IsZero() {
		return
	}
	ev := model.UsageEvent{
		Harness:     model.Codex,
		DedupKey:    dedupKey(a.scope(), total),
		SessionID:   a.session,
		ParentID:    a.parent,
		ProjectPath: a.cwd,
		Timestamp:   ts,
		Model:       a.model,
		Provider:    a.provider,
		Tokens:      tk,
	}
	if ev.DedupKey != "" {
		if i, ok := a.index[ev.DedupKey]; ok {
			b.Events[i].Tokens = maxTokens(b.Events[i].Tokens, ev.Tokens)
			if ev.Timestamp.After(b.Events[i].Timestamp) {
				b.Events[i].Timestamp = ev.Timestamp
			}
			return
		}
		a.index[ev.DedupKey] = len(b.Events)
	}
	if a.model == "" {
		a.unmodeled = append(a.unmodeled, len(b.Events))
	}
	b.Events = append(b.Events, ev)
}

func (a *accumulator) scope() string {
	if a.parent != "" {
		return a.parent // fork/subagent children replay the parent's history
	}
	return a.session
}

// dedupKey embeds the cumulative snapshot: it is unique per request, identical
// for the two record kinds describing one request, and identical for sibling
// rollouts that replay the same history.
func dedupKey(scope string, u *rawUsage) string {
	return fmt.Sprintf("codex:%s:%d-%d-%d-%d-%d-%d", scope,
		u.InputTokens, u.CachedInputTokens, u.CacheWriteInputTokens,
		u.OutputTokens, u.ReasoningOutputTokens, u.TotalTokens)
}

// snapshot is the normalized cumulative totals, used for duplicate detection.
func snapshot(u *rawUsage) model.Tokens {
	cached := u.CachedInputTokens
	if u.CacheReadInputTokens > cached {
		cached = u.CacheReadInputTokens
	}
	cached = clamp(cached, u.InputTokens)
	write := clamp(u.CacheWriteInputTokens, u.InputTokens-cached)
	reasoning := clamp(u.ReasoningOutputTokens, u.OutputTokens)
	return model.Tokens{
		Input:      u.InputTokens - cached - write,
		Output:     u.OutputTokens - reasoning,
		CacheRead:  cached,
		CacheWrite: write,
		Reasoning:  reasoning,
	}
}

func toTokens(u *rawUsage) model.Tokens { return snapshot(u) }

func (a *accumulator) sessionMeta() []model.SessionMeta {
	if a.session == "" {
		return nil
	}
	m := model.SessionMeta{
		Harness:   model.Codex,
		SessionID: a.session,
		ParentID:  a.parent,
		Title:     a.title,
		Project:   a.cwd,
		StartedAt: a.started,
		UpdatedAt: a.updated,
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = m.StartedAt
	}
	return []model.SessionMeta{m}
}

func (a *accumulator) encodeState() string {
	st := state{
		Session:  a.session,
		Parent:   a.parent,
		Model:    a.model,
		Provider: a.provider,
		Cwd:      a.cwd,
		Title:    a.title,
		Total:    a.prevTotal,
	}
	if !a.started.IsZero() {
		st.Started = a.started.UTC().Format(time.RFC3339Nano)
	}
	if !a.updated.IsZero() {
		st.Updated = a.updated.UTC().Format(time.RFC3339Nano)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return ""
	}
	return string(raw)
}

// contentText extracts the first real user text from a Codex content block
// array (or a plain string), skipping harness-injected wrappers.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return userText(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	for _, blk := range blocks {
		if blk.Type != "" && blk.Type != "input_text" && blk.Type != "text" {
			continue
		}
		if t := userText(blk.Text); t != "" {
			return t
		}
	}
	return ""
}

// userText drops harness-injected user messages (AGENTS.md dumps, environment
// context, instruction wrappers) that are not real prompts.
func userText(s string) string {
	t := strings.TrimSpace(s)
	switch {
	case t == "":
		return ""
	case strings.HasPrefix(t, "<"):
		return ""
	case strings.HasPrefix(t, "# AGENTS.md"):
		return ""
	case strings.HasPrefix(t, "Caveat:"):
		return ""
	}
	return t
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

func clamp(v, max int64) int64 {
	if v < 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}

// sessionFromFilename recovers the thread id from rollout-<ts>-<uuid>[_<uuid>].jsonl.
func sessionFromFilename(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	name = strings.TrimPrefix(name, "rollout-")
	if i := strings.LastIndexByte(name, '_'); i >= 0 && isUUID(name[i+1:]) {
		return name[i+1:] // fork/continuation: the trailing uuid is the thread
	}
	if isUUID(name) {
		return name
	}
	if len(name) > 36 && isUUID(name[len(name)-36:]) {
		return name[len(name)-36:]
	}
	return name
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			c := s[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

func depthWithin(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return 0
	}
	if rel == "." {
		return 0
	}
	return strings.Count(rel, string(filepath.Separator)) + 1
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

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
