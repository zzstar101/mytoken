package store

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/zzstar/mytoken/internal/model"
)

// RecomputeCosts makes one bounded-memory pass, excluding authoritative log costs.
func (s *Store) RecomputeCosts(ctx context.Context, cost func(model.UsageEvent) float64) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var after int64
	changed := false
	for {
		rows, err := tx.QueryContext(ctx, `SELECT rowid,model,resolved_provider,input,output,cache_read,cache_write,reasoning,cost FROM events WHERE rowid>? AND log_cost IS NULL ORDER BY rowid LIMIT 512`, after)
		if err != nil {
			return err
		}
		var args []any
		count := 0
		for rows.Next() {
			var id int64
			var e model.UsageEvent
			var old float64
			if err = rows.Scan(&id, &e.Model, &e.Provider, &e.Tokens.Input, &e.Tokens.Output, &e.Tokens.CacheRead, &e.Tokens.CacheWrite, &e.Tokens.Reasoning, &old); err != nil {
				rows.Close()
				return err
			}
			after = id
			count++
			if next := cost(e); next != old {
				args = append(args, id, next)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(args) > 0 {
			// A VALUES table updates a batch in one statement, without per-event SQL.
			q := `WITH prices(id,cost) AS (VALUES ` + strings.TrimSuffix(strings.Repeat("(?,?),", len(args)/2), ",") + `) UPDATE events SET cost=prices.cost FROM prices WHERE events.rowid=prices.id AND events.log_cost IS NULL`
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
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT resolved_provider,model FROM events WHERE log_cost IS NULL AND timestamp>=?`, stamp(since))
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
		if !known(provider, name) {
			missing[name] = true
		}
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
