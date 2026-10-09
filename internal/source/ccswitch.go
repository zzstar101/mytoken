package source

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"
)

// CCSwitch reads the cc-switch SQLite database, opened read-only. It supplies
// the resolver with three things: request-log matching (which provider served
// an event), cc-switch's current selection per harness (timeline anchor), the
// provider inventory, and the prices stored in the database. Credentials found
// inside it are never returned.
type CCSwitch struct {
	path string

	mu      sync.RWMutex
	db      *sql.DB
	status  Status
	matches map[matchKey][]match
	seed    []CurrentConfig
	prices  []pricing.Rule
	size    int64
	mod     time.Time
}

type matchKey struct {
	app, model                 string
	input, output, read, write int64
}
type match struct {
	at       int64
	provider string
}

// NewCCSwitch returns a source for the cc-switch database at path.
func NewCCSwitch(path string) *CCSwitch {
	return &CCSwitch{path: path, status: Status{Name: "cc-switch", Path: path}, matches: map[matchKey][]match{}}
}

// CCSwitchPath returns the default cc-switch database location.
func CCSwitchPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cc-switch", "cc-switch.db")
}

func (c *CCSwitch) Name() string { return "cc-switch" }

// Status returns the state of the last Load: whether the database exists, when
// it was read, how many providers it holds and any error.
func (c *CCSwitch) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

// Load re-reads the database when it changed on disk. A missing database is
// not an error: cc-switch is optional.
func (c *CCSwitch) Load(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.load(ctx)
}

func (c *CCSwitch) load(ctx context.Context) error {
	fi, e := os.Stat(c.path)
	if e != nil {
		c.status.Exists = false
		c.status.Err = e.Error()
		c.status.ReadAt = time.Now()
		return nil
	}
	wal, _ := os.Stat(c.path + "-wal")
	size := fi.Size()
	mod := fi.ModTime()
	if wal != nil {
		size += wal.Size()
		if wal.ModTime().After(mod) {
			mod = wal.ModTime()
		}
	}
	if c.db != nil && c.size == size && c.mod.Equal(mod) {
		return nil
	}
	if c.db == nil {
		c.db, e = sql.Open("sqlite", sqlitedsn.URI(c.path, "mode=ro"))
		if e != nil {
			return nil
		}
		c.db.SetMaxOpenConns(1)
	}
	c.status.Exists = true
	c.status.ReadAt = time.Now()
	c.status.Err = ""
	c.matches = map[matchKey][]match{}
	c.seed = nil
	c.prices = nil
	rows, e := c.db.QueryContext(ctx, `SELECT l.app_type,l.model,COALESCE(l.request_model,''),COALESCE(l.input_tokens,0),COALESCE(l.output_tokens,0),COALESCE(l.cache_read_tokens,0),COALESCE(l.cache_creation_tokens,0),l.created_at,p.name FROM proxy_request_logs l JOIN providers p ON p.id=l.provider_id AND p.app_type=l.app_type WHERE l.data_source='proxy' OR (l.provider_id IS NOT NULL AND l.provider_id!='')`)
	if e != nil {
		return nil
	}
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
		c.matches[k] = append(c.matches[k], m)
		if request != "" && request != k.model {
			k.model = request
			c.matches[k] = append(c.matches[k], m)
		}
	}
	err := rows.Err()
	rows.Close()
	if err != nil {
		return nil
	}
	for k := range c.matches {
		list := c.matches[k]
		sort.Slice(list, func(i, j int) bool { return list[i].at < list[j].at })
	}
	c.size = size
	c.mod = mod
	c.status.Matches = len(c.matches)
	c.status.Providers = len(c.seed)
	// The current selection seeds the provider timeline at time 0.
	rows, e = c.db.QueryContext(ctx, "SELECT app_type,name FROM providers WHERE is_current=1 ORDER BY id")
	if e != nil {
		return nil
	}
	for rows.Next() {
		var app, name string
		if rows.Scan(&app, &name) == nil && name != "" {
			c.seed = append(c.seed, CurrentConfig{Harness: harnessForApp(app), Provider: name})
		}
	}
	rows.Close()
	return nil
}

func harnessForApp(app string) model.Harness {
	if app == "claude" {
		return model.ClaudeCode
	}
	return model.Harness(app)
}

// Match attributes an event to a provider from cc-switch's request log,
// accepting the closest entry within 120 seconds of the event.
func (c *CCSwitch) Match(e model.UsageEvent) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	key := matchKey{app: appType(e.Harness), model: e.Model, input: e.Tokens.Input, output: e.Tokens.Output + e.Tokens.Reasoning, read: e.Tokens.CacheRead, write: e.Tokens.CacheWrite}
	best := int64(121)
	provider := ""
	for _, m := range c.matches[key] {
		d := e.Timestamp.Unix() - m.at
		if d < 0 {
			d = -d
		}
		if d < best {
			provider = m.provider
			best = d
		}
	}
	if provider == "" {
		return "", false
	}
	return provider, true
}

