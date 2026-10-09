package store

import (
	"context"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/pricing"
	"testing"
	"time"
)

func TestRepricingUsesEventTime(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	cut := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	events := []model.UsageEvent{{DedupKey: "before", SessionID: "s", Model: "snapshot-model", Timestamp: cut.Add(-time.Hour), Tokens: model.Tokens{Input: 1_000_000}}, {DedupKey: "after", SessionID: "s", Model: "snapshot-model", Timestamp: cut.Add(time.Hour), Tokens: model.Tokens{Input: 1_000_000}}}
	if err = st.Commit(ctx, model.Codex, "source", harness.Batch{Events: events}, nil); err != nil {
		t.Fatal(err)
	}
	pricer := pricing.New(t.TempDir())
	one, three, zero := 1.0, 3.0, 0.0
	pricer.SetRules([]pricing.Rule{{Model: "snapshot-model", Input: &one, Output: &zero, CacheRead: &zero, CacheWrite: &zero}, {Model: "snapshot-model", Input: &three, Output: &zero, CacheRead: &zero, CacheWrite: &zero, From: cut}})
	if err = st.EnsurePrices(ctx, pricer.Fingerprint(), pricer.Evaluate); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]float64{"before": 1, "after": 3} {
		var got float64
		if err = st.DB().QueryRow(`SELECT cost FROM events WHERE dedup_key=?`, key).Scan(&got); err != nil || got != want {
			t.Fatalf("%s cost=%g want=%g err=%v", key, got, want, err)
		}
	}
	var total float64
	if err = st.DB().QueryRow(`SELECT sum(cost) FROM daily_usage`).Scan(&total); err != nil || total != 4 {
		t.Fatalf("rollup cost=%g err=%v", total, err)
	}
}
