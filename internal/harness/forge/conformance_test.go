package forge

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

// conformanceFixture is the synthetic Forge installation. The seed scripts live
// under seed/ so the suite never copies them into the materialized tree; the
// database is rebuilt from them at every step.
const conformanceFixture = "testdata/conformance/forge"

// conformanceSteps are the database snapshots the fixture grows through. Forge
// keeps the whole transcript in one column and rewrites it in place, so each
// step writes the delta that follows it.
var conformanceSteps = []string{"step0.sql", "step1.sql", "step2.sql"}

func TestConformance(t *testing.T) {
	harnesstest.Run(t, conformanceCase())
}

func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, conformanceCase())
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.Forge),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: conformanceFixture,
		Steps:   len(conformanceSteps),
		Grow:    growConformance,
	}
}

// growConformance rebuilds the conversations database for one step. The
// materialized root is the Forge home, so the database sits directly in it.
func growConformance(_ testing.TB, dir string, step int) error {
	path := filepath.Join(dir, dbName)
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	scripts := append([]string{"schema.sql"}, conformanceSteps[:step+1]...)
	for _, name := range scripts {
		script, err := os.ReadFile(filepath.Join(conformanceFixture, "seed", name))
		if err != nil {
			return err
		}
		if _, err := db.Exec(string(script)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}
