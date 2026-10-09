package query

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/store"
)

const usageColumns = `harness,session_id,project,day,model,resolved_provider,attrib,input,output,cache_read,cache_write,reasoning,cost,cost_log,cost_estimate,requests,unpriced`

// aggregateSQL uses complete local days from the rollup, and scans only the
// partial boundary days through events_time. Intervals remain exactly [From,To).
func aggregateSQL(f Filter, hourly bool) (string, []any) {
	if hourly {
		return eventAggregateSQL(f, true)
	}
	middle := f
	var edges []Filter
	if !f.Range.From.IsZero() && !f.Range.From.Equal(floor(f.Range.From, false)) {
		end := step(floor(f.Range.From, false), false)
		if !f.Range.To.IsZero() && !end.Before(f.Range.To) {
			return eventAggregateSQL(f, false)
		}
		edge := f
		edge.Range.To = end
		edges = append(edges, edge)
		middle.Range.From = end
	}
	if !f.Range.To.IsZero() && !f.Range.To.Equal(floor(f.Range.To, false)) {
		start := floor(f.Range.To, false)
		if !middle.Range.From.IsZero() && !start.After(middle.Range.From) {
			edge := middle
			edge.Range.To = f.Range.To
			if len(edges) == 0 {
				return eventAggregateSQL(edge, false)
			}
			edges = append(edges, edge)
			middle.Range.To = middle.Range.From
		} else {
			edge := f
			edge.Range.From = start
			edges = append(edges, edge)
			middle.Range.To = start
		}
	}
	where, args := filterSQL(middle)
	where = strings.ReplaceAll(where, "timestamp", "day")
	q := "SELECT " + usageColumns + " FROM daily_usage" + where
	for _, edge := range edges {
		eq, ea := eventAggregateSQL(edge, false)
		q += " UNION ALL " + eq
		args = append(args, ea...)
	}
	return q, args
}

func eventAggregateSQL(f Filter, hourly bool) (string, []any) {
	where, args := filterSQL(f)
	if !f.Range.From.IsZero() || !f.Range.To.IsZero() {
		// Keep a harness filter from selecting the (harness,dedup_key) unique
		// index and scanning that harness's entire history. The text expression
		// preserves equality semantics but lets events_time bound the fact scan;
		// unlike INDEXED BY it also works while initial bulk indexes are deferred.
		where = strings.ReplaceAll(where, "harness IN", "(harness || '') IN")
	}
	bucket := "mytoken_day(timestamp)"
	if hourly {
		// The Go-backed SQL function uses the same local/DST rules as series().
		bucket = "mytoken_hour(timestamp)"
	}
	return `SELECT harness,session_id,project,` + bucket + ` AS day,model,resolved_provider,attrib,sum(input),sum(output),sum(cache_read),sum(cache_write),sum(reasoning),sum(CASE WHEN priced THEN cost ELSE 0 END),sum(CASE WHEN log_cost IS NOT NULL THEN cost ELSE 0 END),sum(CASE WHEN log_cost IS NULL AND priced THEN cost ELSE 0 END),count(*),sum(NOT priced) FROM events` + where + ` GROUP BY harness,session_id,project,day,model,resolved_provider,attrib`, args
}

func scanUsage(ctx context.Context, tx *sql.Tx, f Filter, hourly bool) ([]AttributedEvent, error) {
	query, args := aggregateSQL(f, hourly)
	var zone string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='daily_timezone'`).Scan(&zone); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	// CLI display-timezone overrides must never rewrite persistent summaries.
	if zone != store.LocalDayZone() {
		query, args = eventAggregateSQL(f, hourly)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AttributedEvent{}
	for rows.Next() {
		var v AttributedEvent
		var stamp int64
		if err = rows.Scan(&v.Harness, &v.SessionID, &v.ProjectPath, &stamp, &v.Model, &v.ResolvedProvider, &v.Attrib, &v.Tokens.Input, &v.Tokens.Output, &v.Tokens.CacheRead, &v.Tokens.CacheWrite, &v.Tokens.Reasoning, &v.Cost, &v.costLog, &v.costEstimate, &v.requests, &v.unpriced); err != nil {
			return nil, err
		}
		v.Timestamp = time.Unix(0, stamp).UTC()
		v.Priced = v.unpriced == 0
		out = append(out, v)
	}
	return out, rows.Err()
}

func eventSource(v *AttributedEvent) {
	v.requests = 1
	if v.CostUSD != nil {
		v.CostSource = CostSourceLog
		v.costLog = v.Cost
	} else if v.Priced {
		v.CostSource = CostSourceEstimate
		v.costEstimate = v.Cost
	} else {
		v.CostSource = CostSourceUnpriced
		v.unpriced = 1
		v.Cost = 0
	}
}
