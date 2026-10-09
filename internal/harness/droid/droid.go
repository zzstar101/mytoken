// Package droid parses Factory Droid (Droid CLI) session transcripts.
//
// Droid keeps one directory per session day under <factory>/sessions:
//
//	<factory>/sessions/<day>/<session>.jsonl
//	<factory>/sessions/<day>/<session>.settings.json
//
// <factory> is $FACTORY_DIR when set, else ~/.factory. $MYTOKEN_DROID_DIRS
// overrides the whole root set (filepath.SplitList, one root per entry), which
// is how tests and multi-install machines point at extra trees. Only Unix and
// Windows home layouts matter here: Droid has no XDG or %APPDATA% variant, and
// %USERPROFILE% is what ~/.factory resolves to on Windows.
//
// The transcript is an append-only JSONL stream. Its first record is a
// "session_start" entry carrying the session id and the working directory;
// later records are user, assistant and tool messages. The transcript never
// carries token counts. Droid records usage once per session in the sibling
// settings file, under
// tokenUsage{inputTokens,outputTokens,cacheCreationTokens,cacheReadTokens,thinkingTokens}.
//
// Usage therefore arrives a session at a time, exactly like Crush: this parser
// diffs the recorded cumulative counters against a high-water baseline kept in
// the cursor and emits one event per growth, keyed by the totals it reports
// (droid:<session>:<input>:<output>:<cacheWrite>:<cacheRead>:<reasoning>), so
// every delta has a stable identity and no delta is ever counted twice.
//
// codeburn (MIT, see THIRD_PARTY_NOTICES), which this layout was ported from,
// spreads a session's totals evenly across its assistant calls. That split is
// an estimate and MyToken records only the numbers a tool itself wrote, so the
// totals stay session-scoped here. For the same reason no cost is invented:
// CostUSD is left unset because Droid records no price.
//
// The only conversation text that reaches the snapshot is the session title,
// which is the first user message. Assistant replies, tool output and any
// later user turns are read past, never stored.
package droid

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	// sessionsDirName is the directory Droid keeps its session tree in.
	sessionsDirName = "sessions"
	// transcriptExt is the transcript suffix; its settings sibling replaces it
	// with settingsExt.
	transcriptExt = ".jsonl"
	settingsExt   = ".settings.json"
	// sessionStartType is the record type that opens a transcript.
	sessionStartType = "session_start"
	// systemReminderTag opens Droid's injected context blocks; those are not
	// user messages and never become a title.
	systemReminderTag = "<system-reminder>"

	// fingerprintBytes is how much of a file the change fingerprint covers.
	fingerprintBytes = 4096
	// readBuffer is the buffered reader size for JSONL streams.
	readBuffer = 64 << 10
)

// Parser implements harness.Parser for Droid.
type Parser struct {
	roots []string
}

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

// New returns a parser over the default roots.
func New() *Parser { return NewWithRoots(defaultRoots()...) }

