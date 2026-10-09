package dsh

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

// The materialized conformance tree mirrors $DSH_HOME/sessions, which is the
// layer Discover reads directly:
//
//	--home-user-fixture-alpha--/session-01a0...0001/session.v4.jsonl     parent, plain
//	--home-user-fixture-alpha--/session-01a0...0001/session.v3.jsonl.zstd  stale decoy
//	--home-user-fixture-alpha--/01b0...0002/session.jsonl.zstd           subagent
const (
	conformanceSteps = 3 // mirrors harnesstest's default Steps
	conformanceSlug  = "--home-user-fixture-alpha--"
	conformanceP1    = "session-01a00000-0000-7000-8000-000000000001"
	conformanceP2    = "01b00000-0000-7000-8000-000000000002"
)

// TestConformance runs the shared conformance suite (docs/HARNESS.md) against
// the checked-in fixture in testdata/conformance/dsh.
func TestConformance(t *testing.T) {
	harnesstest.Run(t, conformanceCase())
}

// BenchmarkConformance is the per-harness parse benchmark.
func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, conformanceCase())
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.DSH),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: "testdata/conformance/dsh",
		Steps:   conformanceSteps,
		Grow:    growConformance,
	}
}

// growConformance materializes one growth step of the fixture. Each step adds
// roughly a third of every seed log (whole lines only). The subagent log is
// zstd-compressed and DSH re-reads a compressed log in full on every parse, so
// a step writes a complete frame holding the records seen so far instead of
// appending bytes to the frame. session.v3.jsonl.zstd is a stale, lower-version
// sibling of the parent log: it is written once and never grows, so Discover
// must keep selecting the v4 plain log over it.
func growConformance(t testing.TB, dir string, step int) error {
	t.Helper()
	if step == 0 {
		if err := writeZstdStep(filepath.Join(dir, conformanceSlug, conformanceP1, "session.v3.jsonl.zstd"), "parent.v3.jsonl", conformanceSteps-1); err != nil {
			return err
		}
	}
	if err := writePlainStep(filepath.Join(dir, conformanceSlug, conformanceP1, "session.v4.jsonl"), "parent.jsonl", step); err != nil {
		return err
	}
	return writeZstdStep(filepath.Join(dir, conformanceSlug, conformanceP2, "session.jsonl.zstd"), "child.jsonl", step)
}

// seedLines reads one seed log and returns its records as newline-terminated
// lines, so a growth step can never split a JSON record.
func seedLines(name string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join("testdata", "conformance", "dsh", "seed", name))
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line+"\n")
		}
	}
	return lines, nil
}

// stepPrefix is the number of records visible at step, matching the suite's
// ceil(records*(step+1)/Steps) growth rule.
func stepPrefix(total, step int) int {
	take := (total*(step+1) + conformanceSteps - 1) / conformanceSteps
	if take > total {
		take = total
	}
	return take
}

func writePlainStep(path, seed string, step int) error {
	lines, err := seedLines(seed)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(lines[:stepPrefix(len(lines), step)], "")), 0o644)
}

func writeZstdStep(path, seed string, step int) error {
	lines, err := seedLines(seed)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		return err
	}
	if _, err := zw.Write([]byte(strings.Join(lines[:stepPrefix(len(lines), step)], ""))); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
