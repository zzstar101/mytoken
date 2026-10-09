package store

import (
	"context"
	"fmt"
	"github.com/zzstar/mytoken/internal/harness"
	"github.com/zzstar/mytoken/internal/model"
	"path/filepath"
	"testing"
	"time"
)

func TestRecomputeCosts(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	zero := 0.0
	events := []model.UsageEvent{}
	for i := 0; i < 1100; i++ {
		events = append(events, model.UsageEvent{DedupKey: fmt.Sprint(i), SessionID: "s", Model: "known", Tokens: model.Tokens{Input: 1000000}})
	}
	events = append(events, model.UsageEvent{DedupKey: "log", SessionID: "s", Model: "unknown", CostUSD: &zero}, model.UsageEvent{DedupKey: "unknown", SessionID: "s", Model: "unknown"})
	if err = st.Commit(ctx, model.Codex, "s", harness.Batch{Events: events}, nil); err != nil {
		t.Fatal(err)
	}
	ch, cancel := st.Subscribe()
	defer cancel()
	if err = st.RecomputeCosts(ctx, func(e model.UsageEvent) float64 {
		if e.Model == "known" {
			return 2
		}
		return 0
	}); err != nil {
		t.Fatal(err)
	}
	var sum float64
	if err = st.DB().QueryRow("SELECT sum(cost) FROM events").Scan(&sum); err != nil || sum != 2200 {
		t.Fatalf("sum=%v err=%v", sum, err)
	}
	select {
	case <-ch:
	default:
		t.Fatal("no notification")
	}
	models, err := st.UnpricedModels(ctx, time.Time{}, func(provider, name string) bool { return name == "known" })
	if err != nil || len(models) != 1 || models[0] != "unknown" {
		t.Fatalf("%v %v", models, err)
	}
	if err = st.RecomputeCosts(ctx, func(e model.UsageEvent) float64 { return 3 }); err != nil {
		t.Fatal(err)
	}
	var cost float64
	if err = st.DB().QueryRow("SELECT cost FROM events WHERE dedup_key='log'").Scan(&cost); err != nil || cost != 0 {
		t.Fatalf("log overwritten: %v %v", cost, err)
	}
}

func TestReplayKeepsRootOwner(t *testing.T) {
	for _, childFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(childFirst), func(t *testing.T) {
			st, err := Open(filepath.Join(t.TempDir(), "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			parent := model.UsageEvent{Harness: model.Codex, DedupKey: "shared", SessionID: "parent", Tokens: model.Tokens{Input: 10}}
			child := parent
			child.SessionID = "child"
			child.ParentID = "parent"
			child.Tokens.Input = 99
			events := []model.UsageEvent{parent, child}
			if childFirst {
				events[0], events[1] = child, parent
			}
			for _, e := range events {
				if err := st.Commit(context.Background(), model.Codex, e.SessionID, harness.Batch{Events: []model.UsageEvent{e}}, nil); err != nil {
					t.Fatal(err)
				}
			}
			var owner string
			var input int
			if err := st.DB().QueryRow("SELECT session_id,input FROM events WHERE dedup_key='shared'").Scan(&owner, &input); err != nil {
				t.Fatal(err)
			}
			if owner != "parent" || input != 10 {
				t.Fatalf("owner=%s input=%d", owner, input)
			}
		})
	}
}

func TestCommitManyAtomic(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	writes := []Write{{Harness: model.Codex, Path: "first", Batch: harness.Batch{Events: []model.UsageEvent{{DedupKey: "valid", SessionID: "parent"}}, Next: harness.Cursor{Offset: 1}}}, {Harness: model.Codex, Path: "second", Batch: harness.Batch{Events: []model.UsageEvent{{SessionID: "child"}}}}}
	for i := 1; i < 129; i++ {
		writes[0].Batch.Events = append(writes[0].Batch.Events, model.UsageEvent{DedupKey: fmt.Sprint(i), SessionID: "parent"})
	}
	if err := st.CommitMany(context.Background(), writes); err == nil {
		t.Fatal("invalid group accepted")
	}
	var events, cursors int
	if err := st.DB().QueryRow("SELECT (SELECT count(*) FROM events),(SELECT count(*) FROM cursors)").Scan(&events, &cursors); err != nil {
		t.Fatal(err)
	}
	if events != 0 || cursors != 0 {
		t.Fatalf("partial group: events=%d cursors=%d", events, cursors)
	}
	writes[1].Batch.Events[0].DedupKey = "other"
	if err := st.CommitMany(context.Background(), writes); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow("SELECT (SELECT count(*) FROM events),(SELECT count(*) FROM cursors)").Scan(&events, &cursors); err != nil {
		t.Fatal(err)
	}
	if events != 130 || cursors != 2 {
		t.Fatalf("events=%d cursors=%d", events, cursors)
	}
}

func TestBatchDedupCursorAndRebuild(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	e := model.UsageEvent{Harness: model.Codex, DedupKey: "request", SessionID: "s", Timestamp: now, Tokens: model.Tokens{Input: 10}}
	b := harness.Batch{Events: []model.UsageEvent{e}, Next: harness.Cursor{Offset: 99, Extra: "state"}}
	if err = st.Commit(ctx, model.Codex, "source", b, []Resolution{{Provider: "openai", Attrib: model.AttribInferred, Cost: 0.1}}); err != nil {
		t.Fatal(err)
	}
	b.Events[0].Tokens.Input = 20
	if err = st.Commit(ctx, model.Codex, "source", b, nil); err != nil {
		t.Fatal(err)
	}
	var count, input int
	if err = st.DB().QueryRow("SELECT count(*),sum(input) FROM events").Scan(&count, &input); err != nil {
		t.Fatal(err)
	}
	if count != 1 || input != 20 {
		t.Fatalf("count=%d input=%d", count, input)
	}
	c, err := st.Cursor(ctx, model.Codex, "source")
	if err != nil || c.Offset != 99 || c.Extra != "state" {
		t.Fatalf("cursor %+v %v", c, err)
	}
	if err = st.SetSetting(ctx, "keep", "yes"); err != nil {
		t.Fatal(err)
	}
	if err = st.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if err = st.DB().QueryRow("SELECT count(*) FROM events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rebuild %d %v", count, err)
	}
	v, err := st.Setting(ctx, "keep")
	if err != nil || v != "yes" {
		t.Fatalf("setting %q %v", v, err)
	}
}

func TestBatchAtomicAndNotification(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	ch, cancel := st.Subscribe()
	defer cancel()
	err = st.Commit(ctx, model.Codex, "s", harness.Batch{Events: []model.UsageEvent{{Harness: model.Codex, DedupKey: "a", SessionID: "s", Timestamp: time.Now()}, {Harness: model.Codex, SessionID: "s"}}}, nil)
	if err == nil {
		t.Fatal("missing dedup key accepted")
	}
	var n int
	st.DB().QueryRow("SELECT count(*) FROM events").Scan(&n)
	if n != 0 {
		t.Fatal("partial transaction")
	}
	if err = st.Commit(ctx, model.Codex, "s", harness.Batch{}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no commit notification")
	}
}
