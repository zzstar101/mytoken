// Package crush parses Crush (charmbracelet/crush) session usage.
//
// Crush keeps one SQLite database per project, by default inside the project's
// .crush directory; Crush also tracks those databases in a global registry file
// (projects.json) so it can resume sessions from anywhere. Layout:
//
//	<project>/.crush/crush.db                  per-project database
//	<crush data dir>/projects.json             registry: {"projects":[{"path":..., "data_dir":...}]}
//
// with the data dir resolving to $CRUSH_GLOBAL_DATA, else $XDG_DATA_HOME/crush,
// else ~/.local/share/crush (on Windows %LOCALAPPDATA%\crush then
// ~/AppData/Local/crush). A project's data_dir is used as-is when absolute and
// resolved against the project path when relative; when a project has no
// data_dir entry the project-local .crush directory is used.
//
// Schema (internal/db/migrations):
//
//	sessions(id TEXT PRIMARY KEY, parent_session_id TEXT, title TEXT,
//	         message_count INTEGER, prompt_tokens INTEGER, completion_tokens INTEGER,
//	         cost REAL, updated_at INTEGER, created_at INTEGER)
//	messages(id TEXT PRIMARY KEY, session_id TEXT, role TEXT, parts TEXT,
//	         model TEXT, provider TEXT, is_summary_message INTEGER,
//	         created_at INTEGER, updated_at INTEGER, finished_at INTEGER)
//
// Token accounting. Crush does not store per-message usage: only the session row
// carries cumulative prompt_tokens/completion_tokens and cost. Usage events are
// therefore emitted as monotone deltas of those cumulative counters, keyed
//
//	crush:<session id>:<cumulative prompt tokens>:<cumulative completion tokens>
//
// so that a session whose counters grow produces a new immutable event per
// growth step and summing deltas reproduces the session totals. A counter that
// goes backwards (a rewritten or compacted session) lowers the baseline instead
// of emitting negative usage. prompt_tokens is Crush's full prompt count and is
// mapped to Tokens.Input; Crush stores no cache split, so CacheRead/CacheWrite
// stay 0. Model and provider are taken from the session's most recent message
// that recorded them — never invented (docs/SPEC.md).
//
// A cost-only update (the cumulative cost grew while both token counters stood
// still) gets a key of its own,
//
//	crush:<session id>:<prompt>:<completion>:cost:<cumulative cost>
//
// because the store folds a repeated DedupKey last-write-wins: reusing the
// token key for a zero-token event would blank the tokens of the event that
// carried them.
//
// Incrementality. SQLite rewrites its header (change counter) on every commit,
// so the first-4KB fingerprint of a database changes whenever anything is
// written; the cursor stores a combined signature (database size+mtime plus the
// -wal/-shm size+mtime) for the cheap unchanged fast path, and Cursor.Extra
// keeps the per-session cumulative baselines. Read-only access is required:
// databases are opened file:...?mode=ro&immutable=0 with a busy timeout, with an
// immutable fallback for a hot write-ahead log we cannot lock, and a locked or
// unreadable database simply returns the previous cursor so the next scan
// retries. A missing sessions/messages table (or a non-Crush database) yields an
// empty batch, never an error.
package crush

import (
	"context"
	"crypto/sha1"
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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zzstar/mytoken/internal/harness"
	"github.com/zzstar/mytoken/internal/model"
)

const (
	fingerprintBytes = 4096
	busyTimeoutMS    = 5000

	kindSQLite = "sqlite"

	dbName         = "crush.db"
	crushDirName   = ".crush"
	projectsName   = "projects.json"
	crushDataDir   = "crush"
	crushGlobalEnv = "CRUSH_GLOBAL_DATA"
)

// Parser reads Crush usage from one or more project data directories.
type Parser struct {
	roots []string
}

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

// New returns a parser rooted at the default Crush data directories.
func New() *Parser { return &Parser{roots: defaultRoots()} }

