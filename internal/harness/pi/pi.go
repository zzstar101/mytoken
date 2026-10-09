// Package pi parses Pi coding-agent session logs.
//
// # Log layout
//
//	~/.pi/agent/sessions/<cwd-slug>/<timestamp>_<session-id>.jsonl      (top level)
//	~/.pi/agent/sessions/<cwd-slug>/<timestamp>_<session-id>/forks/<timestamp>_<session-id>.jsonl
//	~/.pi/agent/sessions/<cwd-slug>/<timestamp>_<session-id>/<child-id>/run-N/session.jsonl
//
// Plain, uncompressed JSONL, so it is resumed by byte offset.
//
// # Two log shapes
//
// Session logs (the vast majority) start with a session header; anything else is
// ignored by Discover. The exception is the async-subagent transcript of a
// background "reviewer" run, which is stored next to its siblings as
//
//	<slug>/subagent-artifacts/<run-id>_<agent>_transcript.jsonl
//
// and is a JSONL log of its own: its records are
// {"version":1,"recordType":"message","source":"async","runId":"<uuid>",
// "agent":"reviewer","cwd":"/path","ts":<ms>,"timestamp":"<RFC3339>",
// "sourceEventType":"initial_prompt"|"message_end"|"tool_start"|"tool_end",
// "role":..., "model"?, "text"?, "usage"?} — i.e. the usage object sits at the
// TOP level of the record instead of under "message". Those 91 assistant
// message_end records of the reference corpus appear in no session log at all
// (their (input, output, ts) tuples match nothing), so they are counted here;
// skipping them would undercount ~10.5M tokens. The sibling
// <run-id>_<agent>_meta.json is only read for the provider named by its "model"
// field (e.g. "rn" out of "rn/gpt-6.1-sol:high"); its aggregate "usage" is
// deliberately NOT counted (it would double count the transcript) and its
// "task" prompt is never read.
//
// # Record schema
//
// Line 1 is the session header:
//
//	{"type":"session","version":3,"id":"<uuidv7>","timestamp":"<RFC3339>",
//	 "cwd":"/path","parentSession":"/abs/path/to/parent.jsonl"?}
//
// Every later line is {"type":"...","id":"<hex>","parentId":"<id|null>",
// "timestamp":"<RFC3339>", ...}. Record types seen in the wild: message,
// model_change, thinking_level_change, session_info, custom_message,
// context_edit, compaction and custom.
//
// Only one shape carries usage: a record of type "message" whose
// message.role == "assistant" and which has message.usage:
//
//	message.usage = {input, output, cacheRead, cacheWrite, totalTokens,
//	                 cost:{input,output,cacheRead,cacheWrite,total},
//	                 reasoning?, cacheWrite1h?}
//	message.provider, message.model (or message.responseModel),
//	message.responseId ("resp_<hex>"), message.api, message.timestamp (ms epoch)
//
// compaction records hold a summary but no usage, so they are skipped.
//
// # Token accounting
//
// Verified over all 14257 usage records of the reference corpus:
// totalTokens == input + output + cacheRead + cacheWrite for every record, and
// adding reasoning breaks that identity for the 9769 records with
// reasoning > 0. So input already excludes cacheRead and output already
// excludes reasoning; no normalization is needed and none is applied:
//
//	Input      = input        (excludes cacheRead/cacheWrite)
//	Output     = output       (excludes reasoning)
//	CacheRead  = cacheRead
//	CacheWrite = cacheWrite
//	Reasoning  = reasoning    (separate, additive)
//
// usage.cacheWrite1h is a subset of cacheWrite that totalTokens already counts
// (and that is always 0 in the reference corpus), so it is deliberately ignored
// to avoid double counting. CostUSD is usage.cost.total when it is non-zero.
//
// # Deduplication
//
// Pi rewrites a message every time a streamed response grows, so the same
// message appears up to five times (15895 assistant records vs 14391 distinct
// ids). DedupKey is message.responseId when present, else the record id, else
// "<session>#<timestamp>"; the last copy wins downstream.
//
// # Sessions
//
// Top-level sessions have no parentSession. A fork header carries
// parentSession as the parent log's absolute path, so ParentID is the session
// id parsed out of that file name. Subagent runs have no parentSession at all;
// their parent is the session directory they live under, so ParentID is parsed
// from the ancestor directory name "<timestamp>_<session-id>". An async-run
// transcript has no recorded parent either (its meta file names none), so its
// ParentID stays empty. The title is the first real user message: role ==
// "user" with non-empty text, skipping plugin-injected "<system-reminder>"
// blocks and slash commands such as "/compact" (a dragged-in path like
// "/Users/…" is not a command and is kept). For an async run the title comes
// from the initial_prompt record. Provider and model are only set when the log
// names them; no model→provider inference is done.
package pi

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
	"io/fs"
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
	// fingerprintBytes is how much of a file is hashed into Cursor.Fingerprint.
	fingerprintBytes = 4096
	// readBuffer is the line reader's buffer size (it still grows per line).
	readBuffer = 64 << 10
	// headerLimit caps how much of the first line Discover inspects.
	headerLimit = 64 << 10

	kindJSONL = "jsonl"
)

