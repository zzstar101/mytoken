// Package grok parses Grok CLI ("Grok Build", xAI's coding agent) recordings
// (model.Harness "grok").
//
// # On-disk layout
//
// Grok Build keeps all of its state under $GROK_HOME (default ~/.grok;
// %USERPROFILE%\.grok on Windows):
//
//	$GROK_HOME/logs/unified.jsonl                       long-lived unified log
//	$GROK_HOME/sessions/<url-encoded-cwd>/<session-uuid>/
//	    summary.json                                    info{id,cwd}, title, model
//	    signals.json                                    primaryModelId, modelsUsed[]
//	    updates.jsonl                                   ACP session updates
//
// The unified log records one inference per line for every session and
// outlives the per-session directories, so a session that already appears in
// the log is dropped from Discover (otherwise the same inference would be
// counted twice).
//
// # Records
//
// Every line of logs/unified.jsonl is a notification object with top-level
// msg/sid/pid/ts and a ctx payload. Only these carry usage:
//
//	msg=="shell.turn.inference_done"
//	    ctx{prompt_tokens, cached_prompt_tokens, completion_tokens,
//	        reasoning_tokens, loop_index}
//
// Context records (ctx.model, ctx.cwd, ctx.current_model_id) are folded into
// the incremental cursor state and attached to later usage records.
//
// Every line of a session's updates.jsonl is one ACP session/update
// notification. The recorded usage lives in
//
//	params.update.sessionUpdate=="turn_completed"
//	    params.update{prompt_id, usage{inputTokens, outputTokens,
//	        cachedReadTokens, cacheCreationTokens, reasoningTokens,
//	        modelUsage{<modelId>: …}}}
//
// Repeated turn_completed updates for one prompt_id are cumulative, so the
// last one wins. The same notification carries the context size in
// params._meta.totalTokens; a drop below half of the previous value means the
// conversation was compacted, and the next usage record is tagged with
// model.BoundaryCompact. The first user_message_chunk supplies the session
// title (harness.Title of the first user message).
//
// # Billing
//
// cachedReadTokens and cacheCreationTokens are subsets of inputTokens, and
// reasoningTokens is a subset of outputTokens, exactly as the provider reports
// them, so they are subtracted out:
//
//	Input      = inputTokens     - cachedReadTokens - cacheCreationTokens
//	CacheRead  = cachedReadTokens
//	CacheWrite = cacheCreationTokens
//	Output     = outputTokens    - reasoningTokens
//	Reasoning  = min(reasoningTokens, outputTokens)
//
// Older Grok builds wrote no authoritative usage at all and can only be
// estimated from the totalTokens curve. MyToken does not store estimates, so
// those recordings are ignored; only recorded provider numbers are emitted.
//
// # Identity
//
// Each recorded inference becomes one event. Unified log events are keyed
// "grok:unified:<sid>:<ts>:<loop_index>"; session updates are keyed
// "grok:<sid>:<prompt_id>" (or "grok:<sid>:turn-<n>" when the recording has no
// prompt id). A recorded prompt_id is also reported as the event's RequestID;
// the turn number fallback is not, because Grok did not record it. Grok records
// no parent session and no provider, so those fields stay empty; the project is
// summary.info.cwd (log ctx.cwd).
//
// The record shapes follow codeburn's providers/grok.ts
// (https://github.com/getagentseal/codeburn, MIT — see THIRD_PARTY_NOTICES).
package grok

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	kindJSONL = "jsonl"

	sessionsDirName = "sessions"
	logsDirName     = "logs"
	unifiedLogName  = "unified.jsonl"
	updatesFileName = "updates.jsonl"
	summaryFileName = "summary.json"
	signalsFileName = "signals.json"

	// fingerprintBytes is how much of a file's head is hashed into the cursor
	// fingerprint; a change means the file was rewritten rather than appended.
	fingerprintBytes = 4096
	readBuffer       = 64 << 10

	defaultModel = "grok-build"
)

// Parser implements harness.Parser for Grok CLI recordings.
type Parser struct {
	roots []string
}

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

// New returns a parser rooted at the default Grok home.
func New() *Parser { return NewWithRoots() }

// NewWithRoot returns a parser rooted at one Grok home.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser for the given Grok homes (the directories that
// hold logs/ and sessions/).
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

