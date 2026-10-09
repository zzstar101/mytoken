package attrib

import (
	"context"
	"database/sql"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/source"
	"github.com/zzstar101/mytoken/internal/store"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGenericProviderHost(t *testing.T) {
	r := &Resolver{}
	for _, provider := range []string{"custom", "openai-compatible", "default", "proxy", "", " CUSTOM "} {
		for _, tt := range []struct{ base, want string }{{"https://api.relay.example:443/v1", "api.relay.example"}, {"http://api.relay.example:80/v1", "api.relay.example"}, {"https://api.relay.example:8443/v1", "api.relay.example:8443"}, {"https://api.openai.com/v1", "api.openai.com"}, {"http://[::1]:8080/v1", "[::1]:8080"}} {
			got, _ := r.Resolve(t.Context(), model.UsageEvent{Provider: provider, BaseURL: tt.base, Model: "gpt-5"})
			if got != tt.want {
				t.Errorf("%q %q: got %q want %q", provider, tt.base, got, tt.want)
			}
		}
	}
	got, _ := r.Resolve(t.Context(), model.UsageEvent{Provider: "Named", BaseURL: "https://api.relay.example"})
	if got != "Named" {
		t.Fatal(got)
	}
	r.rules = []Rule{{Provider: "custom"}}
	got, kind := r.Resolve(t.Context(), model.UsageEvent{BaseURL: "https://api.relay.example"})
	if got != "custom" || kind != model.AttribUserRule {
		t.Fatal(got, kind)
	}
}

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

// A resolver with no provider source at all still attributes from user rules,
// the log's own provider and the model name.
func TestResolveWithoutSources(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "own.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r, err := NewWithSources(st)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(r.Sources()) != 0 {
		t.Fatalf("sources=%v", r.Sources())
	}
	ctx := context.Background()
	if _, a := r.Resolve(ctx, model.UsageEvent{Provider: "Gateway", Model: "claude-sonnet-4"}); a != model.AttribLog {
		t.Fatal(a)
	}
	p, a := r.Resolve(ctx, model.UsageEvent{Model: "claude-sonnet-4", Timestamp: time.Now()})
	if p != "anthropic" || a != model.AttribInferred {
		t.Fatalf("infer %q %q", p, a)
	}
	// A source that is present but empty behaves the same way.
	r2, err := NewWithSources(st, source.NewCCSwitch(filepath.Join(dir, "absent.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if p, a := r2.Resolve(ctx, model.UsageEvent{Model: "claude-sonnet-4", Timestamp: time.Now()}); p != "anthropic" || a != model.AttribInferred {
		t.Fatalf("empty source %q %q", p, a)
	}
	if s := r2.Sources(); len(s) != 1 || s[0].Status().Exists {
		t.Fatalf("status=%+v", s)
	}
}
