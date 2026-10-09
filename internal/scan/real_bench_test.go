package scan_test

import (
	"context"
	"github.com/zzstar101/mytoken/internal/app"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This opt-in probe reads configured source logs but writes only a benchmark
// copy. The fixed guard deliberately refuses normal application data paths.
func TestCopiedDatabaseScanPerformance(t *testing.T) {
	if os.Getenv("MYTOKEN_REAL_PERF") == "" {
		t.Skip("opt-in copied-database scan probe")
	}
	home, err := filepath.Abs(os.Getenv("MYTOKEN_HOME"))
	if err != nil || !(home == "/tmp/mtbench" || strings.HasPrefix(home, "/tmp/mtbench/")) {
		t.Fatal("probe requires MYTOKEN_HOME under /tmp/mtbench")
	}
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
	defer func() { close(stop); <-done; t.Logf("peak Go heap %.2f MiB", float64(peak.Load())/(1<<20)) }()
	at := time.Now()
	a, err := app.OpenLocal()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	t.Logf("open %s", time.Since(at))
	ctx := context.Background()
	for _, name := range []string{"incremental-before", "rebuild", "incremental-after"} {
		at = time.Now()
		if name == "rebuild" {
			err = a.Scanner.Rebuild(ctx)
		} else {
			err = a.Scanner.Scan(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
		done, total := a.Scanner.Progress()
		t.Logf("%s %s sources=%d/%d", name, time.Since(at), done, total)
	}
}
