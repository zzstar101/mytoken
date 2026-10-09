package query

import (
	"context"
	"encoding/json"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
	"strings"
	"testing"
	"time"
)

func TestSessionSignalsDoNotConvertNativeBills(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	cost := 2.0
	events := []model.UsageEvent{
		{DedupKey: "a", SessionID: "s", Timestamp: time.Unix(1, 0), RequestID: "request-exact", Boundary: model.BoundaryCompact, Bill: &model.Bill{Amount: 20, Unit: "credits"}, CostUSD: &cost, Tokens: model.Tokens{Input: 10}},
		{DedupKey: "b", SessionID: "s", Timestamp: time.Unix(2, 0), Bill: &model.Bill{Amount: 0, Unit: "tool-units"}},
		{DedupKey: "c", SessionID: "s", Timestamp: time.Unix(3, 0)},
	}
	if err = st.Commit(ctx, model.Codex, "signals", harness.Batch{Events: events}, nil); err != nil {
		t.Fatal(err)
	}
	q := NewService(st)
	_, _, got, err := q.Session(ctx, model.Codex, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].RequestID != "request-exact" || got[0].Boundary != model.BoundaryCompact || got[0].Bill == nil || *got[0].Bill != *events[0].Bill {
		t.Fatalf("signals %+v", got)
	}
	if got[1].Bill == nil || got[1].Bill.Amount != 0 || got[1].Bill.Unit != "tool-units" || got[2].Bill != nil {
		t.Fatalf("zero/absent bills %+v", got)
	}
	total, err := q.Totals(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if total.CostUSD != 2 || total.CostLogUSD != 2 || total.Unpriced != 2 {
		t.Fatalf("native units altered USD: %+v", total)
	}
	raw, err := json.Marshal(got[2])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"requestId"`, `"boundary"`, `"bill"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("empty optional key %s: %s", key, raw)
		}
	}
}