// Parser reads Pi session logs. It holds no mutable state and is safe for
// concurrent use.
type Parser struct{ roots []string }

// New returns the parser for the default root: $PI_HOME/sessions, falling back
// to ~/.pi/agent/sessions.
func New() *Parser { return NewWithRoots() }

// NewWithRoot returns a parser rooted at root. It exists for tests and for
// tools that keep logs outside the home directory.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser over the given roots. An empty list falls back
// to the default root.
func NewWithRoots(roots ...string) *Parser {
	cp := make([]string, 0, len(roots))
	for _, r := range roots {
		if r != "" {
			cp = append(cp, r)
		}
	}
	if len(cp) == 0 {
		cp = []string{filepath.Join(harness.EnvOr("PI_HOME", ".pi", "agent"), "sessions")}
	}
	return &Parser{roots: cp}
}

func (p *Parser) Harness() model.Harness { return model.Pi }

// Roots returns the directories to watch.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

// Discover walks every root and returns one Source per session log, sorted by
// path so scans are deterministic.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	var out []harness.Source
	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		werr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// Unreadable entries (permissions, races with deletion) are
				// skipped; a missing root is simply not discovered.
				return nil
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}
			kind, ok := piLogKind(path)
			if !ok {
				return nil
			}
			out = append(out, harness.Source{Path: path, Kind: kind})
			return nil
		})
		if werr != nil && !errors.Is(werr, fs.SkipAll) {
			return nil, werr
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// piLogKind classifies a JSONL file by its first line: a Pi session log, an
// async-subagent transcript, or something else entirely. This is what keeps
// artifact mirrors and unrelated JSONL files out of the scan.
func piLogKind(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, headerLimit)
	line, err := br.ReadSlice('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", false // a header line is tiny
		}
		return "", false
	}
	var r piRecord
	if json.Unmarshal(bytes.TrimSpace(line), &r) != nil {
		return "", false
	}
	switch {
	case r.Type == "session":
		return kindJSONL, true
	case r.RecordType == "message" && r.Source == "async":
		return kindJSONL, true
	}
	return "", false
}

// Parse reads one session log incrementally from cur.Offset. A trailing
// incomplete line is never consumed and never counted.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return harness.Batch{}, err
	}
	st, err := os.Stat(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	fp, err := fingerprint(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	if unchanged(cur, st, fp) {
		return harness.Batch{Next: cur}, nil
	}
	off := cur.Offset
	// A shrunken file, an offset past EOF or a different fingerprint (the file
	// was rewritten/truncated) means the log no longer matches the cursor.
	if off < 0 || off > st.Size() || (cur.Fingerprint != "" && cur.Fingerprint != fp) {
		off = 0
	}
	f, err := os.Open(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	defer f.Close()
	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return harness.Batch{}, err
		}
	}
	s := newSession(p.rootFor(src.Path), src.Path)
	if strings.Contains(filepath.Base(src.Path), "_transcript.jsonl") {
		s.provider = asyncMetaProvider(src.Path)
	}
	n, rerr := readCompleteLines(f, s.consume)
	b := s.batch()
	b.Next = harness.Cursor{
		Offset:      off + n,
		Size:        st.Size(),
		ModTime:     st.ModTime(),
		Fingerprint: fp,
		Extra:       kindJSONL,
	}
	if rerr != nil {
		// Keep whatever complete lines were read and retry from there.
		return b, nil
	}
	return b, nil
}

