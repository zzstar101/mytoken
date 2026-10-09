package query

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// benchmarkFixture deliberately has 200K requests but only 600 sessions. It
// writes bounded batches so fixture creation does not mask query allocations.
func benchmarkFixture(tb testing.TB) (Service, time.Time) {
	tb.Helper()
	st, err := store.Open(filepath.Join(tb.TempDir(), "benchmark.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { st.Close() })
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	const total = 200000
	for session := 0; session < 600; session++ {
		n := total / 600
		if session < total%600 {
			n++
		}
		id := fmt.Sprintf("session-%03d", session)
		parent := ""
		if session%10 == 1 {
			parent = fmt.Sprintf("session-%03d", session-1)
		}
		events := make([]model.UsageEvent, n)
		res := make([]store.Resolution, n)
		for j := range events {
			at := start.Add(time.Duration(session)*2*time.Hour + time.Duration(j)*time.Minute)
			events[j] = model.UsageEvent{DedupKey: fmt.Sprintf("event-%03d-%04d", session, j), SessionID: id, ParentID: parent, Timestamp: at, ProjectPath: fmt.Sprintf("/benchmark/project-%02d", session%20), Model: fmt.Sprintf("model-%d", session%4), Tokens: model.Tokens{Input: int64(100 + j%13), Output: 20, CacheRead: 200, CacheWrite: 10, Reasoning: 5}}
			// Exercise populated per-request metadata without adding rollup dimensions.
			events[j].RequestID = fmt.Sprintf("req-%03d-%04d-0123456789abcdef", session, j)
			if j%80 == 0 {
				events[j].Boundary = model.BoundaryCompact
			}
			if j%100 == 0 {
				events[j].Bill = &model.Bill{Amount: 1.25, Unit: "credits"}
			}
			res[j] = store.Resolution{Provider: fmt.Sprintf("provider-%d", session%3), Attrib: model.AttribLog, Cost: .001, Priced: true}
		}
		if err = st.Commit(ctx, model.Codex, id, harness.Batch{Events: events}, res); err != nil {
			tb.Fatal(err)
		}
	}
	return NewService(st), start
}

type queryCase struct {
	name string
	run  func() error
}

func benchmarkCases(q Service, start time.Time) []queryCase {
	ctx := context.Background()
	f := Filter{}
	return []queryCase{
		{"Totals", func() error { _, e := q.Totals(ctx, f); return e }},
		{"Daily", func() error { _, e := q.Daily(ctx, f); return e }},
		{"Hourly24h", func() error {
			_, e := q.Hourly(ctx, Filter{Range: Range{From: start, To: start.AddDate(0, 0, 1)}})
			return e
		}},
		{"HourlyHarness24h", func() error {
			_, e := q.Hourly(ctx, Filter{Range: Range{From: start, To: start.AddDate(0, 0, 1)}, Harnesses: []model.Harness{model.Codex}})
			return e
		}},
		{"ByModel", func() error { _, e := q.ByModel(ctx, f); return e }},
		{"ByProvider", func() error { _, e := q.ByProvider(ctx, f); return e }},
		{"ByHarness", func() error { _, e := q.ByHarness(ctx, f); return e }},
		{"ByProject", func() error { _, e := q.ByProject(ctx, f); return e }},
		{"Sessions6", func() error { _, _, e := q.Sessions(ctx, f, "", 6, 0); return e }},
		{"SessionsAll", func() error { _, _, e := q.Sessions(ctx, f, "", 0, 0); return e }},
		{"Session", func() error { _, _, _, e := q.Session(ctx, model.Codex, "session-000"); return e }},
	}
}

func BenchmarkService(b *testing.B) {
	q, start := benchmarkFixture(b)
	for _, tc := range benchmarkCases(q, start) {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := tc.run(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Opt-in because scheduling noise makes hard deadlines unsuitable for shared CI.
// The allocation ceiling is a conservative upper bound on additional Go heap
// retained by one query (not a process-RSS assertion about the test runner).
func TestQueryPerformanceBudget(t *testing.T) {
	if os.Getenv("MYTOKEN_STRICT_PERF") == "" {
		t.Skip("set MYTOKEN_STRICT_PERF=1 for the 200K performance gate")
	}
	q, start := benchmarkFixture(t)
	for _, tc := range benchmarkCases(q, start) {
		t.Run(tc.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			at := time.Now()
			err := tc.run()
			elapsed := time.Since(at)
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatal(err)
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("%s %d allocated bytes", elapsed, allocated)
			if elapsed >= 100*time.Millisecond {
				t.Errorf("query exceeded 100ms: %s", elapsed)
			}
			if allocated >= 50<<20 {
				t.Errorf("query exceeded 50MiB allocation budget: %d", allocated)
			}
		})
	}
}
