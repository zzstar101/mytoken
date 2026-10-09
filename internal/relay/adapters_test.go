package relay

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/source"
)

// --- Detect ---------------------------------------------------------------

// TestDetectOrder pins docs/RELAY.md §3: an official host is decided without a
// request, sub2api wins over new-api when both markers exist, and a plain site
// with no marker stays unknown.
func TestDetectOrder(t *testing.T) {
	ctx := context.Background()

	// 1. A site that answers 404 everywhere stays unknown, and only the two
	//    documented probes were tried.
	g := newGateway()
	origin, h := g.site(t)
	site, err := Detect(ctx, h, origin, cred(origin))
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if site.Kind != KindUnknown || site.Layers != 0 {
		t.Fatalf("unknown site = %+v", site)
	}
	if got := g.requests(); len(got) != 2 || got[0] != "GET /v1/sub2api/billing" || got[1] != "GET /api/status" {
		t.Fatalf("requests = %v", got)
	}

	for _, tc := range []struct {
		name string
		host string
		want Kind
	}{
		{"deepseek", "api.deepseek.com", KindDeepSeek},
		{"openrouter", "openrouter.ai", KindOpenRouter},
		{"siliconflow cn", "api.siliconflow.cn", KindSilicon},
		{"siliconflow com", "api.siliconflow.com", KindSilicon},
		{"moonshot cn", "api.moonshot.cn", KindMoonshot},
		{"zai", "open.bigmodel.cn", KindZAI},
		{"minimax", "api.minimax.chat", KindMiniMax},
	} {
		t.Run("official/"+tc.name, func(t *testing.T) {
			got, ok := officialKind("https://" + tc.host)
			if !ok || got != tc.want {
				t.Fatalf("officialKind(%s) = %v,%v want %v", tc.host, got, ok, tc.want)
			}
		})
	}

	// 2. sub2api's marker beats new-api's, even when /api/status also answers.
	g2 := newGateway()
	g2.handle("/v1/sub2api/billing", func(w http.ResponseWriter, r *http.Request) {
		jsonBody(w, 200, map[string]any{"effective_rate_multiplier": 0.5})
	})
	g2.handle("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-new-api-version", "v0.0.1")
		jsonBody(w, 200, map[string]any{"data": map[string]any{"quota_per_unit": 500000}})
	})
	origin2, h2 := g2.site(t)
	site2, err := Detect(ctx, h2, origin2, cred(origin2))
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if site2.Kind != KindSub2API {
		t.Fatalf("kind = %v, want sub2api (sub2api marker wins)", site2.Kind)
	}
	if !site2.Layers.Has(LayerRatio) || !site2.Layers.Has(LayerBalance) || !site2.Layers.Has(LayerBills) {
		t.Fatalf("sub2api layers = %v", site2.Layers.LayerNames())
	}

	// 3. new-api's version header decides when sub2api is silent.
	g3 := newGateway()
	g3.handle("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-new-api-version", "v1.0.0-rc.39")
		jsonBody(w, 200, map[string]any{"data": map[string]any{"quota_per_unit": 1000000}})
	})
	origin3, h3 := g3.site(t)
	site3, err := Detect(ctx, h3, origin3, cred(origin3))
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if site3.Kind != KindNewAPI {
		t.Fatalf("kind = %v, want newapi", site3.Kind)
	}
	if site3.Version != "v1.0.0-rc.39" || site3.QuotaPerUnit != 1000000 {
		t.Fatalf("version/qpu = %q/%v", site3.Version, site3.QuotaPerUnit)
	}

	// 4. A rejected key is fatal: no point probing the rest.
	g5 := newGateway()
	g5.handle("/v1/sub2api/billing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	origin5, h5 := g5.site(t)
	if _, err := Detect(ctx, h5, origin5, cred(origin5)); !errors.Is(err, errKeyRejected) {
		t.Fatalf("err = %v, want errKeyRejected", err)
	}
	forbidErr(t, err)
	if len(g5.requests()) != 1 {
		t.Fatalf("requests = %v, want the probe to stop", g5.requests())
	}
}

// TestDetectOfficialHostNeedsNoRequest guards the offline path: an official
// origin is classified from the host table, so no traffic is generated.
func TestDetectOfficialHostNeedsNoRequest(t *testing.T) {
	ctx := context.Background()
	g := newGateway()
	origin, h := g.site(t)
	// Point the host table at the test server by faking the origin's host.
	officialKinds["127.0.0.1"] = KindDeepSeek
	t.Cleanup(func() { delete(officialKinds, "127.0.0.1") })
	site, err := Detect(ctx, h, origin, cred(origin))
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if site.Kind != KindDeepSeek {
		t.Fatalf("kind = %v, want deepseek", site.Kind)
	}
	if site.Layers != LayerBalance {
		t.Fatalf("layers = %v, want balance only", site.Layers.LayerNames())
	}
	if len(g.requests()) != 0 {
		t.Fatalf("official host made requests: %v", g.requests())
	}
}