// NewWithRoot returns a parser rooted at root.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser rooted at the given directories. Roots are
// either Crush data directories (containing crush.db and/or projects.json) or
// project directories (containing .crush/crush.db). An empty list falls back to
// the default roots.
func NewWithRoots(roots ...string) *Parser {
	seen := make(map[string]bool, len(roots))
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		r = filepath.Clean(r)
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	if len(out) == 0 {
		out = defaultRoots()
	}
	return &Parser{roots: out}
}

func defaultRoots() []string {
	if v := os.Getenv("MYTOKEN_CRUSH_DIRS"); v != "" {
		var out []string
		for _, p := range filepath.SplitList(v) {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return dataDirCandidates()
}

// dataDirCandidates mirrors the lookup order Crush uses for its global data
// directory (internal/config/load.go).
func dataDirCandidates() []string {
	if v := os.Getenv(crushGlobalEnv); v != "" {
		return []string{filepath.Clean(v)}
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return []string{filepath.Join(v, crushDataDir)}
	}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return []string{filepath.Join(v, crushDataDir)}
		}
		if home, err := os.UserHomeDir(); err == nil {
			return []string{filepath.Join(home, "AppData", "Local", crushDataDir)}
		}
	}
	return []string{harness.EnvOr("XDG_DATA_HOME", ".local", "share", crushDataDir)}
}

func (p *Parser) Harness() model.Harness { return model.Crush }

func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// registry is Crush's project registry (projects.json).
type registry struct {
	Projects []registryProject `json:"projects"`
}

type registryProject struct {
	Path    string `json:"path"`
	DataDir string `json:"data_dir"`
}

