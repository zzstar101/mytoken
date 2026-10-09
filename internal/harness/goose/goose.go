// Package goose parses the Goose CLI session database.
//
// Goose (block/goose) keeps every session in one SQLite database. The database
// is found at, in order:
//
//	$GOOSE_PATH_ROOT/data/sessions/sessions.db
//	$XDG_DATA_HOME/goose/sessions/sessions.db        (default ~/.local/share)
//	%APPDATA%/Block/goose/sessions/sessions.db       (Windows)
//
// MYTOKEN_GOOSE_DIRS overrides the search path with a list of directories (or
// database files) separated by the platform's list separator; a directory may
// be the sessions directory itself or the Goose root above data/sessions.
//
// The sessions table keeps a running total per session in
// accumulated_input_tokens / accumulated_output_tokens instead of one row per
// model call, so usage is reported as the growth of those totals: the first
// parse reports the whole total and later parses report only what the session
// gained. The cursor keeps the high-water mark, so a database that is rewritten
// or truncated can never produce negative or duplicated usage.
//
// Goose does not record cache reads/writes, reasoning tokens, request ids, a
// parent session, cost or a context-compaction marker, so those fields stay
// empty. The project is the session's working directory and the title comes
// from the first user message.
package goose

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"

	_ "modernc.org/sqlite"
)

const (
	// sessionsDBName is the database Goose stores its sessions in.
	sessionsDBName = "sessions.db"
	// kindSQLite marks a source that lives inside a SQLite database.
	kindSQLite = "sqlite"
	// rootsEnv overrides the search path with a list separated by the
	// platform's list separator.
	rootsEnv = "MYTOKEN_GOOSE_DIRS"
	// gooseRootEnv is Goose's own data-root override.
	gooseRootEnv = "GOOSE_PATH_ROOT"
	// busyTimeoutMS is how long a read waits for a concurrent writer.
	busyTimeoutMS = 5000
	// fingerprintBytes is how much of the database head is hashed.
	fingerprintBytes = 4096
)

// Parser reads Goose session databases.
type Parser struct{ roots []string }

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

// New returns a parser for the default Goose locations.
func New() *Parser { return NewWithRoots(defaultRoots()...) }

// NewWithRoot returns a parser for one root.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser for the given roots, dropping duplicates and
// empty entries.
func NewWithRoots(roots ...string) *Parser {
	seen := make(map[string]bool, len(roots))
	out := make([]string, 0, len(roots))
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
		out = append(out, root)
	}
	return &Parser{roots: out}
}

// Harness reports the harness these events belong to.
func (p *Parser) Harness() model.Harness { return model.Goose }

// Roots returns the configured search roots.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// defaultRoots resolves the platform locations Goose writes to.
func defaultRoots() []string {
	if dirs := strings.TrimSpace(os.Getenv(rootsEnv)); dirs != "" {
		return filepath.SplitList(dirs)
	}
	var out []string
	if root := strings.TrimSpace(os.Getenv(gooseRootEnv)); root != "" {
		out = append(out, filepath.Join(root, "data", "sessions"))
	}
	if runtime.GOOS == "windows" {
		base := strings.TrimSpace(os.Getenv("APPDATA"))
		if base == "" {
			base = filepath.Join(harness.Home(), "AppData", "Roaming")
		}
		return append(out, filepath.Join(base, "Block", "goose", "sessions"))
	}
	base := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if base == "" {
		base = filepath.Join(harness.Home(), ".local", "share")
	}
	return append(out, filepath.Join(base, "goose", "sessions"))
}