// rootFor returns the configured root that contains path, or "" when none does.
func (p *Parser) rootFor(path string) string {
	best := ""
	for _, r := range p.roots {
		if r == "" {
			continue
		}
		rel, err := filepath.Rel(r, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if len(r) > len(best) {
			best = r
		}
	}
	return best
}

func unchanged(cur harness.Cursor, st os.FileInfo, fp string) bool {
	return cur.Fingerprint != "" && cur.Fingerprint == fp &&
		cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime())
}

// fingerprint returns the sha1 of the first fingerprintBytes bytes.
func fingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.CopyN(h, f, fingerprintBytes); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readCompleteLines feeds every '\n'-terminated line of r to fn and returns the
// number of bytes consumed, which always ends on a line boundary. A trailing
// chunk without '\n' is never passed to fn and never counted, so a partially
// written record is left for the next scan.
func readCompleteLines(r io.Reader, fn func(line []byte) error) (int64, error) {
	br := bufio.NewReaderSize(r, readBuffer)
	var n int64
	for {
		line, err := br.ReadBytes('\n')
		if err == nil {
			if e := fn(line[:len(line)-1]); e != nil {
				return n, e
			}
			n += int64(len(line))
			continue
		}
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		return n, err
	}
}

// ---------------------------------------------------------------------------
// session accumulation (one file == one session)
// ---------------------------------------------------------------------------

// session accumulates the events and metadata of a single session log.
type session struct {
	id        string
	parentID  string
	project   string
	provider  string // async transcripts: read from the run's meta file
	userTitle string
	startedAt time.Time
	updatedAt time.Time
	events    []model.UsageEvent
}

func newSession(root, path string) *session {
	// Fallbacks for a log whose header line has not been written yet: the file
	// name carries the id for top-level and fork logs, the relative directory
	// does for run logs, and an async transcript is named after its run id.
	id := idFromSessionPath(path)
	if id == "" {
		if base := filepath.Base(path); strings.HasSuffix(base, "_transcript.jsonl") {
			if rid := strings.TrimSuffix(base, "_transcript.jsonl"); looksLikeSessionID(rid) {
				id = rid
			}
		}
	}
	if id == "" {
		rel := filepath.Base(filepath.Dir(path))
		if r, err := filepath.Rel(root, filepath.Dir(path)); err == nil &&
			r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			rel = r
		}
		id = rel
	}
	s := &session{id: id}
	// Run logs and (rarely) fork logs carry no parentSession; their parent is
	// the session directory they live under.
	base := filepath.Base(path)
	if base == "session.jsonl" || filepath.Base(filepath.Dir(path)) == "forks" {
		s.parentID = parentFromPath(root, path)
	}
	return s
}

func (s *session) consume(line []byte) error {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	var r piRecord
	if err := json.Unmarshal(line, &r); err != nil {
		return nil // partial or foreign record: skip
	}
	if t := parseTS(r.Timestamp); !t.IsZero() && t.After(s.updatedAt) {
		s.updatedAt = t
	}
	// Async subagent runs use a record shape of their own: no "type", a
	// "recordType"/"sourceEventType" pair, and a top-level usage object.
	if r.RecordType == "message" && r.Source == "async" {
		s.consumeAsync(r)
		return nil
	}
	switch r.Type {
	case "session":
		if r.ID != "" {
			s.id = r.ID
		}
		if r.Cwd != "" {
			s.project = r.Cwd
		}
		if ps := r.ParentSession; ps != "" {
			// A fork header points at the parent log's path, not its id.
			if id := idFromSessionPath(ps); id != "" {
				s.parentID = id
			}
		}
		if t := parseTS(r.Timestamp); !t.IsZero() && (s.startedAt.IsZero() || t.Before(s.startedAt)) {
			s.startedAt = t
		}

	case "message":
		m := r.Message
		if m == nil {
			break
		}
		if mt := msTime(m.Timestamp); !mt.IsZero() && mt.After(s.updatedAt) {
			s.updatedAt = mt
		}
		switch {
		case strings.EqualFold(m.Role, "assistant") && m.Usage != nil:
			if m.Usage.IsZero() {
				break
			}
			ts := msTime(m.Timestamp)
			if ts.IsZero() {
				ts = parseTS(r.Timestamp)
			}
			if ts.IsZero() {
				ts = s.startedAt
			}
			mdl := m.Model
			if mdl == "" {
				mdl = m.ResponseModel
			}
			s.events = append(s.events, model.UsageEvent{
				Harness:     model.Pi,
				DedupKey:    firstNonEmpty(m.ResponseID, r.ID, s.id+"#"+string(r.Timestamp)),
				SessionID:   s.id,
				ParentID:    s.parentID,
				ProjectPath: s.project,
				Timestamp:   ts,
				Model:       mdl,
				Provider:    m.Provider,
				Tokens:      m.Usage.tokens(),
				CostUSD:     m.Usage.costUSD(),
			})
		case strings.EqualFold(m.Role, "user") && s.userTitle == "":
			if txt := firstText(m.Content); isTitleCandidate(txt) {
				s.userTitle = txt
			}
		}
	}
	return nil
}

