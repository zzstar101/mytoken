package claudedesktop

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

// TestConformance runs the shared conformance suite (docs/HARNESS.md) against
// the checked-in fixture in testdata/conformance/claudedesktop.
func TestConformance(t *testing.T) {
	harnesstest.Run(t, conformanceCase())
}

// BenchmarkConformance is the per-harness parse benchmark.
func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, conformanceCase())
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.ClaudeDesktop),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: "testdata/conformance/claudedesktop",
	}
}

// TestCoworkProjectLayout pins the workspace derivation and the project naming
// observed in real Claude Desktop installs: the ledger lives at
// <app>/<workspace>/usage-ledger/*.ndjson and the session metadata is written
// beside it as <workspace>/local_<id>.json. Session titles must never leak into
// the project name (docs/HARNESS.md §5).
func TestCoworkProjectLayout(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "usage-ledger"), 0o755); err != nil {
		t.Fatalf("mkdir usage-ledger: %v", err)
	}
	writeFixtureFile(t, filepath.Join(workspace, "spaces.json"),
		`{"spaces":[{"id":"sp-1","name":"demo-workspace"}]}`)
	writeFixtureFile(t, filepath.Join(workspace, "local_sp.json"),
		`{"spaceId":"sp-1","title":"must not become the project"}`)
	writeFixtureFile(t, filepath.Join(workspace, "local_folder.json"),
		`{"userSelectedFolders":["/Users/demo/proj"]}`)
	writeFixtureFile(t, filepath.Join(workspace, "local_cwd.json"),
		`{"cwd":"/Users/demo/cwdproj","title":"must not become the project"}`)

	ledger := filepath.Join(workspace, "usage-ledger", "2026-10-03.ndjson")
	if got := coworkWorkspaceDir(ledger); got != workspace {
		t.Fatalf("coworkWorkspaceDir(%q) = %q, want %q", ledger, got, workspace)
	}
	for _, tc := range []struct{ session, want string }{
		{"local_sp", "demo-workspace"},
		{"local_folder", "proj"},
		{"local_cwd", "cwdproj"},
		{"local_missing", projectCowork},
	} {
		if got := coworkProject(ledger, tc.session, "cowork"); got != tc.want {
			t.Errorf("coworkProject(%q, cowork) = %q, want %q", tc.session, got, tc.want)
		}
	}
	if got := coworkProject(ledger, "local_missing", "code"); got != projectCode {
		t.Errorf("coworkProject(local_missing, code) = %q, want %q", got, projectCode)
	}
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
