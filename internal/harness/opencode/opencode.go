// Package opencode parses OpenCode session usage.
//
// OpenCode keeps its state in a data directory that defaults to
// ~/.local/share/opencode ($XDG_DATA_HOME/opencode). Two generations of
// on-disk storage exist and both are read:
//
//	sqlite  <root>/opencode.db (and channel builds such as opencode-stable.db)
//	        tables:
//	          session(id, parent_id, directory, title, time_created, time_updated, ...)
//	          message(id, session_id, time_created, time_updated, data)        -- v1
//	          session_message(id, session_id, type, seq, ..., data)            -- v2
//	        `data` holds the JSON message info (MessageV2.Info): role, tokens,
//	        cost, modelID/providerID, time{created,completed}, path{root,cwd}.
//	        Only rows with a usage payload are used: v1 rows need
//	        role='assistant' and a non-null tokens object, v2 rows need
//	        type='assistant' (session_message also stores agent-switched /
//	        model-switched rows that carry no usage).
//	json    <root>/storage/message/<sessionID>/<messageID>.json  (legacy, pre-sqlite)
//	        <root>/storage/session/<projectID>/<sessionID>.json
//
// Token accounting. opencode reports tokens{input,output,reasoning,cache{read,write}}
// where `input` already excludes the cache classes, so the classes map
// one-to-one onto model.Tokens with no subtraction:
//
//	input       -> Tokens.Input      (clamped at 0)
//	output      -> Tokens.Output
//	reasoning   -> Tokens.Reasoning
//	cache.read  -> Tokens.CacheRead
//	cache.write -> Tokens.CacheWrite
//
// Model and provider come from modelID/providerID (or the nested
// model{id,providerID}) and are never inferred: an absent provider stays empty
// (docs/SPEC.md). Cost is only reported when the log carries a finite, positive
// `cost` (opencode writes 0 for "unknown").
//
// Dedup. Every event is keyed "opencode:<message id>", which is stable across
// rescans and identical for a message seen through both the sqlite tables and a
// leftover legacy JSON file. Repeated observations of one key inside a single
// parse are merged field-wise with per-class maxima (opencode appends token
// usage to a message after the row first appears).
//
// Incrementality. Whole-file sources (legacy JSON) use the standard
// size/mtime/fingerprint cursor. SQLite is different: SQLite rewrites its
// header (change counter) on every commit, so the first-4KB fingerprint of a db
// changes whenever *anything* is written. The cursor therefore stores a
// combined signature (db size+mtime plus -wal/-shm size+mtime) for the cheap
// unchanged fast path and an (time_updated, id) watermark in Cursor.Extra to
// resume the message keyset; the watermark is dropped when it is ahead of the
// database's own MAX(time_updated), which is how a replaced or recreated db is
// detected. SQLite is opened read-only (file:...?mode=ro&immutable=0) with a
// busy timeout, and a locked or unreadable db simply returns the previous
// cursor so the next scan retries.
package opencode

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
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

	kindJSON   = "json"
	kindSQLite = "sqlite"

	dbName        = "opencode.db"
	storageDir    = "storage"
	messageSubdir = "message"
	sessionSubdir = "session"
)

// Parser reads OpenCode usage from one or more data directories.
type Parser struct {
	roots []string
}

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

// New returns a parser rooted at the default OpenCode data directory.
func New() *Parser { return &Parser{roots: defaultRoots()} }

// NewWithRoot returns a parser rooted at root.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser rooted at the given directories. An empty list
// falls back to the default roots.
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

// defaultRoots resolves the OpenCode data directory, honouring the env
// overrides used by the harness and by OpenCode itself.
func defaultRoots() []string {
	if v := os.Getenv("MYTOKEN_OPENCODE_DIRS"); v != "" {
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
	if v := os.Getenv("OPENCODE_DATA_HOME"); v != "" {
		return []string{v}
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return []string{filepath.Join(v, "opencode")}
	}
	return []string{harness.EnvOr("XDG_DATA_HOME", ".local", "share", "opencode")}
}

func (p *Parser) Harness() model.Harness { return model.OpenCode }

func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover returns every OpenCode database and legacy JSON file below the
// configured roots.
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
		fi, err := os.Stat(root)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			if isDBName(fi.Name()) {
				add(root, kindSQLite)
			}
			continue
		}
		if entries, err := os.ReadDir(root); err == nil {
			for _, e := range entries {
				if e.IsDir() || !isDBName(e.Name()) {
					continue
				}
				add(filepath.Join(root, e.Name()), kindSQLite)
			}
		}
		for _, dir := range storageDirs(root) {
			_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
					return nil
				}
				add(path, kindJSON)
				return nil
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// isDBName reports whether name looks like an OpenCode sqlite database.
func isDBName(name string) bool {
	if name == dbName {
		return true
	}
	return strings.HasPrefix(name, "opencode-") && strings.HasSuffix(name, ".db")
}