// NewWithRoot returns a parser over a single root.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser over the given roots.
func NewWithRoots(roots ...string) *Parser {
	p := &Parser{}
	seen := make(map[string]bool, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
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
	return p
}

// defaultRoots resolves the Droid data directory.
func defaultRoots() []string {
	if dirs := strings.TrimSpace(os.Getenv("MYTOKEN_DROID_DIRS")); dirs != "" {
		return filepath.SplitList(dirs)
	}
	if dir := strings.TrimSpace(os.Getenv("FACTORY_DIR")); dir != "" {
		return []string{dir}
	}
	return []string{filepath.Join(harness.Home(), ".factory")}
}

// Harness reports the harness this parser reads.
func (p *Parser) Harness() model.Harness { return model.Droid }

// Roots reports the configured roots.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover finds every Droid transcript under the roots. A file counts as a
// transcript only when its first record is a session_start entry, which is the
// same gate codeburn applies. Sessions whose working directory is the factory
// directory itself are Droid's own bookkeeping (its internal sessions) and are
// skipped.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	var out []harness.Source
	seen := make(map[string]bool)
	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, harness.Source{Path: path, Kind: kindJSONL})
	}
	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sessionsDir := filepath.Join(root, sessionsDirName)
		days, err := os.ReadDir(sessionsDir)
		if err != nil {
			continue
		}
		for _, day := range days {
			if !day.IsDir() {
				continue
			}
			files, err := os.ReadDir(filepath.Join(sessionsDir, day.Name()))
			if err != nil {
				continue
			}
			for _, file := range files {
				if file.IsDir() || !strings.HasSuffix(file.Name(), transcriptExt) {
					continue
				}
				path := filepath.Join(sessionsDir, day.Name(), file.Name())
				start, ok := readSessionStart(path)
				if !ok {
					continue
				}
				cwd := start.CWD
				if cwd == "" {
					cwd = day.Name()
				}
				if samePath(cwd, root) {
					continue
				}
				add(path)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Parse folds new transcript records and the session's recorded token totals.
//
// The transcript and its settings file change independently: Droid appends to
// the transcript while it works and rewrites settings.json when a turn
// finishes. A source is therefore only a no-op when both are unchanged, and
// usage is diffed from the settings file even when no new record arrived.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return harness.Batch{}, err
	}
	st, err := os.Stat(src.Path)
	if err != nil || st.IsDir() {
		// Droid deletes transcripts it no longer needs; keep the cursor.
		return harness.Batch{Next: cur}, nil
	}
	fp, err := fingerprint(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	state := decodeState(cur)
	settingsRaw, settingsSig := readSettings(settingsPathFor(src.Path))
	same := cur.Fingerprint != "" && cur.Fingerprint == fp && cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime())
	if same && state.Settings == settingsSig {
		return harness.Batch{Next: cur}, nil
	}
	offset := cur.Offset
	if cur.Fingerprint == "" || (cur.Fingerprint != fp && st.Size() <= cur.Size) {
		// Fresh cursor, or the transcript was rewound or rewritten in place:
		// read it from the start with a fresh transcript context. The token
		// baselines stay, because reported usage never rewinds.
		offset = 0
		state = resetTranscript(state)
	}
	lines, next, err := readNewLines(src.Path, offset)
	if err != nil {
		return harness.Batch{}, err
	}
	acc := &accumulator{
		state:    state,
		fallback: transcriptSessionID(src.Path),
		metas:    make(map[string]model.SessionMeta),
	}
	for _, raw := range lines {
		acc.line(raw)
	}
	acc.applySettings(settingsRaw)
	return acc.batch(st, fp, next, settingsSig), nil
}

// kindJSONL is the source kind of a transcript file.
const kindJSONL = "jsonl"

// cursorState is the parser memory carried between runs in Cursor.Extra.
type cursorState struct {
	SessionID string `json:"session,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	Title     string `json:"title,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
	LastTS    string `json:"lastTs,omitempty"`
	Model     string `json:"model,omitempty"`
	TotalIn   int64  `json:"in,omitempty"`
	TotalOut  int64  `json:"out,omitempty"`
	TotalCW   int64  `json:"cacheWrite,omitempty"`
	TotalCR   int64  `json:"cacheRead,omitempty"`
	TotalR    int64  `json:"reasoning,omitempty"`
	// Settings is the content signature of the settings file, so a rewrite is
	// noticed even when no record was appended.
	Settings string `json:"settings,omitempty"`
}

func decodeState(cur harness.Cursor) cursorState {
	var state cursorState
	if cur.Extra != "" {
		_ = json.Unmarshal([]byte(cur.Extra), &state)
	}
	return state
}

func encodeState(state cursorState) string {
	raw, err := json.Marshal(state)
	if err != nil {
		return ""
	}
	return string(raw)
}

// resetTranscript clears the transcript-derived context while keeping the
// token baselines.
func resetTranscript(state cursorState) cursorState {
	state.SessionID, state.CWD, state.Title, state.StartedAt, state.LastTS = "", "", "", "", ""
	return state
}

// transcriptEntry is one record of a Droid transcript.
type transcriptEntry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Timestamp json.RawMessage `json:"timestamp"`
	CWD       string          `json:"cwd"`
	Message   *transcriptText `json:"message"`
}

