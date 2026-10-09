package store

import (
	"context"
	"fmt"
)

func (s *Store) migrate() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"events", "sessions"} {
		rows, err := tx.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			return err
		}
		columns := map[string]bool{}
		for rows.Next() {
			var id, notnull, pk int
			var name, typ string
			var def any
			if err = rows.Scan(&id, &name, &typ, &notnull, &def, &pk); err != nil {
				rows.Close()
				return err
			}
			columns[name] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		additions := map[string]string{"raw_project": "TEXT NOT NULL DEFAULT ''"}
		if table == "events" {
			additions["priced"] = "INTEGER NOT NULL DEFAULT 0"
		}
		for name, decl := range additions {
			if !columns[name] {
				if _, err = tx.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, name, decl)); err != nil {
					return err
				}
				if name == "raw_project" {
					_, err = tx.Exec("UPDATE " + table + " SET raw_project=project")
				} else {
					_, err = tx.Exec("UPDATE events SET priced=1 WHERE log_cost IS NOT NULL OR cost!=0")
				}
				if err != nil {
					return err
				}
			}
		}
		if table == "sessions" {
			if _, err = tx.Exec("CREATE INDEX IF NOT EXISTS " + table + "_raw_project ON " + table + "(raw_project)"); err != nil {
				return err
			}
		}
	}
	compacted, err := migrateCompact(tx)
	if err != nil {
		return err
	}
	if err = migrateSignals(tx); err != nil {
		return err
	}
	if err = migrateEventIndexes(tx); err != nil {
		return err
	}
	if err = migrateDaily(tx); err != nil {
		return err
	}
	if _, err = tx.Exec(relaySchema); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if compacted {
		if _, err = s.db.Exec("VACUUM"); err != nil {
			return err
		}
	}
	s.projects = newProjectResolver()
	if err = s.EnsureLocalDays(context.Background()); err != nil {
		return err
	}
	return s.NormalizeProjects(context.Background())
}
