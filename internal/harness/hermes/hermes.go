// Package hermes parses Hermes session usage from its SQLite state database.
//
// Hermes keeps everything in one SQLite database per profile:
//
//	<root>/state.db                  $HERMES_HOME or ~/.hermes
//	<root>/profiles/<name>/state.db  extra profiles
//
// Only the usage tables are read:
//
//	sessions(id, parent_session_id, model, billing_provider, billing_base_url,
//	         cwd, git_repo_root, title, started_at, ended_at, last_activity_at,
//	         input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
//	         reasoning_tokens, estimated_cost_usd, actual_cost_usd, ...)
//	session_model_usage(session_id, model, billing_provider, billing_base_url,
//	         task, api_call_count, input_tokens, output_tokens,
//	         cache_read_tokens, cache_write_tokens, reasoning_tokens,
//	         estimated_cost_usd, actual_cost_usd, first_seen, last_seen)
//	messages(id, session_id, role, content, timestamp, ...)
//
// Token accounting. Hermes stores per-session (and per-session-model) counters,
// not per-request usage, and the five classes are already disjoint:
// input_tokens excludes the cache classes and reasoning_tokens is separate from
// output_tokens, so they map one-to-one onto model.Tokens with no subtraction.
// messages.token_count is NULL on every row in the wild and is not used.
//
// One event per (session, model). Because the counters are cumulative, the
// event is a snapshot: the same DedupKey is re-emitted with higher totals when
// the session grows, and the store's upsert on (harness,dedup_key) keeps the
// latest snapshot (no double counting). Re-parsing an unchanged database never
// re-emits anything: the cursor's combined signature (db plus -wal/-shm
// size+mtime) plus the first-4KB fingerprint short-circuits the read.
// A session that never used a model (no session_model_usage row, all-zero
// counters) produces no event but still yields its SessionMeta.
//
// Dating caveat. The schema stores cumulative counters only, so every event is
// dated at the row's last_seen: a session's whole usage lands on the day the
// session was last active. Daily aggregates therefore skew towards that day
// instead of the days the calls actually happened, and the per-message
// timestamps that would allow exact dating are not paired with usage anywhere
// in the database.
//
// Provenance. Provider and BaseURL come from billing_provider/billing_base_url
// and are never inferred; an absent value stays empty. CostUSD carries the USD
// figure Hermes itself recorded (actual_cost_usd, else estimated_cost_usd) and
// stays nil when the log has none — credits and other native units would go to
// model.Bill instead, which Hermes does not expose. RequestID is unavailable:
// Hermes stores no upstream request/response id. Boundary is unavailable: the
// schema has no compact/clear/resume marker (messages.compacted is 0 on every
// observed row and the compression_* session columns only track failures), and
// missing records are not boundaries.
//
// Titles: sessions.title when set, else the first real user message (wrapper
// text such as <system-reminder ...> and slash commands are skipped), collapsed
// to <=60 runes by harness.Title. That title is the only conversation text read.
//
// The database is opened read-only (mode=ro, busy_timeout) with a single
// connection; a missing, locked or unreadable database simply keeps the
// previous cursor so the next scan retries.
package hermes

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
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/sqlitedsn"

	_ "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	fingerprintBytes = 4096
	busyTimeoutMS    = 5000
	stateDBName      = "state.db"
)

