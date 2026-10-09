package hermes

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

// conformanceFixture is the HERMES_HOME level: the directory whose direct child
// is state.db, which is exactly what Discover reads.
const conformanceFixture = "testdata/conformance/hermes"

// TestConformance runs the shared conformance suite (docs/HARNESS.md) against
// the checked-in fixture in testdata/conformance/hermes.
//
// Hermes is sqlite, so the fixture is not a set of files to copy: Case.Grow
// builds the database from the seed scripts in
// testdata/conformance/hermes/seed, one growth step per script. Step 0 creates
// the schema and inserts the first rows; step 1 appends a new session only.
// Case.Steps is 2 to match those two halves.
//
// The seed scripts are line-oriented: one SQL statement per line, with '--'
// comments and blank lines skipped. The closure executes them with
// database/sql (modernc.org/sqlite is already the package's driver) and never
// lets two statements share a line.
//
// Why growth only appends: Hermes stores cumulative counters, and the parser
// emits one snapshot event per (session, model) that the store upserts on
// (harness,dedup_key). An incremental re-parse therefore re-emits the rows it
// already saw with identical fields, which the suite's merger accepts, and any
// *changed* row would be a schema-level edit rather than an append. Growing by
// adding a new session keeps that contract explicit: the merged snapshot must
// equal one cold parse of the final database.
//
// Rows cover:
//   - two sessions, the second a subagent (sessions.parent_session_id)
//   - two models across two providers, cache read and cache write, reasoning
//   - no-usage rows that must never emit an event: a session with an all-zero
//     session_model_usage row
//   - titles derived from the first real user message (sessions.title is empty,
//     the first user row is a <system-reminder> wrapper that must be skipped,
//     and the subagent's message body is a JSON block array)
//
// SENTINEL_PRIVATE_TEXT sits in assistant/tool message bodies and in a
// reasoning_content column, never in a first user message: it proves the
// fixture is the privacy check's target without ever reaching the parsed
// snapshot (the parser keeps only numbers, identifiers and the title).
func TestConformance(t *testing.T) {
	harnesstest.Run(t, conformanceCase())
}

// BenchmarkConformance is the template for the per-harness parse benchmark.
func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, conformanceCase())
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.Hermes),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: conformanceFixture,
		Steps:   2,
		Grow:    growConformanceDB,
	}
}

// growConformanceDB applies seed/step<N>.sql to dir/state.db. It runs one growth
// step at a time so the suite can replay the database in halves and compare the
// incrementally merged snapshot with a single full parse.
func growConformanceDB(t testing.TB, dir string, step int) error {
	t.Helper()
	seed := filepath.Join(conformanceFixture, "seed", fmt.Sprintf("step%d.sql", step))
	data, err := os.ReadFile(seed)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, stateDBName))
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
