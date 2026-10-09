// Package store owns the local, derived SQLite index.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zzstar101/mytoken/internal/sqlitedsn"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	_ "modernc.org/sqlite"
)

type Resolution struct {
	Provider string
	Attrib   model.AttribSource
	Cost     float64
	Priced   bool
}
type Store struct {
	db            *sql.DB
	writer        sync.Mutex
	mu            sync.Mutex
	subscribers   map[chan struct{}]struct{}
	projects      *projectResolver
	closed        bool
	aggregateZone string
}

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
	}
	dsn := path
	if path != ":memory:" {
		dsn = sqlitedsn.URI(path, "_pragma=busy_timeout(5000)")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	st := &Store{db: db, subscribers: make(map[chan struct{}]struct{})}
	// Keep bulk-index pages and temporary journals in memory without changing
	// WAL durability. A negative cache_size is a KiB budget, allocated on demand.
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA busy_timeout=5000; PRAGMA temp_store=MEMORY; PRAGMA cache_size=-8192;
 CREATE TABLE IF NOT EXISTS events (
 harness TEXT NOT NULL,dedup_key TEXT NOT NULL,session_id TEXT NOT NULL,parent_id TEXT NOT NULL DEFAULT '',project TEXT NOT NULL DEFAULT '',timestamp INTEGER NOT NULL,
 model TEXT NOT NULL,provider TEXT NOT NULL DEFAULT '',base_url TEXT NOT NULL DEFAULT '',input INTEGER NOT NULL,output INTEGER NOT NULL,cache_read INTEGER NOT NULL,cache_write INTEGER NOT NULL,reasoning INTEGER NOT NULL,
 log_cost REAL,resolved_provider TEXT NOT NULL,attrib TEXT NOT NULL,cost REAL NOT NULL,PRIMARY KEY(harness,dedup_key));
 CREATE TABLE IF NOT EXISTS sessions(harness TEXT NOT NULL,session_id TEXT NOT NULL,parent_id TEXT NOT NULL DEFAULT '',title TEXT NOT NULL DEFAULT '',project TEXT NOT NULL DEFAULT '',started_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,PRIMARY KEY(harness,session_id));
 CREATE INDEX IF NOT EXISTS sessions_parent ON sessions(harness,parent_id);
 CREATE TABLE IF NOT EXISTS cursors(harness TEXT NOT NULL,path TEXT NOT NULL,cursor TEXT NOT NULL,PRIMARY KEY(harness,path));
 CREATE TABLE IF NOT EXISTS provider_configs(harness TEXT NOT NULL,from_time INTEGER NOT NULL,base_url TEXT NOT NULL,provider TEXT NOT NULL,PRIMARY KEY(harness,from_time));
 CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);`); err != nil {
		db.Close()
		return nil, err
	}
	if err = st.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return st, nil
}
func (s *Store) DB() *sql.DB { return s.db }
func (s *Store) Close() error {
	s.mu.Lock()
	s.closed = true
	for ch := range s.subscribers {
		close(ch)
		delete(s.subscribers, ch)
	}
	s.mu.Unlock()
	return s.db.Close()
}
func (s *Store) Exec(ctx context.Context, q string, args ...any) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}
func (s *Store) Cursor(ctx context.Context, h model.Harness, path string) (harness.Cursor, error) {
	var c harness.Cursor
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT cursor FROM cursors WHERE harness=? AND path=?", h, path).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	err = json.Unmarshal([]byte(raw), &c)
	return c, err
}
func stamp(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

const sessionSQL = `INSERT INTO sessions(harness,session_id,parent_id,title,project,started_at,updated_at,raw_project) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(harness,session_id) DO UPDATE SET parent_id=CASE WHEN excluded.parent_id!='' THEN excluded.parent_id ELSE sessions.parent_id END,title=CASE WHEN excluded.title!='' THEN excluded.title ELSE sessions.title END,project=CASE WHEN excluded.project!='' THEN excluded.project ELSE sessions.project END,raw_project=CASE WHEN excluded.raw_project!='' THEN excluded.raw_project ELSE sessions.raw_project END,started_at=CASE WHEN sessions.started_at=0 THEN excluded.started_at WHEN excluded.started_at=0 THEN sessions.started_at ELSE min(sessions.started_at,excluded.started_at) END,updated_at=max(sessions.updated_at,excluded.updated_at)`

// Write couples a parsed source with its resolutions and checkpoint.
type Write struct {
	Harness     model.Harness
	Path        string
	Batch       harness.Batch
	Resolutions []Resolution
}

func (s *Store) Commit(ctx context.Context, h model.Harness, path string, b harness.Batch, res []Resolution) error {
	return s.CommitMany(ctx, []Write{{h, path, b, res}})
}

// CommitMany atomically persists a group of sources and their cursors.
func (s *Store) CommitMany(ctx context.Context, writes []Write) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	for _, w := range writes {
		for _, e := range w.Batch.Events {
			s.projects.probe(e.ProjectPath)
		}
		for _, m := range w.Batch.Sessions {
			s.projects.probe(m.Project)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	prepared := make(map[string]*sql.Stmt)
	defer func() {
		for _, stmt := range prepared {
			stmt.Close()
		}
	}()
	prepare := func(query string) (*sql.Stmt, error) {
		if stmt := prepared[query]; stmt != nil {
			return stmt, nil
		}
		stmt, err := tx.PrepareContext(ctx, query)
		if err == nil {
			prepared[query] = stmt
		}
		return stmt, err
	}
	for _, w := range writes {
		if len(w.Batch.Events) > 0 && len(w.Resolutions) == 0 {
			if _, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE key='pricing_fingerprint'"); err != nil {
				return err
			}
		}
		if err := s.commit(ctx, w.Harness, w.Path, w.Batch, w.Resolutions, prepare); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notify()
	return nil
}
func (s *Store) commit(ctx context.Context, h model.Harness, path string, b harness.Batch, res []Resolution, prepare func(string) (*sql.Stmt, error)) error {
	if len(res) != 0 && len(res) != len(b.Events) {
		return errors.New("resolution count differs from event count")
	}
	var err error

	const row = `(?,?,?,?,?,?,?,?,?,?,?,?)`
	const suffix = ` ON CONFLICT(harness,dedup_key) DO UPDATE SET dimension_id=excluded.dimension_id,timestamp=excluded.timestamp,input=excluded.input,output=excluded.output,cache_read=excluded.cache_read,cache_write=excluded.cache_write,reasoning=excluded.reasoning,log_cost=excluded.log_cost,cost=excluded.cost,priced=excluded.priced WHERE (event_data.dimension_id,event_data.timestamp,event_data.input,event_data.output,event_data.cache_read,event_data.cache_write,event_data.reasoning,event_data.log_cost,event_data.cost,event_data.priced) IS NOT (excluded.dimension_id,excluded.timestamp,excluded.input,excluded.output,excluded.cache_read,excluded.cache_write,excluded.reasoning,excluded.log_cost,excluded.cost,excluded.priced) AND (SELECT old.session_id=new.session_id OR (old.parent_id!='' AND new.parent_id='') FROM event_dimensions old JOIN event_dimensions new ON new.id=excluded.dimension_id WHERE old.id=event_data.dimension_id)`
	const batchSize = 128
	var values []any
	// Resolve each distinct dimension tuple once per parser batch, rather than
	// doing two dictionary lookups per event through the compatibility view.
	dimensions := map[[10]string]int64{}
	dimensionID := func(key [10]string) (int64, error) {
		if id, ok := dimensions[key]; ok {
			return id, nil
		}
		stmt, err := prepare(`INSERT INTO event_dimensions(` + dimensionColumns + `) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(` + dimensionColumns + `) DO UPDATE SET id=id RETURNING id`)
		if err != nil {
			return 0, err
		}
		var id int64
		err = stmt.QueryRowContext(ctx, key[0], key[1], key[2], key[3], key[4], key[5], key[6], key[7], key[8], key[9]).Scan(&id)
		if err == nil {
			dimensions[key] = id
		}
		return id, err
	}
	flush := func() error {
		if len(values) == 0 {
			return nil
		}
		query := `INSERT INTO event_data(` + factColumns + `) VALUES ` + strings.TrimSuffix(strings.Repeat(row+",", len(values)/12), ",") + suffix
		stmt, e := prepare(query)
		if e == nil {
			_, e = stmt.ExecContext(ctx, values...)
		}
		values = values[:0]
		return e
	}
	sessions, err := prepare(sessionSQL)
	if err != nil {
		return err
	}
	upsert := func(m model.SessionMeta) error {
		if m.SessionID == "" {
			return nil
		}
		if m.Harness == "" {
			m.Harness = h
		}
		_, e := sessions.ExecContext(ctx, m.Harness, m.SessionID, m.ParentID, harness.Title(m.Title), s.projects.resolve(m.Project), stamp(m.StartedAt), stamp(m.UpdatedAt), m.Project)
		return e
	}
	meta := make(map[string]model.SessionMeta)
	collect := func(m model.SessionMeta) {
		if m.SessionID == "" {
			return
		}
		if m.Harness == "" {
			m.Harness = h
		}
		key := string(m.Harness) + "\x00" + m.SessionID
		old, ok := meta[key]
		if !ok {
			meta[key] = m
			return
		}
		if m.ParentID != "" {
			old.ParentID = m.ParentID
		}
		if m.Title != "" {
			old.Title = m.Title
		}
		if m.Project != "" {
			old.Project = m.Project
		}
		if old.StartedAt.IsZero() || (!m.StartedAt.IsZero() && m.StartedAt.Before(old.StartedAt)) {
			old.StartedAt = m.StartedAt
		}
		if m.UpdatedAt.After(old.UpdatedAt) {
			old.UpdatedAt = m.UpdatedAt
		}
		meta[key] = old
	}
	for i, e := range b.Events {
		if e.Harness == "" {
			e.Harness = h
		}
		if e.DedupKey == "" || e.SessionID == "" || e.Harness != h {
			return fmt.Errorf("invalid event: harness/session/dedup key")
		}
		r := Resolution{Provider: e.Provider, Attrib: model.AttribLog}
		if e.CostUSD != nil {
			r.Cost = *e.CostUSD
		}
		if len(res) > 0 {
			r = res[i]
		}
		// Nonzero legacy resolutions are priced; explicit zero requires Priced.
		r.Priced = r.Priced || r.Cost != 0
		if e.CostUSD != nil {
			r.Cost = *e.CostUSD
			r.Priced = true
		}
		id, err := dimensionID([10]string{string(e.Harness), e.SessionID, e.ParentID, s.projects.resolve(e.ProjectPath), e.Model, e.Provider, e.BaseURL, r.Provider, string(r.Attrib), e.ProjectPath})
		if err != nil {
			return err
		}
		values = append(values, e.Harness, e.DedupKey, id, stamp(e.Timestamp), e.Tokens.Input, e.Tokens.Output, e.Tokens.CacheRead, e.Tokens.CacheWrite, e.Tokens.Reasoning, e.CostUSD, r.Cost, r.Priced)
		if len(values) == batchSize*12 {
			if err = flush(); err != nil {
				return err
			}
		}
		collect(model.SessionMeta{Harness: e.Harness, SessionID: e.SessionID, ParentID: e.ParentID, Project: e.ProjectPath, StartedAt: e.Timestamp, UpdatedAt: e.Timestamp})
		if e.ParentID != "" {
			collect(model.SessionMeta{Harness: e.Harness, SessionID: e.ParentID, Project: e.ProjectPath, StartedAt: e.Timestamp, UpdatedAt: e.Timestamp})
		}
	}
	if err = flush(); err != nil {
		return err
	}
	for _, m := range b.Sessions {
		collect(m)
	}
	for _, m := range meta {
		if err = upsert(m); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(b.Next)
	if err != nil {
		return err
	}
	cursor, err := prepare("INSERT INTO cursors VALUES(?,?,?) ON CONFLICT(harness,path) DO UPDATE SET cursor=excluded.cursor")
	if err != nil {
		return err
	}
	_, err = cursor.ExecContext(ctx, h, path, string(raw))
	return err
}

// Rebuild resets checkpoints for a full replay, preserving historical events whose
// source logs may have been rotated away. Replayed events are idempotent upserts.
func (s *Store) Rebuild(ctx context.Context) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "DELETE FROM cursors;"); e != nil {
		return e
	}
	if e = tx.Commit(); e == nil {
		s.projects = newProjectResolver()
		s.notify()
	}
	return e
}
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	e := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", key).Scan(&v)
	if errors.Is(e, sql.ErrNoRows) {
		e = nil
	}
	return v, e
}
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	return s.Exec(ctx, "INSERT INTO settings VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
}

// Subscribe is a coalescing commit signal. Query service applies its one-second throttle.
func (s *Store) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	if s.closed {
		close(ch)
	} else {
		s.subscribers[ch] = struct{}{}
	}
	s.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			if _, ok := s.subscribers[ch]; ok {
				delete(s.subscribers, ch)
				close(ch)
			}
			s.mu.Unlock()
		})
	}
}
func (s *Store) notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
