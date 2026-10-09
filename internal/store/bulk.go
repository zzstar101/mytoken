package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

const eventIndexes = `CREATE INDEX IF NOT EXISTS events_time ON event_data(timestamp,output,input,cache_read,cache_write);
CREATE INDEX IF NOT EXISTS events_session ON event_data(dimension_id,timestamp);`

// migrateEventIndexes replaces only the old timestamp index, retaining its
// public name and all event rows. Tokens in the covering suffix avoid a fact
// table lookup for every request in a reconciliation time window.
func migrateEventIndexes(tx *sql.Tx) error {
	rows, err := tx.Query(`PRAGMA index_info(events_time)`)
	if err != nil {
		return err
	}
	var columns []string
	for rows.Next() {
		var seq, cid int
		var name string
		if err = rows.Scan(&seq, &cid, &name); err != nil {
			rows.Close()
			return err
		}
		columns = append(columns, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(columns) > 0 && strings.Join(columns, ",") != "timestamp,output,input,cache_read,cache_write" {
		if _, err = tx.Exec(`DROP INDEX events_time`); err != nil {
			return err
		}
	}
	_, err = tx.Exec(eventIndexes)
	return err
}

// DeferEmptyIndexes postpones secondary-index construction on a fresh index.
// The caller must invoke the returned function, including on scan cancellation.
// Primary keys remain in place for deduplication; readers remain correct while
// the secondary indexes are absent. Open also restores them after a crash.
func (s *Store) DeferEmptyIndexes(ctx context.Context) (func() error, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	var present bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM events)").Scan(&present); err != nil {
		return nil, err
	}
	if present {
		return func() error { return nil }, nil
	}
	restore := func() error {
		// Cancellation must not leave the running application without indexes.
		return s.Exec(context.WithoutCancel(ctx), eventIndexes+` PRAGMA cache_size=-8192;`)
	}
	// A larger page cache helps bulk writes; ordinary incremental scans keep
	// the smaller budget configured by Open.
	_, err := s.db.ExecContext(ctx, `PRAGMA cache_size=-32768;
DROP INDEX IF EXISTS events_time;
DROP INDEX IF EXISTS events_session;
DROP INDEX IF EXISTS events_provider_time;
DROP INDEX IF EXISTS events_model_time;`)
	if err != nil {
		_, restoreErr := s.db.ExecContext(context.WithoutCancel(ctx), eventIndexes+` PRAGMA cache_size=-8192;`)
		return nil, errors.Join(err, restoreErr)
	}
	return restore, nil
}