// storageDirs returns the legacy storage trees below root, if any.
func storageDirs(root string) []string {
	bases := []string{root, filepath.Join(root, storageDir)}
	var out []string
	for _, base := range bases {
		for _, name := range []string{messageSubdir, sessionSubdir} {
			dir := filepath.Join(base, name)
			if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
				out = append(out, dir)
			}
		}
	}
	return out
}

// Parse reads the events a single source added since cur.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return harness.Batch{Next: cur}, err
	}
	if src.Kind == kindSQLite {
		return p.parseDB(ctx, src, cur)
	}
	return p.parseLegacyJSON(ctx, src, cur)
}

// ---------------------------------------------------------------------------
// sqlite
// ---------------------------------------------------------------------------

// dbState is the incremental watermark persisted in Cursor.Extra.
type dbState struct {
	Updated int64  `json:"u"`
	ID      string `json:"i"`
}

// dbSignature combines the main database file with its write-ahead log, which
// is where in-flight commits land before a checkpoint.
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

// openReadOnly opens path as a read-only sqlite database. harness data is never
// written to.
func openReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	u := url.URL{
		Scheme: "file",
		Path:   abs,
		RawQuery: url.Values{
			"mode":      {"ro"},
			"immutable": {"0"},
			"_pragma":   {fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS)},
		}.Encode(),
	}
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

func (p *Parser) parseDB(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
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
	_ = st
	if cur.Fingerprint != "" && cur.Fingerprint == fp && cur.Size == sig.size && cur.ModTime.Equal(sig.mod) {
		return harness.Batch{Next: cur}, nil
	}
	db, err := openReadOnly(src.Path)
	if err != nil {
		// Locked, unreadable or not a database: keep the cursor and retry on
		// the next scan.
		return harness.Batch{Next: cur}, nil
	}
	defer db.Close()

	tables := tableColumns(ctx, db)
	acc := newAccumulator()

	var state dbState
	if cur.Extra != "" {
		_ = json.Unmarshal([]byte(cur.Extra), &state)
	}
	if max, ok := maxUpdated(ctx, db, tables); !ok || state.Updated > max || state.Updated < 0 {
		state = dbState{}
	}

	sessions := loadSessions(ctx, db, tables)
	for _, id := range sortedKeys(sessions) {
		acc.session(sessions[id])
	}
	readMessages(ctx, db, tables, acc, sessions, &state)

	stateJSON, err := json.Marshal(state)
	if err != nil {
		stateJSON = nil
	}
	next := harness.Cursor{
		Offset:      0,
		Size:        sig.size,
		ModTime:     sig.mod,
		Fingerprint: fp,
		Extra:       string(stateJSON),
	}
	return acc.batch(next), nil
}

// tableColumns maps every table (and view) to its column set, so the queries can
// degrade gracefully across schema generations.
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

// sessionInfo is the per-session metadata opencode stores next to messages.
type sessionInfo struct {
	ID        string
	ParentID  string
	Directory string
	Title     string
	Created   int64
	Updated   int64
}

// loadSessions reads the session tables (v1 `session` and/or v2 `session_v2`).
func loadSessions(ctx context.Context, db *sql.DB, tables map[string]map[string]bool) map[string]sessionInfo {
	out := map[string]sessionInfo{}
	for _, name := range []string{"session", "session_v2"} {
		cols, ok := tables[name]
		if !ok || !cols["id"] {
			continue
		}
		q := "SELECT " + sessionColumns(cols) + " FROM " + quoteIdent(name)
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			continue
		}
		for rows.Next() {
			var info sessionInfo
			if err := rows.Scan(&info.ID, &info.ParentID, &info.Directory, &info.Title, &info.Created, &info.Updated); err != nil {
				continue
			}
			if info.ID == "" {
				continue
			}
			prev, seen := out[info.ID]
			if !seen {
				out[info.ID] = info
				continue
			}
			out[info.ID] = mergeSession(prev, info)
		}
		rows.Close()
	}
	return out
}

