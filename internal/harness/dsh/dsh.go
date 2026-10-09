// Package dsh parses DeepSeek Harness (DSH) session logs.
//
// # Log layout
//
//	$DSH_HOME/sessions/<project-slug>/<session-dir>/session[.v<N>].jsonl[.zstd]
//
// A session directory holds one session log, optionally zstd-compressed and
// optionally versioned (session.v3.jsonl.zstd, session.v4.jsonl.zstd, ...).
// session.lock and every other file in the directory is ignored. When a
// directory holds both a v3 and a v4 log the higher version wins: v4 is a
// strict superset of v3 in every observed case (verified on the 4 directories
// that hold both), so reading both would double count. A plain log beats its
// compressed twin at the same version because only the plain log can be
// resumed by byte offset.
//
// # Record schema
//
// Line 1 is the session header:
//
//	{"type":"session","version":4,"id":"session-<uuid>","createdAt":<ms epoch>,
//	 "cwd":"/path","parentSession":"session-<uuid>"?,"isSeeded":false,
//	 "origin":"subagent"?,"delegationDepth":N,"agentPreset":"standard"}
//
// Every later line is {"type":"...","seq":N,"time":<ms epoch>,"data":{...}}.
//
// Exactly two record types carry usage and become UsageEvents:
//
//   - assistant/message — data.usage = {inputTokens, outputTokens,
//     totalTokens, cacheReadTokens?, cacheWriteTokens?, reasoningTokens?} with
//     the model in data.message.source = {kind:"model", provider, model,
//     replayState}. data.message.id is the harness's own message id and is the
//     preferred DedupKey; data.message.source.replayState.response.responseId
//     ("resp_<hex>") is the fallback, then "<session>#<seq>". DSH rewrites the
//     same message while a response streams (~1330 duplicates in the reference
//     corpus), so the same DedupKey is emitted more than once and the last copy
//     wins downstream.
//   - compaction/summary — data = {compactionId, provider, model, usage, ...}.
//     This is a separate LLM request the harness makes to compact history, so
//     it is counted as its own event; DedupKey is compactionId.
//
// Every other record type (tool/call, tool/result, step/*, turn/*,
// user/message, system/message, request/*, session/title, subagent/*,
// web/deepseek-search-llm-request, assistant/attempt, llm/retry, todo/*,
// permission/*, workspace/changes, compaction/prune, ...) carries no usage and
// is skipped.
//
// # Token accounting
//
// Verified over all 60902 usage records of the reference corpus:
// totalTokens == inputTokens + outputTokens + cacheReadTokens +
// cacheWriteTokens, and reasoningTokens is additive on top of that (every
// record with reasoningTokens > 0 satisfies the identity). So inputTokens
// already excludes the cache classes and outputTokens already excludes
// reasoning; no normalization is needed and none is applied:
//
//	Input      = inputTokens       (excludes cacheRead/cacheWrite)
//	Output     = outputTokens      (excludes reasoning)
//	CacheRead  = cacheReadTokens
//	CacheWrite = cacheWriteTokens
//	Reasoning  = reasoningTokens   (separate, additive)
//
// # Sessions
//
// Top-level sessions are named "session-<uuid>" and carry no parentSession;
// subagent sessions are named "<uuid>" and carry
// parentSession="session-<uuid>", which is the parent's session id verbatim,
// so ParentID is the raw field. The title comes from the last session/title
// record (data.title) or, when the log has none (subagent sessions usually do
// not), from the first real user/message (data.role=="user" plus
// data.content[].text), skipping injected "<system-reminder>" blocks and slash
// commands. Project is the header's cwd. Provider and model come
// from the log; DSH names the provider explicitly ("nerv-base", "claude",
// "rinnebeat", "stepfun", "deepseek-official", "kami-cn", ...) and the model
// separately ("deepseek-flash", "claude-opus-5-5", "step-5-preview", ...), so
// both are set verbatim and no model→provider inference is done.
package dsh

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
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	// fingerprintBytes is how much of a file is hashed into Cursor.Fingerprint.
	fingerprintBytes = 4096
	// readBuffer is the line reader's buffer size (it still grows per line).
	readBuffer = 64 << 10

	kindJSONL   = "jsonl"
	kindJSONLzs = "jsonl.zstd"
)

// sessionFileRE matches the log file names DSH writes: session.jsonl,
// session.jsonl.zstd, session.v3.jsonl, session.v4.jsonl.zstd, ...
var decoderPool sync.Pool

var sessionFileRE = regexp.MustCompile(`^session(?:\.v(\d+))?\.jsonl(?:\.zstd)?$`)