// CurrentConfigs returns cc-switch's current selection per harness.
func (c *CCSwitch) CurrentConfigs() []CurrentConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]CurrentConfig, len(c.seed))
	for i, v := range c.seed {
		v.SeedAtZero = true
		out[i] = v
	}
	return out
}

// Providers lists the providers known to cc-switch. It never returns keys:
// HasKey only records that a credential is configured, and Origin/KeyID are the
// gateway's normalized address and a one-way id for the key.
func (c *CCSwitch) Providers() []ProviderInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.db == nil {
		return nil
	}
	rows, err := c.db.Query(`SELECT app_type,name,COALESCE(settings_config,''),COALESCE(meta,'') FROM providers ORDER BY app_type,name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ProviderInfo
	for rows.Next() {
		var app, name, cfg, meta string
		if err := rows.Scan(&app, &name, &cfg, &meta); err != nil {
			return out
		}
		out = append(out, providerInfo("cc-switch", app, name, ccBaseURL(cfg, meta), ccKey(cfg, meta)))
	}
	return out
}

// providerInfo fills a ProviderInfo, deriving the gateway origin and key id
// from the base URL and the key. The key itself is only used to derive its id.
func providerInfo(sourceName, app, name, base, key string) ProviderInfo {
	info := ProviderInfo{Name: name, App: app, BaseURL: base, HasKey: key != "", Source: sourceName}
	if base != "" {
		if origin, ok := Origin(base); ok {
			info.Origin = origin
		}
	}
	if key != "" {
		info.KeyID = KeyID(key)
	}
	return info
}

// Credentials enumerates the gateway credentials cc-switch holds, read fresh on
// every call: a provider selected, enabled or re-keyed in cc-switch must be
// visible to the next reconciliation without a restart. The key is only kept
// inside the returned Secret and never written to disk by this package.
func (c *CCSwitch) Credentials(ctx context.Context) ([]Credential, error) {
	_ = ctx
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, err := os.Stat(c.path); err != nil {
		return nil, nil
	}
	if c.db == nil {
		return nil, nil
	}
	rows, err := c.db.Query(`SELECT app_type,name,COALESCE(settings_config,''),COALESCE(meta,'') FROM providers ORDER BY app_type,name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		var app, name, cfg, meta string
		if err := rows.Scan(&app, &name, &cfg, &meta); err != nil {
			return out, err
		}
		key := ccKey(cfg, meta)
		base := ccBaseURL(cfg, meta)
		if key == "" || base == "" {
			continue
		}
		origin, ok := Origin(base)
		if !ok || origin == LocalProxyOrigin {
			continue // cc-switch's own proxy is not a gateway
		}
		out = append(out, Credential{
			Origin:   origin,
			KeyID:    KeyID(key),
			Provider: name,
			Harness:  harnessForApp(app),
			Source:   "cc-switch",
			Secret:   NewSecret(key),
		})
	}
	return out, rows.Err()
}

// PriceRules imports the prices stored in the database. Model entries become
// model-level rules carrying unit prices only; provider entries carry a
// multiplier.
func (c *CCSwitch) PriceRules(ctx context.Context) ([]pricing.Rule, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, err := os.Stat(c.path); err != nil {
		return nil, err
	}
	return importPrices(ctx, c.path)
}

// Close releases the read-only handle.
func (c *CCSwitch) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
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

// ccBaseURL extracts the endpoint from a provider's stored settings.
func ccBaseURL(cfg, meta string) string {
	for _, key := range []string{"ANTHROPIC_BASE_URL", "base_url", "baseURL", "OPENAI_BASE_URL"} {
		if v := jsonString(cfg, key); v != "" {
			return v
		}
	}
	return jsonString(meta, "base_url")
}

// ccKey returns the credential configured for a provider, or "". Callers must
// not store or print it: it exists so Credentials can derive the gateway origin
// and a one-way key id, and to hand the reconciliation client a Secret.
func ccKey(cfg, meta string) string {
	for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "apiKey", "api_key", "token", "access_token"} {
		if v := jsonString(cfg, key); v != "" {
			return v
		}
		if v := jsonString(meta, key); v != "" {
			return v
		}
	}
	return ""
}

// hasCCKey reports whether a credential is configured, without reading it.
func hasCCKey(cfg, meta string) bool {
	return ccKey(cfg, meta) != ""
}