// sessionColumns builds the projection for a session table, substituting
// constants for columns an older schema does not have.
func sessionColumns(cols map[string]bool) string {
	text := func(name string) string {
		if !cols[name] {
			return "''"
		}
		return "COALESCE(" + quoteIdent(name) + ",'')"
	}
	integer := func(name string) string {
		if !cols[name] {
			return "0"
		}
		return "COALESCE(" + quoteIdent(name) + ",0)"
	}
	return strings.Join([]string{
		text("id"), text("parent_id"), text("directory"), text("title"),
		integer("time_created"), integer("time_updated"),
	}, ", ")
}

func mergeSession(a, b sessionInfo) sessionInfo {
	a.ParentID = firstNonEmpty(a.ParentID, b.ParentID)
	a.Directory = firstNonEmpty(a.Directory, b.Directory)
	a.Title = firstNonEmpty(a.Title, b.Title)
	if a.Created == 0 || (b.Created != 0 && b.Created < a.Created) {
		a.Created = b.Created
	}
	if b.Updated > a.Updated {
		a.Updated = b.Updated
	}
	return a
}

// maxUpdated reports the newest message watermark present in the database.
func maxUpdated(ctx context.Context, db *sql.DB, tables map[string]map[string]bool) (int64, bool) {
	var max int64
	found := false
	for _, name := range []string{"message", "session_message"} {
		cols, ok := tables[name]
		if !ok || !cols["time_updated"] {
			continue
		}
		var v sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(time_updated),0) FROM "+quoteIdent(name)).Scan(&v); err != nil {
			continue
		}
		found = true
		if v.Int64 > max {
			max = v.Int64
		}
	}
	return max, found
}

// readMessages walks both message generations from the persisted watermark,
// advancing it as rows are consumed.
func readMessages(ctx context.Context, db *sql.DB, tables map[string]map[string]bool, acc *accumulator, sessions map[string]sessionInfo, state *dbState) {
	queries := []struct{ table, filter string }{
		{"message", "json_valid(data) AND json_extract(data,'$.role')='assistant' AND json_extract(data,'$.tokens') IS NOT NULL"},
		{"session_message", "json_valid(data) AND type='assistant' AND json_extract(data,'$.tokens') IS NOT NULL"},
	}
	for _, q := range queries {
		cols, ok := tables[q.table]
		if !ok {
			continue
		}
		complete := true
		for _, c := range []string{"id", "session_id", "data", "time_updated"} {
			if !cols[c] {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		sqlText := "SELECT id, session_id, data, time_updated FROM " + quoteIdent(q.table) +
			" WHERE " + q.filter +
			" AND (time_updated > ? OR (time_updated = ? AND id > ?))" +
			" ORDER BY time_updated, id"
		rows, err := db.QueryContext(ctx, sqlText, state.Updated, state.Updated, state.ID)
		if err != nil {
			continue
		}
		for rows.Next() {
			var (
				id, sid, data string
				updated       int64
			)
			if err := rows.Scan(&id, &sid, &data, &updated); err != nil {
				continue
			}
			if id == "" {
				continue
			}
			var payload messagePayload
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				continue
			}
			info := sessions[sid]
			acc.emit("opencode:"+id, payload, sid, info.ParentID, firstNonEmpty(info.Directory, payload.Path.Root), updated)
			if updated > state.Updated || (updated == state.Updated && id > state.ID) {
				state.Updated, state.ID = updated, id
			}
		}
		rows.Close()
	}
}

// ---------------------------------------------------------------------------
// legacy JSON storage
// ---------------------------------------------------------------------------

// legacyFile is the union of the legacy message and session info shapes; a
// non-empty role marks a message file.
type legacyFile struct {
	messagePayload
	Directory string `json:"directory"`
	ParentID  string `json:"parentID"`
	Title     string `json:"title"`
	ProjectID string `json:"projectID"`
	Version   string `json:"version"`
}

func (p *Parser) parseLegacyJSON(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	st, err := os.Stat(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	fp, err := fingerprint(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	next := cursorFor(st, fp, st.Size(), "")
	if cur.Fingerprint != "" && cur.Fingerprint == fp && cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime()) {
		return harness.Batch{Next: cur}, nil
	}
	raw, err := os.ReadFile(src.Path)
	if err != nil {
		return harness.Batch{Next: next}, nil
	}
	var file legacyFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return harness.Batch{Next: next}, nil
	}
	acc := newAccumulator()
	stem := strings.TrimSuffix(filepath.Base(src.Path), filepath.Ext(src.Path))
	if file.Role == "" {
		// Session info file: <projectID>/<sessionID>.json
		acc.session(sessionInfo{
			ID:        firstNonEmpty(file.ID, stem),
			ParentID:  file.ParentID,
			Directory: file.Directory,
			Title:     file.Title,
			Created:   int64Of(file.Time.Created),
			Updated:   int64Of(file.Time.Updated),
		})
		return acc.batch(next), nil
	}
	// Message file. As in the sqlite generation only assistant messages carry
	// usage; a file without a complete usage payload is a partial write and is
	// retried after the next modification.
	if file.Role != "assistant" || file.Tokens == nil || file.Tokens.Cache == nil || file.Time.Created == nil {
		return harness.Batch{Next: next}, nil
	}
	id := firstNonEmpty(file.ID, stem)
	sid := firstNonEmpty(file.SessionID, file.SnakeSessionID)
	if sid == "" {
		// <root>/storage/message/<sessionID>/<messageID>.json
		if parent := filepath.Base(filepath.Dir(src.Path)); parent != "" && parent != "." {
			sid = parent
		}
	}
	acc.emit("opencode:"+id, file.messagePayload, sid, "", file.Path.Root, 0)
	return acc.batch(next), nil
}

// ---------------------------------------------------------------------------
// payload decoding
// ---------------------------------------------------------------------------

// messagePayload is the usage-relevant subset of opencode's MessageV2.Info.
type messagePayload struct {
	ID             string          `json:"id"`
	SessionID      string          `json:"sessionID"`
	SnakeSessionID string          `json:"session_id"`
	Role           string          `json:"role"`
	ModelID        string          `json:"modelID"`
	ProviderID     string          `json:"providerID"`
	Model          modelRef        `json:"model"`
	Cost           *float64        `json:"cost"`
	Tokens         *tokenPayload   `json:"tokens"`
	Time           messageTime     `json:"time"`
	Path           messagePath     `json:"path"`
	Agent          string          `json:"agent"`
	Mode           string          `json:"mode"`
	Variant        string          `json:"variant"`
	Extra          json.RawMessage `json:"-"`
}

type modelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
}