// TestUnknownKindClient confirms New refuses a site it cannot talk to instead
// of guessing.
func TestUnknownKindClient(t *testing.T) {
	_, h := newGateway().site(t)
	if _, err := New(h, Site{Kind: KindUnknown, Origin: "https://x.invalid"}, cred("https://x.invalid")); err == nil {
		t.Fatal("New(unknown) should fail")
	}
}

// --- new-api ---------------------------------------------------------------

// newAPISite builds a detected new-api site backed by a fixture gateway.
func newAPISite(t *testing.T, fn func(g *gateway)) (origin string, h *HTTP) {
	t.Helper()
	g := newGateway()
	g.handle("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-new-api-version", "v1.0.0")
		jsonBody(w, 200, map[string]any{"data": map[string]any{"quota_per_unit": 500000}})
	})
	g.handle("/v1/sub2api/billing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusNotFound)
	})
	fn(g)
	return g.site(t)
}

func TestNewAPIBalance(t *testing.T) {
	ctx := context.Background()
	origin, h := newAPISite(t, func(g *gateway) {
		g.handle("/api/usage/token/", func(w http.ResponseWriter, r *http.Request) {
			jsonBody(w, 200, map[string]any{"data": map[string]any{
				"total_granted": 500000, "total_used": 120000, "total_available": 380000,
			}})
		})
	})
	site, err := Detect(ctx, h, origin, cred(origin))
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	c, err := New(h, site, cred(origin))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	bal, err := c.Balance(ctx)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	// 380000 quota / 500000 quota-per-unit = $0.76 left.
	if bal.RemainingUSD == nil || *bal.RemainingUSD != 0.76 {
		t.Fatalf("remaining = %v, want 0.76", bal.RemainingUSD)
	}
	if bal.UsedUSD == nil || *bal.UsedUSD != 0.24 {
		t.Fatalf("used = %v, want 0.24", bal.UsedUSD)
	}
	if bal.Origin != origin || bal.KeyID != source.KeyID(sentinelKey) {
		t.Fatalf("balance identity = %+v", bal)
	}
}

