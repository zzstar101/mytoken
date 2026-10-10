package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/source"
)

// officialClient reads the balance of one of the official APIs. Those sites
// expose nothing but the key's quota, so every other capability is unsupported.
type officialClient struct {
	h    *HTTP
	site Site
	cred source.Credential
	kind Kind
}

// endpoints maps a kind to the balance path it answers on. z.ai and MiniMax are
// absent on purpose: their balance contracts could not be confirmed against a
// reference implementation, so they are reported as unsupported instead of
// guessed at.
var endpoints = map[Kind][]string{
	KindOpenRouter: {"/api/v1/key", "/api/v1/credits"},
	KindDeepSeek:   {"/user/balance"},
	KindSilicon:    {"/v1/user/info"},
	KindMoonshot:   {"/v1/users/me/balance"},
}

func (c *officialClient) Kind() Kind { return c.kind }

func (c *officialClient) Snapshot(ctx context.Context) (Snapshot, error) {
	return Snapshot{}, ErrUnsupported
}

func (c *officialClient) Bills(ctx context.Context, afterID int64) ([]Bill, error) {
	return nil, ErrUnsupported
}

func (c *officialClient) Daily(ctx context.Context, from, to time.Time, loc *time.Location) ([]Daily, error) {
	return nil, ErrUnsupported
}

// Balance walks the endpoint list for the kind and returns the first one that
// parses, so a site that moved an endpoint still works.
func (c *officialClient) Balance(ctx context.Context) (Balance, error) {
	paths, ok := endpoints[c.kind]
	if !ok {
		return Balance{}, fmt.Errorf("relay: %s has no confirmed balance endpoint: %w", c.kind, ErrUnsupported)
	}
	var lastErr error
	for _, path := range paths {
		body, err := c.h.get(ctx, c.site.Origin, path, c.cred, true)
		if err != nil {
			lastErr = err
			var se *statusError
			if errors.As(err, &se) && (se.code == 404) {
				continue
			}
			return Balance{}, err
		}
		bal, ok, err := parseOfficialBalance(c.kind, c.site, body.body)
		if err != nil {
			lastErr = err
			continue
		}
		if ok {
			return bal, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("relay: %s balance response was not recognized", c.kind)
	}
	return Balance{}, lastErr
}

// parseOfficialBalance normalizes each official API's balance shape. The field
// names follow the reference implementations: cc-switch's balance.rs for
// DeepSeek, SiliconFlow and OpenRouter, and OpenRouter's documented key
// endpoint. Moonshot's field names are inferred and matched permissively.
func parseOfficialBalance(kind Kind, site Site, body []byte) (Balance, bool, error) {
	out := Balance{Origin: site.Origin, KeyID: site.KeyID, At: time.Now().UTC()}
	remaining := func(v float64) *float64 { x := v; return &x }
	switch kind {
	case KindDeepSeek:
		var res struct {
			BalanceInfos []struct {
				Currency     string    `json:"currency"`
				TotalBalance flexFloat `json:"total_balance"`
			} `json:"balance_infos"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return out, false, err
		}
		// Prefer the USD row; an account topped up in yuan only has CNY.
		pick := -1
		for i, info := range res.BalanceInfos {
			if pick < 0 || strings.EqualFold(info.Currency, "USD") {
				pick = i
			}
		}
		if pick < 0 {
			return out, false, nil
		}
		info := res.BalanceInfos[pick]
		out.RemainingUSD = remaining(float64(info.TotalBalance))
		if c := strings.ToUpper(info.Currency); c != "" && c != "USD" {
			out.Currency = c
		}
		return out, true, nil
	case KindSilicon:
		var res struct {
			Data struct {
				Balance       float64 `json:"balance"`
				ChargeBalance float64 `json:"chargeBalance"`
				TotalBalance  float64 `json:"totalBalance"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return out, false, err
		}
		total := res.Data.TotalBalance
		if total == 0 {
			total = res.Data.Balance + res.Data.ChargeBalance
		}
		if total == 0 {
			return out, false, nil
		}
		out.RemainingUSD = remaining(total)
		if strings.HasSuffix(hostOnly(site.Origin), ".cn") {
			out.Currency = "CNY"
		}
		return out, true, nil
	case KindOpenRouter:
		var key struct {
			Data struct {
				Usage          float64  `json:"usage"`
				Limit          float64  `json:"limit"`
				LimitRemaining *float64 `json:"limit_remaining"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &key); err == nil {
			switch {
			case key.Data.LimitRemaining != nil:
				out.RemainingUSD = key.Data.LimitRemaining
				used := key.Data.Usage
				out.UsedUSD = &used
				return out, true, nil
			case key.Data.Limit > 0:
				left := key.Data.Limit - key.Data.Usage
				out.RemainingUSD = remaining(left)
				used := key.Data.Usage
				out.UsedUSD = &used
				return out, true, nil
			}
		}
		var credits struct {
			Data struct {
				TotalCredits float64 `json:"total_credits"`
				TotalUsage   float64 `json:"total_usage"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &credits); err != nil {
			return out, false, err
		}
		if credits.Data.TotalCredits == 0 && credits.Data.TotalUsage == 0 {
			return out, false, nil
		}
		out.RemainingUSD = remaining(credits.Data.TotalCredits - credits.Data.TotalUsage)
		used := credits.Data.TotalUsage
		out.UsedUSD = &used
		return out, true, nil
	case KindMoonshot:
		// Field names are not confirmed against a reference implementation, so
		// every alias the API family is known to use is tried.
		var res struct {
			Data struct {
				AvailableBalance *float64 `json:"available_balance"`
				CashBalance      *float64 `json:"cash_balance"`
				VoucherBalance   *float64 `json:"voucher_balance"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return out, false, err
		}
		if strings.HasSuffix(hostOnly(site.Origin), ".cn") {
			out.Currency = "CNY"
		}
		if res.Data.AvailableBalance != nil {
			out.RemainingUSD = res.Data.AvailableBalance
			return out, true, nil
		}
		total := 0.0
		if res.Data.CashBalance != nil {
			total += *res.Data.CashBalance
		}
		if res.Data.VoucherBalance != nil {
			total += *res.Data.VoucherBalance
		}
		if total == 0 {
			return out, false, nil
		}
		out.RemainingUSD = remaining(total)
		return out, true, nil
	}
	return out, false, fmt.Errorf("relay: unsupported kind %q", kind)
}

// flexFloat reads a JSON number or a numeric string ("37.04").
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = flexFloat(v)
	return nil
}

func hostOnly(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