// Discover finds every session in the databases under the roots. Each session
// becomes its own source, named "<database>:<session id>", so that cursors and
// incremental reads stay per session.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	var out []harness.Source
	seen := make(map[string]bool)
	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path, ok := databasePath(root)
		if !ok {
			continue
		}
		ids, err := readSessionIDs(ctx, path)
		if err != nil {
			continue
		}
		for _, id := range ids {
			src := path + ":" + id
			if seen[src] {
				continue
			}
			seen[src] = true
			out = append(out, harness.Source{Path: src, Kind: kindSQLite})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Parse reports the usage a session gained since the cursor was written.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	dbPath, sessionID, ok := splitSource(src.Path)
	if !ok {
		return harness.Batch{Next: cur}, nil
	}
	st, err := os.Stat(dbPath)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	fp, err := fingerprint(dbPath)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	state := decodeState(cur.Extra)
	if cur.Fingerprint == fp && cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime()) {
		return harness.Batch{Next: cur}, nil
	}
	// The database only ever grows while Goose appends to it, so a smaller or
	// equally sized file with a new fingerprint was rewritten: start over from
	// the current totals rather than trusting a stale high-water mark.
	if cur.Fingerprint != "" && cur.Fingerprint != fp && st.Size() <= cur.Size {
		state = gooseState{}
	}
	next := harness.Cursor{
		Offset:      st.Size(),
		Size:        st.Size(),
		ModTime:     st.ModTime(),
		Fingerprint: fp,
	}

	db, err := openDB(dbPath)
	if err != nil {
		next.Extra = encodeState(state)
		return harness.Batch{Next: next}, nil
	}
	defer db.Close()

	row, err := readSession(ctx, db, sessionID)
	if err != nil {
		next.Extra = encodeState(state)
		return harness.Batch{Next: next}, nil
	}

	var batch harness.Batch
	started := parseTimeString(row.CreatedAt)
	updated := parseTimeString(row.UpdatedAt)
	if updated.Before(started) {
		updated = started
	}
	tokens := model.Tokens{}
	if row.Input > state.Prompt {
		tokens.Input = row.Input - state.Prompt
	}
	if row.Output > state.Completion {
		tokens.Output = row.Output - state.Completion
	}
	if state.Prompt < row.Input {
		state.Prompt = row.Input
	}
	if state.Completion < row.Output {
		state.Completion = row.Output
	}
	ts := updated
	if ts.IsZero() {
		ts = started
	}
	if tokens.Input != 0 || tokens.Output != 0 {
		// Without a recorded time the usage still has to be reported: leaving
		// the totals unreported would drop it, so the event carries the zero
		// timestamp rather than being dropped.
		batch.Events = append(batch.Events, model.UsageEvent{
			Harness:     model.Goose,
			DedupKey:    fmt.Sprintf("goose:%s:%d:%d", row.ID, row.Input, row.Output),
			SessionID:   row.ID,
			ProjectPath: row.WorkingDir,
			Timestamp:   ts,
			Model:       row.Model,
			Provider:    row.Provider,
			Tokens:      tokens,
		})
	}
	if !started.IsZero() || !updated.IsZero() {
		if updated.Before(started) {
			updated = started
		}
		batch.Sessions = append(batch.Sessions, model.SessionMeta{
			Harness:   model.Goose,
			SessionID: row.ID,
			Title:     harness.Title(firstUserText(ctx, db, row.ID)),
			Project:   projectName(row.WorkingDir),
			StartedAt: started,
			UpdatedAt: updated,
		})
	}
	next.Extra = encodeState(state)
	batch.Next = next
	return batch, nil
}

// gooseState is the high-water mark of a session's running totals.
type gooseState struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
}

func decodeState(raw string) gooseState {
	var state gooseState
	if raw == "" {
		return state
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return gooseState{}
	}
	return state
}

func encodeState(state gooseState) string {
	raw, err := json.Marshal(state)
	if err != nil {
		return ""
	}
	return string(raw)
}

// sessionRow is one row of the sessions table.
type sessionRow struct {
	ID          string
	WorkingDir  string
	CreatedAt   string
	UpdatedAt   string
	Input       int64
	Output      int64
	Provider    string
	ModelConfig []byte
	Model       string
}

// readSession loads one session and the model it recorded.
func readSession(ctx context.Context, db *sql.DB, sessionID string) (sessionRow, error) {
	const q = `SELECT id, working_dir, created_at, updated_at,
		accumulated_input_tokens, accumulated_output_tokens,
		provider_name, CAST(model_config_json AS BLOB)
		FROM sessions WHERE id = ?`
	var (
		row       sessionRow
		working   sql.NullString
		created   sql.NullString
		updated   sql.NullString
		input     sql.NullInt64
		output    sql.NullInt64
		provider  sql.NullString
		modelJSON []byte
	)
	err := db.QueryRowContext(ctx, q, sessionID).Scan(
		&row.ID, &working, &created, &updated, &input, &output, &provider, &modelJSON,
	)
	if err != nil {
		return sessionRow{}, err
	}
	row.WorkingDir = working.String
	row.CreatedAt = strings.TrimSpace(created.String)
	row.UpdatedAt = strings.TrimSpace(updated.String)
	row.Input = maxInt64(input.Int64, 0)
	row.Output = maxInt64(output.Int64, 0)
	row.Provider = provider.String
	row.ModelConfig = modelJSON
	row.Model = modelName(modelJSON)
	return row, nil
}

