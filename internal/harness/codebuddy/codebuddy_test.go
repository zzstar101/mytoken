package codebuddy

import (
	"path/filepath"
	"testing"

	"github.com/zzstar101/mytoken/internal/model"
)

// TestHarnessAndRoots checks that the WorkBuddy reader is instantiated with the
// CodeBuddy harness id and that CODEBUDDY_HOME overrides the default home.
func TestHarnessAndRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEBUDDY_HOME", home)
	p := New()
	if got := p.Harness(); got != model.CodeBuddy {
		t.Errorf("Harness() = %q, want %q", got, model.CodeBuddy)
	}
	want := filepath.Join(home, "projects")
	roots := p.Roots()
	if len(roots) != 1 || roots[0] != want {
		t.Errorf("Roots() = %v, want [%s]", roots, want)
	}
}