// consumeAsync folds one record of an async subagent transcript into the
// session. The run is identified by runId, the cwd is on every record, and only
// "message_end" records with an assistant role carry usage.
func (s *session) consumeAsync(r piRecord) {
	if r.RunID != "" {
		s.id = r.RunID
	}
	if r.Cwd != "" {
		s.project = r.Cwd
	}
	ts := parseTS(r.Timestamp)
	if !ts.IsZero() && (s.startedAt.IsZero() || ts.Before(s.startedAt)) {
		s.startedAt = ts
	}
	switch r.SourceEvent {
	case "initial_prompt":
		if s.userTitle != "" {
			return
		}
		var content []piContent
		if r.Message != nil {
			content = r.Message.Content
		}
		txt := firstNonEmpty(r.Text, firstText(content))
		if isTitleCandidate(txt) {
			s.userTitle = txt
		}
	case "message_end":
		if !strings.EqualFold(r.Role, "assistant") || r.AsyncUsage.IsZero() {
			return
		}
		if ts.IsZero() {
			ts = s.startedAt
		}
		s.events = append(s.events, model.UsageEvent{
			Harness:     model.Pi,
			DedupKey:    s.id + "#" + strings.Trim(string(bytes.TrimSpace(r.Timestamp)), `"`),
			SessionID:   s.id,
			ParentID:    s.parentID,
			ProjectPath: s.project,
			Timestamp:   ts,
			Model:       r.Model,
			Provider:    s.provider,
			Tokens:      r.AsyncUsage.tokens(),
			CostUSD:     r.AsyncUsage.costUSD(),
		})
	}
}

// batch finalizes the accumulated state into a Batch. Session fields are
// backfilled onto every event because the header is normally line 1 but a
// truncated log may not contain it yet.
func (s *session) batch() harness.Batch {
	for i := range s.events {
		if s.events[i].SessionID == "" {
			s.events[i].SessionID = s.id
		}
		if s.events[i].ParentID == "" {
			s.events[i].ParentID = s.parentID
		}
		if s.events[i].ProjectPath == "" {
			s.events[i].ProjectPath = s.project
		}
	}
	updated := s.updatedAt
	if updated.IsZero() {
		updated = s.startedAt
	}
	return harness.Batch{
		Events: s.events,
		Sessions: []model.SessionMeta{{
			Harness:   model.Pi,
			SessionID: s.id,
			ParentID:  s.parentID,
			Title:     harness.Title(s.userTitle),
			Project:   s.project,
			StartedAt: s.startedAt,
			UpdatedAt: updated,
		}},
	}
}