// TestNewAPIPricingLockedBehindWebLogin checks the documented behavior: a site
// that answers 401 on /api/pricing is not an error, L1 simply has to come from
// the bills.
func TestNewAPIPricingLockedBehindWebLogin(t *testing.T) {
	ctx := context.Background()
	origin, h := newAPISite(t, func(g *gateway) {
		g.handle("/api/pricing", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	})
	site, _ := Detect(ctx, h, origin, cred(origin))
	c, err := New(h, site, cred(origin))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := c.Snapshot(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("snapshot err = %v, want ErrUnsupported", err)
	}
	forbidErr(t, err)
}

// TestNewAPIBillsTokenSemantics is the core normalization test: the same log
// row means different token counts depending on other.usage_semantic.
func TestNewAPIBillsTokenSemantics(t *testing.T) {
	ctx := context.Background()
	origin, h := newAPISite(t, func(g *gateway) {
		g.handle("/api/log/token", func(w http.ResponseWriter, r *http.Request) {
			jsonBody(w, 200, map[string]any{"data": map[string]any{"items": [](map[string]any){
				{
					"id": 11, "created_at": 1_760_000_000, "type": 2, "model_name": "claude-sonnet-4-5",
					"quota": 1_500_000, "prompt_tokens": 1000, "completion_tokens": 200,
					"use_time": 900, "is_stream": true, "group": "vip",
					"request_id": "req-1", "upstream_request_id": "up-1",
					"other": `{"model_ratio":2,"group_ratio":0.3,"completion_ratio":5,"cache_ratio":0.1,
						"cache_creation_ratio":1.25,"cache_tokens":300,"cache_write_tokens":400,
						"usage_semantic":"anthropic"}`,
				},
				{
					"id": 12, "created_at": 1_760_000_100, "type": 2, "model_name": "gpt-5-codex",
					"quota": 900_000, "prompt_tokens": 1000, "completion_tokens": 200,
					"use_time": 400, "group": "default",
					"request_id": "req-2",
					"other": `{"model_ratio":1,"group_ratio":1,"completion_ratio":4,
						"cache_tokens":300,"cache_creation_tokens":400,"usage_semantic":"openai"}`,
				},
			}}})
		})
	})
	site, _ := Detect(ctx, h, origin, cred(origin))
	c, _ := New(h, site, cred(origin))
	bills, err := c.Bills(ctx, 0)
	if err != nil {
		t.Fatalf("bills: %v", err)
	}
	if len(bills) != 2 {
		t.Fatalf("bills = %d, want 2", len(bills))
	}
	// Claude-style: prompt_tokens already excludes cache, so Input stays 1000.
	if got := bills[0].Tokens; got.Input != 1000 || got.CacheRead != 300 || got.CacheWrite != 400 {
		t.Fatalf("anthropic tokens = %+v", got)
	}
	// OpenAI-style: prompt_tokens includes cache, so it is subtracted.
	if got := bills[1].Tokens; got.Input != 300 || got.CacheRead != 300 || got.CacheWrite != 400 {
		t.Fatalf("openai tokens = %+v, want Input 300", got)
	}
	// Money is the site's own number, untouched by normalization.
	if bills[0].ChargedUSD != 3 || bills[1].ChargedUSD != 1.8 {
		t.Fatalf("charged = %v/%v, want 3/1.8", bills[0].ChargedUSD, bills[1].ChargedUSD)
	}
	if bills[0].Type != "consume" {
		t.Fatalf("type = %q", bills[0].Type)
	}
	// Ratios come back so L1 can be rebuilt from L3.
	if bills[0].Ratios.Model != 2 || bills[0].Ratios.Group != 0.3 || bills[0].Ratios.Completion != 5 || bills[0].Ratios.Cache != 0.1 {
		t.Fatalf("ratios = %+v", bills[0].Ratios)
	}
	if bills[0].RequestID != "req-1" || bills[0].UpstreamRequestID != "up-1" {
		t.Fatalf("ids = %q/%q", bills[0].RequestID, bills[0].UpstreamRequestID)
	}
	// afterID skips rows the caller already has.
	bills, err = c.Bills(ctx, 11)
	if err != nil || len(bills) != 1 || bills[0].ID != 12 {
		t.Fatalf("bills after id 11 = %+v (%v)", bills, err)
	}
}

// TestNewAPIBillsRefundIsNegative keeps the conservation contract astra-perf
// relies on: a refund log carries a negative charge.
func TestNewAPIBillsRefundIsNegative(t *testing.T) {
	ctx := context.Background()
	origin, h := newAPISite(t, func(g *gateway) {
		g.handle("/api/log/token", func(w http.ResponseWriter, r *http.Request) {
			jsonBody(w, 200, map[string]any{"data": map[string]any{"items": [](map[string]any){
				{"id": 20, "created_at": 1_760_000_000, "type": 6, "model_name": "claude-sonnet-4-5",
					"quota": -1_500_000, "prompt_tokens": 1000, "completion_tokens": 200, "group": "vip",
					"other": `{"model_ratio":2,"group_ratio":0.3,"usage_semantic":"anthropic"}`},
			}}})
		})
	})
	site, _ := Detect(ctx, h, origin, cred(origin))
	c, _ := New(h, site, cred(origin))
	bills, err := c.Bills(ctx, 0)
	if err != nil {
		t.Fatalf("bills: %v", err)
	}
	if len(bills) != 1 || bills[0].Type != "refund" {
		t.Fatalf("bills = %+v", bills)
	}
	if bills[0].ChargedUSD >= 0 {
		t.Fatalf("refund charged = %v, want negative", bills[0].ChargedUSD)
	}
}

// TestNewAPIBillsFixedPriceModel covers per-call billing: model_price is set,
// so the token prices the rules derive must be zero.
func TestNewAPIBillsFixedPriceModel(t *testing.T) {
	ctx := context.Background()
	origin, h := newAPISite(t, func(g *gateway) {
		g.handle("/api/log/token", func(w http.ResponseWriter, r *http.Request) {
			jsonBody(w, 200, map[string]any{"data": map[string]any{"items": [](map[string]any){
				{"id": 30, "created_at": 1_760_000_000, "type": 2, "model_name": "sora-2",
					"quota": 2_500_000, "prompt_tokens": 0, "completion_tokens": 0, "group": "vip",
					"other": `{"model_ratio":0,"model_price":2.5,"group_ratio":0.3,"usage_semantic":"anthropic"}`},
			}}})
		})
	})
	site, _ := Detect(ctx, h, origin, cred(origin))
	c, _ := New(h, site, cred(origin))
	bills, err := c.Bills(ctx, 0)
	if err != nil {
		t.Fatalf("bills: %v", err)
	}
	if bills[0].Ratios.FixedPrice != 2.5 {
		t.Fatalf("fixed price = %v, want 2.5", bills[0].Ratios.FixedPrice)
	}
	in, out, cr, cw := unitPrices(bills[0].Ratios)
	if in != 0 || out != 0 || cr != 0 || cw != 0 {
		t.Fatalf("unit prices = %v/%v/%v/%v, want all zero for a per-call model", in, out, cr, cw)
	}
	// The money is still the site's own figure.
	if bills[0].ChargedUSD != 5 {
		t.Fatalf("charged = %v, want 5", bills[0].ChargedUSD)
	}
}

// TestNewAPIBadJSON checks a malformed body is an error naming the origin.
func TestNewAPIBadJSON(t *testing.T) {
	ctx := context.Background()
	origin, h := newAPISite(t, func(g *gateway) {
		g.handle("/api/log/token", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("<html>gateway error page</html>"))
		})
	})
	site, _ := Detect(ctx, h, origin, cred(origin))
	c, _ := New(h, site, cred(origin))
	if _, err := c.Bills(ctx, 0); err == nil {
		t.Fatal("bad JSON should fail")
	} else {
		forbidErr(t, err)
		if !strings.Contains(err.Error(), origin) {
			t.Fatalf("err = %v, want the origin", err)
		}
	}
	if _, err := c.Daily(ctx, time.Now(), time.Now(), nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("daily err = %v, want ErrUnsupported", err)
	}
}

