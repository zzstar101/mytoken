package hermes

import (
	"testing"
	"time"
)

func TestDefaultRootsEnv(t *testing.T) {
	t.Setenv("HERMES_HOME", "/tmp/hermes-fixture-home")
	roots := DefaultRoots()
	if len(roots) != 1 || roots[0] != "/tmp/hermes-fixture-home" {
		t.Fatalf("DefaultRoots() = %v, want the HERMES_HOME override", roots)
	}
}

func TestTitleText(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"plain", "recompute the totals", "recompute the totals"},
		{"wrapper", `<system-reminder data-role="user-context">ctx</system-reminder>`, ""},
		{"slash command", "/compact now", ""},
		{"blocks", `[{"type":"text","text":"first user message"}]`, "first user message"},
		{"empty", "   ", ""},
	} {
		if got := titleText(tc.in); got != tc.want {
			t.Errorf("%s: titleText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestTitleTextCollapses(t *testing.T) {
	got := titleText("first   user\nmessage")
	if got != "first user message" {
		t.Fatalf("titleText() = %q, want whitespace collapsed", got)
	}
}

func TestUnixSeconds(t *testing.T) {
	got := unixSeconds(1767323045.5)
	want := time.Date(2026, 1, 2, 3, 4, 5, 500000000, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("unixSeconds() = %s, want %s", got, want)
	}
	if !unixSeconds(0).IsZero() {
		t.Fatalf("unixSeconds(0) must be the zero time")
	}
}

func TestDedupKey(t *testing.T) {
	if got := dedupKey("s1", "m1", ""); got != "s1#m1" {
		t.Fatalf("dedupKey() = %q", got)
	}
	if got := dedupKey("s1", "m1", "task"); got != "s1#m1#task" {
		t.Fatalf("dedupKey() = %q", got)
	}
}

func TestCostPointer(t *testing.T) {
	if costPointer(0) != nil {
		t.Fatalf("a zero cost must stay nil")
	}
	if got := costPointer(0.25); got == nil || *got != 0.25 {
		t.Fatalf("costPointer(0.25) = %v", got)
	}
}

func TestCount(t *testing.T) {
	if got := count(-3); got != 0 {
		t.Fatalf("count(-3) = %d, want 0", got)
	}
	if got := count(1200.4); got != 1200 {
		t.Fatalf("count(1200.4) = %d, want 1200", got)
	}
}
