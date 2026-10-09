package attrib

import (
	"context"
	"database/sql"
	"github.com/zzstar/mytoken/internal/model"
	"github.com/zzstar/mytoken/internal/store"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolutionChain(t *testing.T) {
	dir := t.TempDir()
	dbpath := filepath.Join(dir, "cc.db")
	db, e := sql.Open("sqlite", dbpath)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC().Truncate(time.Second)
	_, e = db.Exec(`CREATE TABLE providers(id TEXT,app_type TEXT,name TEXT,is_current INTEGER);CREATE TABLE proxy_request_logs(provider_id TEXT,app_type TEXT,model TEXT,request_model TEXT,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_creation_tokens INTEGER,created_at INTEGER,data_source TEXT);INSERT INTO providers VALUES('p','claude','Gateway',1);INSERT INTO proxy_request_logs VALUES('p','claude','routed','claude-sonnet-4',10,20,3,4,?,'proxy');`, now.Unix())
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	st, e := store.Open(filepath.Join(dir, "own.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	r, e := NewWithPaths(st, dir, dbpath)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	ctx := context.Background()
	event := model.UsageEvent{Harness: model.ClaudeCode, Model: "claude-sonnet-4", Timestamp: now, Tokens: model.Tokens{Input: 10, Output: 20, CacheRead: 3, CacheWrite: 4}}
	p, a := r.Resolve(ctx, event)
	if p != "Gateway" || a != model.AttribCCSwitch {
		t.Fatalf("match %s %s", p, a)
	}
	event.Provider = "Log"
	p, a = r.Resolve(ctx, event)
	if p != "Log" || a != model.AttribLog {
		t.Fatalf("log %s %s", p, a)
	}
	event.Provider = ""
	event.Timestamp = now.Add(121 * time.Second)
	_, a = r.Resolve(ctx, event)
	if a == model.AttribCCSwitch {
		t.Fatal("matched outside time window")
	}
	event.Tokens.Input++
	event.Harness = model.Pi
	p, a = r.Resolve(ctx, event)
	if p != "anthropic" || a != model.AttribInferred {
		t.Fatalf("infer %s %s", p, a)
	}
}
func TestConfigTimelineAndMissingCCSwitch(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".claude"), 0700)
	os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://api.deepseek.com/v1","ANTHROPIC_AUTH_TOKEN":"do-not-store"}}`), 0600)
	st, e := store.Open(filepath.Join(dir, "own.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	r, e := NewWithPaths(st, dir, filepath.Join(dir, "missing.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	p, a := r.Resolve(context.Background(), model.UsageEvent{Harness: model.ClaudeCode, Model: "claude-sonnet-4", Timestamp: time.Now().Add(time.Second)})
	if p != "deepseek" || a != model.AttribConfig {
		t.Fatalf("config %q %q", p, a)
	}
	if _, e = os.Stat(filepath.Join(dir, "missing.db")); !os.IsNotExist(e) {
		t.Fatal("created cc-switch DB")
	}
}
