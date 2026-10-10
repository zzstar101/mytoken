package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
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

// Daily reads the site's per-day usage for the key. sub2api serves the last
// n days ending today in the requested zone (handler/usage_handler.go
// apiKeyDailyUsageRange, at most 90) and lists only days with usage, so the
// window is widened to reach from and every day of [from,to) the site said
// nothing about is returned as zero: "charged nothing" is an answer, and the
// caller replaces its stored days with this list. model_stats is left out:
// it covers a range in the server's own zone and cannot be split by day.
func (c *sub2APIClient) Daily(ctx context.Context, from, to time.Time, loc *time.Location) ([]Daily, error) {
	if loc == nil {
		loc = time.UTC
	}
	zone := zoneName(loc)
	if zone == "" {
		// the server would fall back to its own zone: use one we can name
		loc, zone = time.UTC, "UTC"
	}
	today := dayStart(time.Now().In(loc))
	first := dayStart(from.In(loc))
	end := today.AddDate(0, 0, 1)
	if !to.IsZero() {
		if t := dayStart(to.In(loc)); t.Before(end) {
			end = t
		}
	}
	days := daysBetween(first, today.AddDate(0, 0, 1))
	if days > 90 {
		days = 90
		first = today.AddDate(0, 0, -89)
	}
	q := url.Values{}
	q.Set("days", strconv.Itoa(days))
	q.Set("timezone", zone)
	body, err := c.h.get(ctx, c.site.Origin, "/v1/usage?"+q.Encode(), c.cred, true)
	if err != nil {
		return nil, err
	}
	var res sub2apiUsage
	if err := json.Unmarshal(body.body, &res); err != nil {
		return nil, fmt.Errorf("relay: parse %s/v1/usage: %w", c.site.Origin, err)
	}
	got := map[string]sub2apiDaily{}
	for _, d := range res.DailyUsage {
		got[d.Date] = d
	}
	var out []Daily
	for day := first; day.Before(end); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		d := got[key]
		out = append(out, Daily{
			Origin:     c.site.Origin,
			KeyID:      c.site.KeyID,
			Day:        key,
			Requests:   d.Requests,
			Tokens:     model.Tokens{Input: d.InputTokens, Output: d.OutputTokens, CacheRead: d.CacheReadTokens, CacheWrite: d.CacheWriteTokens},
			ListUSD:    d.Cost,
			ChargedUSD: d.ActualCost,
		})
	}
	return out, nil
}

func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// zoneName is the IANA name of loc. time.Local is called "Local", which no
// server understands: it is resolved from $TZ or the /etc/localtime link, or
// failing that (Windows) from a whole-hour offset as Etc/GMT±N. "" when the
// zone cannot be named.
func zoneName(loc *time.Location) string {
	if loc != time.Local {
		return loc.String()
	}
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(link, "zoneinfo/"); i >= 0 {
			name := link[i+len("zoneinfo/"):]
			if _, err := time.LoadLocation(name); err == nil {
				return name
			}
		}
	}
	// a zone without daylight saving: the same offset in January and July
	_, jan := time.Date(time.Now().Year(), 1, 1, 0, 0, 0, 0, time.Local).Zone()
	_, jul := time.Date(time.Now().Year(), 7, 1, 0, 0, 0, 0, time.Local).Zone()
	if jan == jul && jan%3600 == 0 {
		h := jan / 3600
		switch {
		case h == 0:
			return "UTC"
		case h > 0:
			return fmt.Sprintf("Etc/GMT-%d", h) // POSIX signs are inverted
		default:
			return fmt.Sprintf("Etc/GMT+%d", -h)
		}
	}
	return ""
}

// Bills is not available on sub2api: it aggregates instead of logging.
func (c *sub2APIClient) Bills(ctx context.Context, afterID int64) ([]Bill, error) {
	return nil, ErrUnsupported
}

// daysBetween is the number of whole days in [from,to), at least 1. Rounding
// absorbs the 23- and 25-hour days of a daylight-saving change.
func daysBetween(from, to time.Time) int {
	d := int(math.Round(to.Sub(from).Hours() / 24))
	if d < 1 {
		return 1
	}
	return d
}

// ensure errors is used even when a future revision drops a helper.
var _ = errors.Is
