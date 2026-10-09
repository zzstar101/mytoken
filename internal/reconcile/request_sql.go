package reconcile

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/store"
)

// First probe exact identities through narrow time windows. If every bill with
// that identity has an eligible near event, a farther event cannot win nearest
// matching. Missing, occupied, or clock-skewed IDs retain the full-history scan;
// this is an optimization, never an additional time restriction on exact IDs.
func nearRequestRows(ctx context.Context, tx *sql.Tx, bills []relay.Bill, dims map[int64]*dimension, persisted map[int64]store.RelayMatch) ([]string, error) {
	var windows [][2]any
	for _, b := range bills {
		if b.Type == "refund" {
			continue
		}
		for _, id := range []string{b.RequestID, b.UpstreamRequestID} {
			if id != "" {
				windows = append(windows, [2]any{id, b.At.UnixNano()})
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS reconcile_id_rows(rowid INTEGER PRIMARY KEY);DELETE FROM reconcile_id_rows`); err != nil {
		return nil, err
	}
	data, _ := json.Marshal(windows)
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO reconcile_id_rows SELECT e.rowid FROM json_each(?) w CROSS JOIN event_data e ON e.timestamp>=json_extract(w.value,'$[1]')-120000000000 AND e.timestamp<=json_extract(w.value,'$[1]')+120000000000 WHERE e.request_id=json_extract(w.value,'$[0]')`, string(data)); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.harness,e.dedup_key,e.dimension_id,e.request_id,e.timestamp FROM reconcile_id_rows i JOIN event_data e ON e.rowid=i.rowid`)
	if err != nil {
		return nil, err
	}
	reserved := map[[2]string]bool{}
	for _, m := range persisted {
		if m.DedupKey != "" {
			reserved[[2]string{m.Harness, m.DedupKey}] = true
		}
	}
	times := map[string][]int64{}
	for rows.Next() {
		var h, key, id string
		var dimension, at int64
		if err = rows.Scan(&h, &key, &dimension, &id, &at); err != nil {
			rows.Close()
			return nil, err
		}
		if dims[dimension] != nil && !reserved[[2]string{h, key}] {
			times[id] = append(times[id], at)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var missing []string
	occurrences := map[string]int{}
	for _, w := range windows {
		occurrences[w[0].(string)]++
	}
	for _, w := range windows {
		id, at := w[0].(string), w[1].(int64)
		if occurrences[id] > 1 {
			missing = append(missing, id)
			continue
		}
		near := false
		for _, candidate := range times[id] {
			delta := candidate - at
			if delta < 0 {
				delta = -delta
			}
			if delta <= int64(120*time.Second) {
				near = true
				break
			}
		}
		if !near {
			missing = append(missing, id)
		}
	}
	return missing, nil
}
