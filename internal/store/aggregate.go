package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
)

func init() {
	sqlite.MustRegisterScalarFunction("mytoken_day", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		n, ok := args[0].(int64)
		if !ok {
			return nil, fmt.Errorf("invalid event timestamp %T", args[0])
		}
		t := time.Unix(0, n).In(time.Local)
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local).UnixNano(), nil
	})
	sqlite.MustRegisterScalarFunction("mytoken_hour", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		n, ok := args[0].(int64)
		if !ok {
			return nil, fmt.Errorf("invalid event timestamp %T", args[0])
		}
		t := time.Unix(0, n).In(time.Local)
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.Local).UnixNano(), nil
	})
}

const dailyDimensions = "day,harness,session_id,resolved_provider,model,project,attrib"
const dailyMetrics = "input,output,cache_read,cache_write,reasoning,cost,cost_log,cost_estimate,requests,unpriced"

func dailyValues(prefix string) string {
	p := prefix + "."
	return "mytoken_day(" + p + "timestamp)," + p + "harness," + p + "session_id," + p + "resolved_provider," + p + "model," + p + "project," + p + "attrib," + p + "input," + p + "output," + p + "cache_read," + p + "cache_write," + p + "reasoning,CASE WHEN " + p + "priced THEN " + p + "cost ELSE 0 END,CASE WHEN " + p + "log_cost IS NOT NULL THEN " + p + "cost ELSE 0 END,CASE WHEN " + p + "log_cost IS NULL AND " + p + "priced THEN " + p + "cost ELSE 0 END,1,NOT " + p + "priced"
}

func migrateDaily(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS daily_usage (
 day INTEGER NOT NULL,harness TEXT NOT NULL,session_id TEXT NOT NULL,resolved_provider TEXT NOT NULL,model TEXT NOT NULL,project TEXT NOT NULL,attrib TEXT NOT NULL,
 input INTEGER NOT NULL,output INTEGER NOT NULL,cache_read INTEGER NOT NULL,cache_write INTEGER NOT NULL,reasoning INTEGER NOT NULL,cost REAL NOT NULL,cost_log REAL NOT NULL,cost_estimate REAL NOT NULL,requests INTEGER NOT NULL,unpriced INTEGER NOT NULL,
 PRIMARY KEY(day,harness,session_id,resolved_provider,model,project,attrib)) WITHOUT ROWID;
 DROP INDEX IF EXISTS events_model_time; DROP INDEX IF EXISTS events_provider_time;`)
	if err != nil {
		return err
	}
	var add, sub []string
	for _, col := range strings.Split(dailyMetrics, ",") {
		add = append(add, col+"=daily_usage."+col+"+excluded."+col)
		sub = append(sub, col+"=daily_usage."+col+"-excluded."+col)
	}
	change := func(prefix string, subtract bool) string {
		updates := add
		if subtract {
			updates = sub
		}
		values := dailyValues(prefix)
		for _, col := range strings.Split(dimensionColumns, ",") {
			values = strings.ReplaceAll(values, prefix+"."+col, "d."+col)
		}
		return "INSERT INTO daily_usage(" + dailyDimensions + "," + dailyMetrics + ") SELECT " + values + " FROM event_dimensions d WHERE d.id=" + prefix + ".dimension_id ON CONFLICT(" + dailyDimensions + ") DO UPDATE SET " + strings.Join(updates, ",") + ";"
	}
	remove := change("OLD", true) + "DELETE FROM daily_usage WHERE requests=0 AND (day,harness,session_id,resolved_provider,model,project,attrib)=(SELECT mytoken_day(OLD.timestamp),harness,session_id,resolved_provider,model,project,attrib FROM event_dimensions WHERE id=OLD.dimension_id);"
	for name, body := range map[string]string{"insert": "AFTER INSERT ON event_data BEGIN " + change("NEW", false), "delete": "AFTER DELETE ON event_data BEGIN " + remove, "update": "AFTER UPDATE ON event_data BEGIN " + remove + change("NEW", false)} {
		if _, err = tx.Exec("CREATE TRIGGER IF NOT EXISTS daily_" + name + " " + body + " END"); err != nil {
			return err
		}
	}
	return nil
}

// LocalDayZone identifies the timezone rules used by local-day rollups.
func LocalDayZone() string {
	// Name plus seasonal offsets also identifies fixed zones and /etc/localtime.
	var b strings.Builder
	b.WriteString(time.Local.String())
	for _, year := range []int{1970, 2000, 2020, time.Now().Year(), time.Now().Year() + 1} {
		for _, month := range []time.Month{time.January, time.April, time.July, time.October} {
			name, offset := time.Date(year, month, 1, 0, 0, 0, 0, time.Local).Zone()
			fmt.Fprintf(&b, "/%s:%d", name, offset)
		}
	}
	return b.String()
}

// EnsureLocalDays rebuilds only derived summaries when the local timezone changes.
// Called at database open, never by queries or display-timezone overrides.
func (s *Store) EnsureLocalDays(ctx context.Context) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	key := LocalDayZone()
	if s.aggregateZone == key {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='daily_timezone'`).Scan(&old)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if old != key {
		if _, err = tx.ExecContext(ctx, `DELETE FROM daily_usage; INSERT INTO daily_usage SELECT mytoken_day(timestamp),harness,session_id,resolved_provider,model,project,attrib,sum(input),sum(output),sum(cache_read),sum(cache_write),sum(reasoning),sum(CASE WHEN priced THEN cost ELSE 0 END),sum(CASE WHEN log_cost IS NOT NULL THEN cost ELSE 0 END),sum(CASE WHEN log_cost IS NULL AND priced THEN cost ELSE 0 END),count(*),sum(NOT priced) FROM events GROUP BY 1,2,3,4,5,6,7`); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO settings VALUES('daily_timezone',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err == nil {
		s.aggregateZone = key
	}
	return err
}

// RefreshDaily rebuilds derived rollups while retaining all historical events.
func (s *Store) RefreshDaily(ctx context.Context) error {
	s.writer.Lock()
	s.aggregateZone = ""
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key='daily_timezone'`)
	s.writer.Unlock()
	if err != nil {
		return err
	}
	return s.EnsureLocalDays(ctx)
}
