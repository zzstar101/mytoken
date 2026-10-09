package opencode

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

// conformanceFixture is the XDG_DATA_HOME/opencode level: the directory whose
// direct child is opencode.db, which is exactly what Discover reads.
const conformanceFixture = "testdata/conformance/opencode"

// TestConformance runs the shared conformance suite (docs/HARNESS.md) against
// the checked-in fixture in testdata/conformance/opencode.
//
// Unlike the append-only log formats, OpenCode is sqlite, so the fixture is not
// a set of files to copy: Case.Grow builds the database from the seed scripts
// in testdata/conformance/opencode/seed, one growth step per script. Step 0
// creates the schema and inserts the first half of the rows; step 1 inserts the
// second half. Case.Steps is 2 to match those two halves.
//
// The seed scripts are line-oriented: one SQL statement per line, with '--'
// comments and blank lines skipped. The closure executes them with database/sql
// (modernc.org/sqlite is already the package's driver) and never lets two
// statements share a line.
//
// Rows cover:
//   - two sessions, one of them a subagent (session.parent_id)
//   - both message generations the parser reads: `message` (v1) and
//     `session_message` (v2)
//   - three models across two providers, cache read and cache write, reasoning
//   - no-usage rows that must never emit an event: missing `tokens`,
//     all-zero tokens with zero cost, and the v2 control rows
//     (agent-switched / model-switched) the schema stores next to usage
//
// The timestamp layout is deliberate: v1 and v2 rows interleave rather than
// keeping every `session_message` row newer than every `message` row. readMessages
// resumes each table from its own watermark, so the older v2 row (msg_c4) is
// read on a cold pass and re-read correctly across growth steps. A single shared
// watermark used to drop exactly that row; keeping the interleaved layout means
// the incremental check keeps guarding against a regression.
//
// OpenCode persists the session title as a column (its own first user message),
// so both session rows carry the text of their first user message; the user
// message rows themselves have no usage payload and are filtered out.
//
// Project paths come from session.directory, which is an absolute path inside
// the fixture and not derived from the sqlite file's location, so no
// Case.Rewrite is needed.
//
// SENTINEL_PRIVATE_TEXT sits in the assistant message text of two rows: it
// proves the fixture is the privacy check's target without ever reaching the
// parsed snapshot (the parser keeps only numbers and identifiers, never text).
func TestConformance(t *testing.T) {
	harnesstest.Run(t, conformanceCase())
}

// BenchmarkConformance is the template for the per-harness parse benchmark.
func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, conformanceCase())
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.OpenCode),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: conformanceFixture,
		Steps:   2,
		Grow:    growConformanceDB,
	}
}

// growConformanceDB applies seed/step<N>.sql to dir/opencode.db. It runs one
// growth step at a time so the suite can replay the database in halves and
// compare the incrementally merged snapshot with a single full parse.
func growConformanceDB(t testing.TB, dir string, step int) error {
	t.Helper()
	seed := filepath.Join(conformanceFixture, "seed", fmt.Sprintf("step%d.sql", step))
	data, err := os.ReadFile(seed)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "opencode.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	for i, stmt := range strings.Split(string(data), "\n") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" || strings.HasPrefix(stmt, "--") {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("%s line %d: %w", seed, i+1, err)
		}
	}
	return nil
}
