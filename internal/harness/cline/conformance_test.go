package cline

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
)

// conformanceSeed is the fixture's seed tree. Cline rewrites ui_messages.json
// (and the standalone CLI messages file) in place, so the suite cannot grow the
// fixture by copying whole files: Grow materializes the task directories from
// seed/ and rewrites each JSON array with the step's prefix.
const conformanceSeed = "testdata/conformance/cline/seed"

const (
	conformanceTaskA = "saoudrizwan.claude-dev/tasks/1751000000000/ui_messages.json"
	conformanceTaskB = "saoudrizwan.claude-dev/tasks/1751000009000/ui_messages.json"

	conformanceCLISession = "0c1a2b3c-1111-4222-8333-444455556001"
	conformanceCLIFile    = "cli/sessions/" + conformanceCLISession + "/" + conformanceCLISession + ".messages.json"
)

// Note: the shared clinetask code exposes no reasoning signal — its
// apiReqPayload carries only tokensIn/tokensOut/cacheWrites/cacheReads/cost —
// so every event in golden.json has reasoning 0. The seeded say:"reasoning"
// entry is not a usage record and therefore produces no event.

// conformanceGrowth maps a seeded usage file to the number of array entries
// each growth step writes. The seeds are ordered so that every step adds at
// least one usage-carrying entry, which is what makes the incremental check
// exercise the parser's emitted-entry counter.
var conformanceGrowth = map[string][]int{
	conformanceTaskA:   {3, 6, 9},
	conformanceTaskB:   {2, 4, 7},
	conformanceCLIFile: {1, 2, 4},
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    "cline",
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: "testdata/conformance/cline",
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
			if data, err = truncateSeedJSON(data, take[step]); err != nil {
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

// truncateSeedJSON re-encodes a seeded usage document with only its first n
// records: ui_messages.json is a JSON array, the standalone CLI messages file
// is an object whose "messages" array holds them. The seed holds the file's
// final state, so one growth step is "the same document with n records",
// exactly how Cline rewrites the file in place.
func truncateSeedJSON(data []byte, n int) ([]byte, error) {
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '{' {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, err
		}
		var messages []json.RawMessage
		if err := json.Unmarshal(doc["messages"], &messages); err != nil {
			return nil, err
		}
		prefix, err := json.Marshal(limit(messages, n))
		if err != nil {
			return nil, err
		}
		doc["messages"] = prefix
		return json.Marshal(doc)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return json.Marshal(limit(entries, n))
}

// limit returns the first n items, or every item when there are fewer.
func limit[T any](items []T, n int) []T {
	if n > len(items) {
		n = len(items)
	}
	return items[:n]
}
