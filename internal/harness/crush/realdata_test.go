package crush

import (
	"os"
	"sort"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

// TestRealData parses whatever Crush databases exist on this machine. It is
// gated behind MYTOKEN_REALDATA=1 because CI machines have no session history.
func TestRealData(t *testing.T) {
	if os.Getenv("MYTOKEN_REALDATA") != "1" {
		t.Skip("set MYTOKEN_REALDATA=1 to parse the local Crush data directory")
	}
	p := New()
	present := false
	for _, root := range p.Roots() {
		if fi, err := os.Stat(root); err == nil && fi.IsDir() {
			present = true
		}
	}
	if !present {
		t.Skipf("no Crush data directory under %v", p.Roots())
	}

	res := parseFrom(t, p, nil)
	if len(res.Events) == 0 && len(res.Sessions) == 0 {
		t.Skip("no Crush usage discovered")
	}
	t.Logf("sources: %d, events: %d, sessions: %d", len(res.Cursors), len(res.Events), len(res.Sessions))

	folded := map[string]model.UsageEvent{}
	for _, ev := range res.Events {
		folded[ev.DedupKey] = ev
	}
	if len(folded) != len(res.Events) {
		t.Errorf("%d events collapsed to %d dedup keys", len(res.Events), len(folded))
	}

	for _, ev := range res.Events {
		switch {
		case ev.SessionID == "":
			t.Errorf("event %s has no session id", ev.DedupKey)
		case ev.DedupKey == "":
			t.Errorf("event in session %s has no dedup key", ev.SessionID)
		case ev.Timestamp.IsZero():
			t.Errorf("event %s has no timestamp", ev.DedupKey)
		case ev.Harness != model.Crush:
			t.Errorf("event harness = %q", ev.Harness)
		}
	}

	sessions := map[string]model.SessionMeta{}
	for _, m := range res.Sessions {
		sessions[m.SessionID] = m
	}
	for _, ev := range res.Events {
		if ev.ParentID != "" {
			if _, ok := sessions[ev.ParentID]; !ok {
				t.Errorf("event %s references unknown parent %q", ev.DedupKey, ev.ParentID)
			}
		}
	}
	for _, m := range res.Sessions {
		if m.Harness != model.Crush {
			t.Errorf("session %s harness = %q", m.SessionID, m.Harness)
		}
		if m.UpdatedAt.Before(m.StartedAt) {
			t.Errorf("session %s updated %v before started %v", m.SessionID, m.UpdatedAt, m.StartedAt)
		}
	}

	type agg struct {
		events int
		tokens model.Tokens
		cost   float64
	}
	byModel := map[string]*agg{}
	for _, ev := range folded {
		key := ev.Provider + "/" + ev.Model
		a := byModel[key]
		if a == nil {
			a = &agg{}
			byModel[key] = a
		}
		a.events++
		a.tokens = a.tokens.Add(ev.Tokens)
		if ev.CostUSD != nil {
			a.cost += *ev.CostUSD
		}
	}
	keys := make([]string, 0, len(byModel))
	for k := range byModel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := byModel[k]
		t.Logf("%-30s events=%-5d input=%-9d output=%-7d cost=%.4f",
			k, a.events, a.tokens.Input, a.tokens.Output, a.cost)
	}
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i, id := range ids {
		if i >= 10 {
			break
		}
		m := sessions[id]
		t.Logf("session %s parent=%q project=%q title=%q started=%s",
			m.SessionID, m.ParentID, m.Project, m.Title, m.StartedAt.UTC().Format(time.RFC3339))
	}
}
