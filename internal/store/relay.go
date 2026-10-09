package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/relay"
)

// ErrNotFound means a requested persisted site does not exist.
var ErrNotFound = sql.ErrNoRows

type RelayCursor struct {
	Origin, KeyID string
	LastBillID    int64
	LastSync      time.Time
	LastError     string
}

type RelayMatch struct {
	ID                      int64
	Harness, DedupKey, Kind string
}

const relaySchema = `
CREATE TABLE IF NOT EXISTS relay_sites(origin TEXT NOT NULL,key_id TEXT NOT NULL,kind TEXT NOT NULL,version TEXT NOT NULL,quota_per_unit REAL NOT NULL,layers INTEGER NOT NULL,detected_at INTEGER NOT NULL,providers_json TEXT NOT NULL,PRIMARY KEY(origin,key_id)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS relay_balances(origin TEXT NOT NULL,key_id TEXT NOT NULL,at INTEGER NOT NULL,remaining_usd REAL,used_usd REAL,unlimited INTEGER NOT NULL,PRIMARY KEY(origin,key_id,at)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS relay_daily(origin TEXT NOT NULL,key_id TEXT NOT NULL,day TEXT NOT NULL,model TEXT NOT NULL,requests INTEGER NOT NULL,input INTEGER NOT NULL,output INTEGER NOT NULL,cache_read INTEGER NOT NULL,cache_write INTEGER NOT NULL,reasoning INTEGER NOT NULL DEFAULT 0,list_usd REAL NOT NULL,charged_usd REAL NOT NULL,fetched_at INTEGER NOT NULL,PRIMARY KEY(origin,key_id,day,model)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS relay_cursors(origin TEXT NOT NULL,key_id TEXT NOT NULL,last_bill_id INTEGER NOT NULL,last_sync INTEGER NOT NULL,last_error TEXT NOT NULL,PRIMARY KEY(origin,key_id)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS relay_origins(id INTEGER PRIMARY KEY,origin TEXT NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS relay_bill_dimensions(id INTEGER PRIMARY KEY,origin_id INTEGER NOT NULL,key_id TEXT NOT NULL,type TEXT NOT NULL,model TEXT NOT NULL,grp TEXT NOT NULL,ratios_json TEXT NOT NULL,UNIQUE(origin_id,key_id,type,model,grp,ratios_json));
CREATE TABLE IF NOT EXISTS relay_bill_data(origin_id INTEGER NOT NULL,id INTEGER NOT NULL,dimension_id INTEGER NOT NULL,at INTEGER NOT NULL,request_id TEXT NOT NULL,upstream_request_id TEXT NOT NULL,input INTEGER NOT NULL,output INTEGER NOT NULL,cache_read INTEGER NOT NULL,cache_write INTEGER NOT NULL,reasoning INTEGER NOT NULL,charged_usd REAL NOT NULL,stream INTEGER NOT NULL,latency_ms INTEGER NOT NULL,match_harness TEXT NOT NULL DEFAULT '',match_dedup_key TEXT NOT NULL DEFAULT '',match_kind TEXT NOT NULL DEFAULT '',PRIMARY KEY(origin_id,id)) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS relay_bill_dimension_time ON relay_bill_data(dimension_id,at);
CREATE VIEW IF NOT EXISTS relay_bills AS SELECT o.origin,d.key_id,b.id,b.at,d.type,d.model,d.grp,b.request_id,b.upstream_request_id,b.input,b.output,b.cache_read,b.cache_write,b.reasoning,b.charged_usd,d.ratios_json,b.stream,b.latency_ms,b.match_harness,b.match_dedup_key,b.match_kind FROM relay_bill_data b JOIN relay_origins o ON o.id=b.origin_id JOIN relay_bill_dimensions d ON d.id=b.dimension_id;
CREATE TRIGGER IF NOT EXISTS relay_bills_match_update INSTEAD OF UPDATE OF match_harness,match_dedup_key,match_kind ON relay_bills BEGIN UPDATE relay_bill_data SET match_harness=NEW.match_harness,match_dedup_key=NEW.match_dedup_key,match_kind=NEW.match_kind WHERE origin_id=(SELECT id FROM relay_origins WHERE origin=OLD.origin) AND id=OLD.id; END;
`

func relayTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// RelayTransaction serializes a short local reconciliation snapshot and its
// derived match updates with scanners. Callbacks must use tx, not Store.DB.
func (s *Store) RelayTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PutRelaySite(ctx context.Context, site relay.Site) error {
	raw, err := json.Marshal(site.Providers)
	if err != nil {
		return err
	}
	return s.Exec(ctx, `INSERT INTO relay_sites VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(origin,key_id) DO UPDATE SET kind=excluded.kind,version=excluded.version,quota_per_unit=excluded.quota_per_unit,layers=excluded.layers,detected_at=excluded.detected_at,providers_json=excluded.providers_json`, site.Origin, site.KeyID, site.Kind, site.Version, site.QuotaPerUnit, site.Layers, stamp(site.DetectedAt), string(raw))
}
func (s *Store) RelaySites(ctx context.Context) ([]relay.Site, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT origin,key_id,kind,version,quota_per_unit,layers,detected_at,providers_json FROM relay_sites ORDER BY origin,key_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []relay.Site{}
	for rows.Next() {
		var v relay.Site
		var at int64
		var raw string
		if err = rows.Scan(&v.Origin, &v.KeyID, &v.Kind, &v.Version, &v.QuotaPerUnit, &v.Layers, &at, &raw); err != nil {
			return nil, err
		}
		v.DetectedAt = relayTime(at)
		if err = json.Unmarshal([]byte(raw), &v.Providers); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) RelaySite(ctx context.Context, origin, keyID string) (relay.Site, error) {
	var v relay.Site
	var at int64
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT origin,key_id,kind,version,quota_per_unit,layers,detected_at,providers_json FROM relay_sites WHERE origin=? AND key_id=?`, origin, keyID).Scan(&v.Origin, &v.KeyID, &v.Kind, &v.Version, &v.QuotaPerUnit, &v.Layers, &at, &raw)
	if err == nil {
		v.DetectedAt = relayTime(at)
		err = json.Unmarshal([]byte(raw), &v.Providers)
	}
	return v, err
}
func (s *Store) PutRelayCursor(ctx context.Context, c RelayCursor) error {
	return s.Exec(ctx, `INSERT INTO relay_cursors VALUES(?,?,?,?,?) ON CONFLICT(origin,key_id) DO UPDATE SET last_bill_id=max(relay_cursors.last_bill_id,excluded.last_bill_id),last_sync=excluded.last_sync,last_error=excluded.last_error`, c.Origin, c.KeyID, c.LastBillID, stamp(c.LastSync), c.LastError)
}
func (s *Store) RelayCursor(ctx context.Context, origin, keyID string) (RelayCursor, error) {
	c := RelayCursor{Origin: origin, KeyID: keyID}
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT last_bill_id,last_sync,last_error FROM relay_cursors WHERE origin=? AND key_id=?`, origin, keyID).Scan(&c.LastBillID, &at, &c.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	c.LastSync = relayTime(at)
	return c, err
}
func (s *Store) PutRelayBalance(ctx context.Context, b relay.Balance) error {
	return s.Exec(ctx, `INSERT INTO relay_balances VALUES(?,?,?,?,?,?) ON CONFLICT(origin,key_id,at) DO UPDATE SET remaining_usd=excluded.remaining_usd,used_usd=excluded.used_usd,unlimited=excluded.unlimited`, b.Origin, b.KeyID, stamp(b.At), b.RemainingUSD, b.UsedUSD, b.Unlimited)
}
func (s *Store) LatestRelayBalance(ctx context.Context, origin, keyID string) (*relay.Balance, error) {
	return latestRelayBalance(ctx, s.db, origin, keyID)
}

type relayQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func latestRelayBalance(ctx context.Context, q relayQuerier, origin, keyID string) (*relay.Balance, error) {
	b := &relay.Balance{Origin: origin, KeyID: keyID}
	var at int64
	var remaining, used sql.NullFloat64
	err := q.QueryRowContext(ctx, `SELECT at,remaining_usd,used_usd,unlimited FROM relay_balances WHERE origin=? AND key_id=? ORDER BY at DESC LIMIT 1`, origin, keyID).Scan(&at, &remaining, &used, &b.Unlimited)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.At = relayTime(at)
	if remaining.Valid {
		b.RemainingUSD = &remaining.Float64
	}
	if used.Valid {
		b.UsedUSD = &used.Float64
	}
	return b, nil
}
func (s *Store) PutRelayBills(ctx context.Context, bills []relay.Bill) error {
	if len(bills) == 0 {
		return nil
	}
	return s.RelayTransaction(ctx, func(tx *sql.Tx) error {
		origins := map[string]int64{}
		dimensions := map[string]int64{}
		dim, err := tx.PrepareContext(ctx, `INSERT INTO relay_bill_dimensions(origin_id,key_id,type,model,grp,ratios_json) VALUES(?,?,?,?,?,?) ON CONFLICT(origin_id,key_id,type,model,grp,ratios_json) DO UPDATE SET id=id RETURNING id`)
		if err != nil {
			return err
		}
		defer dim.Close()
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO relay_bill_data(origin_id,id,dimension_id,at,request_id,upstream_request_id,input,output,cache_read,cache_write,reasoning,charged_usd,stream,latency_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(origin_id,id) DO UPDATE SET dimension_id=excluded.dimension_id,at=excluded.at,request_id=excluded.request_id,upstream_request_id=excluded.upstream_request_id,input=excluded.input,output=excluded.output,cache_read=excluded.cache_read,cache_write=excluded.cache_write,reasoning=excluded.reasoning,charged_usd=excluded.charged_usd,stream=excluded.stream,latency_ms=excluded.latency_ms,match_harness='',match_dedup_key='',match_kind='' WHERE (dimension_id,at,request_id,upstream_request_id,input,output,cache_read,cache_write,reasoning,charged_usd,stream,latency_ms) IS NOT (excluded.dimension_id,excluded.at,excluded.request_id,excluded.upstream_request_id,excluded.input,excluded.output,excluded.cache_read,excluded.cache_write,excluded.reasoning,excluded.charged_usd,excluded.stream,excluded.latency_ms)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, b := range bills {
			oid, ok := origins[b.Origin]
			if !ok {
				if err = tx.QueryRowContext(ctx, `INSERT INTO relay_origins(origin) VALUES(?) ON CONFLICT(origin) DO UPDATE SET origin=excluded.origin RETURNING id`, b.Origin).Scan(&oid); err != nil {
					return err
				}
				origins[b.Origin] = oid
			}
			raw, err := json.Marshal(b.Ratios)
			if err != nil {
				return err
			}
			keyRaw, _ := json.Marshal([]string{b.Origin, b.KeyID, b.Type, b.Model, b.Group, string(raw)})
			key := string(keyRaw)
			did, ok := dimensions[key]
			if !ok {
				if err = dim.QueryRowContext(ctx, oid, b.KeyID, b.Type, b.Model, b.Group, string(raw)).Scan(&did); err != nil {
					return err
				}
				dimensions[key] = did
			}
			if _, err = stmt.ExecContext(ctx, oid, b.ID, did, stamp(b.At), b.RequestID, b.UpstreamRequestID, b.Tokens.Input, b.Tokens.Output, b.Tokens.CacheRead, b.Tokens.CacheWrite, b.Tokens.Reasoning, b.ChargedUSD, b.Stream, b.LatencyMS); err != nil {
				return err
			}
		}
		return nil
	})
}

const relayBillSelect = `SELECT origin,key_id,id,at,type,model,grp,request_id,upstream_request_id,input,output,cache_read,cache_write,reasoning,charged_usd,ratios_json,stream,latency_ms FROM relay_bills WHERE origin=? AND key_id=?`

func (s *Store) RelayBills(ctx context.Context, origin, keyID string, from, to time.Time) ([]relay.Bill, error) {
	return s.relayBills(ctx, origin, keyID, from, to, false)
}

// PendingRelayBills excludes already assigned bills; bill-only rows are retried
// because the corresponding source log may arrive after the upstream charge.
func (s *Store) PendingRelayBills(ctx context.Context, origin, keyID string) ([]relay.Bill, error) {
	return s.relayBills(ctx, origin, keyID, time.Time{}, time.Time{}, true)
}
func (s *Store) relayBills(ctx context.Context, origin, keyID string, from, to time.Time, pending bool) ([]relay.Bill, error) {
	query := relayBillSelect
	if pending {
		query += " AND match_kind IN ('','bill-only')"
	}
	args := []any{origin, keyID}
	if !from.IsZero() {
		query += " AND at>=?"
		args = append(args, stamp(from))
	}
	if !to.IsZero() {
		query += " AND at<?"
		args = append(args, stamp(to))
	}
	query += " ORDER BY at,id"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []relay.Bill{}
	ratios := map[string]relay.Ratios{}
	for rows.Next() {
		var b relay.Bill
		var at int64
		var raw string
		if err = rows.Scan(&b.Origin, &b.KeyID, &b.ID, &at, &b.Type, &b.Model, &b.Group, &b.RequestID, &b.UpstreamRequestID, &b.Tokens.Input, &b.Tokens.Output, &b.Tokens.CacheRead, &b.Tokens.CacheWrite, &b.Tokens.Reasoning, &b.ChargedUSD, &raw, &b.Stream, &b.LatencyMS); err != nil {
			return nil, err
		}
		b.At = relayTime(at)
		r, ok := ratios[raw]
		if !ok {
			if err = json.Unmarshal([]byte(raw), &r); err != nil {
				return nil, err
			}
			ratios[raw] = r
		}
		b.Ratios = r
		out = append(out, b)
	}
	return out, rows.Err()
}
func (s *Store) PutRelayDaily(ctx context.Context, days []relay.Daily) error {
	if len(days) == 0 {
		return nil
	}
	return s.RelayTransaction(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO relay_daily VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(origin,key_id,day,model) DO UPDATE SET requests=excluded.requests,input=excluded.input,output=excluded.output,cache_read=excluded.cache_read,cache_write=excluded.cache_write,reasoning=excluded.reasoning,list_usd=excluded.list_usd,charged_usd=excluded.charged_usd,fetched_at=excluded.fetched_at`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		now := time.Now().UnixNano()
		for _, d := range days {
			if _, err = stmt.ExecContext(ctx, d.Origin, d.KeyID, d.Day, d.Model, d.Requests, d.Tokens.Input, d.Tokens.Output, d.Tokens.CacheRead, d.Tokens.CacheWrite, d.Tokens.Reasoning, d.ListUSD, d.ChargedUSD, now); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Store) RelayDaily(ctx context.Context, origin, keyID, from, to string) ([]relay.Daily, error) {
	q := `SELECT origin,key_id,day,model,requests,input,output,cache_read,cache_write,reasoning,list_usd,charged_usd FROM relay_daily WHERE origin=? AND key_id=?`
	args := []any{origin, keyID}
	if from != "" {
		q += " AND day>=?"
		args = append(args, from)
	}
	if to != "" {
		q += " AND day<?"
		args = append(args, to)
	}
	q += " ORDER BY day,model"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []relay.Daily{}
	for rows.Next() {
		var d relay.Daily
		if err = rows.Scan(&d.Origin, &d.KeyID, &d.Day, &d.Model, &d.Requests, &d.Tokens.Input, &d.Tokens.Output, &d.Tokens.CacheRead, &d.Tokens.CacheWrite, &d.Tokens.Reasoning, &d.ListUSD, &d.ChargedUSD); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RelayMatches returns persisted assignments, including unmatched categories.
func (s *Store) RelayMatches(ctx context.Context, origin, keyID string) (map[int64]RelayMatch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,match_harness,match_dedup_key,match_kind FROM relay_bills WHERE origin=? AND key_id=? AND match_kind!=''`, origin, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]RelayMatch{}
	for rows.Next() {
		var m RelayMatch
		if err = rows.Scan(&m.ID, &m.Harness, &m.DedupKey, &m.Kind); err != nil {
			return nil, err
		}
		out[m.ID] = m
	}
	return out, rows.Err()
}

func (s *Store) SaveRelayMatches(ctx context.Context, origin, keyID string, matches []RelayMatch) error {
	if len(matches) == 0 {
		return nil
	}
	return s.RelayTransaction(ctx, func(tx *sql.Tx) error {
		var originID int64
		if err := tx.QueryRowContext(ctx, "SELECT id FROM relay_origins WHERE origin=?", origin).Scan(&originID); err != nil {
			return err
		}
		prepared := map[int]*sql.Stmt{}
		defer func() {
			for _, stmt := range prepared {
				stmt.Close()
			}
		}()
		for start := 0; start < len(matches); start += 128 {
			end := min(start+128, len(matches))
			n := end - start
			stmt := prepared[n]
			if stmt == nil {
				q := `WITH m(id,harness,dedup,kind) AS (VALUES ` + strings.TrimSuffix(strings.Repeat("(?,?,?,?),", n), ",") + `) UPDATE relay_bill_data AS b SET match_harness=m.harness,match_dedup_key=m.dedup,match_kind=m.kind FROM m WHERE b.origin_id=? AND b.id=m.id AND b.dimension_id IN(SELECT id FROM relay_bill_dimensions WHERE key_id=?) AND (b.match_harness,b.match_dedup_key,b.match_kind) IS NOT (m.harness,m.dedup,m.kind)`
				var err error
				stmt, err = tx.PrepareContext(ctx, q)
				if err != nil {
					return err
				}
				prepared[n] = stmt
			}
			args := make([]any, 0, n*4+2)
			for _, m := range matches[start:end] {
				args = append(args, m.ID, m.Harness, m.DedupKey, m.Kind)
			}
			args = append(args, originID, keyID)
			if _, err := stmt.ExecContext(ctx, args...); err != nil {
				return err
			}
		}
		return nil
	})
}