// Parser reads DSH session logs. It holds no mutable state and is safe for
// concurrent use.
type Parser struct{ roots []string }

// New returns the parser for the default root: $DSH_HOME/sessions, falling back
// to ~/.dsh/sessions.
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
		cp = []string{filepath.Join(harness.EnvOr("DSH_HOME", ".dsh"), "sessions")}
	}
	return &Parser{roots: cp}
}

func (p *Parser) Harness() model.Harness { return model.DSH }

// Roots returns the directories to watch.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

// Discover walks every root and returns one Source per session directory,
// picking the highest-version log (see the package doc). Sources are sorted by
// path so scans are deterministic.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	best := map[string]candidate{}
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
			if d.IsDir() {
				return nil
			}
			m := sessionFileRE.FindStringSubmatch(d.Name())
			if m == nil {
				return nil
			}
			ver := 0
			if m[1] != "" {
				ver, _ = strconv.Atoi(m[1])
			}
			c := candidate{path: path, version: ver, zstd: isZstdPath(path)}
			if prev, ok := best[filepath.Dir(path)]; !ok || c.betterThan(prev) {
				best[filepath.Dir(path)] = c
			}
			return nil
		})
		if werr != nil && !errors.Is(werr, fs.SkipAll) {
			return nil, werr
		}
	}
	out := make([]harness.Source, 0, len(best))
	for _, c := range best {
		kind := kindJSONL
		if c.zstd {
			kind = kindJSONLzs
		}
		out = append(out, harness.Source{Path: c.path, Kind: kind})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// candidate is one session log file competing to represent its directory.
type candidate struct {
	path    string
	version int
	zstd    bool
}

// betterThan reports whether c should be read instead of o.
func (c candidate) betterThan(o candidate) bool {
	if c.version != o.version {
		return c.version > o.version
	}
	if c.zstd != o.zstd {
		// Same version: the plain log can be resumed incrementally.
		return !c.zstd
	}
	return c.path > o.path
}

// Parse reads one session log. Plain logs resume from cur.Offset and never
// consume a trailing incomplete line; zstd logs cannot be resumed by offset, so
// they are decompressed and reparsed in full and rely on DedupKey for
// correctness (unchanged files are skipped via Size/ModTime/Fingerprint).
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
	if isZstdPath(src.Path) || src.Kind == kindJSONLzs {
		return p.parseZstd(src, st, fp)
	}
	return p.parseJSONL(src, st, fp, cur)
}

func (p *Parser) parseJSONL(src harness.Source, st os.FileInfo, fp string, cur harness.Cursor) (harness.Batch, error) {
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
	s := newSession(src.Path)
	if off > 0 {
		_ = json.Unmarshal([]byte(cur.Extra), &s.boundary)
	}
	n, rerr := readCompleteLines(f, s.consume)
	b := s.batch()
	boundaryRaw, _ := json.Marshal(s.boundary)
	b.Next = harness.Cursor{
		Offset:      off + n,
		Size:        st.Size(),
		ModTime:     st.ModTime(),
		Fingerprint: fp,
		Extra:       string(boundaryRaw),
	}
	if rerr != nil {
		// Keep whatever complete lines were read and retry from there.
		return b, nil
	}
	return b, nil
}

func (p *Parser) parseZstd(src harness.Source, st os.FileInfo, fp string) (harness.Batch, error) {
	f, err := os.Open(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	defer f.Close()
	next := harness.Cursor{
		Offset:      0,
		Size:        st.Size(),
		ModTime:     st.ModTime(),
		Fingerprint: fp,
		Extra:       kindJSONLzs,
	}
	// The decoder reads frame headers and blocks in small pieces; buffer the
	// file so that is not one syscall per header.
	in := bufio.NewReaderSize(f, 256<<10)
	var dec *zstd.Decoder
	if pooled := decoderPool.Get(); pooled != nil {
		dec = pooled.(*zstd.Decoder)
		err = dec.Reset(in)
	} else {
		dec, err = zstd.NewReader(in, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	}
	if err != nil {
		// Not a decodable frame (yet): the log is probably mid-rewrite. Report
		// no progress and retry on the next scan.
		return harness.Batch{Next: next}, nil
	}
	defer func() {
		// Detach the source so pooled decoders do not retain compressed logs.
		if dec.Reset(nil) == nil {
			decoderPool.Put(dec)
		} else {
			dec.Close()
		}
	}()
	s := newSession(src.Path)
	// A truncated frame decodes its prefix and then fails; the prefix is still
	// valid and the whole file is reparsed on the next scan, so the error is
	// deliberately ignored.
	_, _ = readCompleteLines(dec, s.consume)
	b := s.batch()
	b.Next = next
	return b, nil
}

// unchanged reports whether the file is byte-identical to the one the cursor was
// taken from, which lets zstd (and fully read plain) logs skip reparsing.
func unchanged(cur harness.Cursor, st os.FileInfo, fp string) bool {
	return cur.Fingerprint != "" && cur.Fingerprint == fp &&
		cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime())
}

func isZstdPath(path string) bool { return strings.HasSuffix(path, ".zstd") }

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
	var pending []byte
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			pending = append(pending, line...)
			continue
		}
		if err == nil {
			if len(pending) > 0 {
				line = append(pending, line...)
			}
			if e := fn(line[:len(line)-1]); e != nil {
				return n, e
			}
			n += int64(len(line))
			if len(pending) > 0 {
				pending = line[:0]
				if cap(pending) > 256<<10 {
					pending = nil
				}
			}
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
	logTitle  string // last session/title record, if any
	userTitle string // first real user message, if any
	startedAt time.Time
	updatedAt time.Time
	events    []model.UsageEvent
	boundary  harness.BoundaryState
}