// transcriptText is a message record's body.
type transcriptText struct {
	Role    string          `json:"role"`
	Content []transcriptBlk `json:"content"`
}

type transcriptBlk struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// accumulator folds one parse pass into events and session metadata.
type accumulator struct {
	state    cursorState
	fallback string
	events   eventSet
	metas    map[string]model.SessionMeta
	lines    int
}

// sessionID prefers the id written by session_start, falling back to the
// transcript file name so every event has a stable session.
func (a *accumulator) sessionID() string {
	if a.state.SessionID != "" {
		return a.state.SessionID
	}
	return a.fallback
}

// line folds one transcript record.
func (a *accumulator) line(raw []byte) {
	var entry transcriptEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return
	}
	a.lines++
	if entry.Type == sessionStartType {
		if entry.ID != "" {
			a.state.SessionID = entry.ID
		}
		if entry.CWD != "" {
			a.state.CWD = entry.CWD
		}
		if ts := parseTimestamp(entry.Timestamp); !ts.IsZero() && a.state.StartedAt == "" {
			a.state.StartedAt = formatTime(ts)
		}
	}
	if entry.Message != nil && strings.EqualFold(entry.Message.Role, "user") && a.state.Title == "" {
		if text := messageText(entry.Message.Content); text != "" {
			a.state.Title = harness.Title(text)
		}
	}
	if ts := parseTimestamp(entry.Timestamp); !ts.IsZero() {
		if prev := parseTimeString(a.state.LastTS); prev.IsZero() || ts.After(prev) {
			a.state.LastTS = formatTime(ts)
		}
	}
}

// applySettings folds the session's recorded cumulative counters. A growth is
// reported once; the baseline is a high-water mark, so a counter that moves
// backwards never reports a negative delta.
func (a *accumulator) applySettings(raw []byte) {
	if len(raw) == 0 {
		return
	}
	var settings droidSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return
	}
	if name := normalizeModel(settings.Model); name != "" {
		a.state.Model = name
	}
	usage := settings.TokenUsage
	if usage == nil {
		return
	}
	in := nonNegative(int64(usage.InputTokens))
	out := nonNegative(int64(usage.OutputTokens))
	cacheWrite := nonNegative(int64(usage.CacheCreationTokens))
	cacheRead := nonNegative(int64(usage.CacheReadTokens))
	reasoning := nonNegative(int64(usage.ThinkingTokens))
	ts := a.eventTime()
	if ts.IsZero() {
		// Nothing recorded a clock yet. Leave the baselines alone so the growth
		// is reported once a timestamp exists.
		return
	}
	delta := model.Tokens{
		Input:      grow(in, &a.state.TotalIn),
		Output:     grow(out, &a.state.TotalOut),
		CacheWrite: grow(cacheWrite, &a.state.TotalCW),
		CacheRead:  grow(cacheRead, &a.state.TotalCR),
		Reasoning:  grow(reasoning, &a.state.TotalR),
	}
	if delta == (model.Tokens{}) {
		return
	}
	session := a.sessionID()
	a.events.add(model.UsageEvent{
		Harness:     model.Droid,
		DedupKey:    fmt.Sprintf("droid:%s:%d:%d:%d:%d:%d", session, in, out, cacheWrite, cacheRead, reasoning),
		SessionID:   session,
		ProjectPath: a.state.CWD,
		Timestamp:   ts,
		Model:       a.state.Model,
		Tokens:      delta,
	})
}

// eventTime is the newest recorded clock: the last transcript record, else the
// session start. Droid writes no per-request time of its own.
func (a *accumulator) eventTime() time.Time {
	if ts := parseTimeString(a.state.LastTS); !ts.IsZero() {
		return ts
	}
	return parseTimeString(a.state.StartedAt)
}

