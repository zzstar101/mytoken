package query

import (
	"context"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

func TestDisplayTimezoneNeverRewritesRollups(t *testing.T) {
	old := time.Local
	defer func() { time.Local = old }()
	time.Local = time.UTC
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	at := time.Date(2026, 1, 1, 22, 0, 0, 0, time.UTC)
	if err = st.Commit(ctx, model.Codex, "source", harness.Batch{Events: []model.UsageEvent{{DedupKey: "timezone", SessionID: "s", Timestamp: at, Tokens: model.Tokens{Input: 7}}}}, nil); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err = st.DB().QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	time.Local = time.FixedZone("display-only", 19800)
	points, err := NewService(st).Daily(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Day.In(time.Local).Day() != 2 || points[0].Tokens.Input != 7 {
		t.Fatalf("display day: %+v", points)
	}
	var after int64
	if err = st.DB().QueryRow(`SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("display timezone wrote database: %d→%d", before, after)
	}
	var day int64
	if err = st.DB().QueryRow(`SELECT day FROM daily_usage`).Scan(&day); err != nil {
		t.Fatal(err)
	}
	if day != time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano() {
		t.Fatalf("rollup day mutated: %d", day)
	}
}

func TestAggregateBoundariesAndProvenance(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.Local)
	cost := 3.0
	events := []model.UsageEvent{{DedupKey: "a", SessionID: "s", Timestamp: day.Add(time.Hour), CostUSD: &cost}, {DedupKey: "b", SessionID: "s", Timestamp: day.Add(12 * time.Hour)}, {DedupKey: "c", SessionID: "s", Timestamp: day.Add(25 * time.Hour)}}
	if err = st.Commit(ctx, model.Codex, "source", harness.Batch{Events: events}, []store.Resolution{{}, {Cost: 2, Priced: true}, {}}); err != nil {
		t.Fatal(err)
	}
	q := NewService(st)
	all, err := q.Totals(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if all.CostLogUSD != 3 || all.CostEstimateUSD != 2 || all.CostUSD != 5 || all.Unpriced != 1 {
		t.Fatalf("sources: %+v", all)
	}
	for _, tt := range []struct {
		from, to time.Time
		requests int64
	}{{day.Add(2 * time.Hour), day.Add(26 * time.Hour), 2}, {day.Add(2 * time.Hour), day.Add(13 * time.Hour), 1}, {day, day.Add(12 * time.Hour), 1}, {day.Add(time.Hour), day.Add(time.Hour), 0}} {
		got, err := q.Totals(ctx, Filter{Range: Range{From: tt.from, To: tt.to}})
		if err != nil || got.Requests != tt.requests {
			t.Fatalf("boundary %+v: %+v %v", tt, got, err)
		}
	}
	_, _, own, err := q.Session(ctx, model.Codex, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 3 || own[0].CostSource != CostSourceLog || own[1].CostSource != CostSourceEstimate || own[2].CostSource != CostSourceUnpriced {
		t.Fatalf("events: %+v", own)
	}
}
