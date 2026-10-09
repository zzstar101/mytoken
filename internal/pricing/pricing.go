// Package pricing supplies offline-first, refreshable per-million-token prices.
package pricing

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/zzstar/mytoken/internal/model"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:generate go run ./gen

// WriteSnapshot fetches the primary catalog and writes only token prices.
func WriteSnapshot(ctx context.Context, w io.Writer) error {
	p := New("")
	raw, err := p.fetch(ctx, p.modelsURL)
	if err != nil {
		return err
	}
	prices, err := parseModels(raw)
	if err != nil {
		return err
	}
	var compact struct {
		Prices []Price        `json:"prices"`
		Models map[string]int `json:"models"`
	}
	compact.Models = map[string]int{}
	seen := map[string]int{}
	keys := make([]string, 0, len(prices))
	for k := range prices {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := prices[k]
		raw, _ := json.Marshal(v)
		key := string(raw)
		i, ok := seen[key]
		if !ok {
			i = len(compact.Prices)
			seen[key] = i
			compact.Prices = append(compact.Prices, v)
		}
		compact.Models[k] = i
	}
	return json.NewEncoder(w).Encode(compact)
}

//go:embed snapshot.json
var snapshot []byte

// Price contains USD per million tokens; nil Reasoning uses Output.
type Price struct {
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	CacheRead  float64  `json:"cacheRead"`
	CacheWrite float64  `json:"cacheWrite"`
	Reasoning  *float64 `json:"reasoning,omitempty"`
}
type diskCache struct {
	Fetched time.Time        `json:"fetched"`
	Models  map[string]Price `json:"models"`
}
type Pricer struct {
	mu                    sync.RWMutex
	refreshMu             sync.Mutex
	prices                map[string]Price
	aliases               map[string]Price
	attempted             time.Time
	overrides             map[string]map[string]Price
	rules                 map[[2]string]Rule
	modelAliases          map[[2]string]string
	multipliers           map[string]float64
	dir                   string
	fetched               time.Time
	client                *http.Client
	modelsURL, litellmURL string
}

func New(dataDir string) *Pricer {
	p := &Pricer{dir: dataDir, prices: map[string]Price{}, overrides: map[string]map[string]Price{}, multipliers: map[string]float64{}, client: &http.Client{Timeout: 8 * time.Second}, modelsURL: "https://models.dev/api.json", litellmURL: "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"}
	var compact struct {
		Prices []Price        `json:"prices"`
		Models map[string]int `json:"models"`
	}
	if json.Unmarshal(snapshot, &compact) == nil && len(compact.Models) > 0 {
		for k, i := range compact.Models {
			if i >= 0 && i < len(compact.Prices) {
				p.prices[k] = compact.Prices[i]
			}
		}
	} else {
		_ = json.Unmarshal(snapshot, &p.prices)
	}
	p.index()
	if raw, e := os.ReadFile(filepath.Join(dataDir, "prices.json")); e == nil {
		var c diskCache
		if json.Unmarshal(raw, &c) == nil {
			p.merge(c.Models)
			p.fetched = c.Fetched
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dataDir, "prices-attempt.json")); err == nil {
		_ = json.Unmarshal(raw, &p.attempted)
	}
	return p
}

var dateSuffix = regexp.MustCompile(`[-:]\d{4}-?\d{2}-?\d{2}$`)

func Normalize(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, ":latest")
	name = strings.TrimSuffix(name, "-latest")
	name = dateSuffix.ReplaceAllString(name, "")
	return strings.ReplaceAll(name, ".", "-")
}
func (p *Pricer) Lookup(name string) (Price, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lookup(name)
}
func (p *Pricer) lookup(name string) (Price, bool) {
	v, ok := p.aliases[Normalize(name)]
	return v, ok
}

func firstParty(id string) string {
	n := Normalize(id)
	for _, family := range []struct{ prefix, provider string }{
		{"claude-", "anthropic"}, {"gpt-", "openai"}, {"o1", "openai"}, {"o3", "openai"}, {"o4", "openai"},
		{"gemini-", "google"}, {"deepseek-", "deepseek"}, {"glm-", "zhipuai"}, {"kimi-", "moonshotai"},
		{"mistral-", "mistral"}, {"ministral-", "mistral"}, {"codestral", "mistral"}, {"qwen", "alibaba"}, {"step-", "stepfun"},
	} {
		if strings.HasPrefix(n, family.prefix) {
			return family.provider
		}
	}
	return ""
}

// merge overlays one catalog as a unit so a fresh fallback beats the snapshot.
func (p *Pricer) merge(prices map[string]Price) {
	table := &Pricer{prices: prices}
	table.index()
	for k, v := range prices {
		p.prices[k] = v
	}
	for k, v := range table.aliases {
		p.aliases[k] = v
	}
}

