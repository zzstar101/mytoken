package gemini

import (
	"os"
	"sort"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

// TestRealData parses whatever Gemini CLI history exists on this machine. It
// is gated behind MYTOKEN_REALDATA=1 because CI machines have no session
// history (and this machine only holds antigravity data, so it normally skips).
func TestRealData(t *testing.T) {
	if os.Getenv("MYTOKEN_REALDATA") != "1" {
		t.Skip("set MYTOKEN_REALDATA=1 to parse the local Gemini CLI data directory")
	}
	p := New()
	present := false
	for _, root := range p.Roots() {
		if fi, err := os.Stat(root); err == nil && fi.IsDir() {
			present = true
		}
	}
	if !present {
		t.Skipf("no Gemini CLI tmp directory under %v", p.Roots())
	}

	res := parseFrom(t, p, nil)
	if len(res.Events) == 0 && len(res.Sessions) == 0 {
		t.Skip("no Gemini CLI usage discovered")
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
		case ev.Harness != model.Gemini:
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
		if m.Harness != model.Gemini {
			t.Errorf("session %s harness = %q", m.SessionID, m.Harness)
		}
		if m.UpdatedAt.Before(m.StartedAt) {
			t.Errorf("session %s updated %v before started %v", m.SessionID, m.UpdatedAt, m.StartedAt)
		}
	}

	byModel := map[string]model.Tokens{}
	counts := map[string]int{}
	for _, ev := range folded {
		byModel[ev.Model] = byModel[ev.Model].Add(ev.Tokens)
		counts[ev.Model]++
	}
	keys := make([]string, 0, len(byModel))
	for k := range byModel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		tok := byModel[k]
		t.Logf("%-30s events=%-5d input=%-9d output=%-7d cacheRead=%-8d cacheWrite=%-7d reasoning=%-6d",
			k, counts[k], tok.Input, tok.Output, tok.CacheRead, tok.CacheWrite, tok.Reasoning)
	}
	for i, id := range sortedSessionIDs(sessions) {
		if i >= 10 {
			break
		}
		m := sessions[id]
		t.Logf("session %s parent=%q project=%q title=%q started=%s",
			m.SessionID, m.ParentID, m.Project, m.Title, m.StartedAt.UTC().Format(time.RFC3339))
	}
}

func sortedSessionIDs(sessions map[string]model.SessionMeta) []string {
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
