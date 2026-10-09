package openclaw

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

// conformanceSeed holds the fixture's seeds. OpenClaw grows its recordings in
// place (the assistant appends to a session's .jsonl and to the agent store),
// so Grow writes both from these seeds instead of copying files.
const conformanceSeed = "testdata/conformance/openclaw/seed"

// conformanceGrowth are the append-only recordings: seed file, destination
// under the materialized root, and the number of lines each step writes.
var conformanceGrowth = []struct {
	seed  string
	dst   []string
	lines []int
}{
	{"alpha-1.jsonl", []string{"alpha", "sessions", "alpha-jsonl-1.jsonl"}, []int{4, 8, 12}},
	{"alpha-2.jsonl", []string{"alpha", "sessions", "alpha-jsonl-2.jsonl"}, []int{1, 2, 3}},
	// The store already owns beta-store-1, so this legacy copy must never be
	// discovered; it exists in the fixture to keep that guarantee honest.
	{"beta-legacy.jsonl", []string{"beta", "sessions", "beta-store-1.jsonl"}, []int{3, 3, 3}},
}

// conformanceStoreSteps are the agent-store snapshots, one per step.
var conformanceStoreSteps = []string{"store-step0.sql", "store-step1.sql", "store-step2.sql"}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.OpenClaw),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: "testdata/conformance/openclaw",
		Steps:   len(conformanceStoreSteps),
		Grow:    growConformance,
	}
}

func TestConformance(t *testing.T) {
	harnesstest.Run(t, conformanceCase())
}

func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, conformanceCase())
}

// growConformance materializes the recordings a step's worth of assistant
// activity leaves behind: longer .jsonl files, an updated sessions.json index
// and a store rebuilt from the step's snapshot.
func growConformance(t testing.TB, dir string, step int) error {
	t.Helper()
	for _, growth := range conformanceGrowth {
		dst := filepath.Join(append([]string{dir}, growth.dst...)...)
		if err := writeLines(filepath.Join(conformanceSeed, growth.seed), dst, growth.lines[step]); err != nil {
			return err
		}
	}
	if err := writeIndex(dir); err != nil {
		return err
	}
	return writeStore(dir, conformanceStoreSteps[step])
}

// writeLines writes the first n lines of src to dst. Every line the parser
// reads must end with a newline, so the prefix keeps one.
func writeLines(src, dst string, n int) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if n > len(lines) {
		n = len(lines)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(strings.Join(lines[:n], "\n")+"\n"), 0o644)
}

// writeIndex writes an agent's session index: one session named by id (its file
// is derived) and one named by an explicit path.
func writeIndex(dir string) error {
	sessions := filepath.Join(dir, "alpha", sessionsDir)
	index := map[string]map[string]string{
		"alpha-jsonl-1": {"sessionId": "alpha-jsonl-1"},
		"alpha-jsonl-2": {
			"sessionId":   "alpha-jsonl-2",
			"sessionFile": filepath.Join(sessions, "alpha-jsonl-2.jsonl"),
		},
	}
	raw, err := json.Marshal(index)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(sessions, indexName), raw, 0o644)
}

// writeStore rebuilds <dir>/beta/agent/openclaw-agent.sqlite from the step's
// snapshot.
func writeStore(dir, name string) error {
	path := filepath.Join(dir, "beta", storeDirName, storeFileName)
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	script, err := os.ReadFile(filepath.Join(conformanceSeed, name))
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	if _, err := db.Exec(string(script)); err != nil {
		db.Close()
		return fmt.Errorf("%s: %w", name, err)
	}
	return db.Close()
}