// defaultRoots resolves the Grok home: $MYTOKEN_GROK_DIRS (a list) wins, then
// $GROK_HOME, then ~/.grok.
func defaultRoots() []string {
	if v := os.Getenv("MYTOKEN_GROK_DIRS"); v != "" {
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
	return []string{harness.EnvOr("GROK_HOME", ".grok")}
}

// Harness identifies the recordings this parser understands.
func (p *Parser) Harness() model.Harness { return model.Grok }

// Roots returns the Grok homes this parser scans.
func (p *Parser) Roots() []string { return p.roots }

// Discover lists the unified log and every session recording that is not
// already covered by the unified log.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	var out []harness.Source
	seen := map[string]bool{}

	add := func(path, kind string) {
		path = filepath.Clean(path)
		if seen[path] {
			return
		}
		seen[path] = true
		out = append(out, harness.Source{Path: path, Kind: kind})
	}

	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		logPath := filepath.Join(root, logsDirName, unifiedLogName)
		logged := map[string]bool{}
		if st, err := os.Stat(logPath); err == nil && !st.IsDir() {
			add(logPath, kindJSONL)
			logged = loggedSessionIDs(logPath, st)
		}
		sessionsDir := filepath.Join(root, sessionsDirName)
		projects, err := os.ReadDir(sessionsDir)
		if err != nil {
			continue
		}
		for _, project := range projects {
			if !project.IsDir() {
				continue
			}
			projectDir := filepath.Join(sessionsDir, project.Name())
			sessions, err := os.ReadDir(projectDir)
			if err != nil {
				continue
			}
			for _, session := range sessions {
				if !session.IsDir() {
					continue
				}
				dir := filepath.Join(projectDir, session.Name())
				updates := filepath.Join(dir, updatesFileName)
				summary := filepath.Join(dir, summaryFileName)
				if !isFile(updates) || !isFile(summary) {
					continue
				}
				if logged[session.Name()] || logged[readSummary(summary).Info.ID] {
					// The unified log is authoritative for this session and
					// keeps the per-inference records that the directory
					// aggregates; reading both would count it twice.
					continue
				}
				add(updates, kindJSONL)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Parse reads new lines from one recording. Files are append-only, so parsing
// resumes at the cursor offset; a rewritten file (fingerprint change) or a
// shrunken one is replayed from the beginning.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	st, err := os.Stat(src.Path)
	if err != nil || st.IsDir() {
		return harness.Batch{Next: cur}, nil
	}
	fp, err := fingerprint(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	if cur.Fingerprint == fp && cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime()) {
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

	var batch harness.Batch
	if filepath.Base(src.Path) == unifiedLogName {
		batch = p.parseUnified(src.Path, lines, state)
	} else {
		batch = p.parseUpdates(src.Path, lines, state)
	}
	batch.Next = cursorFor(st, fp, next, state)
	return batch, nil
}

// parseUnified turns logs/unified.jsonl into one event per recorded inference.
func (p *Parser) parseUnified(path string, lines [][]byte, state cursorState) harness.Batch {
	root := filepath.Dir(filepath.Dir(path))
	events := newEventSet()
	metas := map[string]*model.SessionMeta{}
	order := []string{}
	// The log knows the cwd and model of a session through its context
	// records; the session directory (when it still exists) is the fallback
	// and supplies the title, which the log never records.
	projects := map[string]string{}
	models := map[string]string{}
	looked := map[string]bool{}

	meta := func(sid string) *model.SessionMeta {
		if m, ok := metas[sid]; ok {
			return m
		}
		m := &model.SessionMeta{Harness: model.Grok, SessionID: sid}
		metas[sid] = m
		order = append(order, sid)
		return m
	}

	for _, line := range lines {
		var rec unifiedRecord
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		switch rec.Msg {
		case "model changed":
			if rec.SID != "" && rec.Ctx.Model != "" {
				state.Models[rec.SID] = rec.Ctx.Model
			}
			continue
		case "session created":
			if rec.SID != "" && rec.Ctx.CWD != "" {
				state.Cwds[rec.SID] = rec.Ctx.CWD
			}
			continue
		case "model catalog: notifying clients":
			if rec.PID != "" && rec.Ctx.CurrentModelID != "" {
				state.PidModels[rec.PID] = rec.Ctx.CurrentModelID
			}
			continue
		case "shell.turn.inference_done":
		default:
			continue
		}
		if rec.SID == "" {
			continue
		}
		ts := parseTime(rec.TS)
		if ts.IsZero() {
			// Without a timestamp the record cannot be ordered or keyed
			// stably across scans.
			continue
		}
		prompt := intValue(rec.Ctx.PromptTokens)
		completion := intValue(rec.Ctx.CompletionTokens)
		cached := intValue(rec.Ctx.CachedPromptTokens)
		reasoning := intValue(rec.Ctx.ReasoningTokens)
		input := max(int64(0), prompt-cached)
		output := max(int64(0), completion-reasoning)
		if input == 0 && output == 0 && cached == 0 {
			// The inference recorded no token counts at all.
			continue
		}
		loop := intValue(rec.Ctx.LoopIndex)
		if loop <= 0 {
			loop = 1
		}
		m := meta(rec.SID)
		if !looked[rec.SID] {
			looked[rec.SID] = true
			info := lookupSession(root, rec.SID)
			projects[rec.SID] = firstNonEmpty(state.Cwds[rec.SID], info.cwd)
			models[rec.SID] = firstNonEmpty(state.Models[rec.SID], state.PidModels[rec.PID], info.model)
			m.Title = info.title
			m.Project = projectName(projects[rec.SID])
		}
		if m.StartedAt.IsZero() || ts.Before(m.StartedAt) {
			m.StartedAt = ts
		}
		if ts.After(m.UpdatedAt) {
			m.UpdatedAt = ts
		}
		modelID := firstNonEmpty(state.Models[rec.SID], state.PidModels[rec.PID], models[rec.SID], defaultModel)
		events.add(model.UsageEvent{
			Harness:     model.Grok,
			DedupKey:    fmt.Sprintf("grok:unified:%s:%s:%d", rec.SID, timestampKey(rec.TS, ts), loop),
			SessionID:   rec.SID,
			ProjectPath: projects[rec.SID],
			Timestamp:   ts.UTC(),
			Model:       modelID,
			Tokens: model.Tokens{
				Input:     input,
				Output:    output,
				CacheRead: cached,
				Reasoning: min(reasoning, completion),
			},
		})
	}
	return batchFrom(events, metas, order)
}

// parseUpdates turns a session's updates.jsonl into one event per completed
// turn, with the session metadata read from its summary/signals siblings.
func (p *Parser) parseUpdates(path string, lines [][]byte, state cursorState) harness.Batch {
	dir := filepath.Dir(path)
	summary := readSummary(filepath.Join(dir, summaryFileName))
	signals := readSignals(filepath.Join(dir, signalsFileName))
	sid := firstNonEmpty(summary.Info.ID, filepath.Base(dir))

	info := sessionInfo{
		model: firstNonEmpty(summary.CurrentModelID, signals.PrimaryModelID, firstString(signals.ModelsUsed)),
		cwd:   summary.Info.CWD,
	}
	started := parseTime(summary.CreatedAt)
	updated := parseTime(summary.UpdatedAt)
	if updated.IsZero() {
		updated = parseTime(summary.LastActiveAt)
	}

	events := newEventSet()
	for _, line := range lines {
		var rec sessionUpdate
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec.Params.Meta.TotalTokens != nil {
			total := intValue(rec.Params.Meta.TotalTokens)
			if state.HasTotal && state.Total > 0 && total < state.Total/2 {
				state.Compact = true
			}
			state.Total, state.HasTotal = total, true
		}
		upd := rec.Params.Update
		if upd == nil {
			continue
		}
		switch upd.SessionUpdate {
		case "user_message_chunk":
			if info.title == "" {
				info.title = harness.Title(upd.Content.Text)
			}
			continue
		case "turn_completed":
		default:
			continue
		}
		state.Turns++
		usage := upd.Usage
		if usage == nil || usage.empty() {
			continue
		}
		ts := parseTime(rec.TS)
		if ts.IsZero() {
			ts = parseTime(upd.Timestamp)
		}
		if ts.IsZero() {
			continue
		}
		promptID := firstNonEmpty(upd.PromptID, rec.Params.Meta.PromptID)
		requestID := promptID
		if promptID == "" {
			promptID = fmt.Sprintf("turn-%d", state.Turns)
			// The turn number is ours, not something Grok recorded, so it is
			// good enough to keep the update idempotent but must not be
			// reported as a provider request id.
			requestID = ""
		}
		input := intValue(usage.InputTokens)
		cached := intValue(usage.CachedReadTokens)
		written := intValue(usage.CacheCreationTokens)
		output := intValue(usage.OutputTokens)
		reasoning := intValue(usage.ReasoningTokens)
		event := model.UsageEvent{
			Harness:     model.Grok,
			DedupKey:    fmt.Sprintf("grok:%s:%s", sid, promptID),
			SessionID:   sid,
			RequestID:   requestID,
			Timestamp:   ts.UTC(),
			Model:       firstNonEmpty(usage.model(), info.model, defaultModel),
			ProjectPath: info.cwd,
			Tokens: model.Tokens{
				Input:      max(int64(0), input-cached-written),
				Output:     max(int64(0), output-reasoning),
				CacheRead:  cached,
				CacheWrite: written,
				Reasoning:  min(reasoning, output),
			},
		}
		if state.Compact {
			event.Boundary = model.BoundaryCompact
			state.Compact = false
		}
		events.add(event)
	}
	if info.title == "" {
		info.title = harness.Title(firstUserMessage(filepath.Join(dir, updatesFileName)))
	}

	var batch harness.Batch
	for _, event := range events.list {
		batch.Events = append(batch.Events, event)
	}
	if len(lines) > 0 || info.title != "" {
		sort.Slice(batch.Events, func(i, j int) bool {
			if !batch.Events[i].Timestamp.Equal(batch.Events[j].Timestamp) {
				return batch.Events[i].Timestamp.Before(batch.Events[j].Timestamp)
			}
			return batch.Events[i].DedupKey < batch.Events[j].DedupKey
		})
		if started.IsZero() && len(batch.Events) > 0 {
			started = batch.Events[0].Timestamp
		}
		if len(batch.Events) > 0 && batch.Events[len(batch.Events)-1].Timestamp.After(updated) {
			updated = batch.Events[len(batch.Events)-1].Timestamp
		}
		batch.Sessions = append(batch.Sessions, model.SessionMeta{
			Harness:   model.Grok,
			SessionID: sid,
			Title:     info.title,
			Project:   projectName(info.cwd),
			StartedAt: started,
			UpdatedAt: updated,
		})
	}
	return batch
}

// eventSet keeps one event per DedupKey, last write winning: Grok restates a
// turn's cumulative usage on every update.
type eventSet struct {
	list  []model.UsageEvent
	index map[string]int
}

func newEventSet() *eventSet {
	return &eventSet{index: map[string]int{}}
}

func (s *eventSet) add(event model.UsageEvent) {
	if i, ok := s.index[event.DedupKey]; ok {
		if event.Boundary == "" {
			// A restated turn keeps the compaction boundary that the first
			// record of that turn was tagged with.
			event.Boundary = s.list[i].Boundary
		}
		s.list[i] = event
		return
	}
	s.index[event.DedupKey] = len(s.list)
	s.list = append(s.list, event)
}

// batchFrom assembles events and per-session metadata in a stable order.
func batchFrom(events *eventSet, metas map[string]*model.SessionMeta, order []string) harness.Batch {
	batch := harness.Batch{Events: events.list}
	sort.Slice(batch.Events, func(i, j int) bool {
		if !batch.Events[i].Timestamp.Equal(batch.Events[j].Timestamp) {
			return batch.Events[i].Timestamp.Before(batch.Events[j].Timestamp)
		}
		return batch.Events[i].DedupKey < batch.Events[j].DedupKey
	})
	sort.Strings(order)
	for _, sid := range order {
		m := metas[sid]
		if m.StartedAt.After(m.UpdatedAt) {
			m.UpdatedAt = m.StartedAt
		}
		batch.Sessions = append(batch.Sessions, *m)
	}
	return batch
}

// unifiedRecord is one line of logs/unified.jsonl.
type unifiedRecord struct {
	Msg string          `json:"msg"`
	SID string          `json:"sid"`
	PID string          `json:"pid"`
	TS  json.RawMessage `json:"ts"`
	Ctx struct {
		PromptTokens       *num   `json:"prompt_tokens"`
		CachedPromptTokens *num   `json:"cached_prompt_tokens"`
		CompletionTokens   *num   `json:"completion_tokens"`
		ReasoningTokens    *num   `json:"reasoning_tokens"`
		LoopIndex          *num   `json:"loop_index"`
		Model              string `json:"model"`
		CurrentModelID     string `json:"current_model_id"`
		CWD                string `json:"cwd"`
	} `json:"ctx"`
}

// sessionUpdate is one line of a session's updates.jsonl (an ACP
// session/update notification).
type sessionUpdate struct {
	TS     json.RawMessage `json:"ts"`
	Params struct {
		Meta struct {
			TotalTokens *num   `json:"totalTokens"`
			PromptID    string `json:"promptId"`
		} `json:"_meta"`
		Update *struct {
			SessionUpdate string          `json:"sessionUpdate"`
			PromptID      string          `json:"prompt_id"`
			Timestamp     json.RawMessage `json:"timestamp"`
			Usage         *turnUsage      `json:"usage"`
			Content       struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	} `json:"params"`
}

// turnUsage is the provider-reported usage of one completed turn.
type turnUsage struct {
	InputTokens         *num                  `json:"inputTokens"`
	OutputTokens        *num                  `json:"outputTokens"`
	CachedReadTokens    *num                  `json:"cachedReadTokens"`
	CacheCreationTokens *num                  `json:"cacheCreationTokens"`
	ReasoningTokens     *num                  `json:"reasoningTokens"`
	ModelUsage          map[string]*turnUsage `json:"modelUsage"`
}

func (u *turnUsage) empty() bool {
	return u.InputTokens == nil && u.OutputTokens == nil &&
		u.CachedReadTokens == nil && u.CacheCreationTokens == nil &&
		u.ReasoningTokens == nil
}

// model names the model a turn was served by, when the recording says.
func (u *turnUsage) model() string {
	if len(u.ModelUsage) != 1 {
		return ""
	}
	for id, per := range u.ModelUsage {
		if id != "" && per != nil && !per.empty() {
			return id
		}
	}
	return ""
}

// summaryFile is a session's summary.json.
type summaryFile struct {
	Info struct {
		ID  string `json:"id"`
		CWD string `json:"cwd"`
	} `json:"info"`
	CreatedAt      json.RawMessage `json:"created_at"`
	UpdatedAt      json.RawMessage `json:"updated_at"`
	LastActiveAt   json.RawMessage `json:"last_active_at"`
	CurrentModelID string          `json:"current_model_id"`
	SessionSummary string          `json:"session_summary"`
	GeneratedTitle string          `json:"generated_title"`
}

// signalsFile is a session's signals.json.
type signalsFile struct {
	PrimaryModelID string   `json:"primaryModelId"`
	ModelsUsed     []string `json:"modelsUsed"`
}

// sessionInfo is the sidebar metadata Grok keeps beside a session's updates.
type sessionInfo struct {
	model string
	cwd   string
	title string
}

func readSummary(path string) summaryFile {
	var out summaryFile
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func readSignals(path string) signalsFile {
	var out signalsFile
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

// lookupSession resolves the session directory that belongs to a unified-log
// session id, for the title/model/project the log itself does not carry.
func lookupSession(root, sid string) sessionInfo {
	var out sessionInfo
	if sid == "" {
		return out
	}
	projects, err := os.ReadDir(filepath.Join(root, sessionsDirName))
	if err != nil {
		return out
	}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		dir := filepath.Join(root, sessionsDirName, project.Name(), sid)
		summaryPath := filepath.Join(dir, summaryFileName)
		if !isFile(summaryPath) {
			continue
		}
		summary := readSummary(summaryPath)
		if id := summary.Info.ID; id != "" && id != sid {
			continue
		}
		signals := readSignals(filepath.Join(dir, signalsFileName))
		out.cwd = summary.Info.CWD
		out.model = firstNonEmpty(summary.CurrentModelID, signals.PrimaryModelID, firstString(signals.ModelsUsed))
		out.title = harness.Title(firstUserMessage(filepath.Join(dir, updatesFileName)))
		return out
	}
	return out
}

// firstUserMessage streams a session's updates.jsonl and returns the text of
// the first user message chunk, which is the session title.
func firstUserMessage(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, readBuffer)
	for {
		line, err := r.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 && bytes.Contains(trimmed, []byte(`"user_message_chunk"`)) {
			var rec sessionUpdate
			if json.Unmarshal(trimmed, &rec) == nil && rec.Params.Update != nil &&
				rec.Params.Update.SessionUpdate == "user_message_chunk" {
				return rec.Params.Update.Content.Text
			}
		}
		if err != nil {
			return ""
		}
	}
}

// cursorState rides along in the cursor so an incremental parse keeps the
// context records, turn ordinals and compaction signal it has already seen.
type cursorState struct {
	Models    map[string]string `json:"models,omitempty"`
	Cwds      map[string]string `json:"cwds,omitempty"`
	PidModels map[string]string `json:"pidModels,omitempty"`
	Turns     int               `json:"turns,omitempty"`
	Total     int64             `json:"total,omitempty"`
	HasTotal  bool              `json:"hasTotal,omitempty"`
	Compact   bool              `json:"compact,omitempty"`
}

func newState() cursorState {
	return cursorState{
		Models:    map[string]string{},
		Cwds:      map[string]string{},
		PidModels: map[string]string{},
	}
}

func decodeState(cur harness.Cursor) cursorState {
	state := newState()
	if cur.Extra != "" {
		_ = json.Unmarshal([]byte(cur.Extra), &state)
	}
	if state.Models == nil {
		state.Models = map[string]string{}
	}
	if state.Cwds == nil {
		state.Cwds = map[string]string{}
	}
	if state.PidModels == nil {
		state.PidModels = map[string]string{}
	}
	return state
}

func cursorFor(st os.FileInfo, fp string, offset int64, state cursorState) harness.Cursor {
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

// readNewLines returns the complete lines that start at offset and the offset
// just past the last one, so a partially written trailing line is retried.
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

// loggedSessionIDs collects the session ids the unified log records, so
// Discover can drop the per-session directories they replace. The result is
// cached until the log grows.
var (
	logCacheMu sync.Mutex
	logCache   struct {
		path string
		size int64
		mod  time.Time
		ids  map[string]bool
	}
)

func loggedSessionIDs(path string, st os.FileInfo) map[string]bool {
	logCacheMu.Lock()
	defer logCacheMu.Unlock()
	if logCache.path == path && logCache.size == st.Size() && logCache.mod.Equal(st.ModTime()) {
		return logCache.ids
	}
	ids := map[string]bool{}
	if f, err := os.Open(path); err == nil {
		r := bufio.NewReaderSize(f, readBuffer)
		for {
			line, err := r.ReadBytes('\n')
			if bytes.Contains(line, []byte("shell.turn.inference_done")) {
				var rec unifiedRecord
				if json.Unmarshal(bytes.TrimSpace(line), &rec) == nil && rec.SID != "" {
					ids[rec.SID] = true
				}
			}
			if err != nil {
				break
			}
		}
		f.Close()
	}
	logCache.path, logCache.size, logCache.mod, logCache.ids = path, st.Size(), st.ModTime(), ids
	return ids
}

// num accepts a JSON number, a quoted number or null; Grok writes all three.
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

// parseTime accepts RFC 3339, "2006-01-02 15:04:05" and unix seconds or
// milliseconds, quoted or bare.
func parseTime(raw json.RawMessage) time.Time {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return time.Time{}
	}
	s = strings.Trim(s, `"`)
	if s == "" {
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
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return unixTime(f)
	}
	return time.Time{}
}

func unixTime(f float64) time.Time {
	if f <= 0 {
		return time.Time{}
	}
	if f >= 1e11 {
		return time.UnixMilli(int64(f)).UTC()
	}
	return time.Unix(int64(f), 0).UTC()
}

// timestampKey keeps the recording's own timestamp spelling in the dedup key,
// so events stay distinct even when two inferences share a millisecond.
func timestampKey(raw json.RawMessage, ts time.Time) string {
	key := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if key == "" || key == "null" {
		return strconv.FormatInt(ts.UnixMilli(), 10)
	}
	return key
}

func projectName(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Base(filepath.Clean(cwd))
}

func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func firstString(values []string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
