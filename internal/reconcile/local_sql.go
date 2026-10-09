package reconcile

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/source"
	"github.com/zzstar101/mytoken/internal/store"
)

type rollupKey [6]string

// Summaries are safe only when every dictionary dimension behind a rollup
// belongs to the selected site; mixed origins and partial days use exact facts.
// storedMatch performs SELECTs only, including on query_only connections.
func loadEvents(ctx context.Context, st *store.Store, site relay.Site, from, to time.Time, bills []relay.Bill, mode readMode, persisted map[int64]store.RelayMatch) ([]event, []event, []alias, error) {
	var events, totals []event
	var aliases []alias
	err := st.RelayTransaction(ctx, func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='model-aliases'").Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if raw != "" {
			if err = json.Unmarshal([]byte(raw), &aliases); err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, "SELECT id,harness,session_id,resolved_provider,model,project,attrib,base_url FROM event_dimensions")
		if err != nil {
			return err
		}
		dims := map[int64]*dimension{}
		groups := map[rollupKey]*dimension{}
		counts := map[rollupKey]int{}
		selected := map[rollupKey]int{}
		idGroups := map[int64]rollupKey{}
		var ids []any
		providers := map[string]bool{}
		for _, p := range site.Providers {
			providers[p] = true
		}
		for rows.Next() {
			var id int64
			var k rollupKey
			var url string
			if err = rows.Scan(&id, &k[0], &k[1], &k[2], &k[3], &k[4], &k[5], &url); err != nil {
				rows.Close()
				return err
			}
			counts[k]++
			origin, ok := source.Origin(url)
			if !providers[k[2]] && (!ok || origin != site.Origin) {
				continue
			}
			selected[k]++
			d := groups[k]
			if d == nil {
				d = &dimension{harness: k[0], provider: k[2], model: normalize(k[3], k[2], aliases)}
				if mode != pendingMatch {
					d.price, d.priced = pricing.BuiltinLookup(d.model)
				}
				groups[k] = d
			}
			dims[id] = d
			idGroups[id] = k
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		where := func(ids []any) (string, []any) {
			q := ` WHERE dimension_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
			args := append([]any(nil), ids...)
			if !from.IsZero() {
				q += " AND timestamp>=?"
				args = append(args, from.UnixNano())
			}
			if !to.IsZero() {
				q += " AND timestamp<?"
				args = append(args, to.UnixNano())
			}
			return q, args
		}
		if mode != pendingMatch {
			var zone string
			if err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='daily_timezone'").Scan(&zone); err != nil {
				return err
			}
			useDays := zone == store.LocalDayZone() && (from.IsZero() || from.Equal(floorDay(from))) && (to.IsZero() || to.Equal(floorDay(to)))
			var facts []any
			safe := map[rollupKey]*dimension{}
			for _, id := range ids {
				k := idGroups[id.(int64)]
				if !useDays || counts[k] != selected[k] {
					facts = append(facts, id)
				} else {
					safe[k] = groups[k]
				}
			}
			if len(safe) > 0 {
				q := `SELECT harness,session_id,resolved_provider,model,project,attrib,day,input,output,cache_read,cache_write,reasoning,cost,requests FROM daily_usage WHERE 1=1`
				var args []any
				if !from.IsZero() {
					q += " AND day>=?"
					args = append(args, from.UnixNano())
				}
				if !to.IsZero() {
					q += " AND day<?"
					args = append(args, to.UnixNano())
				}
				rows, err = tx.QueryContext(ctx, q, args...)
				if err != nil {
					return err
				}
				for rows.Next() {
					var k rollupKey
					var e event
					if err = rows.Scan(&k[0], &k[1], &k[2], &k[3], &k[4], &k[5], &e.at, &e.tokens.Input, &e.tokens.Output, &e.tokens.CacheRead, &e.tokens.CacheWrite, &e.tokens.Reasoning, &e.cost, &e.count); err != nil {
						rows.Close()
						return err
					}
					if e.dim = safe[k]; e.dim != nil {
						totals = append(totals, e)
					}
				}
				err = rows.Err()
				rows.Close()
				if err != nil {
					return err
				}
			}
			if len(facts) > 0 {
				w, args := where(facts)
				rows, err = tx.QueryContext(ctx, `SELECT dimension_id,mytoken_day(timestamp),sum(input),sum(output),sum(cache_read),sum(cache_write),sum(reasoning),sum(CASE WHEN priced THEN cost ELSE 0 END),count(*) FROM event_data`+w+` GROUP BY dimension_id,mytoken_day(timestamp)`, args...)
				if err != nil {
					return err
				}
				for rows.Next() {
					var id int64
					var e event
					if err = rows.Scan(&id, &e.at, &e.tokens.Input, &e.tokens.Output, &e.tokens.CacheRead, &e.tokens.CacheWrite, &e.tokens.Reasoning, &e.cost, &e.count); err != nil {
						rows.Close()
						return err
					}
					e.dim = dims[id]
					totals = append(totals, e)
				}
				err = rows.Err()
				rows.Close()
				if err != nil {
					return err
				}
			}
		}
		if len(bills) == 0 {
			return nil
		}
		var candidate string
		var candidateArgs []any
		if mode == storedMatch {
			candidate = `SELECT e.rowid FROM relay_bills b JOIN event_data e ON e.harness=b.match_harness AND e.dedup_key=b.match_dedup_key WHERE b.origin=? AND b.key_id=? AND b.match_dedup_key!=''`
			candidateArgs = []any{site.Origin, site.KeyID}
			if !from.IsZero() {
				candidate += " AND b.at>=?"
				candidateArgs = append(candidateArgs, from.UnixNano())
			}
			if !to.IsZero() {
				candidate += " AND b.at<?"
				candidateArgs = append(candidateArgs, to.UnixNano())
			}
		} else {
			if _, err = tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS reconcile_bill_keys(at INTEGER,output INTEGER,input INTEGER);DELETE FROM reconcile_bill_keys;CREATE TEMP TABLE IF NOT EXISTS reconcile_request_keys(id TEXT PRIMARY KEY) WITHOUT ROWID;DELETE FROM reconcile_request_keys;`); err != nil {
				return err
			}
			pairs := make([][3]int64, 0, len(bills))
			var requestIDs []string
			for _, bill := range bills {
				if bill.Type == "refund" {
					continue
				}
				pairs = append(pairs, [3]int64{bill.At.UnixNano(), bill.Tokens.Output, bill.Tokens.Input})
				if bill.RequestID != "" {
					requestIDs = append(requestIDs, bill.RequestID)
				}
				if bill.UpstreamRequestID != "" {
					requestIDs = append(requestIDs, bill.UpstreamRequestID)
				}
			}
			data, _ := json.Marshal(pairs)
			if _, err = tx.ExecContext(ctx, `INSERT INTO reconcile_bill_keys SELECT json_extract(value,'$[0]'),json_extract(value,'$[1]'),json_extract(value,'$[2]') FROM json_each(?)`, string(data)); err != nil {
				return err
			}
			// +0 blocks SQLite's tempting but broad automatic output index.
			candidate = `SELECT e.rowid FROM reconcile_bill_keys b CROSS JOIN event_data e ON e.timestamp>=b.at-120000000000 AND e.timestamp<=b.at+120000000000 WHERE (e.output+0)=b.output AND (e.input=b.input OR e.input+e.cache_read=b.input OR e.input+e.cache_read+e.cache_write=b.input)`
			if mode == pendingMatch && len(requestIDs) > 0 {
				requestIDs, err = nearRequestRows(ctx, tx, bills, dims, persisted)
				if err != nil {
					return err
				}
				candidate += ` UNION SELECT rowid FROM reconcile_id_rows`
			}
			if len(requestIDs) > 0 {
				data, _ = json.Marshal(requestIDs)
				if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO reconcile_request_keys SELECT value FROM json_each(?)`, string(data)); err != nil {
					return err
				}
				candidate += ` UNION SELECT rowid FROM event_data WHERE request_id IN (SELECT id FROM reconcile_request_keys)`
			}
		}
		w, args := where(ids)
		w = strings.ReplaceAll(w, "dimension_id IN", "(dimension_id+0) IN")
		args = append(args, candidateArgs...)
		rows, err = tx.QueryContext(ctx, `SELECT dedup_key,dimension_id,timestamp,input,output,cache_read,cache_write,reasoning,CASE WHEN priced THEN cost ELSE 0 END,request_id,priced FROM event_data`+w+` AND rowid IN (`+candidate+`)`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e event
			var id int64
			if err = rows.Scan(&e.key, &id, &e.at, &e.tokens.Input, &e.tokens.Output, &e.tokens.CacheRead, &e.tokens.CacheWrite, &e.tokens.Reasoning, &e.cost, &e.request, &e.priced); err != nil {
				return err
			}
			e.dim = dims[id]
			events = append(events, e)
		}
		return rows.Err()
	})
	return events, totals, aliases, err
}
