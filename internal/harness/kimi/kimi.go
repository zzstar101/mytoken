// Package kimi parses Kimi CLI wire logs.
//
// Roots: $MYTOKEN_KIMI_DIRS when set, else $KIMI_SHARE_DIR, else ~/.kimi; the
// sessions live under <share>/sessions:
//
//	<share>/sessions/<work-dir>/<session>/wire.jsonl
//	<share>/sessions/<work-dir>/<session>/subagents/<agent>/wire.jsonl
//
// A subagent log is attributed to the agent directory as its own session, with
// the surrounding session as its parent. Project names come from
// <share>/kimi.json (`work_dirs[].path`, keyed by md5 of the path, with an
// optional "<kaos>_<md5>" key for non-local kaos); when the work directory has
// no entry the directory name itself is used.
//
// Token normalization (docs/HARNESS.md §4): a `StatusUpdate` envelope carries
// token_usage/usage with input_cache_read (CacheRead) and input_cache_creation
// (CacheWrite) reported separately from the fresh input. When the log reports
// only the combined `input`, the cache counts are subtracted from it. Kimi
// reports no reasoning or cost split, and the model falls back to the
// configured default from $KIMI_MODEL_NAME / <share>/config.toml.
//
// Only metadata is read: no message text or tool payload ever leaves this
// package, except the first user message, which becomes the <=60 rune title.
package kimi

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
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
	kindJSONL        = "jsonl"
	fingerprintN     = 4096
	sessionsDirName  = "sessions"
	subagentsDirName = "subagents"
	wireName         = "wire.jsonl"
	autoModel        = "kimi-auto"
)

// Parser implements harness.Parser for the Kimi CLI.
type Parser struct {
	roots []string
}

// New returns a parser for the standard Kimi locations.
func New() *Parser { return &Parser{roots: DefaultRoots()} }

// NewWithRoots returns a parser reading the given sessions roots (tests).
func NewWithRoots(roots ...string) *Parser {
	return &Parser{roots: append([]string(nil), roots...)}
}

func init() { harness.Register(New()) }

