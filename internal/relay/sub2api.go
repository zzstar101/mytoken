package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/source"
)

// sub2APIClient talks to a sub2api site: one billing endpoint for the
// multipliers, and one usage endpoint that reports daily and per-model usage.
type sub2APIClient struct {
	h    *HTTP
	site Site
	cred source.Credential
}

func (c *sub2APIClient) Kind() Kind { return KindSub2API }

// sub2apiBilling is /v1/sub2api/billing (handler/gateway_key_billing.go).
type sub2apiBilling struct {
	Object                  string    `json:"object"`
	SchemaVersion           int       `json:"schema_version"`
	GroupRateMultiplier     float64   `json:"group_rate_multiplier"`
	UserRateMultiplier      *float64  `json:"user_rate_multiplier"`
	ResolvedRateMultiplier  float64   `json:"resolved_rate_multiplier"`
	PeakRateEnabled         bool      `json:"peak_rate_enabled"`
	PeakStart               *string   `json:"peak_start"`
	PeakEnd                 *string   `json:"peak_end"`
	PeakRateMultiplier      *float64  `json:"peak_rate_multiplier"`
	AppliedPeakMultiplier   *float64  `json:"applied_peak_multiplier"`
	EffectiveRateMultiplier float64   `json:"effective_rate_multiplier"`
	Timezone                *string   `json:"timezone"`
	ObservedAt              time.Time `json:"observed_at"`
}

// Balance reads the wallet or key quota out of /v1/usage.
func (c *sub2APIClient) Balance(ctx context.Context) (Balance, error) {
	body, err := c.h.get(ctx, c.site.Origin, "/v1/usage?days=1", c.cred, true)
	if err != nil {
		return Balance{}, err
	}
	var res struct {
		Mode      string   `json:"mode"`
		Remaining *float64 `json:"remaining"`
		Balance   *float64 `json:"balance"`
		Quota     *struct {
			Limit     float64 `json:"limit"`
			Used      float64 `json:"used"`
			Remaining float64 `json:"remaining"`
			Unit      string  `json:"unit"`
		} `json:"quota"`
	}
	if err := json.Unmarshal(body.body, &res); err != nil {
		return Balance{}, fmt.Errorf("relay: parse %s/v1/usage: %w", c.site.Origin, err)
	}
	out := Balance{Origin: c.site.Origin, KeyID: c.site.KeyID, At: time.Now().UTC()}
	if res.Quota != nil {
		v := res.Quota.Remaining
		out.RemainingUSD = &v
		u := res.Quota.Used
		out.UsedUSD = &u
		return out, nil
	}
	if res.Remaining != nil {
		v := *res.Remaining
		out.RemainingUSD = &v
	}
	if res.Balance != nil {
		v := *res.Balance
		out.RemainingUSD = &v
	}
	return out, nil
}

// Snapshot reads the billing endpoint: the group multiplier, the key's resolved
// multiplier, and the peak window when the site runs one.
func (c *sub2APIClient) Snapshot(ctx context.Context) (Snapshot, error) {
	body, err := c.h.get(ctx, c.site.Origin, "/v1/sub2api/billing", c.cred, true)
	if err != nil {
		return Snapshot{}, err
	}
	var b sub2apiBilling
	if err := json.Unmarshal(body.body, &b); err != nil {
		return Snapshot{}, fmt.Errorf("relay: parse %s/v1/sub2api/billing: %w", c.site.Origin, err)
	}
	if b.EffectiveRateMultiplier == 0 && b.ResolvedRateMultiplier == 0 && b.GroupRateMultiplier == 0 {
		return Snapshot{}, fmt.Errorf("relay: %s/v1/sub2api/billing has no multiplier: %w", c.site.Origin, ErrUnsupported)
	}
	mult := b.EffectiveRateMultiplier
	out := Snapshot{Multiplier: &mult, At: time.Now().UTC()}
	if b.PeakRateEnabled && b.PeakStart != nil && b.PeakEnd != nil {
		peak := Peak{Start: *b.PeakStart, End: *b.PeakEnd}
		if b.PeakRateMultiplier != nil {
			peak.Multiplier = *b.PeakRateMultiplier
		}
		out.Peak = &peak
	}
	return out, nil
}

