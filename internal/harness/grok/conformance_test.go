package grok

import (
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

// TestConformance runs the shared conformance suite (docs/HARNESS.md) against
// the checked-in fixture in testdata/conformance/grok.
//
// The fixture covers both recorded layouts: logs/unified.jsonl (one event per
// recorded inference, including a session whose directory is dropped because
// the log already covers it) and one session directory whose updates.jsonl
// supplies the per-turn usage, the title and a compaction boundary. Both files
// are append-only, so the suite's line-prefix growth stands in for Case.Grow.
func TestConformance(t *testing.T) {
	harnesstest.Run(t, conformanceCase())
}

// BenchmarkConformance is the template for the per-harness parse benchmark.
func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, conformanceCase())
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.Grok),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: "testdata/conformance/grok",
	}
}