type messageTime struct {
	Created   *num `json:"created"`
	Completed *num `json:"completed"`
	Updated   *num `json:"updated"`
}

type messagePath struct {
	Root string `json:"root"`
	Cwd  string `json:"cwd"`
}

type tokenPayload struct {
	Input     num         `json:"input"`
	Output    num         `json:"output"`
	Reasoning *num        `json:"reasoning"`
	Cache     *cacheUsage `json:"cache"`
}

type cacheUsage struct {
	Read  *num `json:"read"`
	Write *num `json:"write"`
}

// classes maps opencode's token classes onto model.Tokens. opencode's `input`
// excludes cached tokens, so no class is subtracted here.
func (t *tokenPayload) classes() model.Tokens {
	tk := model.Tokens{
		Input:  clampNum(t.Input),
		Output: clampNum(t.Output),
	}
	if t.Reasoning != nil {
		tk.Reasoning = clampNum(*t.Reasoning)
	}
	if t.Cache != nil {
		if t.Cache.Read != nil {
			tk.CacheRead = clampNum(*t.Cache.Read)
		}
		if t.Cache.Write != nil {
			tk.CacheWrite = clampNum(*t.Cache.Write)
		}
	}
	return tk
}

// ---------------------------------------------------------------------------
// accumulator
// ---------------------------------------------------------------------------

type accumulator struct {
	evs   map[string]model.UsageEvent
	order []string
	metas map[string]model.SessionMeta
	mord  []string
}

func newAccumulator() *accumulator {
	return &accumulator{
		evs:   map[string]model.UsageEvent{},
		metas: map[string]model.SessionMeta{},
	}
}

func (a *accumulator) emit(dedup string, p messagePayload, sid, parent, project string, fallbackUpdatedMS int64) {
	if dedup == "" || p.Tokens == nil {
		return
	}
	tk := p.Tokens.classes()
	cost := reportedCost(p.Cost)
	if tk.IsZero() && cost == nil {
		return
	}
	ts := payloadTime(p)
	if ts.IsZero() {
		ts = epochMS(fallbackUpdatedMS)
	}
	ev := model.UsageEvent{
		Harness:     model.OpenCode,
		DedupKey:    dedup,
		SessionID:   sid,
		ParentID:    parent,
		ProjectPath: project,
		Timestamp:   ts,
		Model:       firstNonEmpty(p.ModelID, p.Model.ID),
		Provider:    firstNonEmpty(p.ProviderID, p.Model.ProviderID),
		Tokens:      tk,
		CostUSD:     cost,
	}
	a.add(ev)
}

