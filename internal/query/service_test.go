package query

import (
	"context"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
	"path/filepath"
	"testing"
	"time"
)

func TestAggregatesRollupAndZeroFill(t *testing.T) {
	st, e := store.Open(filepath.Join(t.TempDir(), "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	ctx := context.Background()
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.Local)
	events := []model.UsageEvent{{Harness: model.Codex, DedupKey: "p", SessionID: "parent", Timestamp: day.Add(time.Hour), Model: "gpt-5", Tokens: model.Tokens{Input: 100, CacheRead: 50, CacheWrite: 50}}, {Harness: model.Codex, DedupKey: "c", SessionID: "child", ParentID: "parent", Timestamp: day.Add(3 * time.Hour), Model: "gpt-5", Tokens: model.Tokens{Output: 20}}, {Harness: model.Codex, DedupKey: "other", SessionID: "other", Timestamp: day.Add(30 * time.Hour), Model: "o3", Tokens: model.Tokens{Input: 10}}}
	if e = st.Commit(ctx, model.Codex, "test", harness.Batch{Events: events}, []store.Resolution{{Provider: "a", Attrib: model.AttribLog, Cost: 1}, {Provider: "b", Attrib: model.AttribCCSwitch, Cost: 2}, {Provider: "a", Cost: 3}}); e != nil {
		t.Fatal(e)
	}
	q := NewService(st)
	f := Filter{Range: Range{From: day, To: day.AddDate(0, 0, 3)}}
	tot, e := q.Totals(ctx, f)
	if e != nil || tot.Requests != 3 || tot.Sessions != 2 || tot.Tokens.Total() != 230 || tot.CacheHit != float64(50)/210 {
		t.Fatalf("totals %+v %v", tot, e)
	}
	daily, e := q.Daily(ctx, f)
	if e != nil || len(daily) != 3 || daily[2].Tokens.Total() != 0 {
		t.Fatalf("daily %+v %v", daily, e)
	}
	hour, e := q.Hourly(ctx, Filter{Range: Range{From: day, To: day.Add(24 * time.Hour)}})
	if e != nil || len(hour) != 24 || hour[1].Tokens.Total() != 200 {
		t.Fatalf("hourly %d %v", len(hour), e)
	}
	rows, n, e := q.Sessions(ctx, f, SortTokens, 10, 0)
	if e != nil || n != 2 || rows[0].SessionID != "parent" || rows[0].Tokens.Total() != 220 || rows[0].Children != 1 || len(rows[0].Breakdown) != 2 {
		t.Fatalf("sessions %+v %d %v", rows, n, e)
	}
	parent, children, own, e := q.Session(ctx, model.Codex, "parent")
	if e != nil || parent.Requests != 2 || len(children) != 1 || len(own) != 1 {
		t.Fatalf("detail %+v %+v %+v %v", parent, children, own, e)
	}
	p, e := q.ByProvider(ctx, f)
	if e != nil || len(p) != 2 || p[0].Key != "a" || p[0].Sessions != 2 {
		t.Fatalf("providers %+v %v", p, e)
	}
	filtered, n, e := q.Sessions(ctx, Filter{Providers: []string{"b"}}, SortRecent, 10, 0)
	if e != nil || n != 1 || filtered[0].Requests != 1 || filtered[0].SessionID != "parent" {
		t.Fatalf("filtered %+v %v", filtered, e)
	}
}
