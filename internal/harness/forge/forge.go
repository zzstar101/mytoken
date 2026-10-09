// Package forge parses the Forge (Factory) conversation database.
//
// Forge keeps every conversation in one SQLite database at
// ~/.forge/.forge.db on every platform (%USERPROFILE%\.forge\.forge.db on
// Windows, $HOME/.forge/.forge.db elsewhere); Forge itself has no XDG or
// %APPDATA% location. MYTOKEN_FORGE_DIRS overrides the search path with a list
// of directories or database files separated by the platform's list
// separator.
//
// The conversations table keeps the whole transcript of a conversation in its
// context column as JSON:
//
//	{"messages":[{"message":{"text":{"role","content","model","tool_calls"}},
//	              "usage":{"prompt_tokens":{"actual":1200}, ...}}]}
//
// Every assistant message that carries usage becomes one event. Forge reports
// prompt tokens inclusive of the cached ones, so the cached count is
// subtracted from the prompt count and reported as cache reads. Only the
// "actual" numbers are read: Forge can also store estimated usage alongside
// them, and MyToken does not take estimates.
//
// Forge records no cache writes, reasoning tokens, provider request id, parent
// conversation, compaction marker or cost, so those fields stay empty and the
// cost is left for MyToken's own pricing. The only timestamp Forge stores is
// on the conversation row (updated_at, falling back to created_at), so every
// event of one conversation shares it; a row without either timestamp is
// skipped rather than dated to the epoch.
//
// The project is the workspace the conversation belongs to: the workspace's
// path when the database has a workspaces table mapping the conversation's
// workspace_id to one, and the workspace id itself otherwise. A conversation
// whose workspace id is absent or zero has no project. The title is never used
// as a project label — it is conversation text and is only stored as the
// session title — and the workspace path is also reported as each event's
// ProjectPath.
package forge

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"

	_ "modernc.org/sqlite"
)

const (
	// dbName is the database Forge keeps its conversations in.
	dbName = ".forge.db"
	// legacyDBName is accepted as well, for installations that name the file
	// without the leading dot.
	legacyDBName = "forge.db"
	// defaultDirName is the directory Forge uses below the home directory.
	defaultDirName = ".forge"
	// kindSQLite marks a source that lives inside a SQLite database.
	kindSQLite = "sqlite"
	// rootsEnv overrides the search path with a list separated by the
	// platform's list separator.
	rootsEnv = "MYTOKEN_FORGE_DIRS"
	// busyTimeoutMS is how long a read waits for a concurrent writer.
	busyTimeoutMS = 5000
	// fingerprintBytes is how much of the database head is hashed.
	fingerprintBytes = 4096
)

// Parser reads Forge conversation databases.
type Parser struct{ roots []string }

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

// New returns a parser for the default Forge location.
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
func (p *Parser) Harness() model.Harness { return model.Forge }

// Roots returns the configured search roots.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// defaultRoots resolves the location Forge writes to. Forge puts its database
// below the home directory on every platform.
func defaultRoots() []string {
	if dirs := strings.TrimSpace(os.Getenv(rootsEnv)); dirs != "" {
		return filepath.SplitList(dirs)
	}
	return []string{filepath.Join(harness.Home(), defaultDirName)}
}

// Discover finds every conversation that has a context blob. Each conversation
// becomes its own source, named "<database>:<conversation id>", so cursors and
// incremental reads stay per conversation.
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
		ids, err := readConversationIDs(ctx, path)
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

