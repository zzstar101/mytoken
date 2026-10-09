package pricing

import (
	"context"
	"encoding/json"
	"github.com/zzstar101/mytoken/internal/model"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLiteLLMFallbackOverridesSnapshot(t *testing.T) {
	primaryCalls, fallbackCalls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/primary" {
			primaryCalls++
			http.Error(w, "offline", 503)
			return
		}
		fallbackCalls++
		w.Write([]byte(`{"claude-opus-4-5":{"input_cost_per_token":0.000007,"output_cost_per_token":0.000030}}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	p := New(dir)
	p.modelsURL = srv.URL + "/primary"
	p.litellmURL = srv.URL + "/fallback"
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, current := range []*Pricer{p, New(dir)} {
		if v, ok := current.Lookup("anthropic/claude-opus-4.5"); !ok || v.Input != 7 {
			t.Fatalf("fallback not applied: %+v", v)
		}
		current.modelsURL = srv.URL + "/primary"
		current.litellmURL = srv.URL + "/fallback"
		if err := current.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if primaryCalls != 1 || fallbackCalls != 1 {
		t.Fatalf("fetch calls %d %d", primaryCalls, fallbackCalls)
	}
}

func TestLookupNormalization(t *testing.T) {
	p := New(t.TempDir())
	for _, name := range []string{" CLAUDE-OPUS-4.5 ", "anthropic/claude-opus-4-5", "claude-opus-4.5-20251101", "claude-opus-4-5-latest"} {
		if price, ok := p.Lookup(name); !ok || price.Input != 5 {
			t.Errorf("%q: %+v %v", name, price, ok)
		}
	}
	if _, ok := p.Lookup("definitely-not-a-real-model-xyz"); ok {
		t.Fatal("invented price for unknown model")
	}
}

func TestFirstParty(t *testing.T) {
	raw := []byte(`{"aaa":{"models":{"claude-opus-4.5":{"cost":{"input":99,"output":99}}}},"anthropic":{"models":{"claude-opus-4-5":{"cost":{"input":5,"output":25}}}}}`)
	prices, err := parseModels(raw)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	data, _ := json.Marshal(diskCache{Models: prices})
	if err := os.WriteFile(filepath.Join(dir, "prices.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if v, _ := New(dir).Lookup("claude-opus-4.5-latest"); v.Input != 5 {
		t.Fatal(v)
	}
}

func TestFailedRefreshRetainsOfflinePrices(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "offline", 503) }))
	defer srv.Close()
	for _, cached := range []bool{false, true} {
		dir := t.TempDir()
		if cached {
			raw, _ := json.Marshal(diskCache{Fetched: time.Now().Add(-48 * time.Hour), Models: map[string]Price{"offline-pricing-test-custom": {Input: 7}}})
			if err := os.WriteFile(filepath.Join(dir, "prices.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
		p := New(dir)
		p.modelsURL = srv.URL
		p.litellmURL = srv.URL
		before := calls
		if err := p.Refresh(context.Background()); err == nil {
			t.Fatal("expected network error")
		}
		_ = p.Refresh(context.Background())
		if calls-before != 2 {
			t.Fatalf("retried within 24h: %d requests", calls-before)
		}
		if _, ok := p.Lookup("claude-sonnet-4"); !ok {
			t.Fatal("snapshot lost")
		}
		if cached {
			if v, ok := p.Lookup("offline-pricing-test-custom"); !ok || v.Input != 7 {
				t.Fatal("cache lost")
			}
		}
	}
}

func TestOfflineAndLogCost(t *testing.T) {
	p := New(t.TempDir())
	e := model.UsageEvent{Model: "anthropic/claude-sonnet-4-5-20250929", Tokens: model.Tokens{Input: 1000000, Output: 1000000, CacheRead: 1000000, CacheWrite: 1000000, Reasoning: 1000000}}
	got := p.Cost(e)
	if math.Abs(got-37.05) > 1e-8 {
		t.Fatalf("cost %v", got)
	}
	zero := 0.0
	e.CostUSD = &zero
	if p.Cost(e) != 0 {
		t.Fatal("log zero cost lost")
	}
}
func TestRefreshAndDiskCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"test":{"models":{"pricing-test-custom":{"id":"pricing-test-custom","cost":{"input":2,"output":4,"cache_read":0.2,"cache_write":2.5}}}}}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	p := New(dir)
	p.modelsURL = srv.URL
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := model.UsageEvent{Model: "pricing-test-custom", Tokens: model.Tokens{Input: 1000000, Reasoning: 1000000}}
	if p.Cost(e) != 6 {
		t.Fatalf("cost=%v pricing-test-custom=%+v fetched=%v aliases=%d", p.Cost(e), p.prices["test/pricing-test-custom"], p.fetched, len(p.aliases))
	}
	if New(dir).Cost(e) != 6 {
		t.Fatal("disk cache not read")
	}
}