// DefaultRoots returns the Kimi sessions roots for this machine.
func DefaultRoots() []string {
	if v := os.Getenv("MYTOKEN_KIMI_DIRS"); v != "" {
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
	if v := strings.TrimSpace(os.Getenv("KIMI_SHARE_DIR")); v != "" {
		return []string{filepath.Join(filepath.Clean(v), sessionsDirName)}
	}
	return []string{filepath.Join(harness.Home(), ".kimi", sessionsDirName)}
}

// Harness implements harness.Parser.
func (p *Parser) Harness() model.Harness { return model.Kimi }

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
		workDirs, err := os.ReadDir(root)
		if err != nil {
			continue // missing root is not an error
		}
		for _, workDir := range workDirs {
			if !workDir.IsDir() {
				continue
			}
			workDirPath := filepath.Join(root, workDir.Name())
			sessionDirs, err := os.ReadDir(workDirPath)
			if err != nil {
				continue
			}
			for _, sessionDir := range sessionDirs {
				if !sessionDir.IsDir() {
					continue
				}
				sessionPath := filepath.Join(workDirPath, sessionDir.Name())
				if isFile(filepath.Join(sessionPath, wireName)) {
					add(filepath.Join(sessionPath, wireName))
				}
				subagents, err := os.ReadDir(filepath.Join(sessionPath, subagentsDirName))
				if err != nil {
					continue
				}
				for _, agent := range subagents {
					if !agent.IsDir() {
						continue
					}
					path := filepath.Join(sessionPath, subagentsDirName, agent.Name(), wireName)
					if isFile(path) {
						add(path)
					}
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// state is the incremental parse state persisted in Cursor.Extra: facts that
// live in already-consumed bytes, plus the running record counter used to key
// records that carry no message_id.
type state struct {
	Title     string `json:"t,omitempty"`
	Project   string `json:"p,omitempty"`
	StartedAt string `json:"s,omitempty"`
	UpdatedAt string `json:"u,omitempty"`
	Index     int    `json:"i,omitempty"`
}

type wireRecord struct {
	Timestamp json.RawMessage `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	Message   *wireMessage    `json:"message"`
}

type wireMessage struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// envelope returns the record type and payload, unwrapping the optional
// "message" wrapper the CLI uses on some records.
func (r *wireRecord) envelope() (string, map[string]json.RawMessage) {
	typ, raw := r.Type, r.Payload
	if r.Message != nil {
		typ, raw = r.Message.Type, r.Message.Payload
	}
	if typ == "" || len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", nil
	}
	return typ, fields
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

	root, rel := p.locate(src.Path)
	workDir, sessionID, parentID := sourceIDs(rel)
	shareDir := shareDirOf(root)
	project := projectName(shareDir, workDir)
	configured := configuredModel(shareDir)

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

	acc := &accumulator{
		primary:    sessionID,
		parent:     parentID,
		project:    project,
		title:      st8.Title,
		index:      st8.Index,
		events:     map[string]int{},
		configured: configured,
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
		var rec wireRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // tolerate unknown/partial records
		}
		acc.handle(&b, &rec)
	}

	b.Sessions = acc.session()
	b.Next = harness.Cursor{
		Offset:      off,
		Size:        st.Size(),
		ModTime:     st.ModTime().UTC(),
		Fingerprint: fp,
		Extra:       acc.encodeState(),
	}
	return b, nil
}

// accumulator collects one batch for a single wire log.
type accumulator struct {
	primary    string
	parent     string
	project    string
	title      string
	started    time.Time
	updated    time.Time
	index      int
	configured string
	events     map[string]int
}

func (a *accumulator) handle(b *harness.Batch, rec *wireRecord) {
	typ, payload := rec.envelope()
	if typ == "" || payload == nil || typ == "metadata" {
		return
	}
	ts, ok := parseTime(rawTime(rec.Timestamp))
	if !ok {
		ts = a.started // a record without a timestamp inherits the session start
	}
	a.observe(ts)

	switch typ {
	case "TurnBegin", "SteerInput":
		if a.title == "" {
			if t := userText(payload); t != "" {
				a.title = harness.Title(t)
			}
		}
	case "StatusUpdate":
		a.handleStatus(b, payload, ts)
	}
}

func (a *accumulator) handleStatus(b *harness.Batch, payload map[string]json.RawMessage, ts time.Time) {
	usage := usageOf(payload)
	if usage == nil {
		return
	}
	cacheRead := numberField(usage, "input_cache_read", "cache_read_input_tokens", "cached_input_tokens")
	cacheWrite := numberField(usage, "input_cache_creation", "cache_creation_input_tokens")
	input := numberField(usage, "input_other", "input_tokens")
	if input == 0 {
		input = numberField(usage, "input") - cacheRead - cacheWrite
		if input < 0 {
			input = 0
		}
	}
	output := numberField(usage, "output", "output_tokens")
	if input == 0 && output == 0 && cacheRead == 0 && cacheWrite == 0 {
		return // no usage recorded for this record
	}

	id := stringField(payload, "message_id")
	index := a.index
	a.index++
	key := "kimi:" + a.primary + ":" + firstNonEmpty(id, strconv.Itoa(index))
	ev := model.UsageEvent{
		Harness:     model.Kimi,
		DedupKey:    key,
		RequestID:   id,
		SessionID:   a.primary,
		ParentID:    a.parent,
		ProjectPath: a.project,
		Timestamp:   ts,
		Model: firstNonEmpty(
			stringField(payload, "model"),
			stringField(payload, "model_name"),
			a.configured,
		),
		Provider: "kimi",
		Tokens: model.Tokens{
			Input:      input,
			Output:     output,
			CacheRead:  cacheRead,
			CacheWrite: cacheWrite,
		},
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

func (a *accumulator) session() []model.SessionMeta {
	if a.started.IsZero() && a.updated.IsZero() && a.title == "" && a.project == "" {
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
	return []model.SessionMeta{{
		Harness:   model.Kimi,
		SessionID: a.primary,
		ParentID:  a.parent,
		Title:     a.title,
		Project:   a.project,
		StartedAt: started,
		UpdatedAt: updated,
	}}
}

func (a *accumulator) encodeState() string {
	s := state{Title: a.title, Project: a.project, Index: a.index}
	if !a.started.IsZero() {
		s.StartedAt = a.started.UTC().Format(time.RFC3339Nano)
	}
	if !a.updated.IsZero() {
		s.UpdatedAt = a.updated.UTC().Format(time.RFC3339Nano)
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

// locate returns the sessions root that owns path plus the path relative to it.
func (p *Parser) locate(path string) (string, string) {
	for _, root := range p.roots {
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return root, rel
	}
	return filepath.Dir(filepath.Dir(path)), filepath.Base(path)
}

// sourceIDs derives the work directory, session id and parent session id from
// a path relative to the sessions root.
func sourceIDs(rel string) (workDir, sessionID, parentID string) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 {
		return "", "", ""
	}
	workDir = parts[0]
	sessionID = parts[1]
	if len(parts) >= 4 && parts[2] == subagentsDirName {
		// <work-dir>/<session>/subagents/<agent>/wire.jsonl
		sessionID, parentID = parts[3], parts[1]
	}
	return workDir, sessionID, parentID
}

// shareDirOf returns the Kimi home that owns a sessions root.
func shareDirOf(root string) string {
	if filepath.Base(root) == sessionsDirName {
		return filepath.Dir(root)
	}
	return root
}

// projectName maps a work directory name to a project name via kimi.json.
func projectName(shareDir, workDir string) string {
	raw, err := os.ReadFile(filepath.Join(shareDir, "kimi.json"))
	if err != nil {
		return workDir
	}
	var doc struct {
		WorkDirs []struct {
			Path string `json:"path"`
			Kaos string `json:"kaos"`
		} `json:"work_dirs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return workDir
	}
	for _, wd := range doc.WorkDirs {
		if wd.Path == "" {
			continue
		}
		name := pathBase(wd.Path)
		if name == "" {
			continue
		}
		sum := md5.Sum([]byte(wd.Path))
		hash := hex.EncodeToString(sum[:])
		if workDir == hash || (wd.Kaos != "" && wd.Kaos != "local" && workDir == wd.Kaos+"_"+hash) {
			return name
		}
	}
	return workDir
}

// pathBase mirrors projectNameFromPath: the last path component, else "kimi".
func pathBase(p string) string {
	cleaned := strings.TrimRight(p, `/\`)
	if base := filepath.Base(cleaned); base != "" && base != "." && base != string(filepath.Separator) {
		return base
	}
	if cleaned != "" {
		return cleaned
	}
	return "kimi"
}

var (
	reDefaultModel = regexp.MustCompile(`(?m)^\s*default_model\s*=\s*"?([^"\r\n]+?)"?\s*$`)
	reModelSection = regexp.MustCompile(`(?m)^\s*\[models\.(?:"([^"]+)"|'([^']+)'|([^\]]+))\]\s*$`)
	reModelField   = regexp.MustCompile(`(?m)^\s*model\s*=\s*"?([^"\r\n]+?)"?\s*$`)
)

// configuredModel resolves the model used when a record does not name one:
// $KIMI_MODEL_NAME, else the config.toml default_model, else "kimi-auto".
func configuredModel(shareDir string) string {
	if v := strings.TrimSpace(os.Getenv("KIMI_MODEL_NAME")); v != "" {
		return v
	}
	raw, err := os.ReadFile(filepath.Join(shareDir, "config.toml"))
	if err != nil {
		return autoModel
	}
	text := string(raw)
	m := reDefaultModel.FindStringSubmatch(text)
	if m == nil {
		return autoModel
	}
	key := strings.TrimSpace(m[1])
	if key == "" {
		return autoModel
	}
	for _, loc := range reModelSection.FindAllStringSubmatchIndex(text, -1) {
		name := ""
		for i := 2; i <= 6; i += 2 {
			if loc[i] >= 0 {
				name = text[loc[i]:loc[i+1]]
				break
			}
		}
		if strings.TrimSpace(name) != key {
			continue
		}
		if id := reModelField.FindStringSubmatch(text[loc[1]:]); id != nil {
			if v := strings.TrimSpace(id[1]); v != "" {
				return v
			}
		}
	}
	return key
}

// usageOf unwraps the token usage object of a payload.
func usageOf(payload map[string]json.RawMessage) map[string]json.RawMessage {
	for _, key := range []string{"token_usage", "usage"} {
		raw, ok := payload[key]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err == nil {
			return fields
		}
	}
	return nil
}

// userText extracts the user input of a TurnBegin/SteerInput envelope.
func userText(payload map[string]json.RawMessage) string {
	raw, ok := payload["user_input"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if t := strings.TrimSpace(p.Text); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, " ")
}

// numberField returns the first strictly positive numeric field, accepting
// numeric strings, like the reference implementation.
func numberField(fields map[string]json.RawMessage, keys ...string) int64 {
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var n float64
		if err := json.Unmarshal(raw, &n); err == nil {
			if n > 0 {
				return int64(n)
			}
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && v > 0 {
				return int64(v)
			}
		}
	}
	return 0
}

func stringField(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
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

// rawTime renders a timestamp field that may be an ISO string or an epoch
// number (seconds or milliseconds) as a string for parseTime.
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

func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
