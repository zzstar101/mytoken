package kilo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
)

// conformanceSeed is the fixture's seed tree. Kilo Code rewrites ui_messages.json
// in place, so the suite cannot grow the fixture by copying whole files: Grow
// materializes the task directories from seed/ and rewrites each JSON array
// with the step's prefix.
const conformanceSeed = "testdata/conformance/kilo/seed"

// Note: the shared clinetask code exposes no reasoning signal — its
// apiReqPayload carries only tokensIn/tokensOut/cacheWrites/cacheReads/cost —
// so every event in golden.json has reasoning 0. The seeded say:"reasoning"
// entry is not a usage record and therefore produces no event.

// conformanceGrowth maps a seeded usage file to the number of array entries
// each growth step writes. The seeds are ordered so that every step adds at
// least one usage-carrying entry, which is what makes the incremental check
// exercise the parser's emitted-entry counter.
var conformanceGrowth = map[string][]int{
	"kilocode.kilo-code/tasks/1751200000000/ui_messages.json": {3, 6, 9},
	"kilocode.kilo-code/tasks/1751200009000/ui_messages.json": {2, 4, 7},
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    "kilo",
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: "testdata/conformance/kilo",
		Steps:   3,
		Grow:    growConformance,
	}
}

func TestConformance(t *testing.T) { harnesstest.Run(t, conformanceCase()) }

func BenchmarkConformance(b *testing.B) { harnesstest.Bench(b, conformanceCase()) }

// growConformance materializes the seed tree into dir, truncating every seeded
// usage array to the entry count of this step. Sibling files — the conversation
// history, task metadata, history item and CLI manifest — are written whole:
// only the usage arrays are rewritten as a task progresses.
func growConformance(_ testing.TB, dir string, step int) error {
	return filepath.WalkDir(conformanceSeed, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(conformanceSeed, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if take := conformanceGrowth[filepath.ToSlash(rel)]; len(take) > 0 {
			if data, err = truncateJSONArray(data, take[step]); err != nil {
				return err
			}
		}
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
}

// truncateJSONArray re-encodes the first n entries of a seeded JSON array: the
// seed holds the file's final state, so one growth step is "the same document
// with n entries", exactly how Kilo Code rewrites the array in place.
func truncateJSONArray(data []byte, n int) ([]byte, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	if n > len(entries) {
		n = len(entries)
	}
	return json.Marshal(entries[:n])
}