// readSessionIDs lists the sessions in a database, newest first, the way
// Goose's own listing orders them.
func readSessionIDs(ctx context.Context, path string) ([]string, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id FROM sessions ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if strings.TrimSpace(id.String) != "" {
			out = append(out, id.String)
		}
	}
	return out, rows.Err()
}

// firstUserText returns the first text block of the session's first user
// message, which is what Goose shows as the session title.
func firstUserText(ctx context.Context, db *sql.DB, sessionID string) string {
	const q = `SELECT content_json FROM messages
		WHERE session_id = ? AND role = 'user'
		ORDER BY created_timestamp ASC LIMIT 1`
	var raw []byte
	if err := db.QueryRowContext(ctx, q, sessionID).Scan(&raw); err != nil {
		return ""
	}
	return firstTextBlock(raw)
}

// firstTextBlock pulls the first non-empty text item out of a content_json
// array. Some builds wrap the array in a JSON string.
func firstTextBlock(raw []byte) string {
	items, ok := decodeContent(raw)
	if !ok {
		return ""
	}
	for _, item := range items {
		if item.Type == "text" && strings.TrimSpace(item.Text) != "" {
			return item.Text
		}
	}
	return ""
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func decodeContent(raw []byte) ([]contentItem, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, false
	}
	items, err := unmarshalContent([]byte(trimmed))
	if err != nil {
		return nil, false
	}
	return items, true
}

func unmarshalContent(raw []byte) ([]contentItem, error) {
	var items []contentItem
	if err := json.Unmarshal(raw, &items); err == nil {
		return items, nil
	}
	var inner string
	if err := json.Unmarshal(raw, &inner); err != nil {
		return nil, err
	}
	items = nil
	if err := json.Unmarshal([]byte(inner), &items); err != nil {
		return nil, err
	}
	return items, nil
}

// modelName reads the model out of model_config_json, which Goose stores as
// either an object or a JSON string holding that object.
func modelName(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ""
	}
	var cfg struct {
		ModelName string `json:"model_name"`
	}
	if err := json.Unmarshal([]byte(trimmed), &cfg); err == nil && cfg.ModelName != "" {
		return cfg.ModelName
	}
	var inner string
	if err := json.Unmarshal([]byte(trimmed), &inner); err != nil {
		return ""
	}
	return modelName([]byte(inner))
}

// projectName is the display name for a session's working directory.
func projectName(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "goose"
	}
	name := filepath.Base(filepath.Clean(dir))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "goose"
	}
	return name
}

// databasePath resolves the sessions database under one root. A root may be the
// database itself, the directory holding it, or the Goose root above
// data/sessions.
func databasePath(root string) (string, bool) {
	candidates := []string{root}
	if !strings.HasSuffix(strings.ToLower(root), ".db") {
		candidates = append(candidates,
			filepath.Join(root, sessionsDBName),
			filepath.Join(root, "data", "sessions", sessionsDBName),
		)
	}
	for _, path := range candidates {
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			continue
		}
		return path, true
	}
	return "", false
}

// splitSource splits "<database>:<session id>".
func splitSource(path string) (dbPath, sessionID string, ok bool) {
	i := strings.LastIndex(path, ":")
	if i <= 0 {
		return "", "", false
	}
	dbPath, sessionID = path[:i], path[i+1:]
	if sessionID == "" || !strings.HasSuffix(strings.ToLower(dbPath), ".db") {
		return "", "", false
	}
	return dbPath, sessionID, true
}

// openDB opens a database read-only enough for the scanner: one connection and
// a busy timeout so a writer never turns a read into an error.
func openDB(path string) (*sql.DB, error) {
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

// fingerprint hashes the head of the database together with its size, so a
// rewritten file is never mistaken for the one the cursor was built from.
func fingerprint(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	head := make([]byte, fingerprintBytes)
	n, err := io.ReadFull(file, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	sum := sha1.Sum(head[:n])
	return hex.EncodeToString(sum[:]), nil
}

// parseTimeString accepts the ISO forms Goose writes, with or without a
// timezone. A timestamp without one is UTC.
func parseTimeString(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts.UTC()
		}
		if ts, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return ts.UTC()
		}
	}
	return time.Time{}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
