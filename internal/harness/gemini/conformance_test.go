package gemini

import (
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

// TestConformance runs the shared conformance suite (docs/HARNESS.md) against
// the checked-in fixture in testdata/conformance/gemini.
//
// Fixture layout (Case.New receives the GEMINI_CLI_HOME level, which is where
// Discover starts walking):
//
//	projects.json                              path -> slug registry
//	tmp/demo-proj/chats/session-*-aaa11111.jsonl   jsonl main session
//	tmp/demo-proj/chats/session-*-aaa11111.json    legacy sibling, dropped by Discover
//	tmp/demo-proj/chats/gem-conf-main/session-*-ccc33333.jsonl  nested subagent
//	tmp/<sha256>/chats/session-*-bbb22222.json     legacy-only session, hashed project id
//
// The jsonl main session records no `directories`, so its project comes from
// projects.json through the "demo-proj" slug; the legacy session's project id is
// sha256("/Users/dev/hashed-proj") and resolves through the registry's hashed
// key. The subagent records `directories`, which takes precedence. Covering
// both branches keeps the fixture's absolute paths out of the golden.
//
// projects.json stays on a single line on purpose: the suite's default growth
// writes whole-line prefixes of every fixture file, and a pretty-printed
// registry would be invalid JSON in the early steps, changing every project
// path mid-run.
func TestConformance(t *testing.T) {
	harnesstest.Run(t, conformanceCase())
}

// BenchmarkConformance is the template for the per-harness parse benchmark.
func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, conformanceCase())
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.Gemini),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: "testdata/conformance/gemini",
	}
}
