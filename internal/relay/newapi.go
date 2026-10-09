package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
			ModelName       string  `json:"model_name"`
			ModelRatio      float64 `json:"model_ratio"`
			CompletionRatio float64 `json:"completion_ratio"`
			CacheRatio      float64 `json:"cache_ratio"`
			ModelPrice      float64 `json:"model_price"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body.body, &res); err != nil {
		return Snapshot{}, fmt.Errorf("relay: parse %s/api/pricing: %w", c.site.Origin, err)
	}
	out := Snapshot{Ratios: map[string]Ratios{}, At: time.Now().UTC()}
	for _, m := range res.Data {
		out.Ratios[m.ModelName] = Ratios{
			Model:      m.ModelRatio,
			Completion: ratioOr(m.CompletionRatio, 1),
			Cache:      ratioOr(m.CacheRatio, 1),
			FixedPrice: m.ModelPrice,
		}
	}
	return out, nil
}

// Bills reads the key's most recent logs, newest first, capped at 1000 rows.
// afterID skips everything at or below that id so repeated polls only fetch
// new rows.
func (c *newAPIClient) Bills(ctx context.Context, afterID int64) ([]Bill, error) {
	path := "/api/log/token?p=1&page_size=" + strconv.Itoa(maxRecentItems)
	body, err := c.h.get(ctx, c.site.Origin, path, c.cred, true)
	if err != nil {
		return nil, err
	}
	var res struct {
		Data struct {
			Items []newAPILog `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body.body, &res); err != nil {
		return nil, fmt.Errorf("relay: parse %s/api/log/token: %w", c.site.Origin, err)
	}
	out := make([]Bill, 0, len(res.Data.Items))
	for _, l := range res.Data.Items {
		if l.ID <= afterID {
			continue
		}
		bill, err := c.billFrom(l)
		if err != nil {
			return nil, err
		}
		out = append(out, bill)
	}
	return out, nil
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

	CacheTokens         *int64   `json:"cache_tokens"`
	CacheCreationTokens *int64   `json:"cache_creation_tokens"`
	CacheWriteTokens    *int64   `json:"cache_write_tokens"`
	CacheCreationRatio  *float64 `json:"cache_creation_ratio"`
	UsageSemantic       string   `json:"usage_semantic"`
	InputTokensTotal    *int64   `json:"input_tokens_total"`
	ReasoningTokens     *int64   `json:"reasoning_tokens"`
	ReasoningEffort     string   `json:"reasoning_effort"`
}

// billFrom turns one log row into a Bill, normalizing the token accounting to
// MyToken's convention: Input excludes cache.
func (c *newAPIClient) billFrom(l newAPILog) (Bill, error) {
	var other newAPIOther
	if l.Other != "" {
		if err := json.Unmarshal([]byte(l.Other), &other); err != nil {
			return Bill{}, fmt.Errorf("relay: log %d has unreadable other: %w", l.ID, err)
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
		ID:                l.ID,
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
		ChargedUSD: c.usd(float64(l.Quota)),
		Stream:     l.IsStream,
		LatencyMS:  l.UseTime,
		Ratios: Ratios{
			Model:       valueOr(other.ModelRatio, 0),
			Completion:  ratioOr(valueOr(other.CompletionRatio, 0), 1),
			Cache:       ratioOr(valueOr(other.CacheRatio, 0), 1),
			CacheCreate: ratioOr(valueOr(other.CacheCreationRatio, 0), 1),
			Group:       ratioOr(valueOr(other.GroupRatio, 0), 1),
			FixedPrice:  valueOr(other.ModelPrice, 0),
		},
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
