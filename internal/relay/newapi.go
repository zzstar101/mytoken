package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/source"
)

// maxRecentItems is new-api's hard cap on how many log rows one page can hold,
// so the caller knows the window is a rolling one and must poll regularly.
const maxRecentItems = 1000

// newAPIClient talks to a new-api (or QuantumNous/new-api) site.
type newAPIClient struct {
	h    *HTTP
	site Site
	cred source.Credential
}

func (c *newAPIClient) Kind() Kind { return KindNewAPI }

// quotaPerUnit converts a site's internal quota unit into USD. It defaults to
// new-api's 500000 when the status endpoint did not report one.
func (c *newAPIClient) quotaPerUnit() float64 {
	if c.site.QuotaPerUnit > 0 {
		return c.site.QuotaPerUnit
	}
	return 500000
}

func (c *newAPIClient) usd(quota float64) float64 { return quota / c.quotaPerUnit() }

// Balance reads the key's quota: total granted, used, and what is left.
func (c *newAPIClient) Balance(ctx context.Context) (Balance, error) {
	body, err := c.h.get(ctx, c.site.Origin, "/api/usage/token/", c.cred, true)
	if err != nil {
		return Balance{}, err
	}
	var res struct {
		Data struct {
			TotalGranted   *float64 `json:"total_granted"`
			TotalUsed      *float64 `json:"total_used"`
			TotalAvailable *float64 `json:"total_available"`
			UnlimitedQuota bool     `json:"unlimited_quota"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body.body, &res); err != nil {
		return Balance{}, fmt.Errorf("relay: parse %s/api/usage/token/: %w", c.site.Origin, err)
	}
	out := Balance{Origin: c.site.Origin, KeyID: c.site.KeyID, At: time.Now().UTC(), Unlimited: res.Data.UnlimitedQuota}
	if res.Data.TotalAvailable != nil && !res.Data.UnlimitedQuota {
		v := c.usd(*res.Data.TotalAvailable)
		out.RemainingUSD = &v
	}
	if res.Data.TotalUsed != nil {
		v := c.usd(*res.Data.TotalUsed)
		out.UsedUSD = &v
	}
	return out, nil
}

// Snapshot reads /api/pricing. Sites that locked pricing behind a web login
// answer 401 to an sk- key; that is not an error, it just means L1 has to come
// from the bills instead (see RulesFromBills).
func (c *newAPIClient) Snapshot(ctx context.Context) (Snapshot, error) {
	body, err := c.h.get(ctx, c.site.Origin, "/api/pricing", c.cred, true)
	if err != nil {
		var se *statusError
		if errors.As(err, &se) && (se.code == http.StatusUnauthorized || se.code == http.StatusForbidden) {
			return Snapshot{}, fmt.Errorf("relay: %s/api/pricing needs a web session: %w", c.site.Origin, ErrUnsupported)
		}
		return Snapshot{}, err
	}
	var res struct {
		Data []struct {
			ModelName        string  `json:"model_name"`
			QuotaType        int     `json:"quota_type"`
			ModelRatio       float64 `json:"model_ratio"`
			CompletionRatio  float64 `json:"completion_ratio"`
			CacheRatio       float64 `json:"cache_ratio"`
			CreateCacheRatio float64 `json:"create_cache_ratio"`
			ModelPrice       float64 `json:"model_price"`
			BillingMode      string  `json:"billing_mode"`
			BillingExpr      string  `json:"billing_expr"`
		} `json:"data"`
		GroupRatio  map[string]float64 `json:"group_ratio"`
		UsableGroup map[string]any     `json:"usable_group"`
	}
	if err := json.Unmarshal(body.body, &res); err != nil {
		return Snapshot{}, fmt.Errorf("relay: parse %s/api/pricing: %w", c.site.Origin, err)
	}
	out := Snapshot{Ratios: map[string]Ratios{}, At: time.Now().UTC()}
	for _, m := range res.Data {
		r := Ratios{
			Model:       m.ModelRatio,
			Completion:  ratioOr(m.CompletionRatio, 1),
			Cache:       ratioOr(m.CacheRatio, 1),
			CacheCreate: ratioOr(m.CreateCacheRatio, 1),
		}
		if m.QuotaType == 1 {
			// billed per call; model_ratio means nothing
			r.FixedPrice = m.ModelPrice
			r.Model = 0
		}
		if m.BillingMode == "tiered_expr" {
			r.Expr = m.BillingExpr
			if r.Expr == "" {
				continue
			}
		}
		out.Ratios[m.ModelName] = r
	}
	// group_ratio lists every group; usable_group the ones this user may
	// pick. The key bills in one of the latter.
	if len(res.GroupRatio) > 0 {
		out.Groups = map[string]float64{}
		for g, v := range res.GroupRatio {
			if _, ok := res.UsableGroup[g]; ok || len(res.UsableGroup) == 0 {
				out.Groups[g] = v
			}
		}
	}
	return out, nil
}

// Bills reads the key's most recent logs, newest first, capped at 1000 rows.
//
// new-api numbers these rows by their position in the response (1 = newest;
// model/log.go assignDisplayLogIds), not by the database id, so the numbers
// shift on every new request. Each bill therefore gets an id derived from its
// own content (see billID) and afterID is ignored: the window is re-read on
// every poll and the store skips rows it already has unchanged.
func (c *newAPIClient) Bills(ctx context.Context, afterID int64) ([]Bill, error) {
	path := "/api/log/token?p=1&page_size=" + strconv.Itoa(maxRecentItems)
	body, err := c.h.get(ctx, c.site.Origin, path, c.cred, true)
	if err != nil {
		return nil, err
	}
	var res struct {
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body.body, &res); err != nil {
		return nil, fmt.Errorf("relay: parse %s/api/log/token: %w", c.site.Origin, err)
	}
	if res.Success != nil && !*res.Success {
		return nil, fmt.Errorf("relay: %s/api/log/token refused the request: %w", c.site.Origin, errRejected)
	}
	// Released new-api answers with a bare array; some forks page it.
	var items []newAPILog
	if d := strings.TrimSpace(string(res.Data)); d != "" && d != "null" {
		if d[0] == '[' {
			err = json.Unmarshal(res.Data, &items)
		} else {
			var paged struct {
				Items []newAPILog `json:"items"`
			}
			err = json.Unmarshal(res.Data, &paged)
			items = paged.Items
		}
		if err != nil {
			return nil, fmt.Errorf("relay: parse %s/api/log/token: %w", c.site.Origin, err)
		}
	}
	out := make([]Bill, 0, len(items))
	seen := map[int64]int{}
	for _, l := range items {
		bill, err := c.billFrom(l)
		if err != nil {
			return nil, err
		}
		id := c.billID(l)
		// Two rows identical in every field (same second, model, tokens and
		// charge, no request id) keep apart by how many came before them.
		n := seen[id]
		seen[id] = n + 1
		if n > 0 {
			id = mixID(id, uint64(n))
		}
		bill.ID = id
		out = append(out, bill)
	}
	return out, nil
}

// billID is a stable positive id for a log row: the request id when the site
// records one, else the row's own content. The key id is mixed in because
// the store keys bills by origin, and one site may hold several keys.
func (c *newAPIClient) billID(l newAPILog) int64 {
	h := fnv.New64a()
	h.Write([]byte(c.site.KeyID))
	h.Write([]byte{0})
	if l.RequestID != "" {
		h.Write([]byte(l.RequestID))
		h.Write([]byte{0, byte(l.Type)})
	} else {
		fmt.Fprintf(h, "%d|%d|%s|%d|%d|%d|%d|%s", l.CreatedAt, l.Type, l.ModelName, l.Quota, l.PromptTokens, l.CompletionTokens, l.UseTime, l.Other)
	}
	return positive(h.Sum64())
}

func mixID(id int64, n uint64) int64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d#%d", id, n)
	return positive(h.Sum64())
}

func positive(v uint64) int64 {
	v &= math.MaxInt64
	if v == 0 {
		v = 1
	}
	return int64(v)
}

// newAPILog is one row of new-api's log table (model/log.go).
type newAPILog struct {
	ID                int64  `json:"id"`
	CreatedAt         int64  `json:"created_at"`
	Type              int    `json:"type"`
	ModelName         string `json:"model_name"`
	Quota             int64  `json:"quota"`
	PromptTokens      int64  `json:"prompt_tokens"`
	CompletionTokens  int64  `json:"completion_tokens"`
	UseTime           int64  `json:"use_time"`
	IsStream          bool   `json:"is_stream"`
	Group             string `json:"group"`
	RequestID         string `json:"request_id"`
	UpstreamRequestID string `json:"upstream_request_id"`
	Other             string `json:"other"`
}

// newAPIOther is the subset of the log's `other` object that carries the
// ratios and the token accounting (service/log_info_generate.go).
type newAPIOther struct {
	ModelRatio      *float64 `json:"model_ratio"`
	GroupRatio      *float64 `json:"group_ratio"`
	CompletionRatio *float64 `json:"completion_ratio"`
	CacheRatio      *float64 `json:"cache_ratio"`
	ModelPrice      *float64 `json:"model_price"`
	UserGroupRatio  *float64 `json:"user_group_ratio"`
	BillingMode     string   `json:"billing_mode"`
	ExprB64         string   `json:"expr_b64"`

	CacheTokens          *int64   `json:"cache_tokens"`
	CacheCreationTokens  *int64   `json:"cache_creation_tokens"`
	CacheWriteTokens     *int64   `json:"cache_write_tokens"`
	CacheCreationRatio   *float64 `json:"cache_creation_ratio"`
	CacheCreation1h      *int64   `json:"cache_creation_tokens_1h"`
	CacheCreationRatio1h *float64 `json:"cache_creation_ratio_1h"`
	UsageSemantic        string   `json:"usage_semantic"`
	InputTokensTotal     *int64   `json:"input_tokens_total"`
	ReasoningTokens      *int64   `json:"reasoning_tokens"`
	ReasoningEffort      string   `json:"reasoning_effort"`
}

// billFrom turns one log row into a Bill, normalizing the token accounting to
// MyToken's convention: Input excludes cache.
func (c *newAPIClient) billFrom(l newAPILog) (Bill, error) {
	var other newAPIOther
	if l.Other != "" {
		if err := json.Unmarshal([]byte(l.Other), &other); err != nil {
			return Bill{}, fmt.Errorf("relay: log at %d has unreadable other: %w", l.CreatedAt, err)
		}
	}
	var cacheRead, cacheWrite int64
	if other.CacheTokens != nil {
		cacheRead = *other.CacheTokens
	}
	if other.CacheWriteTokens != nil {
		cacheWrite = *other.CacheWriteTokens
	} else if other.CacheCreationTokens != nil {
		cacheWrite = *other.CacheCreationTokens
	}
	var cacheWrite1h int64
	if other.CacheCreation1h != nil {
		cacheWrite1h = min(max(*other.CacheCreation1h, 0), cacheWrite)
	}
	input := l.PromptTokens
	// new-api decides the meaning of prompt_tokens from the request's relay
	// format and records it as other.usage_semantic = "anthropic"
	// (service/text_quota.go usageSemanticFromUsage). Claude-style upstreams
	// already exclude cache from prompt_tokens; every other style includes it.
	if !strings.EqualFold(other.UsageSemantic, "anthropic") {
		input -= cacheRead + cacheWrite
	}
	if input < 0 {
		// OpenAI-style cache writes can overlap prompt_tokens; the remainder
		// must not become a negative base charge.
		input = 0
	}
	out := Bill{
		Origin:            c.site.Origin,
		KeyID:             c.site.KeyID,
		At:                time.Unix(l.CreatedAt, 0).UTC(),
		Model:             l.ModelName,
		Group:             l.Group,
		RequestID:         l.RequestID,
		UpstreamRequestID: l.UpstreamRequestID,
		Tokens: model.Tokens{
			Input:      input,
			Output:     l.CompletionTokens,
			CacheRead:  cacheRead,
			CacheWrite: cacheWrite,
		},
		CacheWrite1h: cacheWrite1h,
		ChargedUSD:   c.usd(float64(l.Quota)),
		Stream:       l.IsStream,
		LatencyMS:    l.UseTime,
		Ratios: Ratios{
			Model:         valueOr(other.ModelRatio, 0),
			Completion:    ratioOr(valueOr(other.CompletionRatio, 0), 1),
			Cache:         ratioOr(valueOr(other.CacheRatio, 0), 1),
			CacheCreate:   ratioOr(valueOr(other.CacheCreationRatio, 0), 1),
			Group:         ratioOr(valueOr(other.GroupRatio, 0), 1),
			FixedPrice:    fixedPrice(other.ModelPrice, other.ModelRatio),
			CacheCreate1h: valueOr(other.CacheCreationRatio1h, 0),
		},
	}
	if other.BillingMode == "tiered_expr" {
		// The ratios are zero here; the expression the site used is in the
		// row itself.
		out.Ratios.FixedPrice = 0
		if raw, err := base64.StdEncoding.DecodeString(other.ExprB64); err == nil {
			out.Ratios.Expr = string(raw)
		}
	}
	switch l.Type {
	case 2:
		out.Type = "consume"
	case 6:
		out.Type = "refund"
	default:
		out.Type = fmt.Sprintf("type-%d", l.Type)
	}
	return out, nil
}

// Daily is not available on new-api: it keeps per-request logs, which Bills
// already covers.
func (c *newAPIClient) Daily(ctx context.Context, from, to time.Time, loc *time.Location) ([]Daily, error) {
	return nil, ErrUnsupported
}

func ratioOr(v, def float64) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return def
	}
	return v
}

func valueOr(v *float64, def float64) float64 {
	if v == nil {
		return def
	}
	return *v
}

// fixedPrice is the per-call price of a row billed per call. new-api logs
// model_price on every row: -1 (or a stale price) when the model is billed
// by ratio, in which case model_ratio is set instead.
func fixedPrice(price, ratio *float64) float64 {
	if price == nil || *price <= 0 || valueOr(ratio, 0) > 0 {
		return 0
	}
	return *price
}
