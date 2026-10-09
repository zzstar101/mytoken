package pi

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// TestRealData runs Discover+Parse over the real Pi session tree and reports
// the totals per provider/model. It is gated by MYTOKEN_REALDATA=1 because it
// reads the developer's home directory, and it never prints message content —
// only counts and session titles.
//
//	MYTOKEN_REALDATA=1 go test -run TestRealData -v ./internal/harness/pi/
func TestRealData(t *testing.T) {
	if os.Getenv("MYTOKEN_REALDATA") != "1" {
		t.Skip("set MYTOKEN_REALDATA=1 to run against the real session tree")
	}
	p := New()
	roots := p.Roots()
	if len(roots) == 0 {
		t.Fatal("no roots configured")
	}
	if _, err := os.Stat(roots[0]); err != nil {
		t.Skipf("real root %s is not present: %v", roots[0], err)
	}

	ctx := context.Background()
	start := time.Now()
	srcs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	discoverDur := time.Since(start)

	var (
		events   []model.UsageEvent
		sessions []model.SessionMeta
		files    int
	)
	parseStart := time.Now()
	for _, src := range srcs {
		files++
		b, err := p.Parse(ctx, src, harness.Cursor{})
		if err != nil {
			t.Errorf("Parse(%s): %v", src.Path, err)
			continue
		}
		events = append(events, b.Events...)
		sessions = append(sessions, b.Sessions...)
	}
	parseDur := time.Since(parseStart)

	// Deduplicate exactly like the store does: last copy wins.
	last := map[string]model.UsageEvent{}
	order := make([]string, 0, len(events))
	for _, e := range events {
		if _, ok := last[e.DedupKey]; !ok {
			order = append(order, e.DedupKey)
		}
		last[e.DedupKey] = e
	}

	type key struct{ provider, model string }
	agg := map[key]struct {
		n    int
		toks model.Tokens
		cost float64
	}{}
	var total model.Tokens
	var totalCost float64
	for _, k := range order {
		e := last[k]
		kk := key{e.Provider, e.Model}
		a := agg[kk]
		a.n++
		a.toks = a.toks.Add(e.Tokens)
		if e.CostUSD != nil {
			a.cost += *e.CostUSD
			totalCost += *e.CostUSD
		}
		agg[kk] = a
		total = total.Add(e.Tokens)
	}

	keys := make([]key, 0, len(agg))
	for k := range agg {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].provider != keys[j].provider {
			return keys[i].provider < keys[j].provider
		}
		return keys[i].model < keys[j].model
	})

	var withParent int
	for _, s := range sessions {
		if s.ParentID != "" {
			withParent++
		}
	}

	t.Logf("roots: %v", roots)
	t.Logf("files: %d, sessions: %d (%d with a parent)", files, len(sessions), withParent)
	t.Logf("events: %d raw, %d deduplicated", len(events), len(order))
	t.Logf("discover: %s, parse: %s, total: %s", discoverDur.Round(time.Millisecond),
		parseDur.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
	t.Logf("%-20s %-28s %8s %12s %12s %12s %12s %12s", "PROVIDER", "MODEL", "REQS",
		"INPUT", "OUTPUT", "CACHEREAD", "CACHEWRITE", "REASONING")
	for _, k := range keys {
		a := agg[k]
		t.Logf("%-20s %-28s %8d %12d %12d %12d %12d %12d", k.provider, k.model, a.n,
			a.toks.Input, a.toks.Output, a.toks.CacheRead, a.toks.CacheWrite, a.toks.Reasoning)
	}
	t.Logf("TOTAL input=%d output=%d cacheRead=%d cacheWrite=%d reasoning=%d cost=%.4f",
		total.Input, total.Output, total.CacheRead, total.CacheWrite, total.Reasoning, totalCost)

	if len(sessions) == 0 {
		t.Errorf("no sessions parsed from the real tree")
	}
	// Sanity: every event must carry a session id, a timestamp and a dedup key.
	for _, e := range events {
		if e.SessionID == "" || e.DedupKey == "" || e.Timestamp.IsZero() {
			t.Fatalf("malformed event: %+v", e)
		}
	}
	// The parent of a fork or a subagent run must itself be a known session id.
	known := map[string]bool{}
	for _, s := range sessions {
		known[s.SessionID] = true
	}
	for _, s := range sessions {
		if s.ParentID != "" && !known[s.ParentID] {
			t.Errorf("session %s points at unknown parent %s", s.SessionID, s.ParentID)
		}
	}
	// Every session must have a project path and a sane time range.
	for _, s := range sessions {
		if s.Project == "" {
			t.Errorf("session %s has no project", s.SessionID)
		}
		if s.UpdatedAt.Before(s.StartedAt) {
			t.Errorf("session %s: UpdatedAt %s before StartedAt %s", s.SessionID, s.UpdatedAt, s.StartedAt)
		}
	}
}