// --- sub2api ---------------------------------------------------------------

func TestSub2APIBalanceAndSnapshot(t *testing.T) {
	ctx := context.Background()
	g := newGateway()
	g.handle("/v1/sub2api/billing", func(w http.ResponseWriter, r *http.Request) {
		jsonBody(w, 200, map[string]any{
			"group_rate_multiplier": 1, "resolved_rate_multiplier": 0.4,
			"peak_rate_enabled": true, "peak_start": "09:00", "peak_end": "18:00",
			"peak_rate_multiplier": 1.5, "applied_peak_multiplier": 1.5,
			"effective_rate_multiplier": 0.6, "timezone": "Asia/Shanghai",
		})
	})
	g.handle("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		jsonBody(w, 200, map[string]any{
			"mode":  "quota_limited",
			"quota": map[string]any{"limit": 100, "used": 30, "remaining": 70, "unit": "USD"},
		})
	})
	origin, h := g.site(t)
	site, err := Detect(ctx, h, origin, cred(origin))
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	c, err := New(h, site, cred(origin))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	bal, err := c.Balance(ctx)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.RemainingUSD == nil || *bal.RemainingUSD != 70 {
		t.Fatalf("remaining = %v, want 70", bal.RemainingUSD)
	}
	snap, err := c.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Multiplier == nil || *snap.Multiplier != 0.6 {
		t.Fatalf("multiplier = %v, want 0.6 (resolved × peak)", snap.Multiplier)
	}
	if snap.Peak == nil || snap.Peak.Start != "09:00" || snap.Peak.End != "18:00" || snap.Peak.Multiplier != 1.5 {
		t.Fatalf("peak = %+v", snap.Peak)
	}
	if _, err := c.Bills(ctx, 0); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("bills err = %v, want ErrUnsupported", err)
	}
}

func TestSub2APIDaily(t *testing.T) {
	ctx := context.Background()
	g := newGateway()
	g.handle("/v1/sub2api/billing", func(w http.ResponseWriter, r *http.Request) {
		jsonBody(w, 200, map[string]any{"effective_rate_multiplier": 0.5})
	})
	g.handle("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("start_date"); got != "2026-01-01" {
			t.Errorf("start_date = %q", got)
		}
		if got := r.URL.Query().Get("end_date"); got != "2026-01-03" {
			t.Errorf("end_date = %q, want the day before 'to'", got)
		}
		if got := r.URL.Query().Get("timezone"); got == "" {
			t.Error("timezone missing")
		}
		jsonBody(w, 200, map[string]any{
			"daily_usage": []map[string]any{
				{"date": "2026-01-02", "requests": 9, "input_tokens": 1000, "output_tokens": 200,
					"cache_read_tokens": 50, "cache_write_tokens": 60, "cost": 4, "actual_cost": 2},
			},
			"model_stats": []map[string]any{
				{"model": "claude-sonnet-4-5", "requests": 5, "input_tokens": 600, "output_tokens": 100,
					"cache_creation_tokens": 60, "cache_read_tokens": 50, "cost": 3, "actual_cost": 1.5},
			},
		})
	})
	origin, h := g.site(t)
	site, _ := Detect(ctx, h, origin, cred(origin))
	c, _ := New(h, site, cred(origin))
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("no tzdata")
	}
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, loc)
	to := time.Date(2026, 1, 4, 0, 0, 0, 0, loc)
	rows, err := c.Daily(ctx, from, to, loc)
	if err != nil {
		t.Fatalf("daily: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 1 day + 1 model", len(rows))
	}
	if rows[0].Day != "2026-01-02" || rows[0].ListUSD != 4 || rows[0].ChargedUSD != 2 {
		t.Fatalf("day row = %+v", rows[0])
	}
	if rows[1].Model != "claude-sonnet-4-5" || rows[1].Tokens.CacheWrite != 60 || rows[1].ChargedUSD != 1.5 {
		t.Fatalf("model row = %+v", rows[1])
	}
}

