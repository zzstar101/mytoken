package app

import (
	"context"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
	"path/filepath"
	"testing"
)

func TestOpenRepricesExistingEvents(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	st, err := store.Open(filepath.Join(dir, "mytoken.db"))
	if err != nil {
		t.Fatal(err)
	}
	e := model.UsageEvent{Model: "claude-opus-4-5", SessionID: "s", DedupKey: "e", Tokens: model.Tokens{Input: 1000000}}
	if err = st.Commit(context.Background(), model.Codex, "p", harness.Batch{Events: []model.UsageEvent{e}}, nil); err != nil {
		t.Fatal(err)
	}
	st.Close()
	a, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var cost float64
	if err = a.Store.DB().QueryRow("SELECT cost FROM events").Scan(&cost); err != nil || cost != 5 {
		t.Fatalf("cost=%v err=%v", cost, err)
	}
}
