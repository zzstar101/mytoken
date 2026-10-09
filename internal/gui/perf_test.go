package gui

import (
	"os"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// trayBudget is the popover's first-frame budget (docs/ROADMAP.md) for the
// work MyToken does on the UI thread: building the view. The headless
// tester rasterizes on the CPU (glass, shadows) at a cost the GPU does not
// pay, so the test times the view callback, not the whole tester frame.
const trayBudget = 50 * time.Millisecond

func trayState(b testing.TB) *State {
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	s := NewState(NewDemoService(now), Hooks{Settings: NewDemoSettings(), Now: func() time.Time { return now },
		Progress: func() (int, int) { return 10, 10 }}, nil)
	s.Start()
	return s
}

// trayFrame opens a fresh popover and returns how long its first view
// build took.
func trayFrame(s *State) time.Duration {
	var first time.Duration
	tt := ui.NewTester(func(c *ui.Context) {
		start := time.Now()
		s.TrayView(c)
		if first == 0 {
			first = time.Since(start)
		}
	}, TrayWidth, TrayHeight)
	tt.SetScale(1)
	return first
}

// TestTrayFirstFrame opens the popover once its data is loaded, as the
// running app does, and times the first frame. It reports the time and
// fails over budget only under MYTOKEN_STRICT_PERF=1 (shared CI runners
// are noisy).
func TestTrayFirstFrame(t *testing.T) {
	s := trayState(t)
	trayFrame(s) // warm fonts and caches shared by every window
	best := time.Hour
	for range 5 {
		best = min(best, trayFrame(s))
	}
	t.Logf("tray first frame: %v (budget %v)", best, trayBudget)
	if best > trayBudget && os.Getenv("MYTOKEN_STRICT_PERF") == "1" {
		t.Fatalf("tray first frame %v over budget %v", best, trayBudget)
	}
}

func BenchmarkTrayFirstFrame(b *testing.B) {
	s := trayState(b)
	trayFrame(s)
	b.ResetTimer()
	for b.Loop() {
		trayFrame(s)
	}
}
