package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/source"
	"github.com/zzstar101/mytoken/internal/store"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

type settingsService struct {
	st      *store.Store
	p       *pricing.Pricer
	mu      sync.Mutex
	sources []source.Source
}

var _ Settings = (*settingsService)(nil)

func NewSettings(st *store.Store, p *pricing.Pricer) Settings {
	return NewSettingsWithSources(st, p, source.NewCCSwitch(source.CCSwitchPath()))
}

// NewSettingsWithSources is NewSettings with explicit provider sources; price
// import uses whichever source implements source.Pricing.
func NewSettingsWithSources(st *store.Store, p *pricing.Pricer, sources ...source.Source) Settings {
	return &settingsService{st: st, p: p, sources: sources}
}
func readSetting[T any](ctx context.Context, st *store.Store, key string) ([]T, error) {
	raw, err := st.Setting(ctx, key)
	out := []T{}
	if err == nil && raw != "" {
		err = json.Unmarshal([]byte(raw), &out)
	}
	return out, err
}
func (s *settingsService) PriceRules(ctx context.Context) ([]PriceRule, error) {
	return readSetting[PriceRule](ctx, s.st, "price-rules")
}
func (s *settingsService) ModelAliases(ctx context.Context) ([]ModelAlias, error) {
	return readSetting[ModelAlias](ctx, s.st, "model-aliases")
}
func pricingRules(rules []PriceRule) []pricing.Rule {
	out := make([]pricing.Rule, len(rules))
	for i, r := range rules {
		out[i] = pricing.Rule{Provider: r.Provider, Model: r.Model, Multiplier: r.Multiplier, Input: r.Input, Output: r.Output, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite, Source: r.Source, From: r.From}
	}
	return out
}