func (a *accumulator) add(ev model.UsageEvent) {
	if ev.DedupKey == "" {
		return
	}
	if prev, ok := a.evs[ev.DedupKey]; ok {
		prev.Tokens = maxTokens(prev.Tokens, ev.Tokens)
		if ev.CostUSD != nil && (prev.CostUSD == nil || *ev.CostUSD > *prev.CostUSD) {
			prev.CostUSD = ev.CostUSD
		}
		prev.SessionID = firstNonEmpty(prev.SessionID, ev.SessionID)
		prev.ParentID = firstNonEmpty(prev.ParentID, ev.ParentID)
		prev.ProjectPath = firstNonEmpty(prev.ProjectPath, ev.ProjectPath)
		prev.Model = firstNonEmpty(prev.Model, ev.Model)
		prev.Provider = firstNonEmpty(prev.Provider, ev.Provider)
		if prev.Timestamp.IsZero() {
			prev.Timestamp = ev.Timestamp
		}
		a.evs[ev.DedupKey] = prev
		return
	}
	a.evs[ev.DedupKey] = ev
	a.order = append(a.order, ev.DedupKey)
}

func (a *accumulator) session(info sessionInfo) {
	if info.ID == "" {
		return
	}
	started := epochMS(info.Created)
	updated := epochMS(info.Updated)
	if updated.Before(started) {
		updated = started
	}
	meta := model.SessionMeta{
		Harness:   model.OpenCode,
		SessionID: info.ID,
		ParentID:  info.ParentID,
		Title:     harness.Title(info.Title),
		Project:   info.Directory,
		StartedAt: started,
		UpdatedAt: updated,
	}
	if prev, ok := a.metas[info.ID]; ok {
		meta = mergeMeta(prev, meta)
	} else {
		a.mord = append(a.mord, info.ID)
	}
	a.metas[info.ID] = meta
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

func mergeMeta(a, b model.SessionMeta) model.SessionMeta {
	a.ParentID = firstNonEmpty(a.ParentID, b.ParentID)
	a.Title = firstNonEmpty(a.Title, b.Title)
	a.Project = firstNonEmpty(a.Project, b.Project)
	if a.StartedAt.IsZero() || (!b.StartedAt.IsZero() && b.StartedAt.Before(a.StartedAt)) {
		a.StartedAt = b.StartedAt
	}
	if b.UpdatedAt.After(a.UpdatedAt) {
		a.UpdatedAt = b.UpdatedAt
	}
	return a
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// num decodes JSON numbers that may arrive as integers, floats, quoted strings
// or null.
type num int64

func (n *num) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	if len(s) > 1 && s[0] == '"' && s[len(s)-1] == '"' {
		s = strings.TrimSpace(s[1 : len(s)-1])
		if s == "" {
			*n = 0
			return nil
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("opencode: cannot decode %q as a number", string(b))
	}
	*n = num(f)
	return nil
}

func clampNum(n num) int64 {
	if n < 0 {
		return 0
	}
	return int64(n)
}

func int64Of(n *num) int64 {
	if n == nil {
		return 0
	}
	return int64(*n)
}

// payloadTime returns the message's completion timestamp (UTC, zero when the
// log carries none).
func payloadTime(p messagePayload) time.Time {
	if p.Time.Created != nil {
		return epochMS(int64(*p.Time.Created))
	}
	return time.Time{}
}

// epochMS normalises a unix timestamp expressed in milliseconds or seconds.
func epochMS(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	if v < 1e12 {
		v *= 1000
	}
	return time.UnixMilli(v).UTC()
}

// reportedCost keeps only a finite, positive cost: opencode writes 0 for
// "unknown", and a provider-reported cost wins over any estimate.
func reportedCost(c *float64) *float64 {
	if c == nil {
		return nil
	}
	v := *c
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return nil
	}
	return &v
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func cursorFor(st os.FileInfo, fp string, offset int64, extra string) harness.Cursor {
	return harness.Cursor{
		Offset:      offset,
		Size:        st.Size(),
		ModTime:     st.ModTime(),
		Fingerprint: fp,
		Extra:       extra,
	}
}

// fingerprint returns the sha1 of the first fingerprintBytes of path.
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