func (p *Pricer) index() {
	keys := make([]string, 0, len(p.prices))
	for k := range p.prices {
		keys = append(keys, k)
	}
	rank := func(k string) int {
		provider, _, qualified := strings.Cut(k, "/")
		if qualified && provider == firstParty(k) {
			return 0
		}
		if !qualified {
			return 1
		}
		return 2
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := rank(keys[i]), rank(keys[j])
		if a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
	p.aliases = make(map[string]Price, len(keys))
	for _, k := range keys {
		n := Normalize(k)
		if _, ok := p.aliases[n]; !ok {
			p.aliases[n] = p.prices[k]
		}
	}
}

// HasPrice distinguishes unknown models from legitimately free models.
func (p *Pricer) HasPrice(provider, name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok, _ := p.effective(provider,name)
	return ok
}
func (p *Pricer) SetMultiplier(provider string, multiplier float64) {
	if multiplier < 0 {
		return
	}
	p.mu.Lock()
	p.multipliers[provider] = multiplier
	p.mu.Unlock()
}
func (p *Pricer) SetPrice(provider, name string, price Price) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.overrides[provider] == nil {
		p.overrides[provider] = map[string]Price{}
	}
	p.overrides[provider][Normalize(name)] = price
}
func (p *Pricer) Cost(e model.UsageEvent) float64 {
 cost,_:=p.Evaluate(e)
 return cost
}
func (p *Pricer) fetch(ctx context.Context, url string) ([]byte, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if e != nil {
		return nil, e
	}
	resp, e := p.client.Do(req)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pricing HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}
func (p *Pricer) Refresh(ctx context.Context) error {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	p.mu.RLock()
	fresh := time.Since(p.fetched) < 24*time.Hour || time.Since(p.attempted) < 24*time.Hour
	p.mu.RUnlock()
	if fresh {
		return nil
	}
	p.mu.Lock()
	p.attempted = time.Now()
	attempt, _ := json.Marshal(p.attempted)
	p.mu.Unlock()
	if err := os.MkdirAll(p.dir, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(p.dir, "prices-attempt.json"), attempt, 0600); err != nil {
		return err
	}
	raw, e := p.fetch(ctx, p.modelsURL)
	var prices map[string]Price
	if e == nil {
		prices, e = parseModels(raw)
	}
	if e != nil {
		raw, e = p.fetch(ctx, p.litellmURL)
		if e == nil {
			prices, e = parseLiteLLM(raw)
		}
	}
	if e != nil {
		return e
	}
	now := time.Now()
	p.mu.Lock()
	p.merge(prices)
	p.fetched = now
	p.mu.Unlock()
	cache, _ := json.Marshal(diskCache{Fetched: now, Models: prices})
	if e = os.MkdirAll(p.dir, 0700); e != nil {
		return e
	}
	return os.WriteFile(filepath.Join(p.dir, "prices.json"), cache, 0600)
}
func parseModels(raw []byte) (map[string]Price, error) {
	var data map[string]struct {
		Models map[string]struct {
			ID   string `json:"id"`
			Cost *struct {
				Input, Output float64
				CacheRead     float64  `json:"cache_read"`
				CacheWrite    float64  `json:"cache_write"`
				Reasoning     *float64 `json:"reasoning"`
			}
		}
	}
	if e := json.Unmarshal(raw, &data); e != nil {
		return nil, e
	}
	out := map[string]Price{}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for id, m := range data[k].Models {
			if m.Cost == nil {
				continue
			}
			c := m.Cost
			p := Price{c.Input, c.Output, c.CacheRead, c.CacheWrite, c.Reasoning}
			out[strings.ToLower(k+"/"+id)] = p
		}
	}
	if len(out) == 0 {
		return nil, errors.New("models.dev returned no prices")
	}
	return out, nil
}
func parseLiteLLM(raw []byte) (map[string]Price, error) {
	var data map[string]json.RawMessage
	if e := json.Unmarshal(raw, &data); e != nil {
		return nil, e
	}
	out := map[string]Price{}
	for id, r := range data {
		var m struct {
			Input     *float64 `json:"input_cost_per_token"`
			Output    float64  `json:"output_cost_per_token"`
			Read      float64  `json:"cache_read_input_token_cost"`
			Write     float64  `json:"cache_creation_input_token_cost"`
			Reasoning *float64 `json:"output_cost_per_reasoning_token"`
		}
		if json.Unmarshal(r, &m) != nil || m.Input == nil {
			continue
		}
		if m.Reasoning != nil {
			*m.Reasoning *= 1e6
		}
		out[strings.ToLower(id)] = Price{*m.Input * 1e6, m.Output * 1e6, m.Read * 1e6, m.Write * 1e6, m.Reasoning}
	}
	if len(out) == 0 {
		return nil, errors.New("LiteLLM returned no prices")
	}
	return out, nil
}