// parentFromPath returns the session id embedded in an ancestor directory named
// "<timestamp>_<session-id>": Pi stores a session's runs under
// <session-dir>/<child-id>/run-N/ and its forks under <session-dir>/forks/,
// where <session-dir> is named after the session's log file.
func parentFromPath(root, path string) string {
	dir := filepath.Dir(path)
	if root != "" {
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "" // outside the configured root: no reliable ancestry
		}
		parts := strings.Split(rel, string(filepath.Separator))
		for i := len(parts) - 1; i >= 0; i-- {
			if id := idFromDirName(parts[i]); id != "" {
				return id
			}
		}
		return ""
	}
	for i := 0; i < 6 && dir != "" && dir != "." && dir != string(filepath.Separator); i++ {
		if id := idFromDirName(filepath.Base(dir)); id != "" {
			return id
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

// idFromDirName extracts a session id from a "<timestamp>_<id>" directory name.
func idFromDirName(name string) string {
	i := strings.LastIndex(name, "_")
	if i < 0 || i+1 >= len(name) {
		return ""
	}
	if id := name[i+1:]; looksLikeSessionID(id) {
		return id
	}
	return ""
}

// idFromSessionPath extracts the session id from a session log path such as
// ".../2026-09-17T13-55-57-058Z_01a0afa7-1201-7289-8afe-c0ad477ee17b.jsonl".
func idFromSessionPath(p string) string {
	base := strings.TrimSuffix(filepath.Base(strings.TrimSpace(p)), ".jsonl")
	i := strings.LastIndex(base, "_")
	if i < 0 || i+1 >= len(base) {
		return ""
	}
	if id := base[i+1:]; looksLikeSessionID(id) {
		return id
	}
	return ""
}

// looksLikeSessionID matches the UUIDv7-shaped ids Pi writes (8-4-4-4-12 hex).
func looksLikeSessionID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// record schema
// ---------------------------------------------------------------------------

type piRecord struct {
	Type          string          `json:"type"`
	Version       int             `json:"version"`
	ID            string          `json:"id"`
	ParentID      string          `json:"parentId"`
	Cwd           string          `json:"cwd"`
	ParentSession string          `json:"parentSession"`
	Timestamp     json.RawMessage `json:"timestamp"`
	Message       *piMessage      `json:"message"`

	// Async-subagent transcripts ("source":"async") carry their usage object
	// at the top level instead of under "message".
	RecordType  string        `json:"recordType"`
	Source      string        `json:"source"`
	SourceEvent string        `json:"sourceEventType"`
	Role        string        `json:"role"`
	RunID       string        `json:"runId"`
	Model       string        `json:"model"`
	Text        string        `json:"text"`
	AsyncUsage  *piAsyncUsage `json:"usage"`
}

type piMessage struct {
	Role          string      `json:"role"`
	Provider      string      `json:"provider"`
	Model         string      `json:"model"`
	ResponseModel string      `json:"responseModel"`
	ResponseID    string      `json:"responseId"`
	API           string      `json:"api"`
	Timestamp     num         `json:"timestamp"`
	Usage         *piUsage    `json:"usage"`
	Content       []piContent `json:"content"`
}

type piContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// firstText joins the text blocks of a message.
func firstText(cs []piContent) string {
	var b strings.Builder
	for _, c := range cs {
		if c.Type == "text" || c.Type == "" {
			b.WriteString(c.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// slashCmdRE matches "/name" or "/name args" but not a path such as
// "/Users/zzstar/Downloads/x.rar…", whose next character is '/'.
var slashCmdRE = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9_-]*(\s|$)`)

// isTitleCandidate reports whether a user message can serve as the session
// title: plugin-injected system reminders and slash commands are skipped.
func isTitleCandidate(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || strings.HasPrefix(t, "<system-reminder") || slashCmdRE.MatchString(t) {
		return false
	}
	return true
}

// piUsage is message.usage. See the package doc for why these five fields map
// one-to-one onto model.Tokens without any subtraction.
type piUsage struct {
	Input        num     `json:"input"`
	Output       num     `json:"output"`
	CacheRead    num     `json:"cacheRead"`
	CacheWrite   num     `json:"cacheWrite"`
	TotalTokens  num     `json:"totalTokens"`
	Reasoning    num     `json:"reasoning"`
	CacheWrite1h num     `json:"cacheWrite1h"` // subset of cacheWrite: ignored
	Cost         *piCost `json:"cost"`
}

// IsZero reports whether the record accounts for no tokens at all. The real
// corpus contains usage objects whose fields are all zero (failed requests, e.g.
// HTTP 429 or 400 with a cost object attached), so the counted fields are
// compared rather than the struct value.
func (u *piUsage) IsZero() bool {
	return u == nil || (u.Input == 0 && u.Output == 0 && u.CacheRead == 0 &&
		u.CacheWrite == 0 && u.Reasoning == 0)
}

func (u *piUsage) tokens() model.Tokens {
	return model.Tokens{
		Input:      clamp(u.Input),
		Output:     clamp(u.Output),
		CacheRead:  clamp(u.CacheRead),
		CacheWrite: clamp(u.CacheWrite),
		Reasoning:  clamp(u.Reasoning),
	}
}

// costUSD reports the log's own cost, or nil when it does not name one.
func (u *piUsage) costUSD() *float64 {
	if u == nil || u.Cost == nil {
		return nil
	}
	t := float64(u.Cost.Total)
	if t == 0 {
		return nil
	}
	return &t
}

type piCost struct {
	Input      fnum `json:"input"`
	Output     fnum `json:"output"`
	CacheRead  fnum `json:"cacheRead"`
	CacheWrite fnum `json:"cacheWrite"`
	Total      fnum `json:"total"`
}

// piAsyncUsage is the top-level usage object of an async transcript record. It
// has no totalTokens and no reasoning field, and its cost is a plain number
// rather than an object.
type piAsyncUsage struct {
	Input      num  `json:"input"`
	Output     num  `json:"output"`
	CacheRead  num  `json:"cacheRead"`
	CacheWrite num  `json:"cacheWrite"`
	Reasoning  num  `json:"reasoning"`
	Cost       fnum `json:"cost"`
}

// IsZero reports whether the record accounts for no tokens at all.
func (u *piAsyncUsage) IsZero() bool {
	return u == nil || (u.Input == 0 && u.Output == 0 && u.CacheRead == 0 &&
		u.CacheWrite == 0 && u.Reasoning == 0)
}

func (u *piAsyncUsage) tokens() model.Tokens {
	return model.Tokens{
		Input:      clamp(u.Input),
		Output:     clamp(u.Output),
		CacheRead:  clamp(u.CacheRead),
		CacheWrite: clamp(u.CacheWrite),
		Reasoning:  clamp(u.Reasoning),
	}
}

// costUSD reports the log's own cost, or nil when it does not name one. Async
// transcripts report 0 for every request of the reference corpus.
func (u *piAsyncUsage) costUSD() *float64 {
	if u == nil || u.Cost == 0 {
		return nil
	}
	c := float64(u.Cost)
	return &c
}

// asyncMetaProvider reads the provider an async run's meta file names, e.g. "rn"
// out of "rn/gpt-6.1-sol:high". Only the model field is decoded: the meta's
// "task" prompt and "usage" aggregate are never read, because the aggregate
// would double count the transcript's own records.
func asyncMetaProvider(path string) string {
	base := filepath.Base(path)
	i := strings.Index(base, "_transcript.jsonl")
	if i <= 0 {
		return ""
	}
	meta := filepath.Join(filepath.Dir(path), base[:i]+"_meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		return ""
	}
	var m struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	if p, _, ok := strings.Cut(m.Model, "/"); ok && p != "" {
		return p
	}
	return ""
}

func clamp(n num) int64 {
	if n < 0 {
		return 0
	}
	return int64(n)
}

// num decodes a JSON number that may arrive as an integer, a float or a quoted
// string; null and absent values decode to 0.
type num int64

func (n *num) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err == nil {
		*n = num(f)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return err
		}
		*n = num(v)
		return nil
	}
	return fmt.Errorf("pi: cannot decode %s as a number", b)
}

// fnum decodes a JSON number without truncating it to an integer.
type fnum float64

func (f *fnum) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	var v float64
	if err := json.Unmarshal(b, &v); err == nil {
		*f = fnum(v)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		x, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return err
		}
		*f = fnum(x)
		return nil
	}
	return fmt.Errorf("pi: cannot decode %s as a number", b)
}

// parseTS decodes a timestamp that arrives either as an RFC3339 string or as a
// millisecond epoch number.
func parseTS(b json.RawMessage) time.Time {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return time.Time{}
	}
	var s string
	if json.Unmarshal(b, &s) == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC()
		}
		if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
			return msTime(num(ms))
		}
		return time.Time{}
	}
	var n num
	if json.Unmarshal(b, &n) == nil {
		return msTime(n)
	}
	return time.Time{}
}

func msTime(ms num) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(ms)).UTC()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