func newSession(path string) *session {
	// The session directory is named after the session id, so it is a usable
	// fallback when the header line has not been written yet.
	return &session{id: filepath.Base(filepath.Dir(path))}
}

func (s *session) consume(line []byte) error {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	if t, ok := skippable(line); ok {
		if t.After(s.updatedAt) {
			s.updatedAt = t
		}
		return nil
	}
	var fast struct {
		dshRecord
		Data struct {
			Title        string    `json:"title"`
			Usage        *dshUsage `json:"usage"`
			Message      *dshMsg   `json:"message"`
			CompactionID string    `json:"compactionId"`
			Provider     string    `json:"provider"`
			Model        string    `json:"model"`
		} `json:"data"`
	}
	// Decode useful payload fields directly, skipping conversation bodies once.
	// Fall back for foreign payload shapes to preserve record-specific tolerance.
	err := json.Unmarshal(line, &fast)
	r := fast.dshRecord
	if err != nil {
		r = dshRecord{}
		if json.Unmarshal(line, &r) != nil {
			return nil
		}
	}
	if t := msTime(r.Time); !t.IsZero() && t.After(s.updatedAt) {
		s.updatedAt = t
	}
	switch r.Type {
	case "session":
		if r.ID != "" {
			s.id = r.ID
		}
		if r.ParentSession != "" {
			s.parentID = r.ParentSession
		}
		if r.Cwd != "" {
			s.project = r.Cwd
		}
		if t := msTime(r.CreatedAt); !t.IsZero() && (s.startedAt.IsZero() || t.Before(s.startedAt)) {
			s.startedAt = t
		}

	case "session/title":
		d := dshTitle{Title: fast.Data.Title}
		if (err == nil || json.Unmarshal(r.Data, &d) == nil) && strings.TrimSpace(d.Title) != "" {
			s.logTitle = d.Title // the last one wins
		}

	case "user/message":
		if s.userTitle != "" {
			break
		}
		if err == nil {
			// Only a prospective user title needs conversation text.
			if json.Unmarshal(line, &r) != nil {
				break
			}
		}
		var d dshUserMessage
		if json.Unmarshal(r.Data, &d) == nil && strings.EqualFold(d.Role, "user") {
			if t := firstText(d.Content); isTitleCandidate(t) {
				s.userTitle = t
			}
		}

	case "assistant/message":
		d := dshAssistant{Usage: fast.Data.Usage, Message: fast.Data.Message}
		if err != nil {
			d = dshAssistant{}
		}
		if (err != nil && json.Unmarshal(r.Data, &d) != nil) || d.Usage.IsZero() {
			break
		}
		var provider, mdl, respID string
		if m := d.Message; m != nil {
			if src := m.Source; src != nil {
				provider, mdl = src.Provider, src.Model
				if rs := src.ReplayState; rs != nil && rs.Response != nil {
					respID = rs.Response.ResponseID
				}
			}
		}
		ts := msTime(r.Time)
		if ts.IsZero() {
			ts = s.startedAt
		}
		s.events = append(s.events, model.UsageEvent{
			Harness:     model.DSH,
			DedupKey:    firstNonEmpty(mID(d.Message), respID, fmt.Sprintf("%s#%d", s.id, int64(r.Seq))),
			RequestID:   respID,
			Boundary:    s.boundary.Apply(firstNonEmpty(mID(d.Message), respID, fmt.Sprintf("%s#%d", s.id, int64(r.Seq)))),
			SessionID:   s.id,
			ParentID:    s.parentID,
			ProjectPath: s.project,
			Timestamp:   ts,
			Model:       mdl,
			Provider:    provider,
			Tokens:      d.Usage.tokens(),
		})

	case "compaction/summary":
		s.boundary.Pending = model.BoundaryCompact
		d := dshCompaction{Usage: fast.Data.Usage, CompactionID: fast.Data.CompactionID, Provider: fast.Data.Provider, Model: fast.Data.Model}
		if err != nil {
			d = dshCompaction{}
		}
		if (err != nil && json.Unmarshal(r.Data, &d) != nil) || d.Usage.IsZero() {
			break
		}
		ts := msTime(r.Time)
		if ts.IsZero() {
			ts = s.startedAt
		}
		s.events = append(s.events, model.UsageEvent{
			Harness:     model.DSH,
			DedupKey:    firstNonEmpty(d.CompactionID, fmt.Sprintf("%s#compaction#%d", s.id, int64(r.Seq))),
			SessionID:   s.id,
			ParentID:    s.parentID,
			ProjectPath: s.project,
			Timestamp:   ts,
			Model:       d.Model,
			Provider:    d.Provider,
			Tokens:      d.Usage.tokens(),
		})
	}
	return nil
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
	title := s.logTitle
	if title == "" {
		title = s.userTitle
	}
	updated := s.updatedAt
	if updated.IsZero() {
		updated = s.startedAt
	}
	return harness.Batch{
		Events: s.events,
		Sessions: []model.SessionMeta{{
			Harness:   model.DSH,
			SessionID: s.id,
			ParentID:  s.parentID,
			Title:     harness.Title(title),
			Project:   s.project,
			StartedAt: s.startedAt,
			UpdatedAt: updated,
		}},
	}
}

