package query

import (
	"context"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
	"testing"
)

func TestUnpricedAggregates(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	zero := 0.0
	events := []model.UsageEvent{{DedupKey: "unknown", SessionID: "child", ParentID: "parent", Model: "missing"}, {DedupKey: "free", SessionID: "parent", Model: "free", CostUSD: &zero}}
	if err = st.Commit(ctx, model.Codex, "source", harness.Batch{Events: events}, nil); err != nil {
		t.Fatal(err)
	}
	q := NewService(st)
	total, err := q.Totals(ctx, Filter{})
	if err != nil || total.Unpriced != 1 || total.Requests != 2 || total.CostUSD != 0 {
		t.Fatalf("total=%+v err=%v", total, err)
	}
	for _, buckets := range []func(context.Context, Filter) ([]Bucket, error){q.ByProvider, q.ByModel, q.ByProject, q.ByHarness} {
		rows, err := buckets(ctx, Filter{})
		if err != nil {
			t.Fatal(err)
		}
		var n int64
		for _, r := range rows {
			n += r.Unpriced
		}
		if n != 1 {
			t.Fatalf("unpriced=%d", n)
		}
	}
	row, children, own, err := q.Session(ctx, model.Codex, "parent")
	if err != nil || row.Unpriced != 1 || len(children) != 1 || children[0].Unpriced != 1 || len(own) != 1 || !own[0].Priced {
		t.Fatalf("row=%+v children=%+v own=%+v err=%v", row, children, own, err)
	}
	var n int64
	for _, b := range row.Breakdown {
		n += b.Unpriced
	}
	if n != 1 {
		t.Fatalf("breakdown unpriced=%d", n)
	}
}
