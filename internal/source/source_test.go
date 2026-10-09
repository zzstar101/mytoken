package source

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	_ "modernc.org/sqlite"
)

const ccSchema = `CREATE TABLE providers(id TEXT,app_type TEXT,name TEXT,settings_config TEXT,meta TEXT,cost_multiplier TEXT,is_current INTEGER);
CREATE TABLE proxy_request_logs(provider_id TEXT,app_type TEXT,model TEXT,request_model TEXT,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_creation_tokens INTEGER,created_at INTEGER,data_source TEXT);
CREATE TABLE model_pricing(model_id TEXT,display_name TEXT,input_cost_per_million TEXT,output_cost_per_million TEXT,cache_read_cost_per_million TEXT,cache_creation_cost_per_million TEXT);`

func newCC(t *testing.T) (string, time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cc-switch.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err = db.Exec(ccSchema); err != nil {
		t.Fatal(err)
	}
	// Two providers for one app, one of them current, with a key and a base URL.
	if _, err = db.Exec(`INSERT INTO providers VALUES
		('p1','claude','Gateway','{"env":{"ANTHROPIC_BASE_URL":"https://relay.example/v1","ANTHROPIC_AUTH_TOKEN":"secret-token"}}','{}','0.5',1),
		('p2','claude','Backup','{"env":{"ANTHROPIC_BASE_URL":"https://backup.example/v1"}}','{}','0.5',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO proxy_request_logs VALUES('p1','claude','routed','claude-sonnet-4',10,20,3,4,?,'proxy'),('p2','claude','other','gpt-5',10,20,3,4,?,'proxy')`, now.Unix(), now.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO model_pricing VALUES('fixture','Fixture','2','3','0.2','4'),('fixture-20260101','Dated','99','3','0.2','4')`); err != nil {
		t.Fatal(err)
	}
	return path, now
}

// A loaded cc-switch database matches requests, lists providers without ever
// exposing keys, and imports its prices.
func TestCCSwitchSource(t *testing.T) {
	path, now := newCC(t)
	c := NewCCSwitch(path)
	defer c.Close()
	if err := c.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := c.Status()
	if !st.Exists || st.Err != "" || st.ReadAt.IsZero() {
		t.Fatalf("status=%+v", st)
	}
	event := model.UsageEvent{Harness: model.ClaudeCode, Model: "claude-sonnet-4", Timestamp: now, Tokens: model.Tokens{Input: 10, Output: 20, CacheRead: 3, CacheWrite: 4}}
	if provider, ok := c.Match(event); !ok || provider != "Gateway" {
		t.Fatalf("match=%q ok=%v", provider, ok)
	}
	if _, ok := c.Match(model.UsageEvent{Harness: model.ClaudeCode, Model: "claude-sonnet-4", Timestamp: now.Add(121 * time.Second), Tokens: model.Tokens{Input: 10, Output: 20, CacheRead: 3, CacheWrite: 4}}); ok {
		t.Fatal("matched outside the 120s window")
	}
	providers := c.Providers()
	if len(providers) != 2 {
		t.Fatalf("providers=%+v", providers)
	}
	var gateway, backup ProviderInfo
	for _, p := range providers {
		if p.Name == "Gateway" {
			gateway = p
		}
		if p.Name == "Backup" {
			backup = p
		}
	}
	if gateway.App != "claude" || gateway.BaseURL != "https://relay.example/v1" || !gateway.HasKey || gateway.Source != "cc-switch" {
		t.Fatalf("gateway=%+v", gateway)
	}
	if backup.HasKey || backup.BaseURL != "https://backup.example/v1" {
		t.Fatalf("backup=%+v", backup)
	}
	rules, err := c.PriceRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 1 model (the dated duplicate is deduped by Normalize) + 2 providers
	// + 2 materialized model copies.
	if len(rules) != 5 {
		t.Fatalf("rules=%+v", rules)
	}
	for _, r := range rules {
		if r.Model != "" && r.Multiplier != 0 {
			t.Fatalf("model rule %q carries multiplier %v", r.Model, r.Multiplier)
		}
		if r.Source != "cc-switch" {
			t.Fatalf("source=%q", r.Source)
		}
	}
	var models, providerRules, scoped int
	for _, r := range rules {
		switch {
		case r.Provider == "" && r.Model != "":
			models++
		case r.Provider != "" && r.Model == "":
			providerRules++
			if r.Multiplier != 0.5 {
				t.Fatalf("provider multiplier=%v", r.Multiplier)
			}
		case r.Provider != "" && r.Model != "":
			scoped++
			if r.Multiplier != 0 {
				t.Fatalf("scoped multiplier=%v", r.Multiplier)
			}
			if r.Input == nil || *r.Input != 2 {
				t.Fatalf("scoped price=%+v", r)
			}
		}
	}
	if models != 1 || providerRules != 2 || scoped != 2 {
		t.Fatalf("models=%d providers=%d scoped=%d", models, providerRules, scoped)
	}
	seed := c.CurrentConfigs()
	if len(seed) != 1 || seed[0].Provider != "Gateway" || seed[0].Harness != model.ClaudeCode || !seed[0].SeedAtZero {
		t.Fatalf("seed=%+v", seed)
	}
}

// A missing database is not an error, and never gets created.
func TestCCSwitchMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	c := NewCCSwitch(path)
	defer c.Close()
	if err := c.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(); st.Exists || st.Err == "" {
		t.Fatalf("status=%+v", st)
	}
	if _, ok := c.Match(model.UsageEvent{Model: "claude-sonnet-4"}); ok {
		t.Fatal("matched without a database")
	}
	if _, err := c.PriceRules(context.Background()); err == nil {
		t.Fatal("missing source must fail")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("created the database")
	}
}

// A database whose contents changed on disk is re-read.
func TestCCSwitchReload(t *testing.T) {
	path, now := newCC(t)
	c := NewCCSwitch(path)
	defer c.Close()
	if err := c.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.matches) == 0 {
		t.Fatal("no matches loaded")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(time.Minute).Unix()
	if _, err = db.Exec(`INSERT INTO proxy_request_logs VALUES('p2','claude','other','gpt-5',10,20,3,4,?,'proxy')`, later); err != nil {
		t.Fatal(err)
	}
	db.Close()
	// Size and mtime changed, so the next Load picks the new row up.
	time.Sleep(10 * time.Millisecond)
	if err = c.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, list := range c.matches {
		for _, m := range list {
			if m.at == later {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("new row not loaded")
	}
}

// The harness config source reads the current endpoint of each tool and never
// reports the credentials stored beside it.
func TestHarnessConfigSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://api.deepseek.com/v1","ANTHROPIC_AUTH_TOKEN":"secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codex", "config.toml"), []byte("model_provider = \"relay\"\n[model_providers.relay]\nbase_url = \"https://relay.example/v1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHarnessConfig(dir)
	defer h.Close()
	if err := h.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.Status().Exists || h.Status().Providers != 2 {
		t.Fatalf("status=%+v", h.Status())
	}
	configs := h.CurrentConfigs()
	if len(configs) != 2 {
		t.Fatalf("configs=%+v", configs)
	}
	byHarness := map[model.Harness]CurrentConfig{}
	for _, c := range configs {
		byHarness[c.Harness] = c
	}
	if c := byHarness[model.ClaudeCode]; c.Base != "https://api.deepseek.com/v1" || c.SeedAtZero {
		t.Fatalf("claude=%+v", c)
	}
	if c := byHarness[model.Codex]; c.Provider != "relay" || c.Base != "https://relay.example/v1" {
		t.Fatalf("codex=%+v", c)
	}
	// No credential ever reaches a source's output.
	if strings.Contains(h.Status().Detail, "secret") {
		t.Fatal("detail leaked a credential")
	}
	// An empty home directory yields nothing but is not an error.
	empty := NewHarnessConfig(t.TempDir())
	defer empty.Close()
	if err := empty.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(empty.CurrentConfigs()) != 0 {
		t.Fatalf("configs=%+v", empty.CurrentConfigs())
	}
}

// cc-switch's credentials: one per provider with both a key and a base URL,
// identified by a one-way key id, with the key itself never in the output.
func TestCCSwitchCredentials(t *testing.T) {
	path, _ := newCC(t)
	c := NewCCSwitch(path)
	defer c.Close()
	if err := c.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	creds, err := c.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Backup has no key, so only Gateway yields a credential.
	if len(creds) != 1 {
		t.Fatalf("creds=%+v", creds)
	}
	got := creds[0]
	if got.Origin != "https://relay.example" || got.Provider != "Gateway" || got.Source != "cc-switch" {
		t.Fatalf("cred=%+v", got)
	}
	if got.Harness != model.ClaudeCode {
		t.Fatalf("harness=%v", got.Harness)
	}
	if got.KeyID != KeyID("secret-token") {
		t.Fatalf("keyID=%q", got.KeyID)
	}
	if got.Secret.Reveal() != "secret-token" {
		t.Fatalf("secret=%q", got.Secret.Reveal())
	}
	// The key must not survive any formatting path.
	for _, s := range []string{got.Secret.String(), fmt.Sprintf("%v", got.Secret), fmt.Sprintf("%#v", got.Secret)} {
		if strings.Contains(s, "secret-token") {
			t.Fatalf("secret leaked as %q", s)
		}
	}
	// Providers() exposes the same identity without the key.
	infos := c.Providers()
	for _, p := range infos {
		if strings.Contains(p.Name, "secret") {
			t.Fatalf("provider name leaked a key: %q", p.Name)
		}
	}
	var found bool
	for _, p := range infos {
		if p.Name == "Gateway" {
			found = true
			if p.Origin != "https://relay.example" || p.KeyID != KeyID("secret-token") || !p.HasKey {
				t.Fatalf("gateway info=%+v", p)
			}
		}
		if p.Name == "Backup" && (p.Origin != "https://backup.example" || p.HasKey || p.KeyID != "") {
			t.Fatalf("backup info=%+v", p)
		}
	}
	if !found {
		t.Fatal("gateway not listed")
	}
}

// A cc-switch database pointing at its own local proxy yields no credential:
// that is a proxy, not a gateway.
func TestCCSwitchCredentialsSkipLocalProxy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cc-switch.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ccSchema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO providers VALUES
		('p1','claude','Local','{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:15721","ANTHROPIC_AUTH_TOKEN":"secret-token"}}','{}','1',1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	c := NewCCSwitch(path)
	defer c.Close()
	if err := c.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	creds, err := c.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 0 {
		t.Fatalf("creds=%+v, want none for a local proxy", creds)
	}
	// The provider is still listed for attribution and pickers.
	if len(c.Providers()) != 1 {
		t.Fatalf("providers=%+v", c.Providers())
	}
}

// The harness config files hand over the key they already send, wrapped so it
// redacts itself everywhere else.
func TestHarnessConfigCredentials(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://relay.example/v1","ANTHROPIC_AUTH_TOKEN":"claude-key"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codex", "config.toml"), []byte("model_provider = \"relay\"\n[model_providers.relay]\nbase_url = \"https://relay.example/v1\"\nenv_key = \"MYTOKEN_TEST_CODEX_KEY\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MYTOKEN_TEST_CODEX_KEY", "codex-key")
	h := NewHarnessConfig(dir)
	defer h.Close()
	if err := h.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	creds, err := h.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 2 {
		t.Fatalf("creds=%+v", creds)
	}
	// Both harnesses point at the same gateway, so they share one origin but
	// have their own key ids.
	seen := map[model.Harness]string{}
	for _, c := range creds {
		if c.Origin != "https://relay.example" || c.Source != "harness-config" {
			t.Fatalf("cred=%+v", c)
		}
		seen[c.Harness] = c.KeyID
	}
	if seen[model.ClaudeCode] != KeyID("claude-key") || seen[model.Codex] != KeyID("codex-key") {
		t.Fatalf("key ids=%v", seen)
	}
	if !strings.Contains(h.Status().Detail, "relay.example") || strings.Contains(h.Status().Detail, "key") {
		t.Fatalf("detail=%q", h.Status().Detail)
	}
	// An empty home yields nothing.
	empty := NewHarnessConfig(t.TempDir())
	defer empty.Close()
	if creds, err = empty.Credentials(context.Background()); err != nil || len(creds) != 0 {
		t.Fatalf("creds=%+v err=%v", creds, err)
	}
}

// Credentials are re-read on every call: changing the key in the config file
// must be visible without restarting anything.
func TestHarnessConfigCredentialsAreFresh(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(dir, ".claude", "settings.json")
	write := func(token string) {
		if err := os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://relay.example/v1","ANTHROPIC_AUTH_TOKEN":"`+token+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("first-key")
	h := NewHarnessConfig(dir)
	defer h.Close()
	creds, err := h.Credentials(context.Background())
	if err != nil || len(creds) != 1 {
		t.Fatalf("creds=%+v err=%v", creds, err)
	}
	first := creds[0].KeyID
	write("second-key")
	creds, err = h.Credentials(context.Background())
	if err != nil || len(creds) != 1 {
		t.Fatalf("creds=%+v err=%v", creds, err)
	}
	if creds[0].KeyID == first || creds[0].KeyID != KeyID("second-key") {
		t.Fatalf("keyID=%q, want the new key's id", creds[0].KeyID)
	}
	if creds[0].Secret.Reveal() != "second-key" {
		t.Fatal("secret not refreshed")
	}
}
