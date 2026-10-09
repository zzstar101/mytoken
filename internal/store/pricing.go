package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

// RecomputeCosts makes one bounded-memory pass, excluding authoritative log costs.
func (s *Store) RecomputeCosts(ctx context.Context, cost func(model.UsageEvent) float64) error {
	return s.RecomputePrices(ctx, func(e model.UsageEvent) (float64, bool) { v := cost(e); return v, v != 0 })
}

// RecomputePrices persists availability independently of the numeric cost.
func (s *Store) RecomputePrices(ctx context.Context, cost func(model.UsageEvent) (float64, bool)) error {
	return s.ReplacePricing(ctx, nil, cost)
}

// ReplacePricing atomically saves settings with the derived price state.
func (s *Store) ReplacePricing(ctx context.Context, settings map[string]string, cost func(model.UsageEvent) (float64, bool)) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	// External settings writes must not leave a stale price checkpoint behind.
	if settings == nil {
		settings = map[string]string{}
	}
	settings["pricing_fingerprint"] = ""
	return s.replacePricing(ctx, settings, cost)
}

// EnsurePrices avoids full-index passes when the catalog and settings are unchanged.
func (s *Store) EnsurePrices(ctx context.Context, fingerprint string, cost func(model.UsageEvent) (float64, bool)) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	var previous string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='pricing_fingerprint'").Scan(&previous)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if previous == fingerprint {
		return nil
	}
	return s.replacePricing(ctx, map[string]string{"pricing_fingerprint": fingerprint}, cost)
}

func (s *Store) replacePricing(ctx context.Context, settings map[string]string, cost func(model.UsageEvent) (float64, bool)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range settings {
		if _, err = tx.ExecContext(ctx, "INSERT INTO settings VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value); err != nil {
			return err
		}
	}
	var after int64
	changed := len(settings) > 0
	for {
		rows, err := tx.QueryContext(ctx, `SELECT rowid,model,resolved_provider,input,output,cache_read,cache_write,reasoning,cost,priced FROM events WHERE rowid>? AND log_cost IS NULL ORDER BY rowid LIMIT 512`, after)
		if err != nil {
			return err
		}
		var args []any
		count := 0
		for rows.Next() {
			var id int64
			var e model.UsageEvent
			var old float64
			var priced bool
			if err = rows.Scan(&id, &e.Model, &e.Provider, &e.Tokens.Input, &e.Tokens.Output, &e.Tokens.CacheRead, &e.Tokens.CacheWrite, &e.Tokens.Reasoning, &old, &priced); err != nil {
				rows.Close()
				return err
			}
			after = id
			count++
			if next, ok := cost(e); next != old || ok != priced {
				args = append(args, id, next, ok)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(args) > 0 {
			// A VALUES table updates a batch in one statement, without per-event SQL.
			q := `WITH prices(id,cost,priced) AS (VALUES ` + strings.TrimSuffix(strings.Repeat("(?,?,?),", len(args)/3), ",") + `) UPDATE events SET cost=prices.cost,priced=prices.priced FROM prices WHERE events.rowid=prices.id AND events.log_cost IS NULL`
			if _, err = tx.ExecContext(ctx, q, args...); err != nil {
				return err
			}
			changed = true
		}
		if count < 512 {
			break
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if changed {
		s.notify()
	}
	return nil
}

// UnpricedModels reports missing prices, not zero-priced or log-priced events.
func (s *Store) UnpricedModels(ctx context.Context, since time.Time, known func(provider, name string) bool) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT resolved_provider,model FROM events WHERE priced=0 AND timestamp>=?`, stamp(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	missing := map[string]bool{}
	for rows.Next() {
		var provider, name string
		if err = rows.Scan(&provider, &name); err != nil {
			return nil, err
		}
		missing[name] = true
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	result := make([]string, 0, len(missing))
	for name := range missing {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}