// Parse reports the usage of one conversation. Forge keeps the transcript in a
// single JSON column rather than one row per call, so the parser walks the
// messages and reports every assistant message that carries usage. The cursor
// is only used to skip a database that did not change; the events themselves
// are re-derived, which keeps them identical to a cold read as long as the
// conversation row is not restamped.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	dbPath, conversationID, ok := splitSource(src.Path)
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
	if cur.Fingerprint == fp && cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime()) {
		return harness.Batch{Next: cur}, nil
	}
	next := harness.Cursor{
		Offset:      st.Size(),
		Size:        st.Size(),
		ModTime:     st.ModTime(),
		Fingerprint: fp,
	}

	db, err := openDB(dbPath)
	if err != nil {
		return harness.Batch{Next: next}, nil
	}
	defer db.Close()

	row, err := readConversation(ctx, db, conversationID)
	if err != nil {
		return harness.Batch{Next: next}, nil
	}

	started := parseTimeString(row.CreatedAt)
	updated := parseTimeString(row.UpdatedAt)
	if updated.Before(started) {
		updated = started
	}
	// Forge stamps the conversation, not the individual message.
	ts := updated
	if ts.IsZero() {
		ts = started
	}

	messages := decodeContext(row.Context)
	// The workspace label names the project, on the session and on every event
	// of it. It never comes from the conversation title: titles are
	// conversation text and projects are shown outside the conversation.
	project := workspaceLabel(ctx, db, row.WorkspaceID)
	var batch harness.Batch
	for i, message := range messages {
		if !strings.EqualFold(strings.TrimSpace(message.Message.Text.Role), "assistant") {
			continue
		}
		prompt := message.usageCount("prompt_tokens")
		output := message.usageCount("completion_tokens")
		cached := message.usageCount("cached_tokens")
		input := maxInt64(prompt-cached, 0)
		if input == 0 && output == 0 {
			continue
		}
		if ts.IsZero() {
			continue
		}
		callID := firstCallID(message.Message.Text.ToolCalls)
		key := fmt.Sprintf("forge:%s:%s", conversationID, callID)
		if callID == "" {
			key = fmt.Sprintf("forge:%s:%s:%d:%d:%d", conversationID, message.Message.Text.Model, prompt, output, i)
		}
		batch.Events = append(batch.Events, model.UsageEvent{
			Harness:     model.Forge,
			DedupKey:    key,
			SessionID:   conversationID,
			Timestamp:   ts,
			Model:       message.Message.Text.Model,
			ProjectPath: project,
			// Forge stores no provider request id; the first tool call id is
			// the only per-message identifier it exposes.
			RequestID: callID,
			Tokens: model.Tokens{
				Input:     input,
				Output:    output,
				CacheRead: cached,
			},
		})
	}

	if !started.IsZero() || !updated.IsZero() {
		batch.Sessions = append(batch.Sessions, model.SessionMeta{
			Harness:   model.Forge,
			SessionID: conversationID,
			Title:     firstNonEmpty(harness.Title(row.Title), harness.Title(firstUserText(messages))),
			Project:   project,
			StartedAt: started,
			UpdatedAt: updated,
		})
	}
	batch.Next = next
	return batch, nil
}

// conversationRow is one row of the conversations table.
type conversationRow struct {
	ID          string
	Title       string
	WorkspaceID string
	Context     string
	CreatedAt   string
	UpdatedAt   string
}

// readConversation loads one conversation. workspace_id is cast to text the way
// the reference parser does: Forge can store numbers beyond the range of a
// signed 64 bit integer.
func readConversation(ctx context.Context, db *sql.DB, conversationID string) (conversationRow, error) {
	const q = `SELECT conversation_id, title, CAST(workspace_id AS TEXT) AS workspace_id,
		context, created_at, updated_at
		FROM conversations WHERE conversation_id = ?`
	var (
		row       conversationRow
		title     sql.NullString
		workspace sql.NullString
		context   sql.NullString
		created   sql.NullString
		updated   sql.NullString
	)
	err := db.QueryRowContext(ctx, q, conversationID).Scan(
		&row.ID, &title, &workspace, &context, &created, &updated,
	)
	if err != nil {
		return conversationRow{}, err
	}
	row.Title = strings.TrimSpace(title.String)
	row.WorkspaceID = strings.TrimSpace(workspace.String)
	row.Context = context.String
	row.CreatedAt = strings.TrimSpace(created.String)
	row.UpdatedAt = strings.TrimSpace(updated.String)
	return row, nil
}

// readConversationIDs lists the conversations that have a context blob, which
// is what the reference parser reports as sessions.
func readConversationIDs(ctx context.Context, path string) ([]string, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT conversation_id FROM conversations
		WHERE context IS NOT NULL ORDER BY conversation_id`)
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

// contextMessage is one entry of the context JSON's messages array.
type contextMessage struct {
	Message struct {
		Text struct {
			Role      string          `json:"role"`
			Content   string          `json:"content"`
			Model     string          `json:"model"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		} `json:"text"`
	} `json:"message"`
	Usage map[string]usageValue `json:"usage"`
}

// usageCount reads one usage counter, treating a missing or negative value as
// zero.
func (m contextMessage) usageCount(key string) int64 {
	value, ok := m.Usage[key]
	if !ok {
		return 0
	}
	return maxInt64(value.Actual, 0)
}

// usageValue is Forge's {"actual": n} wrapper. A bare number is accepted too.
type usageValue struct{ Actual int64 }

func (v *usageValue) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if raw[0] == '{' {
		var wrapper struct {
			Actual count `json:"actual"`
		}
		if err := json.Unmarshal(raw, &wrapper); err != nil {
			return nil
		}
		v.Actual = int64(wrapper.Actual)
		return nil
	}
	var value count
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	v.Actual = int64(value)
	return nil
}

// count accepts the numeric shapes Forge stores: a number, a numeric string or
// null. Anything else counts as zero.
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