// jsonString reads a nested JSON string field, searching env/options/auth first
// because cc-switch stores settings under those keys.
func jsonString(raw, key string) string {
	if raw == "" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return ""
	}
	for _, sub := range []string{"env", "options", "auth", "tokens"} {
		if inner, ok := m[sub].(map[string]any); ok {
			if v, ok := inner[key].(string); ok && v != "" {
				return v
			}
		}
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// importPrices reads cc-switch's price tables through a short-lived read-only
// connection, mirroring the settings import path.
func importPrices(ctx context.Context, path string) ([]pricing.Rule, error) {
	q := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(1500)"}}
	db, err := sql.Open("sqlite", sqlitedsn.URI(path, q.Encode()))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rules, err := ccModelPricing(ctx, tx)
	if err != nil {
		return nil, err
	}
	providers, names, err := ccProviders(ctx, tx)
	if err != nil {
		return nil, err
	}
	// Materialize provider/model selectors: the provider rule carries the
	// multiplier, the model-scoped copy only narrows which prices apply.
	out := rules
	for _, name := range names {
		n := providers[name]
		out = append(out, pricing.Rule{Provider: name, Multiplier: n, Source: "cc-switch"})
		for _, m := range rules {
			m.Provider = name
			out = append(out, m)
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// ccModelPricing reads model_pricing into model-level rules.
func ccModelPricing(ctx context.Context, tx *sql.Tx) ([]pricing.Rule, error) {
	rows, err := tx.QueryContext(ctx, `SELECT model_id,input_cost_per_million,output_cost_per_million,cache_read_cost_per_million,cache_creation_cost_per_million FROM model_pricing ORDER BY model_id`)
	if err != nil {
		return nil, fmt.Errorf("cc-switch model pricing: %w", err)
	}
	models := []pricing.Rule{}
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		var raw [4]string
		if err = rows.Scan(&name, &raw[0], &raw[1], &raw[2], &raw[3]); err != nil {
			rows.Close()
			return nil, err
		}
		rates := [4]float64{}
		for i, v := range raw {
			rates[i], err = strconv.ParseFloat(v, 64)
			if err != nil || !validRate(rates[i]) {
				rows.Close()
				return nil, fmt.Errorf("invalid cc-switch rate for %q: %q", name, v)
			}
		}
		// A model rule supplies unit prices only: its multiplier stays 0
		// ("unset") so the provider's own multiplier still applies.
		r := pricing.Rule{Model: name, Input: &rates[0], Output: &rates[1], CacheRead: &rates[2], CacheWrite: &rates[3], Source: "cc-switch"}
		key := pricing.Normalize(name)
		if !seen[key] {
			models = append(models, r)
			seen[key] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return models, nil
}

// ccProviders reads the per-provider multipliers, tolerating older schemas
// without cost_multiplier/meta columns.
func ccProviders(ctx context.Context, tx *sql.Tx) (map[string]float64, []string, error) {
	cols, err := ccColumns(ctx, tx, "providers")
	if err != nil {
		return nil, nil, err
	}
	if len(cols) == 0 {
		return nil, nil, fmt.Errorf("cc-switch providers table missing")
	}
	globals := map[string]string{}
	pc, err := ccColumns(ctx, tx, "proxy_config")
	if err != nil {
		return nil, nil, err
	}
	if pc["app_type"] && pc["default_cost_multiplier"] {
		rows, err := tx.QueryContext(ctx, "SELECT app_type,default_cost_multiplier FROM proxy_config")
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var app, value string
			if err = rows.Scan(&app, &value); err != nil {
				rows.Close()
				return nil, nil, err
			}
			globals[app] = value
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	multiplier, meta, app := "NULL", "NULL", "''"
	if cols["cost_multiplier"] {
		multiplier = "cost_multiplier"
	}
	if cols["meta"] {
		meta = "meta"
	}
	if cols["app_type"] {
		app = "app_type"
	}
	rows, err := tx.QueryContext(ctx, "SELECT name,"+app+","+multiplier+","+meta+" FROM providers ORDER BY name,"+app)
	if err != nil {
		return nil, nil, err
	}
	providers := map[string]float64{}
	names := []string{}
	for rows.Next() {
		var name, app string
		var value, metadata sql.NullString
		if err = rows.Scan(&name, &app, &value, &metadata); err != nil {
			rows.Close()
			return nil, nil, err
		}
		raw := globals[app]
		if raw == "" {
			raw = "1"
		}
		if !cols["cost_multiplier"] && globals[app] == "" && metadata.Valid {
			var m map[string]json.RawMessage
			if err = json.Unmarshal([]byte(metadata.String), &m); err != nil {
				rows.Close()
				return nil, nil, err
			}
			if v, ok := m["costMultiplier"]; ok && string(v) != "null" {
				var s string
				if json.Unmarshal(v, &s) == nil {
					raw = s
				} else {
					raw = string(v)
				}
			}
		}
		if value.Valid && value.String != "" {
			raw = value.String
		}
		n, e := strconv.ParseFloat(raw, 64)
		if e != nil || !validRate(n) {
			rows.Close()
			return nil, nil, fmt.Errorf("invalid cc-switch multiplier for %q: %q", name, raw)
		}
		if n == 0 {
			n = 1
		}
		if old, ok := providers[name]; ok {
			if old != n {
				rows.Close()
				return nil, nil, fmt.Errorf("cc-switch provider %q has conflicting app multipliers", name)
			}
			continue
		}
		if name != "" {
			providers[name] = n
			names = append(names, name)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	return providers, names, nil
}

func ccColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id, notnull, pk int
		var name, kind string
		var def any
		if err = rows.Scan(&id, &name, &kind, &notnull, &def, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}
