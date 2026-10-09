package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestPricedMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE events(harness TEXT NOT NULL,dedup_key TEXT NOT NULL,session_id TEXT NOT NULL,parent_id TEXT NOT NULL DEFAULT '',project TEXT NOT NULL DEFAULT '',timestamp INTEGER NOT NULL,model TEXT NOT NULL,provider TEXT NOT NULL DEFAULT '',base_url TEXT NOT NULL DEFAULT '',input INTEGER NOT NULL,output INTEGER NOT NULL,cache_read INTEGER NOT NULL,cache_write INTEGER NOT NULL,reasoning INTEGER NOT NULL,log_cost REAL,resolved_provider TEXT NOT NULL,attrib TEXT NOT NULL,cost REAL NOT NULL,PRIMARY KEY(harness,dedup_key)); INSERT INTO events VALUES('codex','unknown','s','','',0,'unknown','','',0,0,0,0,0,NULL,'','',0),('codex','free','s','','',0,'free','','',0,0,0,0,0,0,'','',0);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	for i := 0; i < 2; i++ {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var priced int
		if err = st.DB().QueryRow("SELECT sum(priced) FROM events").Scan(&priced); err != nil || priced != 1 {
			t.Fatalf("priced=%d err=%v", priced, err)
		}
		if i == 1 {
			if err = st.Rebuild(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		st.Close()
	}
}
