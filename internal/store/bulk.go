package store

import (
	"context"
	"errors"
)

const eventIndexes = `CREATE INDEX IF NOT EXISTS events_time ON event_data(timestamp);
CREATE INDEX IF NOT EXISTS events_session ON event_data(dimension_id,timestamp);`

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