// slashCommandRE matches a leading slash command, which is never a title.
var slashCommandRE = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9_-]*(\s|$)`)

// Parser reads Hermes state databases. The zero value is not usable; use New or
// NewWithRoots.
type Parser struct {
	roots []string
}

// New returns a parser for the default Hermes home.
func New() *Parser { return &Parser{roots: DefaultRoots()} }

// NewWithRoots returns a parser reading the given Hermes homes.
func NewWithRoots(roots ...string) *Parser {
	if len(roots) == 0 {
		roots = DefaultRoots()
	}
	return &Parser{roots: roots}
}

func init() { harness.Register(New()) }

// DefaultRoots returns $HERMES_HOME, else ~/.hermes.
func DefaultRoots() []string {
	if v := strings.TrimSpace(os.Getenv("HERMES_HOME")); v != "" {
		return []string{v}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	return []string{filepath.Join(home, ".hermes")}
}

// Harness identifies the parser.
func (p *Parser) Harness() model.Harness { return model.Hermes }

// Roots returns the configured Hermes homes.
func (p *Parser) Roots() []string { return p.roots }

// Discover returns every state.db under the configured roots, the default
// profile first (sorted by path).
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	seen := map[string]struct{}{}
	out := make([]harness.Source, 0, len(p.roots))
	add := func(path string) {
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		out = append(out, harness.Source{Path: path, Kind: "sqlite"})
	}
	for _, root := range p.roots {
		if root == "" {
			continue
		}
		fi, err := os.Stat(root)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			if fi.Mode().IsRegular() && filepath.Base(root) == stateDBName {
				add(root)
			}
			continue
		}
		rootDB := filepath.Join(root, stateDBName)
		if st, err := os.Stat(rootDB); err == nil && st.Mode().IsRegular() {
			add(rootDB)
		}
		entries, err := os.ReadDir(filepath.Join(root, "profiles"))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dbPath := filepath.Join(root, "profiles", e.Name(), stateDBName)
			if st, err := os.Stat(dbPath); err == nil && st.Mode().IsRegular() {
				add(dbPath)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Parse reads one state database. Anything but a sqlite source is a no-op.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if src.Kind != "sqlite" {
		return harness.Batch{Next: cur}, nil
	}
	return p.parseDB(ctx, src, cur)
}

type dbSignature struct {
	size int64
	mod  time.Time
}

// signatureOf combines the database with its -wal/-shm companions: a commit can
// land entirely in the write-ahead log before any checkpoint touches the db.
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

// openReadOnly opens path as a read-only sqlite database. Harness data is never
// written to.
func openReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	q := url.Values{
		"mode":      {"ro"},
		"immutable": {"0"},
		"_pragma":   {fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS)},
	}
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

func (p *Parser) parseDB(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
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
		// Locked, unreadable or not a database: keep the cursor and retry.
		return harness.Batch{Next: cur}, nil
	}
	defer db.Close()

	tables := tableColumns(ctx, db)
	sessions, err := loadSessions(ctx, db, tables)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	usage, err := loadModelUsage(ctx, db, tables)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	titles := loadTitles(ctx, db, tables, sessions)

	// Latest activity per session, from the cumulative rows and the session row.
	activity := map[string]float64{}
	bump := func(id string, at float64) {
		if at > activity[id] {
			activity[id] = at
		}
	}
	for _, u := range usage {
		bump(u.SessionID, u.LastSeen)
	}
	for id, s := range sessions {
		bump(id, s.Ended)
		bump(id, s.LastActivity)
	}

	events := make([]model.UsageEvent, 0, len(usage)+len(sessions))
	addEvent := func(s sessionRow, modelName, provider, baseURL, task string, tokens model.Tokens, cost, at float64) {
		if tokens.IsZero() {
			return
		}
		ts := unixSeconds(at)
		if ts.IsZero() {
			ts = unixSeconds(s.Started)
		}
		events = append(events, model.UsageEvent{
			Harness:     model.Hermes,
			DedupKey:    dedupKey(s.ID, modelName, task),
			SessionID:   s.ID,
			ParentID:    s.ParentID,
			ProjectPath: s.Project,
			Timestamp:   ts,
			Model:       modelName,
			Provider:    firstNonEmpty(provider, s.Provider),
			BaseURL:     firstNonEmpty(baseURL, s.BaseURL),
			Tokens:      tokens,
			CostUSD:     costPointer(cost),
		})
	}

	for _, u := range usage {
		s, ok := sessions[u.SessionID]
		if !ok {
			// A usage row without its session row: keep the session id and the
			// usage, which is all the counters depend on.
			s = sessionRow{ID: u.SessionID}
		}
		addEvent(s, firstNonEmpty(u.Model, s.Model, "unknown"),
			u.Provider, u.BaseURL, u.Task,
			model.Tokens{Input: u.In, Output: u.Out, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Reasoning: u.Reasoning},
			u.Cost, firstNonZero(u.LastSeen, u.FirstSeen))
	}
	for id, s := range sessions {
		if hasUsageRow(usage, id) {
			continue
		}
		addEvent(s, firstNonEmpty(s.Model, "unknown"), s.Provider, s.BaseURL, "",
			model.Tokens{Input: s.In, Output: s.Out, CacheRead: s.CacheRead, CacheWrite: s.CacheWrite, Reasoning: s.Reasoning},
			s.Cost, s.Started)
	}

	metas := make([]model.SessionMeta, 0, len(sessions))
	for id, s := range sessions {
		title := strings.TrimSpace(s.Title)
		if title == "" {
			title = titles[id]
		}
		updated := activity[id]
		if updated < s.Started {
			updated = s.Started
		}
		meta := model.SessionMeta{
			Harness:   model.Hermes,
			SessionID: id,
			ParentID:  s.ParentID,
			Title:     harness.Title(title),
			Project:   s.Project,
			StartedAt: unixSeconds(s.Started),
			UpdatedAt: unixSeconds(updated),
		}
		if meta.UpdatedAt.IsZero() {
			meta.UpdatedAt = meta.StartedAt
		}
		metas = append(metas, meta)
	}

	return harness.Batch{Events: events, Sessions: metas, Next: harness.Cursor{
		Offset:      0,
		Size:        sig.size,
		ModTime:     sig.mod,
		Fingerprint: fp,
	}}, nil
}

type sessionRow struct {
	ID           string
	ParentID     string
	Model        string
	Provider     string
	BaseURL      string
	Project      string
	Title        string
	Started      float64
	Ended        float64
	LastActivity float64
	In           int64
	Out          int64
	CacheRead    int64
	CacheWrite   int64
	Reasoning    int64
	Cost         float64
}

type usageRow struct {
	SessionID  string
	Model      string
	Provider   string
	BaseURL    string
	Task       string
	In         int64
	Out        int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
	Cost       float64
	FirstSeen  float64
	LastSeen   float64
}

// loadSessions reads every session row, tolerating schema generations that lack
// some columns.
func loadSessions(ctx context.Context, db *sql.DB, tables map[string]map[string]bool) (map[string]sessionRow, error) {
	cols, ok := tables["sessions"]
	if !ok || !cols["id"] {
		return map[string]sessionRow{}, nil
	}
	q := "SELECT " + strings.Join([]string{
		textExpr(cols, "id"),
		textExpr(cols, "parent_session_id"),
		textExpr(cols, "model"),
		textExpr(cols, "billing_provider"),
		textExpr(cols, "billing_base_url"),
		textExpr(cols, "cwd"),
		textExpr(cols, "git_repo_root"),
		textExpr(cols, "title"),
		numExpr(cols, "started_at"),
		numExpr(cols, "ended_at"),
		numExpr(cols, "last_activity_at"),
		numExpr(cols, "input_tokens"),
		numExpr(cols, "output_tokens"),
		numExpr(cols, "cache_read_tokens"),
		numExpr(cols, "cache_write_tokens"),
		numExpr(cols, "reasoning_tokens"),
		numExpr(cols, "actual_cost_usd"),
		numExpr(cols, "estimated_cost_usd"),
	}, ", ") + " FROM " + quoteIdent("sessions")

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]sessionRow{}
	for rows.Next() {
		var (
			r                                       sessionRow
			cwd, gitRoot                            string
			started, ended, activity                float64
			in, outTok, cacheRead, cacheWrite, reas float64
			actual, estimated                       float64
		)
		if err := rows.Scan(&r.ID, &r.ParentID, &r.Model, &r.Provider, &r.BaseURL, &cwd, &gitRoot, &r.Title,
			&started, &ended, &activity, &in, &outTok, &cacheRead, &cacheWrite, &reas, &actual, &estimated); err != nil {
			return nil, err
		}
		r.Project = firstNonEmpty(cwd, gitRoot)
		r.Started, r.Ended, r.LastActivity = started, ended, activity
		r.In, r.Out, r.CacheRead, r.CacheWrite, r.Reasoning = count(in), count(outTok), count(cacheRead), count(cacheWrite), count(reas)
		r.Cost = firstNonZero(actual, estimated)
		out[r.ID] = r
	}
	return out, rows.Err()
}

// loadModelUsage reads the per-session-model counters.
func loadModelUsage(ctx context.Context, db *sql.DB, tables map[string]map[string]bool) ([]usageRow, error) {
	cols, ok := tables["session_model_usage"]
	if !ok || !cols["session_id"] {
		return nil, nil
	}
	q := "SELECT " + strings.Join([]string{
		textExpr(cols, "session_id"),
		textExpr(cols, "model"),
		textExpr(cols, "billing_provider"),
		textExpr(cols, "billing_base_url"),
		textExpr(cols, "task"),
		numExpr(cols, "input_tokens"),
		numExpr(cols, "output_tokens"),
		numExpr(cols, "cache_read_tokens"),
		numExpr(cols, "cache_write_tokens"),
		numExpr(cols, "reasoning_tokens"),
		numExpr(cols, "actual_cost_usd"),
		numExpr(cols, "estimated_cost_usd"),
		numExpr(cols, "first_seen"),
		numExpr(cols, "last_seen"),
	}, ", ") + " FROM " + quoteIdent("session_model_usage")

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []usageRow
	for rows.Next() {
		var (
			u                                       usageRow
			in, outTok, cacheRead, cacheWrite, reas float64
			actual, estimated                       float64
		)
		if err := rows.Scan(&u.SessionID, &u.Model, &u.Provider, &u.BaseURL, &u.Task,
			&in, &outTok, &cacheRead, &cacheWrite, &reas, &actual, &estimated, &u.FirstSeen, &u.LastSeen); err != nil {
			return nil, err
		}
		u.In, u.Out, u.CacheRead, u.CacheWrite, u.Reasoning = count(in), count(outTok), count(cacheRead), count(cacheWrite), count(reas)
		u.Cost = firstNonZero(actual, estimated)
		out = append(out, u)
	}
	return out, rows.Err()
}

// loadTitles derives the first usable user message per session, for sessions
// whose own title is empty.
func loadTitles(ctx context.Context, db *sql.DB, tables map[string]map[string]bool, sessions map[string]sessionRow) map[string]string {
	cols, ok := tables["messages"]
	if !ok || !cols["session_id"] || !cols["content"] || !cols["role"] {
		return nil
	}
	q := "SELECT " + strings.Join([]string{textExpr(cols, "session_id"), textExpr(cols, "content")}, ", ") +
		" FROM " + quoteIdent("messages") + " WHERE " + quoteIdent("role") + " = 'user'"
	order := make([]string, 0, 2)
	if cols["timestamp"] {
		order = append(order, quoteIdent("timestamp"))
	}
	if cols["id"] {
		order = append(order, quoteIdent("id"))
	}
	if len(order) > 0 {
		q += " ORDER BY " + strings.Join(order, ", ")
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			return out
		}
		if _, done := out[id]; done {
			continue
		}
		if s, ok := sessions[id]; ok && strings.TrimSpace(s.Title) != "" {
			continue
		}
		if text := titleText(content); text != "" {
			out[id] = text
		}
	}
	return out
}

// tableColumns maps every table and view to its column set so the queries can
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

// textExpr returns a column expression that always yields text.
func textExpr(cols map[string]bool, name string) string {
	if cols[name] {
		return "COALESCE(" + quoteIdent(name) + ",'')"
	}
	return "''"
}

// numExpr returns a column expression that always yields a number.
func numExpr(cols map[string]bool, name string) string {
	if cols[name] {
		return "COALESCE(" + quoteIdent(name) + ",0)"
	}
	return "0"
}

// count converts a sqlite numeric (integer or real) to a token count.
func count(v float64) int64 {
	if math.IsNaN(v) || v <= 0 {
		return 0
	}
	return int64(math.Round(v))
}

// unixSeconds converts Hermes' REAL seconds-since-epoch to UTC.
func unixSeconds(sec float64) time.Time {
	if sec <= 0 || math.IsNaN(sec) {
		return time.Time{}
	}
	whole := int64(sec)
	return time.Unix(whole, int64(math.Round((sec-float64(whole))*1e9))).UTC()
}

func costPointer(v float64) *float64 {
	if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// titleText extracts a title candidate from a user message body, which may be
// plain text or a JSON block array.
func titleText(raw string) string {
	text := strings.TrimSpace(textOf(raw))
	if text == "" || !titleCandidate(text) {
		return ""
	}
	return harness.Title(text)
}

func titleCandidate(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "<") {
		return false
	}
	return !slashCommandRE.MatchString(s)
}

// textOf flattens a string-or-blocks message body.
func textOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '[' {
		return raw
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(raw), &blocks); err != nil {
		return raw
	}
	var b strings.Builder
	for _, blk := range blocks {
		text := strings.TrimSpace(blk.Text)
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(text)
	}
	if b.Len() == 0 {
		return raw
	}
	return b.String()
}

func dedupKey(sessionID, modelName, task string) string {
	key := sessionID + "#" + modelName
	if task != "" {
		key += "#" + task
	}
	return key
}

func hasUsageRow(rows []usageRow, sessionID string) bool {
	for _, u := range rows {
		if u.SessionID == sessionID {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(values ...float64) float64 {
	for _, v := range values {
		if v > 0 && !math.IsNaN(v) {
			return v
		}
	}
	return 0
}