// decodeContext pulls the messages out of the context column. A context that
// is empty or not valid JSON yields no messages; the session itself is still
// reported.
func decodeContext(raw string) []contextMessage {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed[0] != '{' {
		return nil
	}
	var envelope struct {
		Messages []contextMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		return nil
	}
	return envelope.Messages
}

// firstUserText is the first non-empty user message, used as the title when
// Forge has not stored one.
func firstUserText(messages []contextMessage) string {
	for _, message := range messages {
		if !strings.EqualFold(strings.TrimSpace(message.Message.Text.Role), "user") {
			continue
		}
		if text := strings.TrimSpace(message.Message.Text.Content); text != "" {
			return text
		}
	}
	return ""
}

// firstCallID is the first tool call id of a message, the identifier the
// reference parser prefers for deduplication.
func firstCallID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var calls []struct {
		CallID string `json:"call_id"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil {
		return ""
	}
	for _, call := range calls {
		if id := strings.TrimSpace(call.CallID); id != "" {
			return id
		}
	}
	return ""
}

// workspaceLabel names the workspace a conversation belongs to. Forge's
// conversation row only carries a numeric workspace id, so the label is the
// workspace's on-disk path when the database maps the id to one, and the id
// itself otherwise. The conversation title is deliberately never used: a title
// is conversation text, and project labels are shown outside the conversation.
func workspaceLabel(ctx context.Context, db *sql.DB, workspaceID string) string {
	id := strings.TrimSpace(workspaceID)
	if id == "" || id == "0" {
		return ""
	}
	if path := workspacePath(ctx, db, id); path != "" {
		return path
	}
	return id
}

// workspacePath resolves a workspace id to its directory. Forge's own schema is
// the conversations table alone, but an installation that also keeps a
// workspaces table (either spelling, with any of the usual id and path column
// names) can be labelled by its directory instead of by a bare number. Every
// name here comes from sqlite_master and is quoted before it reaches SQL.
func workspacePath(ctx context.Context, db *sql.DB, workspaceID string) string {
	names, err := workspaceTables(ctx, db)
	if err != nil {
		return ""
	}
	for _, name := range names {
		columns, err := tableColumns(ctx, db, name)
		if err != nil {
			continue
		}
		pathColumn := firstColumn(columns, "path", "root_path", "workspace_path", "directory", "root", "dir", "folder")
		idColumn := firstColumn(columns, "workspace_id", "id", "uuid", "workspace_uuid")
		if pathColumn == "" || idColumn == "" {
			continue
		}
		query := fmt.Sprintf("SELECT CAST(%s AS TEXT) FROM %s WHERE CAST(%s AS TEXT) = ?",
			quoteIdent(pathColumn), quoteIdent(name), quoteIdent(idColumn))
		var path sql.NullString
		if err := db.QueryRowContext(ctx, query, workspaceID).Scan(&path); err != nil {
			continue
		}
		if value := strings.TrimSpace(path.String); value != "" {
			return value
		}
	}
	return ""
}

// workspaceTables lists the workspace tables the database has, if any.
func workspaceTables(ctx context.Context, db *sql.DB) ([]string, error) {
	const q = `SELECT name FROM sqlite_master
		WHERE type = 'table' AND lower(name) IN ('workspaces', 'workspace')
		ORDER BY name`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// tableColumns lists a table's column names.
func tableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+quoteIdent(table)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// firstColumn returns the first of the wanted column names the table has, in
// the order the caller prefers them.
func firstColumn(columns []string, wanted ...string) string {
	for _, want := range wanted {
		for _, have := range columns {
			if strings.EqualFold(have, want) {
				return have
			}
		}
	}
	return ""
}

// quoteIdent quotes a SQLite identifier, so a table or column name read out of
// sqlite_master cannot change the shape of the query it is used in.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// databasePath resolves the conversations database under one root. A root may
// be the database itself, the directory holding it, or the Forge root.
func databasePath(root string) (string, bool) {
	candidates := []string{root}
	if !strings.HasSuffix(strings.ToLower(root), ".db") {
		candidates = append(candidates,
			filepath.Join(root, dbName),
			filepath.Join(root, legacyDBName),
			filepath.Join(root, defaultDirName, dbName),
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

// splitSource splits "<database>:<conversation id>".
func splitSource(path string) (dbPath, conversationID string, ok bool) {
	i := strings.LastIndex(path, ":")
	if i <= 0 {
		return "", "", false
	}
	dbPath, conversationID = path[:i], path[i+1:]
	if conversationID == "" || !strings.HasSuffix(strings.ToLower(dbPath), ".db") {
		return "", "", false
	}
	return dbPath, conversationID, true
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

// parseTimeString accepts the timestamp shapes Forge writes, with or without a
// timezone and with or without fractional seconds. A timestamp without a zone
// is UTC.
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