// --- official --------------------------------------------------------------

func TestOfficialBalances(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		kind Kind
		body any
		want float64
	}{
		{"deepseek", KindDeepSeek, map[string]any{"balance_infos": []map[string]any{
			{"currency": "CNY", "total_balance": 20},
			{"currency": "USD", "total_balance": 5.5},
		}}, 5.5},
		{"siliconflow", KindSilicon, map[string]any{"data": map[string]any{"balance": 3, "chargeBalance": 1, "totalBalance": 4}}, 4},
		{"openrouter key endpoint", KindOpenRouter, map[string]any{"data": map[string]any{"usage": 2, "limit": 10}}, 8},
		{"openrouter credits endpoint", KindOpenRouter, map[string]any{"data": map[string]any{"total_credits": 10, "total_usage": 3}}, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway()
			g.handle("/user/balance", func(w http.ResponseWriter, r *http.Request) { jsonBody(w, 200, tc.body) })
			g.handle("/v1/user/info", func(w http.ResponseWriter, r *http.Request) { jsonBody(w, 200, tc.body) })
			g.handle("/api/v1/key", func(w http.ResponseWriter, r *http.Request) { jsonBody(w, 200, tc.body) })
			g.handle("/api/v1/credits", func(w http.ResponseWriter, r *http.Request) { jsonBody(w, 200, tc.body) })
			g.handle("/v1/users/me/balance", func(w http.ResponseWriter, r *http.Request) {
				jsonBody(w, 200, map[string]any{"data": map[string]any{"available_balance": 6}})
			})
			origin, h := g.site(t)
			c := &officialClient{h: h, site: Site{Origin: origin, KeyID: source.KeyID(sentinelKey)}, cred: cred(origin), kind: tc.kind}
			bal, err := c.Balance(ctx)
			if err != nil {
				t.Fatalf("balance: %v", err)
			}
			if bal.RemainingUSD == nil || *bal.RemainingUSD != tc.want {
				t.Fatalf("remaining = %v, want %v", bal.RemainingUSD, tc.want)
			}
			if _, err := c.Snapshot(ctx); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("snapshot err = %v", err)
			}
		})
	}
}

// TestOfficialUnsupportedKinds keeps the "do not guess" promise for the two
// official APIs whose balance contract could not be confirmed.
func TestOfficialUnsupportedKinds(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []Kind{KindZAI, KindMiniMax} {
		g := newGateway()
		origin, h := g.site(t)
		c := &officialClient{h: h, site: Site{Origin: origin}, cred: cred(origin), kind: kind}
		if _, err := c.Balance(ctx); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%s balance err = %v, want ErrUnsupported", kind, err)
		}
		if len(g.requests()) != 0 {
			t.Fatalf("%s made a request for an endpoint we do not know", kind)
		}
	}
}

// TestOfficialEndpointFallback covers a site that moved its balance endpoint:
// the first candidate 404s, the second answers.
func TestOfficialEndpointFallback(t *testing.T) {
	ctx := context.Background()
	g := newGateway()
	g.handle("/api/v1/key", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	})
	g.handle("/api/v1/credits", func(w http.ResponseWriter, r *http.Request) {
		jsonBody(w, 200, map[string]any{"data": map[string]any{"total_credits": 12, "total_usage": 2}})
	})
	origin, h := g.site(t)
	c := &officialClient{h: h, site: Site{Origin: origin}, cred: cred(origin), kind: KindOpenRouter}
	bal, err := c.Balance(ctx)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.RemainingUSD == nil || *bal.RemainingUSD != 10 {
		t.Fatalf("remaining = %v, want 10", bal.RemainingUSD)
	}
}
