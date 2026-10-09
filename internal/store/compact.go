package store

import (
	"database/sql"
	"strings"
)

const dimensionColumns = "harness,session_id,parent_id,project,model,provider,base_url,resolved_provider,attrib,raw_project"
const factColumns = "harness,dedup_key,dimension_id,timestamp,input,output,cache_read,cache_write,reasoning,log_cost,cost,priced"

// migrateCompact dictionaries repeated strings, retaining exact dedup keys and
// a stable integer rowid. The events view preserves the SQL read/update surface.
// All DDL and the copy run in the caller's migration transaction.
func migrateCompact(tx *sql.Tx) (bool, error) {
	var exists int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='event_data' AND type='table'`).Scan(&exists); err != nil {
		return false, err
	}
	if exists != 0 {
		return false, nil
	}
	_, err := tx.Exec(`
 DROP TRIGGER IF EXISTS daily_insert; DROP TRIGGER IF EXISTS daily_update; DROP TRIGGER IF EXISTS daily_delete;
 CREATE TABLE event_dimensions(id INTEGER PRIMARY KEY,harness TEXT NOT NULL,session_id TEXT NOT NULL,parent_id TEXT NOT NULL,project TEXT NOT NULL,model TEXT NOT NULL,provider TEXT NOT NULL,base_url TEXT NOT NULL,resolved_provider TEXT NOT NULL,attrib TEXT NOT NULL,raw_project TEXT NOT NULL,UNIQUE(` + dimensionColumns + `));
 INSERT INTO event_dimensions(` + dimensionColumns + `) SELECT DISTINCT ` + dimensionColumns + ` FROM events;
 CREATE INDEX dimensions_session ON event_dimensions(harness,session_id);
 CREATE INDEX dimensions_raw_project ON event_dimensions(raw_project,project);
 CREATE TABLE event_data(harness TEXT NOT NULL,dedup_key TEXT NOT NULL,dimension_id INTEGER NOT NULL REFERENCES event_dimensions(id),timestamp INTEGER NOT NULL,input INTEGER NOT NULL,output INTEGER NOT NULL,cache_read INTEGER NOT NULL,cache_write INTEGER NOT NULL,reasoning INTEGER NOT NULL,log_cost REAL,cost REAL NOT NULL,priced INTEGER NOT NULL,PRIMARY KEY(harness,dedup_key));
 INSERT INTO event_data(rowid,` + factColumns + `) SELECT e.rowid,e.harness,e.dedup_key,d.id,e.timestamp,e.input,e.output,e.cache_read,e.cache_write,e.reasoning,e.log_cost,e.cost,e.priced FROM events e JOIN event_dimensions d ON ` + dimensionMatch("e", "d") + `;
 DROP TABLE events;
 CREATE VIEW events AS SELECT e.rowid AS rowid,e.harness,e.dedup_key,d.session_id,d.parent_id,d.project,e.timestamp,d.model,d.provider,d.base_url,e.input,e.output,e.cache_read,e.cache_write,e.reasoning,e.log_cost,d.resolved_provider,d.attrib,e.cost,e.priced,d.raw_project FROM event_data e JOIN event_dimensions d ON d.id=e.dimension_id;
 `)
	if err != nil {
		return false, err
	}
	var dims []string
	for _, c := range strings.Split(dimensionColumns, ",") {
		dims = append(dims, "NEW."+c)
	}
	dimInsert := `INSERT INTO event_dimensions(` + dimensionColumns + `) VALUES(` + strings.Join(dims, ",") + `) ON CONFLICT DO NOTHING;`
	dimID := `(SELECT id FROM event_dimensions d WHERE ` + dimensionMatch("NEW", "d") + `)`
	var vals, updates []string
	for _, c := range strings.Split(factColumns, ",") {
		v := "NEW." + c
		if c == "dimension_id" {
			v = dimID
		}
		vals = append(vals, v)
		updates = append(updates, c+"="+v)
	}
	insert := `INSERT INTO event_data(` + factColumns + `) VALUES(` + strings.Join(vals, ",") + `) ON CONFLICT(harness,dedup_key) DO UPDATE SET dimension_id=excluded.dimension_id,timestamp=excluded.timestamp,input=excluded.input,output=excluded.output,cache_read=excluded.cache_read,cache_write=excluded.cache_write,reasoning=excluded.reasoning,log_cost=excluded.log_cost,cost=excluded.cost,priced=excluded.priced WHERE (SELECT session_id=NEW.session_id OR (parent_id!='' AND NEW.parent_id='') FROM event_dimensions WHERE id=event_data.dimension_id);`
	_, err = tx.Exec(`CREATE TRIGGER events_insert INSTEAD OF INSERT ON events BEGIN ` + dimInsert + insert + ` END;
 CREATE TRIGGER events_update INSTEAD OF UPDATE ON events BEGIN ` + dimInsert + `UPDATE event_data SET ` + strings.Join(updates, ",") + ` WHERE rowid=OLD.rowid; END;
 CREATE TRIGGER events_delete INSTEAD OF DELETE ON events BEGIN DELETE FROM event_data WHERE rowid=OLD.rowid; END;`)
	return true, err
}

func dimensionMatch(a, b string) string {
	var equal []string
	for _, c := range strings.Split(dimensionColumns, ",") {
		equal = append(equal, a+"."+c+"="+b+"."+c)
	}
	return strings.Join(equal, " AND ")
}