// batch assembles the parse result and the next cursor.
func (a *accumulator) batch(st os.FileInfo, fp string, offset int64, settingsSig string) harness.Batch {
	if session := a.sessionID(); session != "" {
		started := parseTimeString(a.state.StartedAt)
		updated := parseTimeString(a.state.LastTS)
		if !started.IsZero() || !updated.IsZero() {
			if updated.IsZero() {
				updated = started
			}
			if started.IsZero() {
				started = updated
			}
			a.metas[session] = model.SessionMeta{
				Harness:   model.Droid,
				SessionID: session,
				Title:     a.state.Title,
				Project:   projectName(a.state.CWD),
				StartedAt: started,
				UpdatedAt: updated,
			}
		}
	}
	sessions := make([]model.SessionMeta, 0, len(a.metas))
	for _, meta := range a.metas {
		sessions = append(sessions, meta)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].SessionID < sessions[j].SessionID })
	a.state.Settings = settingsSig
	return harness.Batch{
		Events:   a.events.list(),
		Sessions: sessions,
		Next: harness.Cursor{
			Offset:      offset,
			Size:        st.Size(),
			ModTime:     st.ModTime(),
			Fingerprint: fp,
			Extra:       encodeState(a.state),
		},
	}
}

// eventSet keeps one event per dedup key, last write wins.
type eventSet struct {
	order []string
	byKey map[string]model.UsageEvent
}

func (s *eventSet) add(event model.UsageEvent) {
	if s.byKey == nil {
		s.byKey = make(map[string]model.UsageEvent)
	}
	if _, ok := s.byKey[event.DedupKey]; !ok {
		s.order = append(s.order, event.DedupKey)
	}
	s.byKey[event.DedupKey] = event
}

func (s *eventSet) list() []model.UsageEvent {
	if len(s.order) == 0 {
		return nil
	}
	out := make([]model.UsageEvent, 0, len(s.order))
	for _, key := range s.order {
		out = append(out, s.byKey[key])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DedupKey < out[j].DedupKey })
	return out
}

// droidSettings is the subset of a session's settings file MyToken reads.
type droidSettings struct {
	Model      string       `json:"model"`
	TokenUsage *droidTokens `json:"tokenUsage"`
}

type droidTokens struct {
	InputTokens         count `json:"inputTokens"`
	OutputTokens        count `json:"outputTokens"`
	CacheCreationTokens count `json:"cacheCreationTokens"`
	CacheReadTokens     count `json:"cacheReadTokens"`
	ThinkingTokens      count `json:"thinkingTokens"`
}

// count is a token counter. Droid writes numbers, but a few builds quote them,
// so both spellings are accepted; anything unusable counts as zero.
type count int64

func (c *count) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil
		}
		*c = count(int64(value))
		return nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	if math.IsNaN(value) || value < 0 {
		return nil
	}
	*c = count(int64(value))
	return nil
}

// readSessionStart reads a transcript's first record and reports whether it
// opens a session.
func readSessionStart(path string) (transcriptEntry, bool) {
	file, err := os.Open(path)
	if err != nil {
		return transcriptEntry{}, false
	}
	defer file.Close()
	raw, _ := bufio.NewReaderSize(file, readBuffer).ReadBytes('\n')
	if len(raw) == 0 {
		return transcriptEntry{}, false
	}
	var entry transcriptEntry
	if err := json.Unmarshal(bytes.TrimSpace(raw), &entry); err != nil {
		return transcriptEntry{}, false
	}
	if entry.Type != sessionStartType {
		return transcriptEntry{}, false
	}
	return entry, true
}

// readNewLines reads the complete JSONL records that start at offset and
// reports where the next read should resume. A trailing partial line is left
// for the next run.
func readNewLines(path string, offset int64) ([][]byte, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	reader := bufio.NewReaderSize(file, readBuffer)
	var lines [][]byte
	pos := offset
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 && raw[len(raw)-1] == '\n' {
			pos += int64(len(raw))
			if line := bytes.TrimSpace(raw); len(line) > 0 {
				lines = append(lines, line)
			}
		}
		if err != nil {
			break
		}
	}
	return lines, pos, nil
}

