package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCopiedDatabaseMigrationPerformance(t *testing.T) {
	if os.Getenv("MYTOKEN_MIGRATION_PERF") == "" {
		t.Skip("opt-in copied-database migration probe")
	}
	home, err := filepath.Abs(os.Getenv("MYTOKEN_HOME"))
	if err != nil || !strings.HasPrefix(home, "/tmp/mtbench/") {
		t.Fatal("migration probe requires copy under /tmp/mtbench/")
	}
	path := filepath.Join(home, "mytoken.db")
	columns := `rowid,harness,dedup_key,session_id,parent_id,project,timestamp,model,provider,base_url,input,output,cache_read,cache_write,reasoning,log_cost,resolved_provider,attrib,cost,priced,raw_project`
	n := 21
	digest := func(db *sql.DB) string {
		rows, err := db.Query(`SELECT ` + columns + ` FROM events ORDER BY harness,dedup_key`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		hash := sha256.New()
		enc := json.NewEncoder(hash)
		for rows.Next() {
			values := make([]any, n)
			args := make([]any, n)
			for i := range values {
				args[i] = &values[i]
			}
			if err = rows.Scan(args...); err != nil {
				t.Fatal(err)
			}
			if err = enc.Encode(values); err != nil {
				t.Fatal(err)
			}
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%x", hash.Sum(nil))
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var signals int
	if err = db.QueryRow(`SELECT count(*) FROM pragma_table_info('events') WHERE name='request_id'`).Scan(&signals); err != nil {
		t.Fatal(err)
	}
	if signals > 0 {
		columns += `,request_id,boundary,bill_amount,bill_unit`
		n += 4
	}
	before := digest(db)
	db.Close()
	at := time.Now()
	st, err := Open(path)
	elapsed := time.Since(at)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if after := digest(st.DB()); before != after {
		t.Fatalf("lossy migration digest %s → %s", before, after)
	}
	t.Logf("store.Open migration %s; all %d fields including rowid unchanged (SHA256 %s)", elapsed, n, before)
}
