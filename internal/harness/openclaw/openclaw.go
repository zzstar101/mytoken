// Package openclaw parses OpenClaw agent transcripts (model.Harness
// "openclaw"), including the clawdbot/moltbot/moldbot installations the agent
// was previously shipped as.
//
// # On-disk layout
//
// Every installation roots at an "agents" directory:
//
//	~/.openclaw/agents                     (also ~/.clawdbot, ~/.moltbot, ~/.moldbot)
//	  <agent>/sessions/sessions.json       index: id -> {sessionId, sessionFile}
//	  <agent>/sessions/<sessionId>.jsonl   pre-2026-09-01 transcripts
//	  <agent>/agent/openclaw-agent.sqlite  agent-schema store (transcript_events)
//
// Since the 2026-09-01 migration the transcript lives in the per-agent SQLite
// store and the .jsonl files are archived or renamed. The migration copied
// each JSONL line verbatim into transcript_events.event_json, so one reducer
// reads both eras; a session that the store already holds is dropped from
// Discover, because a legacy file that is still on disk (partial migration,
// restored backup) must not shadow the migrated history.
//
// # Records
//
// One JSON object per line (or per transcript_events row):
//
//	{type:"session", id, timestamp, …}                session id + start time
//	{type:"model_change", modelId}                    current model
//	{type:"custom", customType:"model-snapshot", data:{modelId}}
//	{type:"message", message:{role:"user", content:[{type:"text",text}]}}
//	{type:"message", message:{role:"assistant", model, content:[…],
//	    usage:{input,output,cacheRead,cacheWrite,totalTokens,cost:{total}}}}
//
// Only assistant messages that carry usage become events; the first user
// message supplies the session title (harness.Title). Input and output are
// stored as recorded (OpenClaw reports cache reads/writes outside the input
// count, so nothing is subtracted) and the recording has no reasoning figure,
// so Reasoning stays 0.
//
// Each call is identified by its envelope id (RequestID); an envelope without
// one is keyed by a hash of its usage plus its occurrence index, so repeats
// stay distinct and a later-era reparse of the same session still collapses to
// one event. cost.total is stored only when the provider recorded a non-zero
// figure — a missing cost is priced by MyToken, not guessed here.
//
// OpenClaw records neither a parent session nor a compaction marker, and the
// JSONL era records no workspace, so ParentID and Boundary stay empty and
// Project is the agent directory name. A call without a usable timestamp falls
// back to the session entry then the store file's mtime, so real spend is not
// dropped.
//
// The record shapes follow codeburn's providers/openclaw.ts
// (https://github.com/getagentseal/codeburn, MIT — see THIRD_PARTY_NOTICES).
package openclaw

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"

	_ "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	kindJSONL  = "jsonl"
	kindSQLite = "sqlite"

	agentsEnv      = "MYTOKEN_OPENCLAW_DIRS"
	agentsDirName  = "agents"
	sessionsDir    = "sessions"
	indexName      = "sessions.json"
	storeDirName   = "agent"
	storeFileName  = "openclaw-agent.sqlite"
	transcriptName = "transcript_events"

	fingerprintBytes = 4096
	readBuffer       = 64 << 10
	busyTimeoutMS    = 5000

	// maxDecodedEventBytes bounds a decompressed transcript row; the schema
	// check bounds the stored form at 4 MiB.
	maxDecodedEventBytes = 8 << 20

	defaultModel = "openclaw-auto"
)

// storeRoots are the installations OpenClaw has shipped under, in discovery
// order.
var storeRoots = [][2]string{
	{".openclaw", "agents"},
	{".clawdbot", "agents"},
	{".moltbot", "agents"},
	{".moldbot", "agents"},
}

// Parser implements harness.Parser for OpenClaw agent transcripts.
type Parser struct {
	roots []string
}

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

// New returns a parser rooted at the default agents directories.
func New() *Parser { return NewWithRoots() }

// NewWithRoot returns a parser rooted at one agents directory.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser for the given agents directories.
func NewWithRoots(roots ...string) *Parser {
	p := &Parser{}
	seen := map[string]bool{}
	for _, root := range roots {
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		if seen[root] {
			continue
		}
		seen[root] = true
		p.roots = append(p.roots, root)
	}
	if len(p.roots) == 0 {
		p.roots = defaultRoots()
	}
	return p
}

