package cline

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness/clinetask"
	"github.com/zzstar101/mytoken/internal/model"
)

// TestRealData runs Discover+Parse over the real Cline tree — the VS Code
// family globalStorage tasks directories plus the standalone CLI's ~/.cline/data
// tree — and reports the totals per provider/model. It is gated by
// MYTOKEN_REALDATA=1 because it reads the developer's home directory, and it
// never prints message content: only counts, ids and titles.
//
//	MYTOKEN_REALDATA=1 go test -run TestRealData -v ./internal/harness/cline/
func TestRealData(t *testing.T) {
	if os.Getenv("MYTOKEN_REALDATA") != "1" {
		t.Skip("set MYTOKEN_REALDATA=1 to run against the real Cline tree")
	}
	p := New()
	if len(p.Roots()) == 0 {
		t.Fatal("no roots configured")
	}
	present := false
	for _, r := range p.Roots() {
		if _, err := os.Stat(r); err == nil {
			present = true
			break
		}
	}
	if !present {
		t.Skip("no real Cline root is present on this machine")
	}

	sum, err := clinetask.SweepRealData(context.Background(), p)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	logRealData(t, sum)

	if len(sum.Sessions) == 0 {
		t.Log("no Cline tasks found; nothing to assert beyond the sweep")
		return
	}
	assertRealData(t, sum)
}

func logRealData(t *testing.T, s *clinetask.RealDataSummary) {
	t.Helper()
	t.Logf("roots: %v", s.Roots)
	t.Logf("files: %d, sessions: %d (%d with a parent)", s.Files, len(s.Sessions), s.SessionsWithParent())
	t.Logf("events: %d raw, %d deduplicated", len(s.Events), len(s.Deduped))
	t.Logf("discover: %s, parse: %s, total: %s", s.DiscoverDur.Round(time.Millisecond),
		s.ParseDur.Round(time.Millisecond), s.TotalDur.Round(time.Millisecond))
	t.Logf("%-24s %-30s %6s %12s %12s %12s %12s %12s %10s", "PROVIDER", "MODEL", "REQS",
		"INPUT", "OUTPUT", "CACHEREAD", "CACHEWRITE", "REASONING", "COST")
	for _, r := range s.Rows() {
		t.Logf("%-24s %-30s %6d %12d %12d %12d %12d %12d %10.4f", r.Provider, r.Model,
			r.Requests, r.Tokens.Input, r.Tokens.Output, r.Tokens.CacheRead,
			r.Tokens.CacheWrite, r.Tokens.Reasoning, r.CostUSD)
	}
	t.Logf("TOTAL input=%d output=%d cacheRead=%d cacheWrite=%d reasoning=%d cost=%.4f",
		s.TotalTokens.Input, s.TotalTokens.Output, s.TotalTokens.CacheRead,
		s.TotalTokens.CacheWrite, s.TotalTokens.Reasoning, s.TotalCostUSD)
}

func assertRealData(t *testing.T, s *clinetask.RealDataSummary) {
	t.Helper()
	for _, e := range s.Events {
		if e.SessionID == "" || e.DedupKey == "" || e.Timestamp.IsZero() {
			t.Fatalf("malformed event: %+v", e)
		}
		if e.Harness != model.Cline {
			t.Fatalf("event from another harness: %+v", e)
		}
	}
	known := map[string]bool{}
	for _, sess := range s.Sessions {
		known[sess.SessionID] = true
	}
	for _, sess := range s.Sessions {
		if sess.ParentID != "" && !known[sess.ParentID] {
			t.Errorf("session %s points at unknown parent %s", sess.SessionID, sess.ParentID)
		}
	}
	for _, sess := range s.Sessions {
		if sess.UpdatedAt.Before(sess.StartedAt) {
			t.Errorf("session %s: UpdatedAt %s before StartedAt %s", sess.SessionID, sess.UpdatedAt, sess.StartedAt)
		}
	}
}
