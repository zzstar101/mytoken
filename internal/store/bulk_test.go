package store

import (
	"context"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

func TestDeferredIndexesRestoreAfterCancellation(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	count := func(want int) {
		t.Helper()
		var n int
		if err := st.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name IN ('events_time','events_session','events_provider_time','events_model_time')").Scan(&n); err != nil || n != want {
			t.Fatalf("indexes=%d want=%d err=%v", n, want, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restore, err := st.DeferEmptyIndexes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count(0)
	batch := harness.Batch{Events: []model.UsageEvent{{SessionID: "s", DedupKey: "request", Tokens: model.Tokens{Input: 10}}}}
	for i := 0; i < 2; i++ {
		if err := st.Commit(ctx, model.Codex, "source", batch, nil); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := st.DB().QueryRow("SELECT count(*) FROM events").Scan(&n); err != nil || n != 1 {
		t.Fatalf("deduplication: count=%d err=%v", n, err)
	}
	cancel()
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	count(2)
	var cacheSize int
	if err := st.DB().QueryRow("PRAGMA cache_size").Scan(&cacheSize); err != nil || cacheSize != -8192 {
		t.Fatalf("cache_size=%d err=%v", cacheSize, err)
	}
	// Incremental scans must keep every query index available.
	restore, err = st.DeferEmptyIndexes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	count(2)
	if err := restore(); err != nil {
		t.Fatal(err)
	}
}
