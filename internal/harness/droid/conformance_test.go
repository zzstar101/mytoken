package droid

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

// conformanceFixture is the synthetic Droid tree. The raw transcripts and
// settings files live under seed/ so the suite never copies them into the
// materialized tree; growConformance lays out the sessions itself.
const conformanceFixture = "testdata/conformance/droid"

// conformanceSteps is the fixture's growth plan. lines is how many transcript
// records a session has at each step; usageAt is the step at which its settings
// file starts carrying tokenUsage.
//
// alpha grows at every step but only learns its totals in the last one, where
// the transcript itself is unchanged: that is Droid's settings rewrite, and the
// parser must still report the growth. bravo appears complete in the last step.
// charlie never records usage at all, so it must still be reported as a session.
var conformanceSteps = struct {
	lines   map[string][4]int
	usageAt map[string]int
}{
	lines: map[string][4]int{
		"alpha":   {3, 6, 10, 10},
		"bravo":   {0, 0, 0, 4},
		"charlie": {4, 0, 0, 0},
	},
	usageAt: map[string]int{"alpha": 3, "bravo": 3},
}

func TestConformance(t *testing.T) {
	harnesstest.Run(t, conformanceCase())
}

func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, conformanceCase())
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.Droid),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: conformanceFixture,
		Steps:   4,
		Grow:    growConformance,
	}
}

// growConformance materializes one step of the fixture. Droid's transcript is
// append-only and its settings file is rewritten in place, so each step writes
// the record prefix the session has reached and the settings content it has
// flushed.
func growConformance(t testing.TB, dir string, step int) error {
	t.Helper()
	seed := filepath.Join(conformanceFixture, "seed")
	for _, session := range []struct {
		day  string
		name string
	}{
		{day: "2026-03-02", name: "charlie"},
		{day: "2026-03-02", name: "alpha"},
		{day: "2026-03-03", name: "bravo"},
	} {
		lines := conformanceSteps.lines[session.name][step]
		if lines <= 0 {
			continue
		}
		if err := writeTranscript(dir, seed, session.day, session.name, lines); err != nil {
			return err
		}
		settings := session.name + ".settings.json"
		if at, ok := conformanceSteps.usageAt[session.name]; ok && step >= at {
			settings = session.name + ".usage.json"
		}
		if err := writeSettings(dir, seed, session.day, session.name, settings); err != nil {
			return err
		}
	}
	return nil
}

// writeTranscript writes the first lines records of a seed transcript. The
// trailing newline is restored because the parser only folds complete lines.
func writeTranscript(dir, seed, day, name string, lines int) error {
	raw, err := os.ReadFile(filepath.Join(seed, name+transcriptExt))
	if err != nil {
		return err
	}
	records := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if lines > len(records) {
		lines = len(records)
	}
	body := strings.Join(records[:lines], "\n") + "\n"
	path := filepath.Join(dir, sessionsDirName, day, name+transcriptExt)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

// writeSettings writes one seed settings file next to its transcript.
func writeSettings(dir, seed, day, name, seedFile string) error {
	raw, err := os.ReadFile(filepath.Join(seed, seedFile))
	if err != nil {
		return err
	}
	path := filepath.Join(dir, sessionsDirName, day, name+settingsExt)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}
