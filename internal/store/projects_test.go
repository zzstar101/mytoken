package store

import (
	"context"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalProjects(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "repos", "demo")
	work := filepath.Join(root, "worktrees", "id", "demo")
	dead := filepath.Join(root, "worktrees", "gone", "demo")
	sub := filepath.Join(main, ".data", "design-sample")
	for _, p := range []string{filepath.Join(main, ".git", "worktrees", "id"), work, sub} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: "+filepath.Join(main, ".git", "worktrees", "id")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	events := []model.UsageEvent{}
	for i, p := range []string{dead, work, sub, main} {
		events = append(events, model.UsageEvent{Harness: model.Codex, SessionID: p, DedupKey: string(rune('a' + i)), ProjectPath: p})
	}
	if err = st.Commit(context.Background(), model.Codex, "log", harness.Batch{Events: events}, nil); err != nil {
		t.Fatal(err)
	}
	var count, raw int
	if err = st.DB().QueryRow("SELECT count(DISTINCT project),count(DISTINCT raw_project) FROM events").Scan(&count, &raw); err != nil || count != 1 || raw != 4 {
		t.Fatalf("canonical=%d raw=%d err=%v", count, raw, err)
	}
	var project string
	st.DB().QueryRow("SELECT project FROM events LIMIT 1").Scan(&project)
	if project != main {
		t.Fatalf("project=%q want %q", project, main)
	}
	if err = st.Exec(context.Background(), "UPDATE events SET project=raw_project; UPDATE sessions SET project=raw_project"); err != nil {
		t.Fatal(err)
	}
	if err = st.NormalizeProjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	st.DB().QueryRow("SELECT count(DISTINCT project) FROM events").Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
}
