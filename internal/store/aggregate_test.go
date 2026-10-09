package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

func TestDailyAggregateConsistency(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(42))
	events := make([]model.UsageEvent, 500)
	resolutions := make([]Resolution, len(events))
	for i := range events {
		events[i] = model.UsageEvent{DedupKey: fmt.Sprint(i), SessionID: fmt.Sprint(i % 7), Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(rng.Intn(10000)) * time.Minute), Model: fmt.Sprint(i % 3), Tokens: model.Tokens{Input: int64(rng.Intn(100)), Output: int64(rng.Intn(40)), CacheRead: int64(rng.Intn(20)), CacheWrite: int64(rng.Intn(10)), Reasoning: int64(rng.Intn(5))}}
		resolutions[i] = Resolution{Provider: "test", Cost: float64(i%5) * 0.1, Priced: i%5 != 0}
		if i%11 == 0 {
			c := 0.2
			events[i].CostUSD = &c
		}
	}
	check := func() {
		t.Helper()
		assertDailyRows(t, st)
		const cols = `coalesce(sum(input),0),coalesce(sum(output),0),coalesce(sum(cache_read),0),coalesce(sum(cache_write),0),coalesce(sum(reasoning),0)`
		var a, b [8]float64
		if err := st.DB().QueryRow(`SELECT `+cols+`,coalesce(sum(CASE WHEN priced THEN cost ELSE 0 END),0),count(*),coalesce(sum(NOT priced),0) FROM events`).Scan(&a[0], &a[1], &a[2], &a[3], &a[4], &a[5], &a[6], &a[7]); err != nil {
			t.Fatal(err)
		}
		if err := st.DB().QueryRow(`SELECT `+cols+`,coalesce(sum(cost),0),coalesce(sum(requests),0),coalesce(sum(unpriced),0) FROM daily_usage`).Scan(&b[0], &b[1], &b[2], &b[3], &b[4], &b[5], &b[6], &b[7]); err != nil {
			t.Fatal(err)
		}
		for i := range a {
			if math.Abs(a[i]-b[i]) > 1e-8 {
				t.Fatalf("aggregate[%d]: events=%v summary=%v", i, a, b)
			}
		}
	}
	if err = st.Commit(ctx, model.Codex, "source", harness.Batch{Events: events}, resolutions); err != nil {
		t.Fatal(err)
	}
	check()
	events[0].Timestamp = events[0].Timestamp.Add(48 * time.Hour)
	events[0].Tokens.Input = 999
	if err = st.Commit(ctx, model.Codex, "source", harness.Batch{Events: events[:1]}, resolutions[:1]); err != nil {
		t.Fatal(err)
	}
	check()
	if err = st.RecomputePrices(ctx, func(e model.UsageEvent) (float64, bool) { return float64(e.Tokens.Input) * 0.01, true }); err != nil {
		t.Fatal(err)
	}
	check()
	if err = st.Exec(ctx, `DELETE FROM events WHERE CAST(dedup_key AS INTEGER)%3=0`); err != nil {
		t.Fatal(err)
	}
	check()
	if err = st.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	check()
}

// Compare every dimension bucket against an independent, row-at-a-time Go sum.
func assertDailyRows(t *testing.T, st *Store) {
	t.Helper()
	type key struct {
		day  int64
		dims [6]string
	}
	expected := map[key][10]float64{}
	rows, err := st.DB().Query(`SELECT timestamp,harness,session_id,resolved_provider,model,project,attrib,input,output,cache_read,cache_write,reasoning,cost,log_cost,priced FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k key
		var timestamp int64
		var v [10]float64
		var cost float64
		var log sql.NullFloat64
		var priced bool
		if err = rows.Scan(&timestamp, &k.dims[0], &k.dims[1], &k.dims[2], &k.dims[3], &k.dims[4], &k.dims[5], &v[0], &v[1], &v[2], &v[3], &v[4], &cost, &log, &priced); err != nil {
			t.Fatal(err)
		}
		local := time.Unix(0, timestamp).In(time.Local)
		k.day = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local).UnixNano()
		if priced {
			v[5] = cost
		} else {
			v[9] = 1
		}
		if log.Valid {
			v[6] = cost
		} else if priced {
			v[7] = cost
		}
		v[8] = 1
		sum := expected[k]
		for i := range sum {
			sum[i] += v[i]
		}
		expected[k] = sum
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	rows, err = st.DB().Query(`SELECT ` + dailyDimensions + `,` + dailyMetrics + ` FROM daily_usage`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k key
		var actual [10]float64
		if err = rows.Scan(&k.day, &k.dims[0], &k.dims[1], &k.dims[2], &k.dims[3], &k.dims[4], &k.dims[5], &actual[0], &actual[1], &actual[2], &actual[3], &actual[4], &actual[5], &actual[6], &actual[7], &actual[8], &actual[9]); err != nil {
			t.Fatal(err)
		}
		want, ok := expected[k]
		if !ok {
			t.Fatalf("unexpected rollup %+v: %v", k, actual)
		}
		for i := range actual {
			if math.Abs(actual[i]-want[i]) > 1e-8 {
				t.Fatalf("rollup %+v metric %d got %g want %g", k, i, actual[i], want[i])
			}
		}
		delete(expected, k)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(expected) != 0 {
		t.Fatalf("missing %d rollup groups", len(expected))
	}
}

func TestRebuildPreservesMissingSourceHistory(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, key := range []string{"deleted-source", "present-source"} {
		if err = st.Commit(ctx, model.Codex, key, harness.Batch{Events: []model.UsageEvent{{DedupKey: key, SessionID: key, Tokens: model.Tokens{Input: 10}}}, Next: harness.Cursor{Offset: 12}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err = st.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if err = st.Commit(ctx, model.Codex, "present-source", harness.Batch{Events: []model.UsageEvent{{DedupKey: "present-source", SessionID: "present-source", Tokens: model.Tokens{Input: 20}}}}, nil); err != nil {
		t.Fatal(err)
	}
	var count, input int
	if err = st.DB().QueryRow(`SELECT count(*),sum(input) FROM events`).Scan(&count, &input); err != nil {
		t.Fatal(err)
	}
	if count != 2 || input != 30 {
		t.Fatalf("history lost: count=%d input=%d", count, input)
	}
	c, err := st.Cursor(ctx, model.Codex, "deleted-source")
	if err != nil || c.Offset != 0 {
		t.Fatalf("cursor not reset: %+v %v", c, err)
	}
}

func TestDailyAggregatesTimezoneChange(t *testing.T) {
	old := time.Local
	defer func() { time.Local = old }()
	time.Local = time.UTC
	path := t.TempDir() + "/db"
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 23, 30, 0, 0, time.UTC)
	if err = st.Commit(context.Background(), model.Codex, "source", harness.Batch{Events: []model.UsageEvent{{DedupKey: "x", SessionID: "s", Timestamp: at}}}, nil); err != nil {
		t.Fatal(err)
	}
	st.Close()
	time.Local = time.FixedZone("offset", 19800)
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var day int64
	if err = st.DB().QueryRow(`SELECT day FROM daily_usage`).Scan(&day); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 1, 2, 0, 0, 0, 0, time.Local).UnixNano()
	if day != want {
		t.Fatalf("day=%d want=%d", day, want)
	}
}