// fromPricingRules converts a provider source's rules into the stored contract.
func fromPricingRules(rules []pricing.Rule) []PriceRule {
	out := make([]PriceRule, len(rules))
	for i, r := range rules {
		out[i] = PriceRule{Provider: r.Provider, Model: r.Model, Multiplier: r.Multiplier, Input: r.Input, Output: r.Output, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite, Source: r.Source, From: r.From}
	}
	return out
}
func aliasMap(aliases []ModelAlias) map[[2]string]string {
	out := map[[2]string]string{}
	for _, a := range aliases {
		out[[2]string{a.Provider, a.From}] = a.To
	}
	return out
}
func LoadPricingSettings(ctx context.Context, st *store.Store, p *pricing.Pricer) error {
	s := NewSettings(st, p).(*settingsService)
	rules, err := s.PriceRules(ctx)
	if err != nil {
		return err
	}
	aliases, err := s.ModelAliases(ctx)
	if err != nil {
		return err
	}
	p.SetRules(pricingRules(rules))
	p.SetAliases(aliasMap(aliases))
	return nil
}
func validRate(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
func (s *settingsService) SetPriceRules(ctx context.Context, rules []PriceRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setPriceRules(ctx, rules)
}
func (s *settingsService) setPriceRules(ctx context.Context, rules []PriceRule) error {
	rules = append([]PriceRule{}, rules...)
	seen := map[[4]string]bool{}
	for i := range rules {
		r := &rules[i]
		r.Provider = strings.TrimSpace(r.Provider)
		r.Model = strings.TrimSpace(r.Model)
		if !r.From.IsZero() {
			r.From = r.From.UTC()
		}
		if r.Source == "" {
			r.Source = "user"
		}
		if r.Provider == "" && r.Model == "" {
			return fmt.Errorf("price rule requires provider or model")
		}
		if r.Source != "user" && r.Source != "cc-switch" {
			return fmt.Errorf("invalid price source %q", r.Source)
		}
		if !validRate(r.Multiplier) {
			return fmt.Errorf("invalid multiplier")
		}
		for _, v := range []*float64{r.Input, r.Output, r.CacheRead, r.CacheWrite} {
			if v != nil && !validRate(*v) {
				return fmt.Errorf("invalid token price")
			}
		}
		// Same selector and start instant is a duplicate; a later From makes it
		// a separate, time-effective rule.
		key := [4]string{r.Provider, pricing.Normalize(r.Model), r.Source, r.From.UTC().Format(time.RFC3339Nano)}
		if seen[key] {
			return fmt.Errorf("duplicate price rule for %q/%q", r.Provider, r.Model)
		}
		seen[key] = true
	}
	old, err := s.PriceRules(ctx)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return err
	}
	s.p.SetRules(pricingRules(rules))
	err = s.st.ReplacePricing(ctx, map[string]string{"price-rules": string(raw)}, s.p.Evaluate)
	if err != nil {
		s.p.SetRules(pricingRules(old))
	}
	return err
}
func (s *settingsService) SetModelAliases(ctx context.Context, aliases []ModelAlias) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	aliases = append([]ModelAlias{}, aliases...)
	seen := map[[2]string]bool{}
	for i := range aliases {
		a := &aliases[i]
		a.From = strings.TrimSpace(a.From)
		a.To = strings.TrimSpace(a.To)
		if a.From == "" || a.To == "" || a.From == a.To {
			return fmt.Errorf("alias requires distinct nonempty model names")
		}
		key := [2]string{a.Provider, a.From}
		if seen[key] {
			return fmt.Errorf("duplicate model alias")
		}
		seen[key] = true
	}
	// Aliases are one-hop: reject chains so pricing and query display cannot diverge.
	for _, a := range aliases {
		for _, b := range aliases {
			if a.To == b.From && (b.Provider == "" || a.Provider == b.Provider) {
				return fmt.Errorf("model alias chains are not supported")
			}
		}
	}
	old, err := s.ModelAliases(ctx)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(aliases)
	if err != nil {
		return err
	}
	s.p.SetAliases(aliasMap(aliases))
	err = s.st.ReplacePricing(ctx, map[string]string{"model-aliases": string(raw)}, s.p.Evaluate)
	if err != nil {
		s.p.SetAliases(aliasMap(old))
	}
	return err
}
func (s *settingsService) ImportCCSwitch(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rules []PriceRule
	found := false
	for _, src := range s.sources {
		p, ok := src.(source.Pricing)
		if !ok {
			continue
		}
		found = true
		imported, err := p.PriceRules(ctx)
		if err != nil {
			return 0, err
		}
		rules = append(rules, fromPricingRules(imported)...)
	}
	if !found {
		return 0, errors.New("no provider source can import prices")
	}
	old, err := s.PriceRules(ctx)
	if err != nil {
		return 0, err
	}
	for _, r := range old {
		if r.Source != "cc-switch" {
			rules = append(rules, r)
		}
	}
	count := 0
	for _, r := range rules {
		if r.Source == "cc-switch" {
			count++
		}
	}
	if err = s.setPriceRules(ctx, rules); err != nil {
		return 0, err
	}
	return count, nil
}
func (s *settingsService) Providers(ctx context.Context) ([]string, error) {
	rows, err := s.st.DB().QueryContext(ctx, "SELECT DISTINCT resolved_provider FROM events WHERE resolved_provider!='' ORDER BY resolved_provider")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
func (s *settingsService) SearchCatalog(ctx context.Context, q string, limit int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.p.SearchCatalog(q, limit), nil
}
func (s *settingsService) UnpricedModels(ctx context.Context) ([]UnpricedModel, error) {
	rows, err := s.st.DB().QueryContext(ctx, "SELECT model,resolved_provider,count(*),sum(input+output+cache_read+cache_write+reasoning) FROM events WHERE priced=0 GROUP BY model,resolved_provider")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UnpricedModel{}
	for rows.Next() {
		var v UnpricedModel
		if err = rows.Scan(&v.Model, &v.Provider, &v.Requests, &v.Tokens); err != nil {
			return nil, err
		}
		v.Model = s.p.CanonicalModel(v.Provider, v.Model)
		v.Suggestions = s.p.SearchCatalog(v.Model, 5)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tokens != out[j].Tokens {
			return out[i].Tokens > out[j].Tokens
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Provider < out[j].Provider
	})
	return out, rows.Err()
}