// sub2apiUsage is /v1/usage (handler/gateway_handler.go Usage).
type sub2apiUsage struct {
	Mode       string             `json:"mode"`
	DailyUsage []sub2apiDaily     `json:"daily_usage"`
	ModelStats []sub2apiModelStat `json:"model_stats"`
}

type sub2apiDaily struct {
	Date             string  `json:"date"`
	Requests         int64   `json:"requests"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	Cost             float64 `json:"cost"`
	ActualCost       float64 `json:"actual_cost"`
}

type sub2apiModelStat struct {
	Model               string  `json:"model"`
	Requests            int64   `json:"requests"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	TotalTokens         int64   `json:"total_tokens"`
	Cost                float64 `json:"cost"`
	ActualCost          float64 `json:"actual_cost"`
}

// Daily reads the site's per-day and per-model usage. The day boundary follows
// the caller's time zone so it lines up with the user's own records.
func (c *sub2APIClient) Daily(ctx context.Context, from, to time.Time, loc *time.Location) ([]Daily, error) {
	if loc == nil {
		loc = time.UTC
	}
	// sub2api takes whole dates in the requested zone; the range is half-open.
	q := url.Values{}
	q.Set("days", strconv.Itoa(daysBetween(from.In(loc), to.In(loc))))
	q.Set("timezone", loc.String())
	if !from.IsZero() {
		q.Set("start_date", from.In(loc).Format("2006-01-02"))
	}
	if !to.IsZero() {
		q.Set("end_date", to.In(loc).AddDate(0, 0, -1).Format("2006-01-02"))
	}
	body, err := c.h.get(ctx, c.site.Origin, "/v1/usage?"+q.Encode(), c.cred, true)
	if err != nil {
		return nil, err
	}
	var res sub2apiUsage
	if err := json.Unmarshal(body.body, &res); err != nil {
		return nil, fmt.Errorf("relay: parse %s/v1/usage: %w", c.site.Origin, err)
	}
	var out []Daily
	for _, d := range res.DailyUsage {
		out = append(out, Daily{
			Origin:     c.site.Origin,
			KeyID:      c.site.KeyID,
			Day:        d.Date,
			Requests:   d.Requests,
			Tokens:     model.Tokens{Input: d.InputTokens, Output: d.OutputTokens, CacheRead: d.CacheReadTokens, CacheWrite: d.CacheWriteTokens},
			ListUSD:    d.Cost,
			ChargedUSD: d.ActualCost,
		})
	}
	for _, m := range res.ModelStats {
		out = append(out, Daily{
			Origin:     c.site.Origin,
			KeyID:      c.site.KeyID,
			Day:        from.In(loc).Format("2006-01-02"),
			Model:      m.Model,
			Requests:   m.Requests,
			Tokens:     model.Tokens{Input: m.InputTokens, Output: m.OutputTokens, CacheRead: m.CacheReadTokens, CacheWrite: m.CacheCreationTokens},
			ListUSD:    m.Cost,
			ChargedUSD: m.ActualCost,
		})
	}
	return out, nil
}

// Bills is not available on sub2api: it aggregates instead of logging.
func (c *sub2APIClient) Bills(ctx context.Context, afterID int64) ([]Bill, error) {
	return nil, ErrUnsupported
}

// daysBetween is the number of days in [from,to), at least 1.
func daysBetween(from, to time.Time) int {
	if to.Before(from) || to.Equal(from) {
		return 1
	}
	d := int(to.Sub(from).Hours()/24) + 1
	if d < 1 {
		return 1
	}
	return d
}

// ensure errors is used even when a future revision drops a helper.
var _ = errors.Is
