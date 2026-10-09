// Package attrib resolves providers without reading conversation content.
package attrib

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pelletier/go-toml/v2"
	"github.com/zzstar/mytoken/internal/model"
	"github.com/zzstar/mytoken/internal/store"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type matchKey struct {
	app, model                 string
	input, output, read, write int64
}
type match struct {
	at       int64
	provider string
}
type config struct {
	from           int64
	base, provider string
}
type Rule struct {
	Harness  model.Harness `json:"harness"`
	Model    string        `json:"model"`
	From, To time.Time
	Provider string `json:"provider"`
}
type Resolver struct {
	st           *store.Store
	home, ccPath string
	mu           sync.RWMutex
	refresh      sync.Mutex
	cc           *sql.DB
	matches      map[matchKey][]match
	configs      map[model.Harness][]config
	rules        []Rule
	hosts        map[string]string
	ccSize       int64
	ccMod        time.Time
}

func New(st *store.Store) (*Resolver, error) {
	home, e := os.UserHomeDir()
	if e != nil {
		return nil, e
	}
	return NewWithPaths(st, home, filepath.Join(home, ".cc-switch", "cc-switch.db"))
}
func NewWithPaths(st *store.Store, home, ccPath string) (*Resolver, error) {
	r := &Resolver{st: st, home: home, ccPath: ccPath, matches: map[matchKey][]match{}, configs: map[model.Harness][]config{}, hosts: map[string]string{}}
	if e := r.Refresh(context.Background()); e != nil {
		return nil, e
	}
	if err := r.migrateGenericProviders(context.Background()); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
func (r *Resolver) migrateGenericProviders(ctx context.Context) error {
	done, err := r.st.Setting(ctx, "attribution.generic-host.v1")
	if err != nil || done == "1" {
		return err
	}
	rows, err := r.st.DB().QueryContext(ctx, `SELECT DISTINCT resolved_provider,base_url FROM events WHERE lower(trim(resolved_provider)) IN ('','custom','openai-compatible','default','proxy') AND base_url!='' AND attrib!=?`, model.AttribUserRule)
	if err != nil {
		return err
	}
	type change struct{ old, base, next string }
	var changes []change
	for rows.Next() {
		var old, base string
		if err = rows.Scan(&old, &base); err != nil {
			rows.Close()
			return err
		}
		if next := displayProvider(old, base); next != old {
			changes = append(changes, change{old, base, next})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range changes {
		if err = r.st.Exec(ctx, "UPDATE events SET resolved_provider=? WHERE resolved_provider=? AND base_url=? AND attrib!=?", c.next, c.old, c.base, model.AttribUserRule); err != nil {
			return err
		}
	}
	return r.st.SetSetting(ctx, "attribution.generic-host.v1", "1")
}

func (r *Resolver) Close() error {
	r.refresh.Lock()
	defer r.refresh.Unlock()
	if r.cc != nil {
		return r.cc.Close()
	}
	return nil
}
func appType(h model.Harness) string {
	switch h {
	case model.ClaudeCode:
		return "claude"
	case model.Codex:
		return "codex"
	case model.Gemini:
		return "gemini"
	}
	return string(h)
}
func (r *Resolver) Resolve(_ context.Context, e model.UsageEvent) (string, model.AttribSource) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.rules {
		if (v.Harness == "" || v.Harness == e.Harness) && (v.Model == "" || v.Model == e.Model) && (v.From.IsZero() || !e.Timestamp.Before(v.From)) && (v.To.IsZero() || e.Timestamp.Before(v.To)) {
			return v.Provider, model.AttribUserRule
		}
	}
	if provider := displayProvider(e.Provider, e.BaseURL); provider != "" {
		return provider, model.AttribLog
	}
	key := matchKey{appType(e.Harness), e.Model, e.Tokens.Input, e.Tokens.Output + e.Tokens.Reasoning, e.Tokens.CacheRead, e.Tokens.CacheWrite}
	best := int64(121)
	provider := ""
	for _, m := range r.matches[key] {
		d := e.Timestamp.Unix() - m.at
		if d < 0 {
			d = -d
		}
		if d < best {
			provider = m.provider
			best = d
		}
	}
	if provider != "" {
		return displayProvider(provider, e.BaseURL), model.AttribCCSwitch
	}
	configs := r.configs[e.Harness]
	at := e.Timestamp.UnixNano()
	for i := len(configs) - 1; i >= 0; i-- {
		if configs[i].from <= at && configs[i].provider != "" {
			return displayProvider(configs[i].provider, e.BaseURL), model.AttribConfig
		}
	}
	return Infer(e.Model), model.AttribInferred
}

// displayProvider preserves named providers and identifies generic relays by host.
func displayProvider(provider, base string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "custom", "openai-compatible", "default", "proxy":
		u, err := url.Parse(base)
		if err == nil && u.Hostname() != "" && (u.Scheme == "https" || u.Scheme == "http") {
			host := strings.ToLower(u.Hostname())
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			port := u.Port()
			if port != "" && !(u.Scheme == "https" && port == "443") && !(u.Scheme == "http" && port == "80") {
				host += ":" + port
			}
			return host
		}
	}
	return provider
}

func Infer(name string) string {
	name = strings.ToLower(name)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	for _, v := range []struct{ prefix, provider string }{{"claude-", "anthropic"}, {"gpt-", "openai"}, {"chatgpt-", "openai"}, {"o1", "openai"}, {"o3", "openai"}, {"o4", "openai"}, {"gemini-", "google"}, {"deepseek-", "deepseek"}, {"kimi-", "moonshot"}, {"moonshot-", "moonshot"}, {"glm-", "zhipu"}, {"qwen", "alibaba"}, {"mistral", "mistral"}, {"codestral", "mistral"}} {
		if strings.HasPrefix(name, v.prefix) {
			return v.provider
		}
	}
	return "unknown"
}

// ProviderForURL maps a host to a provider; unknown gateways retain their hostname.
func ProviderForURL(base string) string {
	u, e := url.Parse(base)
	if e != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	for _, v := range []struct{ domain, provider string }{{"anthropic.com", "anthropic"}, {"openai.com", "openai"}, {"googleapis.com", "google"}, {"deepseek.com", "deepseek"}, {"moonshot.cn", "moonshot"}, {"moonshot.ai", "moonshot"}, {"bigmodel.cn", "zhipu"}, {"aliyuncs.com", "alibaba"}, {"openrouter.ai", "openrouter"}, {"siliconflow.cn", "siliconflow"}, {"siliconflow.com", "siliconflow"}, {"volces.com", "bytedance"}, {"mistral.ai", "mistral"}} {
		if host == v.domain || strings.HasSuffix(host, "."+v.domain) {
			return v.provider
		}
	}
	return host
}
func sanitizedURL(base string) string {
	u, e := url.Parse(base)
	if e != nil || u.Hostname() == "" {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
func (r *Resolver) Refresh(ctx context.Context) error {
	r.refresh.Lock()
	defer r.refresh.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if raw, e := r.st.Setting(ctx, "attribution.rules"); e == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &r.rules)
	}
	if raw, e := r.st.Setting(ctx, "attribution.hosts"); e == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &r.hosts)
	}
	r.loadCC(ctx)
	for h, v := range r.currentConfigs() {
		v.base = sanitizedURL(v.base)
		if v.base != "" {
			host := ProviderForURL(v.base)
			if custom, ok := r.hosts[host]; ok {
				host = custom
			}
			v.provider = host
		}
		if v.provider == "" {
			continue
		}
		var base, provider string
		e := r.st.DB().QueryRowContext(ctx, "SELECT base_url,provider FROM provider_configs WHERE harness=? ORDER BY from_time DESC LIMIT 1", h).Scan(&base, &provider)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if base != v.base || provider != v.provider {
			if e = r.st.Exec(ctx, "INSERT INTO provider_configs VALUES(?,?,?,?)", h, time.Now().UnixNano(), v.base, v.provider); e != nil {
				return e
			}
		}
	}
	rows, e := r.st.DB().QueryContext(ctx, "SELECT harness,from_time,base_url,provider FROM provider_configs ORDER BY from_time")
	if e != nil {
		return e
	}
	defer rows.Close()
	configs := map[model.Harness][]config{}
	for rows.Next() {
		var h model.Harness
		var c config
		if e = rows.Scan(&h, &c.from, &c.base, &c.provider); e != nil {
			return e
		}
		configs[h] = append(configs[h], c)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	r.configs = configs
	return nil
}
func (r *Resolver) currentConfigs() map[model.Harness]config {
	out := map[model.Harness]config{}
	if raw, e := os.ReadFile(filepath.Join(r.home, ".claude", "settings.json")); e == nil {
		var v struct {
			Env map[string]string `json:"env"`
		}
		if json.Unmarshal(raw, &v) == nil {
			out[model.ClaudeCode] = config{base: v.Env["ANTHROPIC_BASE_URL"]}
			if out[model.ClaudeCode].base == "" {
				out[model.ClaudeCode] = config{provider: "anthropic"}
			}
		}
	}
	if raw, e := os.ReadFile(filepath.Join(r.home, ".codex", "config.toml")); e == nil {
		var v struct {
			ModelProvider string `toml:"model_provider"`
			BaseURL       string `toml:"base_url"`
			Providers     map[string]struct {
				BaseURL string `toml:"base_url"`
			} `toml:"model_providers"`
		}
		if toml.Unmarshal(raw, &v) == nil {
			base := v.BaseURL
			if p, ok := v.Providers[v.ModelProvider]; ok && p.BaseURL != "" {
				base = p.BaseURL
			}
			provider := v.ModelProvider
			if provider == "" {
				provider = "openai"
			}
			out[model.Codex] = config{base: base, provider: provider}
		}
	}
	return out
}
func (r *Resolver) loadCC(ctx context.Context) {
	fi, e := os.Stat(r.ccPath)
	if e != nil {
		return
	}
	wal, _ := os.Stat(r.ccPath + "-wal")
	size := fi.Size()
	mod := fi.ModTime()
	if wal != nil {
		size += wal.Size()
		if wal.ModTime().After(mod) {
			mod = wal.ModTime()
		}
	}
	if r.cc != nil && r.ccSize == size && r.ccMod.Equal(mod) {
		return
	}
	if r.cc == nil {
		u := url.URL{Scheme: "file", Path: r.ccPath, RawQuery: "mode=ro"}
		r.cc, e = sql.Open("sqlite", u.String())
		if e != nil {
			return
		}
		r.cc.SetMaxOpenConns(1)
	}
	rows, e := r.cc.QueryContext(ctx, `SELECT l.app_type,l.model,COALESCE(l.request_model,''),COALESCE(l.input_tokens,0),COALESCE(l.output_tokens,0),COALESCE(l.cache_read_tokens,0),COALESCE(l.cache_creation_tokens,0),l.created_at,p.name FROM proxy_request_logs l JOIN providers p ON p.id=l.provider_id AND p.app_type=l.app_type WHERE l.data_source='proxy' OR (l.provider_id IS NOT NULL AND l.provider_id!='')`)
	if e != nil {
		return
	}
	matches := map[matchKey][]match{}
	for rows.Next() {
		var k matchKey
		var request string
		var m match
		if rows.Scan(&k.app, &k.model, &request, &k.input, &k.output, &k.read, &k.write, &m.at, &m.provider) != nil {
			continue
		}
		if m.at > 1e12 {
			m.at /= 1000
		}
		matches[k] = append(matches[k], m)
		if request != "" && request != k.model {
			k.model = request
			matches[k] = append(matches[k], m)
		}
	}
	err := rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	for k := range matches {
		sort.Slice(matches[k], func(i, j int) bool { return matches[k][i].at < matches[k][j].at })
	}
	r.matches = matches
	r.ccSize = size
	r.ccMod = mod
	// Backfill only from cc-switch's current selection, never today's raw config.
	rows, e = r.cc.QueryContext(ctx, "SELECT app_type,name FROM providers WHERE is_current=1 ORDER BY id")
	if e != nil {
		return
	}
	type current struct{ app, name string }
	var curr []current
	for rows.Next() {
		var v current
		if rows.Scan(&v.app, &v.name) == nil {
			curr = append(curr, v)
		}
	}
	rows.Close()
	for _, v := range curr {
		h := model.Harness(v.app)
		if v.app == "claude" {
			h = model.ClaudeCode
		}
		_ = r.st.Exec(ctx, `INSERT INTO provider_configs(harness,from_time,base_url,provider) SELECT ?,0,'',? WHERE NOT EXISTS(SELECT 1 FROM provider_configs WHERE harness=?)`, h, v.name, h)
	}
}
func (r *Resolver) String() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return fmt.Sprintf("attribution: %d matching keys", len(r.matches))
}