func mID(m *dshMsg) string {
	if m == nil {
		return ""
	}
	return m.ID
}

// ---------------------------------------------------------------------------
// record schema
// ---------------------------------------------------------------------------

type dshRecord struct {
	Type          string          `json:"type"`
	Version       int             `json:"version"`
	ID            string          `json:"id"`
	CreatedAt     num             `json:"createdAt"`
	Cwd           string          `json:"cwd"`
	ParentSession string          `json:"parentSession"`
	Seq           num             `json:"seq"`
	Time          num             `json:"time"`
	Data          json.RawMessage `json:"data"`
}

type dshTitle struct {
	Title string `json:"title"`
}

type dshUserMessage struct {
	Role    string       `json:"role"`
	Content []dshContent `json:"content"`
}

type dshContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// firstText joins the text blocks of a message.
func firstText(cs []dshContent) string {
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
// title: injected context blocks and slash commands are skipped.
func isTitleCandidate(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || strings.HasPrefix(t, "<system-reminder") || slashCmdRE.MatchString(t) {
		return false
	}
	return true
}

type dshAssistant struct {
	Usage   *dshUsage `json:"usage"`
	Message *dshMsg   `json:"message"`
}

type dshMsg struct {
	ID     string  `json:"id"`
	Role   string  `json:"role"`
	Source *dshSrc `json:"source"`
}

type dshSrc struct {
	Kind        string     `json:"kind"`
	Provider    string     `json:"provider"`
	Model       string     `json:"model"`
	ReplayState *dshReplay `json:"replayState"`
}

type dshReplay struct {
	Response *dshResp `json:"response"`
}

type dshResp struct {
	ResponseID string `json:"responseId"`
}

type dshCompaction struct {
	CompactionID string    `json:"compactionId"`
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	Usage        *dshUsage `json:"usage"`
}

// dshUsage is data.usage. See the package doc for why these five fields map
// one-to-one onto model.Tokens without any subtraction.
type dshUsage struct {
	InputTokens      num `json:"inputTokens"`
	OutputTokens     num `json:"outputTokens"`
	TotalTokens      num `json:"totalTokens"`
	CacheReadTokens  num `json:"cacheReadTokens"`
	CacheWriteTokens num `json:"cacheWriteTokens"`
	ReasoningTokens  num `json:"reasoningTokens"`
}

// IsZero reports whether the record accounts for no tokens at all. The real
// corpus contains usage objects whose fields are all zero (failed requests), so
// the counted fields are compared rather than the struct value.
func (u *dshUsage) IsZero() bool {
	return u == nil || (u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 && u.ReasoningTokens == 0)
}

func (u *dshUsage) tokens() model.Tokens {
	return model.Tokens{
		Input:      clamp(u.InputTokens),
		Output:     clamp(u.OutputTokens),
		CacheRead:  clamp(u.CacheReadTokens),
		CacheWrite: clamp(u.CacheWriteTokens),
		Reasoning:  clamp(u.ReasoningTokens),
	}
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
	return fmt.Errorf("dsh: cannot decode %s as a number", b)
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
