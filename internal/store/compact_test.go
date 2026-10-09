package store

import (
	"context"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"testing"
)

func TestCompactEventsRetainExactFields(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	e := model.UsageEvent{DedupKey: "codex:very-long-lossless-dedup", SessionID: "session-id", ParentID: "parent-id", ProjectPath: "/missing/project", Model: "raw-model", Provider: "log-provider", BaseURL: "https://provider.example/v1", Tokens: model.Tokens{Input: 123}}
	if err = st.Commit(ctx, model.Codex, "source", harness.Batch{Events: []model.UsageEvent{e}}, []Resolution{{Provider: "resolved", Attrib: model.AttribLog, Cost: 2}}); err != nil {
		t.Fatal(err)
	}
	var dimensions int
	if err = st.DB().QueryRow(`SELECT count(*) FROM event_dimensions`).Scan(&dimensions); err != nil {
		t.Fatal(err)
	}
	if dimensions != 1 {
		t.Fatalf("dimensions %d", dimensions)
	}
	var key, session, parent, project, mod, provider, base, resolved string
	if err = st.DB().QueryRow(`SELECT dedup_key,session_id,parent_id,raw_project,model,provider,base_url,resolved_provider FROM events`).Scan(&key, &session, &parent, &project, &mod, &provider, &base, &resolved); err != nil {
		t.Fatal(err)
	}
	if key != e.DedupKey || session != e.SessionID || parent != e.ParentID || project != e.ProjectPath || mod != e.Model || provider != e.Provider || base != e.BaseURL || resolved != "resolved" {
		t.Fatalf("lossy event: %q %q %q %q %q %q %q %q", key, session, parent, project, mod, provider, base, resolved)
	}
	if err = st.Exec(ctx, `UPDATE events SET resolved_provider='new-provider'`); err != nil {
		t.Fatal(err)
	}
	if err = st.DB().QueryRow(`SELECT resolved_provider FROM events`).Scan(&resolved); err != nil || resolved != "new-provider" {
		t.Fatalf("update %q %v", resolved, err)
	}
}