// defaultRoots resolves the agents directories: $MYTOKEN_OPENCLAW_DIRS (a list)
// wins, then the four known installations.
func defaultRoots() []string {
	if v := os.Getenv(agentsEnv); v != "" {
		var out []string
		for _, root := range filepath.SplitList(v) {
			if root != "" {
				out = append(out, filepath.Clean(root))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	out := make([]string, 0, len(storeRoots))
	for _, parts := range storeRoots {
		out = append(out, filepath.Join(harness.Home(), parts[0], parts[1]))
	}
	return out
}

// Harness identifies the recordings this parser understands.
func (p *Parser) Harness() model.Harness { return model.OpenClaw }

// Roots returns the agents directories this parser scans.
func (p *Parser) Roots() []string { return p.roots }

// Discover lists every transcript the installations hold: one source per
// session, either a .jsonl file or a session inside an agent-schema store.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	var out []harness.Source
	seen := map[string]bool{}

	add := func(path, kind string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, harness.Source{Path: path, Kind: kind})
	}

	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		agents, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, agent := range agents {
			if !agent.IsDir() {
				continue
			}
			agentDir := filepath.Join(root, agent.Name())
			logged, err := storedSessionIDs(ctx, agentDir)
			if err != nil {
				// A locked store must not be read as "no sessions": the
				// legacy files it already imported would come back and the
				// imported history would be counted twice.
				return nil, err
			}
			for _, path := range sessionFiles(agentDir) {
				if logged[sessionIDFromPath(path)] {
					continue
				}
				add(path, kindJSONL)
			}
			for _, sid := range sortedKeys(logged) {
				add(storePath(agentDir, sid), kindSQLite)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// sessionFiles returns the session recordings an agent directory holds: the
// files named by its sessions.json index plus every other .jsonl beside them.
func sessionFiles(agentDir string) []string {
	dir := filepath.Join(agentDir, sessionsDir)
	var out []string
	named := map[string]bool{}

	if raw, err := os.ReadFile(filepath.Join(dir, indexName)); err == nil {
		var index map[string]struct {
			SessionID   string `json:"sessionId"`
			SessionFile string `json:"sessionFile"`
		}
		if json.Unmarshal(raw, &index) == nil {
			keys := make([]string, 0, len(index))
			for key := range index {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				entry := index[key]
				path := entry.SessionFile
				if path == "" && entry.SessionID != "" {
					path = filepath.Join(dir, entry.SessionID+".jsonl")
				}
				if path == "" || named[path] {
					continue
				}
				named[path] = true
				if isFile(path) {
					out = append(out, path)
				}
			}
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if !named[path] {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// sessionIDFromPath is the session a .jsonl recording belongs to.
func sessionIDFromPath(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

// storePath names one session inside an agent store (forge.ts convention).
func storePath(agentDir, sid string) string {
	return filepath.Join(agentDir, storeDirName, storeFileName) + ":" + sid
}

// splitStoreSource splits "<db path>:<session id>".
func splitStoreSource(path string) (string, string, bool) {
	i := strings.LastIndex(path, ":")
	if i < 0 {
		return "", "", false
	}
	dbPath, sid := path[:i], path[i+1:]
	if !strings.HasSuffix(strings.ToLower(dbPath), ".sqlite") || sid == "" {
		return "", "", false
	}
	return dbPath, sid, true
}

// storedSessionIDs lists the sessions an agent's transcript store holds. A
// store without a transcript_events table (older schema or a foreign file) is
// reported as empty so the legacy discovery still covers it.
func storedSessionIDs(ctx context.Context, agentDir string) (map[string]bool, error) {
	out := map[string]bool{}
	path := filepath.Join(agentDir, storeDirName, storeFileName)
	if !isFile(path) {
		return out, nil
	}
	db, err := openStore(path)
	if err != nil {
		if isBusy(err) {
			return nil, err
		}
		return out, nil
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT session_id FROM `+transcriptName+` GROUP BY session_id`)
	if err != nil {
		if isBusy(err) {
			return nil, err
		}
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return out, nil
		}
		if sid != "" {
			out[sid] = true
		}
	}
	return out, rows.Err()
}

// Parse reads new records from one recording.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if dbPath, sid, ok := splitStoreSource(src.Path); ok {
		return p.parseStore(ctx, dbPath, sid, cur)
	}
	st, err := os.Stat(src.Path)
	if err != nil || st.IsDir() {
		return harness.Batch{Next: cur}, nil
	}
	fp, err := fingerprint(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	if unchanged(cur, st, fp) {
		return harness.Batch{Next: cur}, nil
	}
	state := decodeState(cur)
	offset := cur.Offset
	if cur.Fingerprint != fp || st.Size() < cur.Offset {
		offset = 0
		state = newState()
	}
	lines, next, err := readNewLines(src.Path, offset)
	if err != nil {
		return harness.Batch{}, err
	}
	acc := &accumulator{
		agent: agentName(src.Path),
		sid:   sessionIDFromPath(src.Path),
		mtime: st.ModTime(),
		state: state,
	}
	for _, line := range lines {
		acc.line(line, 0)
	}
	acc.finish()
	batch := acc.batch()
	batch.Next = cursorFor(st, fp, next, acc.state, 0)
	return batch, nil
}

// parseStore reads the transcript rows a session added since the cursor.
func (p *Parser) parseStore(ctx context.Context, dbPath, sid string, cur harness.Cursor) (harness.Batch, error) {
	st, err := os.Stat(dbPath)
	if err != nil || st.IsDir() {
		return harness.Batch{Next: cur}, nil
	}
	fp, err := fingerprint(dbPath)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	if unchanged(cur, st, fp) {
		return harness.Batch{Next: cur}, nil
	}
	state := decodeState(cur)
	seq := state.Seq
	if cur.Fingerprint == "" || (cur.Fingerprint != fp && st.Size() <= cur.Size) {
		// The store was replaced (migration, restore, vacuum): replay it from
		// the first row. An append-only store only grows, so a larger file
		// keeps the row position the cursor already folded in.
		seq = 0
		state = newState()
	}
	db, err := openStore(dbPath)
	if err != nil {
		return harness.Batch{}, err
	}
	defer db.Close()

	columns, err := storeColumns(ctx, db)
	if err != nil {
		return harness.Batch{}, err
	}
	if !columns["transcript_events"] {
		// Nothing was folded in, so the cursor keeps the state it arrived with.
		return harness.Batch{Next: cursorFor(st, fp, seq, state, seq)}, nil
	}
	zstdColumn := ""
	if columns["event_zstd"] {
		zstdColumn = ", event_zstd"
	}
	query := `SELECT seq, event_json` + zstdColumn + `, created_at
	          FROM ` + transcriptName + `
	          WHERE session_id = ? AND seq > ? ORDER BY seq`
	rows, err := db.QueryContext(ctx, query, sid, seq)
	if err != nil {
		return harness.Batch{}, err
	}
	defer rows.Close()

	acc := &accumulator{agent: agentName(dbPath), sid: sid, mtime: st.ModTime(), state: state}
	for rows.Next() {
		var (
			rowSeq     int64
			payload    sql.RawBytes
			compressed []byte
			createdAt  int64
		)
		if zstdColumn == "" {
			if err := rows.Scan(&rowSeq, &payload, &createdAt); err != nil {
				return harness.Batch{}, err
			}
		} else {
			if err := rows.Scan(&rowSeq, &payload, &compressed, &createdAt); err != nil {
				return harness.Batch{}, err
			}
		}
		seq = rowSeq
		raw := []byte(payload)
		if len(raw) == 0 && len(compressed) > 0 {
			decoded, ok := decompress(compressed)
			if !ok {
				continue
			}
			raw = decoded
		}
		if len(raw) == 0 {
			continue
		}
		acc.line(raw, createdAt)
	}
	if err := rows.Err(); err != nil {
		return harness.Batch{}, err
	}
	acc.finish()
	batch := acc.batch()
	batch.Next = cursorFor(st, fp, seq, acc.state, seq)
	return batch, nil
}

// accumulator folds transcript records into usage events and the session
// metadata they imply.
type accumulator struct {
	agent string
	sid   string
	mtime time.Time
	state cursorState

	lines  int
	events *eventSet
	metas  map[string]*model.SessionMeta
	order  []string
}

func (a *accumulator) meta(sid string) *model.SessionMeta {
	if a.metas == nil {
		a.metas = map[string]*model.SessionMeta{}
	}
	if m, ok := a.metas[sid]; ok {
		return m
	}
	m := &model.SessionMeta{Harness: model.OpenClaw, SessionID: sid, Project: a.agent}
	a.metas[sid] = m
	a.order = append(a.order, sid)
	return m
}

// line folds one transcript record (JSONL line or store row) into the
// accumulator. createdAtMS is the row's created_at, used only when the record
// and its session carry no timestamp.
func (a *accumulator) line(raw []byte, createdAtMS int64) {
	var entry transcriptEntry
	if json.Unmarshal(raw, &entry) != nil {
		return
	}
	a.lines++
	switch {
	case entry.Type == "session":
		if entry.ID != "" {
			a.state.SessionID = entry.ID
		}
		if provider := firstNonEmpty(entry.Provider, entry.Data.Provider); provider != "" {
			a.state.Provider = provider
		}
		if ts := strings.TrimSpace(string(entry.Timestamp)); ts != "" && ts != "null" {
			a.state.SessionTimestamp = strings.Trim(ts, `"`)
		}
		return
	case entry.Type == "model_change":
		if entry.ModelID != "" {
			a.state.Model = entry.ModelID
		}
		return
	case entry.Type == "custom" && entry.CustomType == "model-snapshot":
		if entry.Data.ModelID != "" {
			a.state.Model = entry.Data.ModelID
		}
		return
	case entry.Type != "message" || entry.Message == nil:
		return
	}
	message := entry.Message
	if message.Role == "user" {
		if a.state.Title == "" {
			for _, block := range message.Content {
				if block.Type == "text" && block.Text != "" {
					a.state.Title = harness.Title(block.Text)
					break
				}
			}
		}
		return
	}
	if message.Role != "assistant" || message.Usage == nil {
		return
	}
	sid := a.state.SessionID
	if sid == "" {
		sid = a.sid
	}
	ts := a.timestamp(entry.Timestamp, createdAtMS)
	if ts.IsZero() {
		return
	}
	callID := entry.ID
	if callID == "" {
		hash := payloadHash(message.Model, entry.Timestamp, message.Usage)
		if a.state.IdLess == nil {
			a.state.IdLess = map[string]int{}
		}
		callID = fmt.Sprintf("h:%s:%d", hash, a.state.IdLess[hash])
		a.state.IdLess[hash]++
	}
	usage := message.Usage
	event := model.UsageEvent{
		Harness:   model.OpenClaw,
		DedupKey:  fmt.Sprintf("openclaw:%s:%s", sid, callID),
		SessionID: sid,
		RequestID: entry.ID,
		Timestamp: ts,
		Model:     firstNonEmpty(message.Model, a.state.Model, defaultModel),
		Provider:  firstNonEmpty(message.Provider, a.state.Provider),
		Tokens: model.Tokens{
			Input:      intValue(usage.Input),
			Output:     intValue(usage.Output),
			CacheRead:  intValue(usage.CacheRead),
			CacheWrite: intValue(usage.CacheWrite),
		},
	}
	if total := usage.costTotal(); total > 0 {
		event.CostUSD = &total
	}
	m := a.meta(sid)
	if m.Title == "" {
		m.Title = a.state.Title
	}
	if m.StartedAt.IsZero() || ts.Before(m.StartedAt) {
		m.StartedAt = ts
	}
	if ts.After(m.UpdatedAt) {
		m.UpdatedAt = ts
	}
	a.eventSink().add(event)
}

// eventSink lazily creates the batch's event set.
func (a *accumulator) eventSink() *eventSet {
	if a.events == nil {
		a.events = newEventSet()
	}
	return a.events
}

// timestamp prefers the record's own timestamp, then its session's, then the
// store row's created_at, then the file mtime.
func (a *accumulator) timestamp(raw json.RawMessage, createdAtMS int64) time.Time {
	if ts := parseTime(raw); !ts.IsZero() {
		return ts
	}
	if ts := parseTimeString(a.state.SessionTimestamp); !ts.IsZero() {
		return ts
	}
	if createdAtMS > 0 {
		return time.UnixMilli(createdAtMS).UTC()
	}
	return a.mtime.UTC()
}

// finish normalizes the batch's session metadata. The session entry records the
// session's window, so it is preferred over the first usage record: that keeps
// StartedAt identical whether the transcript is read in one pass or grown step
// by step (the store merges sessions with min(StartedAt)/max(UpdatedAt), so a
// step that reported a different window would skew the merge).
func (a *accumulator) finish() {
	a.eventSink()
	if a.metas == nil {
		a.metas = map[string]*model.SessionMeta{}
	}
	if a.lines == 0 {
		return
	}
	sid := a.sessionID()
	m, ok := a.metas[sid]
	if !ok {
		m = a.meta(sid)
	}
	if m.Title == "" {
		m.Title = a.state.Title
	}
	if ts := parseTimeString(a.state.SessionTimestamp); !ts.IsZero() {
		m.StartedAt = ts
		if m.UpdatedAt.Before(ts) {
			m.UpdatedAt = ts
		}
	}
	if m.StartedAt.IsZero() {
		m.StartedAt = m.UpdatedAt
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = m.StartedAt
	}
}

func (a *accumulator) sessionID() string {
	if a.state.SessionID != "" {
		return a.state.SessionID
	}
	return a.sid
}

func (a *accumulator) batch() harness.Batch {
	batch := harness.Batch{Events: a.eventSink().list}
	sort.Slice(batch.Events, func(i, j int) bool {
		if !batch.Events[i].Timestamp.Equal(batch.Events[j].Timestamp) {
			return batch.Events[i].Timestamp.Before(batch.Events[j].Timestamp)
		}
		return batch.Events[i].DedupKey < batch.Events[j].DedupKey
	})
	sort.Strings(a.order)
	for _, sid := range a.order {
		m := a.metas[sid]
		if m.StartedAt.IsZero() {
			// A step that only saw records without any timestamp must not
			// contribute a zero window to the store's min/max merge.
			continue
		}
		if m.StartedAt.After(m.UpdatedAt) {
			m.UpdatedAt = m.StartedAt
		}
		batch.Sessions = append(batch.Sessions, *m)
	}
	return batch
}

// eventSet keeps one event per DedupKey.
type eventSet struct {
	list  []model.UsageEvent
	index map[string]int
}

func newEventSet() *eventSet { return &eventSet{index: map[string]int{}} }

func (s *eventSet) add(event model.UsageEvent) {
	if i, ok := s.index[event.DedupKey]; ok {
		s.list[i] = event
		return
	}
	s.index[event.DedupKey] = len(s.list)
	s.list = append(s.list, event)
}

// transcriptEntry is one JSONL line or store row payload.
type transcriptEntry struct {
	Type       string          `json:"type"`
	CustomType string          `json:"customType"`
	ID         string          `json:"id"`
	Timestamp  json.RawMessage `json:"timestamp"`
	Provider   string          `json:"provider"`
	ModelID    string          `json:"modelId"`
	Data       struct {
		Provider string `json:"provider"`
		ModelID  string `json:"modelId"`
	} `json:"data"`
	Message *struct {
		Role     string         `json:"role"`
		Model    string         `json:"model"`
		Provider string         `json:"provider"`
		Usage    *usage         `json:"usage"`
		Content  []contentBlock `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type usage struct {
	Input       *num `json:"input"`
	Output      *num `json:"output"`
	CacheRead   *num `json:"cacheRead"`
	CacheWrite  *num `json:"cacheWrite"`
	TotalTokens *num `json:"totalTokens"`
	Cost        *struct {
		Total *float64 `json:"total"`
	} `json:"cost"`
}

// costTotal is the provider-recorded cost, or 0 when it did not record one.
func (u *usage) costTotal() float64 {
	if u == nil || u.Cost == nil || u.Cost.Total == nil {
		return 0
	}
	total := *u.Cost.Total
	if math.IsNaN(total) || math.IsInf(total, 0) || total <= 0 {
		return 0
	}
	return total
}

// payloadHash identifies an envelope that carries no id.
func payloadHash(modelID string, raw json.RawMessage, u *usage) string {
	parts := []any{
		modelID,
		strings.Trim(strings.TrimSpace(string(raw)), `"`),
		intValue(u.Input), intValue(u.Output),
		intValue(u.CacheRead), intValue(u.CacheWrite),
		u.costTotal(),
	}
	encoded, err := json.Marshal(parts)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8])
}

// cursorState rides along in the cursor: the session context already folded in
// and the byte offset / transcript seq already read.
type cursorState struct {
	SessionID        string         `json:"sessionId,omitempty"`
	SessionTimestamp string         `json:"sessionTs,omitempty"`
	Provider         string         `json:"provider,omitempty"`
	Model            string         `json:"model,omitempty"`
	Title            string         `json:"title,omitempty"`
	IdLess           map[string]int `json:"idLess,omitempty"`
	Seq              int64          `json:"seq,omitempty"`
}

func newState() cursorState { return cursorState{} }

func decodeState(cur harness.Cursor) cursorState {
	var state cursorState
	if cur.Extra != "" {
		if err := json.Unmarshal([]byte(cur.Extra), &state); err != nil {
			return newState()
		}
	}
	return state
}

func cursorFor(st os.FileInfo, fp string, offset int64, state cursorState, seq int64) harness.Cursor {
	state.Seq = seq
	extra := ""
	if raw, err := json.Marshal(state); err == nil {
		extra = string(raw)
	}
	return harness.Cursor{
		Offset:      offset,
		Size:        st.Size(),
		ModTime:     st.ModTime(),
		Fingerprint: fp,
		Extra:       extra,
	}
}

// openStore opens an agent transcript store with a busy timeout.
func openStore(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	q := url.Values{"_pragma": {fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS)}}
	db, err := sql.Open("sqlite", sqlitedsn.URI(abs, q.Encode()))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// storeColumns reports the store's tables and whether transcript_events can
// hold a compressed payload.
func storeColumns(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	out := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type IN ('table','view')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !out[transcriptName] {
		return out, nil
	}
	columns, err := db.QueryContext(ctx, `PRAGMA table_info(`+transcriptName+`)`)
	if err != nil {
		return nil, err
	}
	defer columns.Close()
	for columns.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := columns.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		if name == "event_zstd" {
			out["event_zstd"] = true
		}
	}
	return out, columns.Err()
}

func isBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "locked") || strings.Contains(msg, "busy")
}

// decompress decodes a zstd transcript payload.
var (
	decoderOnce sync.Once
	decoder     *zstd.Decoder
	decoderErr  error
)

func decompress(blob []byte) ([]byte, bool) {
	decoderOnce.Do(func() {
		decoder, decoderErr = zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(maxDecodedEventBytes))
	})
	if decoderErr != nil {
		return nil, false
	}
	out, err := decoder.DecodeAll(blob, nil)
	if err != nil {
		return nil, false
	}
	return out, true
}

func readNewLines(path string, offset int64) ([][]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	r := bufio.NewReaderSize(f, readBuffer)
	pos := offset
	var lines [][]byte
	for {
		chunk, err := r.ReadBytes('\n')
		if n := len(chunk); n > 0 && chunk[n-1] == '\n' {
			pos += int64(n)
			if trimmed := bytes.TrimSpace(chunk); len(trimmed) > 0 {
				lines = append(lines, trimmed)
			}
		}
		if err != nil {
			break
		}
	}
	return lines, pos, nil
}

func fingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, fingerprintBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	sum := sha1.Sum(buf[:n])
	return hex.EncodeToString(sum[:]), nil
}

func unchanged(cur harness.Cursor, st os.FileInfo, fp string) bool {
	return cur.Fingerprint != "" && cur.Fingerprint == fp &&
		cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime())
}

// agentName is the agent directory a recording lives in (two levels up).
func agentName(path string) string {
	return filepath.Base(filepath.Dir(filepath.Dir(path)))
}

// num accepts a JSON number, a quoted number or null.
type num float64

func (n *num) UnmarshalJSON(raw []byte) error {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	s = strings.Trim(s, `"`)
	if s == "" || s == "null" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	*n = num(f)
	return nil
}

func intValue(n *num) int64 {
	if n == nil {
		return 0
	}
	f := float64(*n)
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0
	}
	return int64(math.Round(f))
}

func parseTime(raw json.RawMessage) time.Time {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return time.Time{}
	}
	return parseTimeString(strings.Trim(s, `"`))
}

// parseTimeString accepts RFC 3339, "2006-01-02 15:04:05" and unix seconds or
// milliseconds.
func parseTimeString(s string) time.Time {
	s = strings.TrimSpace(strings.Trim(s, `"`))
	if s == "" || s == "null" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.UTC()
		}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
		if f >= 1e11 {
			return time.UnixMilli(int64(f)).UTC()
		}
		return time.Unix(int64(f), 0).UTC()
	}
	return time.Time{}
}

func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