// fingerprint hashes the head of a file. Append-only transcripts keep the same
// fingerprint as they grow.
func fingerprint(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	buf := make([]byte, fingerprintBytes)
	n, err := io.ReadFull(file, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	sum := sha1.Sum(buf[:n])
	return hex.EncodeToString(sum[:]), nil
}

// readSettings returns the settings file's bytes and a content signature. The
// signature is content-based rather than mtime-based so it stays meaningful
// across materializations of the same fixture.
func readSettings(path string) ([]byte, string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, ""
	}
	sum := sha1.Sum(raw)
	return raw, fmt.Sprintf("%d:%s", len(raw), hex.EncodeToString(sum[:]))
}

// settingsPathFor maps a transcript path to its settings sibling.
func settingsPathFor(path string) string {
	return strings.TrimSuffix(path, transcriptExt) + settingsExt
}

// transcriptSessionID is the session id implied by a transcript file name.
func transcriptSessionID(path string) string {
	name := filepath.Base(path)
	return strings.TrimSuffix(name, transcriptExt)
}

// messageText joins the text blocks of a message.
func messageText(blocks []transcriptBlk) string {
	var parts []string
	for _, block := range blocks {
		if block.Type != "text" {
			continue
		}
		text := strings.TrimSpace(block.Text)
		if text == "" || strings.HasPrefix(text, systemReminderTag) {
			continue
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, " ")
}

// normalizeModel strips the decorations Droid's router wraps around a model id:
// the "custom:" prefix, bracketed routing tags and the attempt number the
// wrapper appends ("custom:GLM-5.1-[Proxy]-0" -> "GLM-5.1").
//
// codeburn strips a trailing "-<digits>" unconditionally, which also shortens
// real ids such as "claude-sonnet-4-5". MyToken's Model is the id used for
// pricing, so the attempt suffix is dropped only when the name was actually
// decorated; an undecorated id is kept exactly as recorded.
var (
	bracketTag  = regexp.MustCompile(`\[[^\]]*\]`)
	attemptTail = regexp.MustCompile(`-\d+$`)
)

func normalizeModel(raw string) string {
	name := strings.TrimSpace(raw)
	decorated := false
	if strings.HasPrefix(name, "custom:") {
		name = strings.TrimPrefix(name, "custom:")
		decorated = true
	}
	if bracketTag.MatchString(name) {
		name = bracketTag.ReplaceAllString(name, "")
		decorated = true
	}
	if decorated {
		name = attemptTail.ReplaceAllString(name, "")
	}
	return strings.Trim(strings.TrimSpace(name), "-")
}

// projectName is the last path element of a working directory, with Windows
// separators accepted.
func projectName(cwd string) string {
	trimmed := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(cwd), "\\", "/"), "/")
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
		return trimmed[idx+1:]
	}
	return trimmed
}

// samePath compares two directory spellings.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return filepath.ToSlash(filepath.Clean(a)) == filepath.ToSlash(filepath.Clean(b))
}

// grow raises base to total and returns the increase.
func grow(total int64, base *int64) int64 {
	if total <= *base {
		return 0
	}
	delta := total - *base
	*base = total
	return delta
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

// parseTimestamp reads a timestamp that may be a string or a number.
func parseTimestamp(raw json.RawMessage) time.Time {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return time.Time{}
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return time.Time{}
		}
		return parseTimeString(text)
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return time.Time{}
	}
	return fromUnixNumber(value)
}

// parseTimeString reads RFC 3339, a zone-less SQL-style stamp (treated as UTC)
// or a numeric unix stamp.
func parseTimeString(raw string) time.Time {
	text := strings.TrimSpace(raw)
	if text == "" {
		return time.Time{}
	}
	if value, err := strconv.ParseFloat(text, 64); err == nil {
		return fromUnixNumber(value)
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05.999999999",
	} {
		if ts, err := time.Parse(layout, text); err == nil {
			return ts.UTC()
		}
	}
	return time.Time{}
}

// fromUnixNumber reads a unix stamp in seconds or milliseconds.
func fromUnixNumber(value float64) time.Time {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return time.Time{}
	}
	if value < 1e12 {
		seconds := math.Floor(value)
		return time.Unix(int64(seconds), int64((value-seconds)*1e9)).UTC()
	}
	return time.UnixMilli(int64(value)).UTC()
}

// formatTime renders a timestamp for the cursor, empty when zero.
func formatTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339Nano)
}
