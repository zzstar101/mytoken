package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// Compare the identical current write path with the legacy narrow time index
// and the covering time index. No source logs or application data are used.
func BenchmarkCommitManyIndex(b *testing.B) {
	for _, cover := range []bool{false, true} {
		name := "narrow"
		if cover {
			name = "covering"
		}
		b.Run(name, func(b *testing.B) {
			st, err := Open(filepath.Join(b.TempDir(), "bench.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			ctx := context.Background()
			if !cover {
				if err = st.Exec(ctx, `DROP INDEX events_time;CREATE INDEX events_time ON event_data(timestamp)`); err != nil {
					b.Fatal(err)
				}
			}
			writes := make([]Write, 20)
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for n := range writes {
				w := &writes[n]
				w.Harness = model.Codex
				w.Path = fmt.Sprint(n)
				w.Batch = harness.Batch{Events: make([]model.UsageEvent, 500)}
				w.Resolutions = make([]Resolution, 500)
				for j := range w.Batch.Events {
					i := n*500 + j
					w.Batch.Events[j] = model.UsageEvent{SessionID: fmt.Sprintf("session-%02d", n), Timestamp: at.Add(time.Duration(i) * time.Second), Model: "gpt-4o", Tokens: model.Tokens{Input: 1000, Output: 200, CacheRead: 500, CacheWrite: 10}}
					w.Resolutions[j] = Resolution{Provider: "proxy", Priced: true, Cost: .01}
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				b.StopTimer()
				for n := range writes {
					for j := range writes[n].Batch.Events {
						e := &writes[n].Batch.Events[j]
						e.DedupKey = fmt.Sprintf("event-%08d-%02d-%03d", iteration, n, j)
						e.RequestID = fmt.Sprintf("upstream-%08d-%02d-%03d", iteration, n, j)
					}
				}
				b.StartTimer()
				if err = st.CommitMany(ctx, writes); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