// Discover returns every Crush database reachable from the configured roots,
// expanding the project registry when present.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	var out []harness.Source
	seen := map[string]bool{}
	addDB := func(dir string) {
		if dir == "" {
			return
		}
		path := filepath.Clean(filepath.Join(dir, dbName))
		if seen[path] {
			return
		}
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() {
			return
		}
		seen[path] = true
		out = append(out, harness.Source{Path: path, Kind: kindSQLite})
	}
	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fi, err := os.Stat(root)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			if strings.HasSuffix(fi.Name(), ".db") {
				path := filepath.Clean(root)
				if !seen[path] {
					seen[path] = true
					out = append(out, harness.Source{Path: path, Kind: kindSQLite})
				}
			}
			continue
		}
		// A root may be a data directory holding crush.db directly, a project
		// directory holding .crush/crush.db, or a directory holding
		// projects.json.
		addDB(root)
		addDB(filepath.Join(root, crushDirName))
		for _, reg := range registryPaths(root) {
			for _, project := range readRegistry(reg) {
				addDB(resolveDataDir(reg, project))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// registryPaths lists the registry files to consult for a root.
func registryPaths(root string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(path string) {
		path = filepath.Clean(path)
		if seen[path] {
			return
		}
		if fi, err := os.Stat(path); err != nil || fi.IsDir() {
			return
		}
		seen[path] = true
		out = append(out, path)
	}
	add(filepath.Join(root, projectsName))
	if v := os.Getenv(crushGlobalEnv); v != "" {
		add(filepath.Join(v, projectsName))
	}
	for _, dir := range dataDirCandidates() {
		if dir != root {
			add(filepath.Join(dir, projectsName))
		}
	}
	return out
}

// resolveDataDir resolves a registry entry to the directory holding crush.db.
// Crush writes absolute project paths; a relative one is resolved against the
// registry's own directory so a relocated registry still resolves.
func resolveDataDir(regPath string, project registryProject) string {
	projectPath := strings.TrimSpace(project.Path)
	if projectPath != "" && !filepath.IsAbs(projectPath) {
		projectPath = filepath.Join(filepath.Dir(regPath), projectPath)
	}
	dir := strings.TrimSpace(project.DataDir)
	switch {
	case dir == "":
		if projectPath == "" {
			return ""
		}
		return filepath.Join(projectPath, crushDirName)
	case filepath.IsAbs(dir):
		return dir
	default:
		return filepath.Join(projectPath, dir)
	}
}

func readRegistry(path string) []registryProject {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var reg registry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil
	}
	return reg.Projects
}

// Parse reads the usage a single database added since cur.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return harness.Batch{Next: cur}, err
	}
	st, err := os.Stat(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	sig, err := signatureOf(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	fp, err := fingerprint(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	if cur.Fingerprint != "" && cur.Fingerprint == fp && cur.Size == sig.size && cur.ModTime.Equal(sig.mod) {
		return harness.Batch{Next: cur}, nil
	}
	db, err := openReadOnly(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	defer db.Close()

	tables := tableColumns(ctx, db)
	if _, ok := tables["sessions"]; !ok {
		// Not a Crush database (or an empty one): still advance the cursor so
		// the file is not rescanned until it changes.
		return harness.Batch{Next: cursorFor(sig, fp, stateOf(crushState{}))}, nil
	}

	state := crushState{}
	if cur.Extra != "" {
		_ = json.Unmarshal([]byte(cur.Extra), &state)
	}
	if state.Sessions == nil {
		state.Sessions = map[string]baseline{}
	}

	acc := newAccumulator(projectPath(src.Path), st.ModTime())
	sessions := readSessions(ctx, db, tables)
	models := readSessionModels(ctx, db, tables)
	for _, s := range sessions {
		if s.MessageCount <= 0 && s.Prompt <= 0 && s.Completion <= 0 && s.Cost <= 0 {
			continue
		}
		acc.session(s)
		acc.usage(s, models[s.ID], state.Sessions)
	}
	next := cursorFor(sig, fp, stateOf(state))
	return acc.batch(next), nil
}

// ---------------------------------------------------------------------------
// database
// ---------------------------------------------------------------------------

type dbSignature struct {
	size int64
	mod  time.Time
}

func signatureOf(path string) (dbSignature, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return dbSignature{}, err
	}
	sig := dbSignature{size: fi.Size(), mod: fi.ModTime()}
	for _, suffix := range []string{"-wal", "-shm"} {
		w, err := os.Stat(path + suffix)
		if err != nil {
			continue
		}
		sig.size += w.Size()
		if w.ModTime().After(sig.mod) {
			sig.mod = w.ModTime()
		}
	}
	return sig, nil
}

// openReadOnly opens path without ever taking a write lock. A database with a
// hot write-ahead log that cannot be locked is retried with immutable=1, which
// reads the last checkpointed snapshot.
func openReadOnly(path string) (*sql.DB, error) {
	db, err := openWithPragmas(path, "mode=ro", "immutable=0")
	if err == nil {
		return db, nil
	}
	if db, ierr := openWithPragmas(path, "immutable=1"); ierr == nil {
		return db, nil
	}
	return nil, err
}

func openWithPragmas(path string, params ...string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	q := url.Values{"_pragma": {fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS)}}
	for _, p := range params {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		q.Set(kv[0], kv[1])
	}
	u := url.URL{Scheme: "file", Path: abs, RawQuery: q.Encode()}
	db, err := sql.Open("sqlite", u.String())
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

func tableColumns(ctx context.Context, db *sql.DB) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type IN ('table','view')`)
	if err != nil {
		return out
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		names = append(names, name)
	}
	rows.Close()
	for _, name := range names {
		cols := map[string]bool{}
		rs, err := db.QueryContext(ctx, `PRAGMA table_info(`+quoteIdent(name)+`)`)
		if err != nil {
			continue
		}
		for rs.Next() {
			var (
				cid, notnull, pk int
				col, ctype       string
				dflt             sql.NullString
			)
			if err := rs.Scan(&cid, &col, &ctype, &notnull, &dflt, &pk); err != nil {
				continue
			}
			cols[col] = true
		}
		rs.Close()
		out[name] = cols
	}
	return out
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sessionRow is one row of the sessions table.
type sessionRow struct {
	ID           string
	ParentID     string
	Title        string
	MessageCount int64
	Prompt       int64
	Completion   int64
	Cost         float64
	Created      int64
	Updated      int64
}

func readSessions(ctx context.Context, db *sql.DB, tables map[string]map[string]bool) []sessionRow {
	cols, ok := tables["sessions"]
	if !ok || !cols["id"] {
		return nil
	}
	q := "SELECT " + strings.Join([]string{
		textCol(cols, "id"),
		textCol(cols, "parent_session_id"),
		textCol(cols, "title"),
		intCol(cols, "message_count"),
		intCol(cols, "prompt_tokens"),
		intCol(cols, "completion_tokens"),
		realCol(cols, "cost"),
		intCol(cols, "created_at"),
		intCol(cols, "updated_at"),
	}, ", ") + " FROM " + quoteIdent("sessions")
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []sessionRow
	for rows.Next() {
		var s sessionRow
		if err := rows.Scan(&s.ID, &s.ParentID, &s.Title, &s.MessageCount, &s.Prompt, &s.Completion, &s.Cost, &s.Created, &s.Updated); err != nil {
			continue
		}
		if s.ID == "" {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// readSessionModels returns the most recent model/provider Crush recorded for
// each session's messages.
func readSessionModels(ctx context.Context, db *sql.DB, tables map[string]map[string]bool) map[string]modelRef {
	cols, ok := tables["messages"]
	if !ok || !cols["session_id"] || !cols["model"] || !cols["provider"] {
		return nil
	}
	order := "created_at"
	if !cols[order] {
		order = "rowid"
	}
	q := "SELECT session_id, COALESCE(model,''), COALESCE(provider,'') FROM " + quoteIdent("messages") +
		" WHERE COALESCE(model,'') <> '' OR COALESCE(provider,'') <> '' ORDER BY " + quoteIdent(order) + " ASC"
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]modelRef{}
	for rows.Next() {
		var sid string
		var ref modelRef
		if err := rows.Scan(&sid, &ref.Model, &ref.Provider); err != nil {
			continue
		}
		if sid == "" {
			continue
		}
		out[sid] = ref // later rows win: the newest model seen for the session
	}
	return out
}

type modelRef struct {
	Model    string
	Provider string
}

func textCol(cols map[string]bool, name string) string {
	if !cols[name] {
		return "''"
	}
	return "COALESCE(" + quoteIdent(name) + ",'')"
}

func intCol(cols map[string]bool, name string) string {
	if !cols[name] {
		return "0"
	}
	return "COALESCE(" + quoteIdent(name) + ",0)"
}

func realCol(cols map[string]bool, name string) string {
	if !cols[name] {
		return "0.0"
	}
	return "COALESCE(" + quoteIdent(name) + ",0.0)"
}

// ---------------------------------------------------------------------------
// state
// ---------------------------------------------------------------------------

// crushState is the incremental watermark persisted in Cursor.Extra: the last
// cumulative counters observed per session.
type crushState struct {
	Sessions map[string]baseline `json:"s"`
}

type baseline struct {
	Prompt     int64   `json:"p"`
	Completion int64   `json:"c"`
	Cost       float64 `json:"$"`
}

func stateOf(s crushState) string {
	if s.Sessions == nil {
		s.Sessions = map[string]baseline{}
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// accumulator
// ---------------------------------------------------------------------------

type accumulator struct {
	project string
	modTime time.Time

	evs   map[string]model.UsageEvent
	order []string
	metas map[string]model.SessionMeta
	mord  []string
}

func newAccumulator(project string, modTime time.Time) *accumulator {
	return &accumulator{
		project: project,
		modTime: modTime,
		evs:     map[string]model.UsageEvent{},
		metas:   map[string]model.SessionMeta{},
	}
}

func (a *accumulator) session(s sessionRow) {
	started := crushTime(s.Created)
	updated := crushTime(s.Updated)
	if updated.Before(started) {
		updated = started
	}
	a.metas[s.ID] = model.SessionMeta{
		Harness:   model.Crush,
		SessionID: s.ID,
		ParentID:  s.ParentID,
		Title:     harness.Title(s.Title),
		Project:   a.project,
		StartedAt: started,
		UpdatedAt: updated,
	}
	a.mord = append(a.mord, s.ID)
}

// usage emits the monotone delta of a session's cumulative counters since the
// last parse and advances the stored baseline.
func (a *accumulator) usage(s sessionRow, ref modelRef, state map[string]baseline) {
	prev := state[s.ID]
	prompt := clampInt(s.Prompt)
	completion := clampInt(s.Completion)
	cost := s.Cost
	if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		cost = 0
	}
	// A counter that moved backwards (rewritten or compacted session) must not
	// produce negative usage; the high-water mark stays the baseline.
	base := prev
	if prompt > base.Prompt {
		base.Prompt = prompt
	}
	if completion > base.Completion {
		base.Completion = completion
	}
	if cost > base.Cost {
		base.Cost = cost
	}
	state[s.ID] = base

	deltaTokens := model.Tokens{
		Input:  prompt - prev.Prompt,
		Output: completion - prev.Completion,
	}
	if deltaTokens.Input < 0 {
		deltaTokens.Input = 0
	}
	if deltaTokens.Output < 0 {
		deltaTokens.Output = 0
	}
	var deltaCost *float64
	if cost > prev.Cost {
		d := cost - prev.Cost
		deltaCost = &d
	}
	if deltaTokens.IsZero() && deltaCost == nil {
		return
	}
	// "crush:<session id>:<cumulative prompt>:<cumulative completion>": neither
	// counter ever decreases, so the key is stable across rescans and unique per
	// growth step.
	key := fmt.Sprintf("crush:%s:%d:%d", s.ID, prompt, completion)
	if deltaTokens.IsZero() {
		// A cost-only update must not reuse the token key: the store folds a
		// repeated DedupKey last-write-wins, so a zero-token event under the
		// token key would blank the tokens of the event that carried them.
		key = fmt.Sprintf("crush:%s:%d:%d:cost:%s", s.ID, prompt, completion, strconv.FormatFloat(cost, 'g', -1, 64))
	}
	ts := crushTime(s.Updated)
	if ts.IsZero() {
		ts = crushTime(s.Created)
	}
	if ts.IsZero() {
		ts = a.modTime.UTC()
	}
	ev := model.UsageEvent{
		Harness:     model.Crush,
		DedupKey:    key,
		SessionID:   s.ID,
		ParentID:    s.ParentID,
		ProjectPath: a.project,
		Timestamp:   ts,
		Model:       ref.Model,
		Provider:    ref.Provider,
		Tokens:      deltaTokens,
		CostUSD:     deltaCost,
	}
	if prev, ok := a.evs[ev.DedupKey]; ok {
		// Should not happen within a single parse; keep the larger observation.
		prev.Tokens = maxTokens(prev.Tokens, ev.Tokens)
		if ev.CostUSD != nil && (prev.CostUSD == nil || *ev.CostUSD > *prev.CostUSD) {
			prev.CostUSD = ev.CostUSD
		}
		a.evs[ev.DedupKey] = prev
		return
	}
	a.evs[ev.DedupKey] = ev
	a.order = append(a.order, ev.DedupKey)
}

func (a *accumulator) batch(next harness.Cursor) harness.Batch {
	events := make([]model.UsageEvent, 0, len(a.order))
	for _, k := range a.order {
		events = append(events, a.evs[k])
	}
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].Timestamp.Equal(events[j].Timestamp) {
			return events[i].Timestamp.Before(events[j].Timestamp)
		}
		return events[i].DedupKey < events[j].DedupKey
	})
	metas := make([]model.SessionMeta, 0, len(a.mord))
	for _, id := range a.mord {
		metas = append(metas, a.metas[id])
	}
	sort.SliceStable(metas, func(i, j int) bool { return metas[i].SessionID < metas[j].SessionID })
	return harness.Batch{Events: events, Sessions: metas, Next: next}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// crushTime normalises a Crush timestamp, which is milliseconds when it is
// large enough and seconds otherwise.
func crushTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	if v < 100_000_000_000 {
		v *= 1000
	}
	return time.UnixMilli(v).UTC()
}

func clampInt(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
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

// projectPath derives the project directory a Crush database belongs to.
func projectPath(dbPath string) string {
	dir := filepath.Dir(dbPath)
	if filepath.Base(dir) == crushDirName {
		return filepath.Dir(dir)
	}
	return dir
}

func cursorFor(sig dbSignature, fp, extra string) harness.Cursor {
	return harness.Cursor{
		Offset:      0,
		Size:        sig.size,
		ModTime:     sig.mod,
		Fingerprint: fp,
		Extra:       extra,
	}
}

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
