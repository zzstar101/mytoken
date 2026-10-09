package crush

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
)

// conformanceSeed holds the fixture's SQL seed. A Crush database is appended to
// in place, so Grow cannot copy fixture files: it replays the schema plus one
// more slice of rows per step.
const conformanceSeed = "testdata/conformance/crush/seed"

// conformanceSteps are the seed's growth slices, applied in order.
var conformanceSteps = []string{"step0.sql", "step1.sql", "step2.sql"}

// conformanceEnv points Crush's global project registry at throwaway
// directories. Without it Discover also consults $CRUSH_GLOBAL_DATA and
// $XDG_DATA_HOME, which on a developer machine hold real databases (see
// crush_test.go's isolate).
func conformanceEnv(tb testing.TB) {
	tb.Helper()
	tb.Setenv("CRUSH_GLOBAL_DATA", tb.TempDir())
	tb.Setenv("XDG_DATA_HOME", tb.TempDir())
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    "crush",
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: "testdata/conformance/crush",
		Steps:   len(conformanceSteps),
		Grow:    growConformance,
		// Crush derives a session's project from the database file's location,
		// and the baseline and incremental checks use different temp roots.
		Rewrite: func(root string, snap *harnesstest.Snapshot) {
			for i := range snap.Events {
				snap.Events[i].ProjectPath = strings.Replace(snap.Events[i].ProjectPath, root, harnesstest.RootPlaceholder, 1)
			}
			for i := range snap.Sessions {
				snap.Sessions[i].Project = strings.Replace(snap.Sessions[i].Project, root, harnesstest.RootPlaceholder, 1)
			}
		},
	}
}

func TestConformance(t *testing.T) {
	conformanceEnv(t)
	harnesstest.Run(t, conformanceCase())
}

func BenchmarkConformance(b *testing.B) {
	conformanceEnv(b)
	harnesstest.Bench(b, conformanceCase())
}

// growConformance rebuilds <dir>/crush.db from the seed's schema and its first
// step+1 slices, so every step is the same database grown by one more batch of
// sessions and messages.
func growConformance(_ testing.TB, dir string, step int) error {
	path := filepath.Join(dir, "crush.db")
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
	for _, name := range append([]string{"schema.sql"}, conformanceSteps[:step+1]...) {
		script, err := os.ReadFile(filepath.Join(conformanceSeed, name))
		if err != nil {
			return err
		}
		if _, err := db.Exec(string(script)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}
