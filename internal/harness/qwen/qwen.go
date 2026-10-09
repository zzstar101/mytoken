// Package qwen parses Qwen Code chat transcripts.
//
// Roots: $MYTOKEN_QWEN_DIRS when set, else $QWEN_DATA_DIR, else
// ~/.qwen/projects. Layout:
//
//	<root>/<project-slug>/chats/*.jsonl
//
// Each line is one JSON object. User turns carry message.parts text; assistant
// turns carry a Gemini-shaped usageMetadata block. Only the first real user
// message of a session is kept (as the session title); no other conversation
// content is read out of the log.
//
// Token normalization (docs/HARNESS.md §4): promptTokenCount includes
// cachedContentTokenCount, so the cached count is split out into CacheRead and
// subtracted from Input when totalTokenCount confirms that accounting. The
// project is the entry's cwd when present, else the project directory name.
package qwen

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
	kindJSONL    = "jsonl"
	fingerprintN = 4096
	chatsDirName = "chats"
)

// Parser implements harness.Parser for Qwen Code.
type Parser struct {
	roots []string
}

// New returns a parser for the standard Qwen Code locations.
func New() *Parser { return &Parser{roots: DefaultRoots()} }

// NewWithRoots returns a parser reading the given project roots (tests).
func NewWithRoots(roots ...string) *Parser {
	return &Parser{roots: append([]string(nil), roots...)}
}

func init() { harness.Register(New()) }

// DefaultRoots returns the Qwen Code project roots for this machine.
func DefaultRoots() []string {
	if v := os.Getenv("MYTOKEN_QWEN_DIRS"); v != "" {
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
	if v := strings.TrimSpace(os.Getenv("QWEN_DATA_DIR")); v != "" {
		return []string{filepath.Clean(v)}
	}
	return []string{filepath.Join(harness.Home(), ".qwen", "projects")}
}

// Harness implements harness.Parser.
func (p *Parser) Harness() model.Harness { return model.Qwen }

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
			chats := filepath.Join(root, proj.Name(), chatsDirName)
			entries, err := os.ReadDir(chats)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() || !isJSONL(e.Name()) {
					continue
				}
				path := filepath.Join(chats, e.Name())
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

// state is the incremental parse state persisted in Cursor.Extra: facts that
// live in already-consumed bytes, so a resumed parse still emits a complete
// SessionMeta.
type state struct {
	Title     string `json:"t,omitempty"`
	Project   string `json:"p,omitempty"`
	StartedAt string `json:"s,omitempty"`
	UpdatedAt string `json:"u,omitempty"`
}

// part is one entry of a message's parts array. Only the text of real (non
// thought) parts is ever looked at, and only to build the session title.
type part struct {
	Text    string `json:"text"`
	Thought bool   `json:"thought"`
}

type entry struct {
	UUID      string `json:"uuid"`
	SessionID string `json:"sessionId"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
	Message   *struct {
		Role  string `json:"role"`
		Parts []part `json:"parts"`
	} `json:"message"`
	UsageMetadata *struct {
		PromptTokenCount        int64 `json:"promptTokenCount"`
		CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
		TotalTokenCount         int64 `json:"totalTokenCount"`
		CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
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
		primary: strings.TrimSuffix(filepath.Base(src.Path), filepath.Ext(src.Path)),
		slug:    projectSlug(src.Path),
		events:  map[string]int{},
	}
	acc.title = st8.Title
	acc.project = st8.Project
	if t, err := time.Parse(time.RFC3339Nano, st8.StartedAt); err == nil {
		acc.started = t.UTC()
	}
	if t, err := time.Parse(time.RFC3339Nano, st8.UpdatedAt); err == nil {
		acc.updated = t.UTC()
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
		var e entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // tolerate unknown/partial records
		}
		acc.handle(&b, &e)
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

type accumulator struct {
	primary string
	slug    string
	title   string
	project string
	started time.Time
	updated time.Time
	events  map[string]int
}

func (a *accumulator) handle(b *harness.Batch, e *entry) {
	ts := parseTime(e.Timestamp)

	if e.Type == "user" && e.Message != nil {
		if a.title == "" {
			if text := userText(e.Message.Parts); text != "" {
				a.title = harness.Title(text)
			}
		}
	}
	if e.Cwd != "" && a.project == "" {
		a.project = e.Cwd
	}
	a.observe(ts)
	if ts.IsZero() {
		ts = a.started // a record without a timestamp inherits the session start
	}

	if e.Type != "assistant" || e.UsageMetadata == nil {
		return
	}
	u := e.UsageMetadata
	if u.PromptTokenCount == 0 && u.CandidatesTokenCount == 0 && u.ThoughtsTokenCount == 0 && u.CachedContentTokenCount == 0 {
		return
	}
	input := u.PromptTokenCount
	cached := u.CachedContentTokenCount
	// totalTokenCount == prompt + candidates + thoughts means prompt still
	// includes the cached prefix, so split it out to avoid double counting.
	if cached > 0 && u.TotalTokenCount != 0 &&
		u.TotalTokenCount == u.PromptTokenCount+u.CandidatesTokenCount+u.ThoughtsTokenCount &&
		u.TotalTokenCount != u.PromptTokenCount+u.CandidatesTokenCount+u.ThoughtsTokenCount+cached {
		input = u.PromptTokenCount - cached
		if input < 0 {
			input = 0
		}
	}
	tk := model.Tokens{
		Input:     input,
		Output:    u.CandidatesTokenCount,
		CacheRead: cached,
		Reasoning: u.ThoughtsTokenCount,
	}
	if tk.IsZero() {
		return
	}
	sid := firstNonEmpty(e.SessionID, a.primary)
	key := "qwen:" + sid + ":" + firstNonEmpty(e.UUID, e.Timestamp+":"+e.Model)
	ev := model.UsageEvent{
		Harness:     model.Qwen,
		DedupKey:    key,
		RequestID:   e.UUID,
		SessionID:   sid,
		ProjectPath: a.projectPath(),
		Timestamp:   ts,
		Model:       firstNonEmpty(e.Model, "qwen-auto"),
		Provider:    "qwen",
		Tokens:      tk,
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

// projectPath prefers the transcript's cwd, then the project directory name.
func (a *accumulator) projectPath() string {
	if a.project != "" {
		return a.project
	}
	return a.slug
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
		Harness:   model.Qwen,
		SessionID: a.primary,
		Title:     a.title,
		Project:   a.projectPath(),
		StartedAt: started,
		UpdatedAt: updated,
	}}
}

func (a *accumulator) encodeState() string {
	s := state{Title: a.title, Project: a.project}
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

// userText joins the real (non-thought) text parts of a user turn.
func userText(parts []part) string {
	var out []string
	for _, part := range parts {
		if part.Thought {
			continue
		}
		if t := strings.TrimSpace(part.Text); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, " ")
}

// projectSlug mirrors the CLI's project directory naming: "-Users-dev-proj"
// becomes "proj".
func projectSlug(path string) string {
	dir := filepath.Base(filepath.Dir(filepath.Dir(path))) // <root>/<slug>/chats/file.jsonl
	if dir == "" || dir == "." || dir == string(filepath.Separator) {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(dir, "-"), "-")
	return parts[len(parts)-1]
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

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999Z0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
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
