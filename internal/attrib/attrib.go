// Package attrib resolves providers without reading conversation content.
package attrib

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/source"
	"github.com/zzstar101/mytoken/internal/store"
)

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

// Resolver attributes an event to a provider. Provider data comes from
// optional sources (cc-switch, local harness configs) consulted in priority
// order; with no source at all it still resolves from user rules, the log
// itself and the model name.
type Resolver struct {
	st      *store.Store
	sources []source.Source
	mu      sync.RWMutex
	refresh sync.Mutex
	configs map[model.Harness][]config
	rules   []Rule
	hosts   map[string]string
}

func New(st *store.Store) (*Resolver, error) {
	home, e := os.UserHomeDir()
	if e != nil {
		return nil, e
	}
	return NewWithPaths(st, home, filepath.Join(home, ".cc-switch", "cc-switch.db"))
}
func NewWithPaths(st *store.Store, home, ccPath string) (*Resolver, error) {
	return NewWithSources(st, source.NewCCSwitch(ccPath), source.NewHarnessConfig(home))
}

// NewWithSources builds a resolver over explicit provider sources, in priority
// order. Tests and future embedders use it to inject paths.
func NewWithSources(st *store.Store, sources ...source.Source) (*Resolver, error) {
	r := &Resolver{st: st, sources: sources, configs: map[model.Harness][]config{}, hosts: map[string]string{}}
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

// Sources returns the configured provider sources in priority order.
func (r *Resolver) Sources() []source.Source {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]source.Source{}, r.sources...)
}

func (r *Resolver) Close() error {
	r.refresh.Lock()
	defer r.refresh.Unlock()
	var first error
	for _, s := range r.sources {
		if e := s.Close(); e != nil && first == nil {
			first = e
		}
	}
	return first
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
	// Request-level sources (cc-switch's proxy log) come next, in priority order.
	for _, s := range r.sources {
		m, ok := s.(source.Matcher)
		if !ok {
			continue
		}
		if provider, ok := m.Match(e); ok {
			return displayProvider(provider, e.BaseURL), model.AttribCCSwitch
		}
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
	for _, s := range r.sources {
		if e := s.Load(ctx); e != nil {
			return e
		}
		if c, ok := s.(source.Configs); ok {
			if e := r.backfillConfigs(ctx, c.CurrentConfigs()); e != nil {
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

// backfillConfigs appends a source's current selections to the provider_configs
// timeline: a selection that predates the timeline anchors it at time 0, a live
// one only when it differs from the latest entry.
func (r *Resolver) backfillConfigs(ctx context.Context, list []source.CurrentConfig) error {
	for _, v := range list {
		base, provider := v.Base, v.Provider
		if base != "" {
			base = sanitizedURL(base)
			if base != "" {
				host := ProviderForURL(base)
				if custom, ok := r.hosts[host]; ok {
					host = custom
				}
				provider = host
			}
		}
		if provider == "" {
			continue
		}
		if v.SeedAtZero {
			if e := r.st.Exec(ctx, `INSERT INTO provider_configs(harness,from_time,base_url,provider) SELECT ?,0,'',? WHERE NOT EXISTS(SELECT 1 FROM provider_configs WHERE harness=?)`, v.Harness, provider, v.Harness); e != nil {
				return e
			}
			continue
		}
		var oldBase, oldProvider string
		e := r.st.DB().QueryRowContext(ctx, "SELECT base_url,provider FROM provider_configs WHERE harness=? ORDER BY from_time DESC LIMIT 1", v.Harness).Scan(&oldBase, &oldProvider)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if oldBase != base || oldProvider != provider {
			if e = r.st.Exec(ctx, "INSERT INTO provider_configs VALUES(?,?,?,?)", v.Harness, time.Now().UnixNano(), base, provider); e != nil {
				return e
			}
		}
	}
	return nil
}
func (r *Resolver) String() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return fmt.Sprintf("attribution: %d sources, %d rules", len(r.sources), len(r.rules))
}
