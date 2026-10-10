package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/source"
)

// officialKinds maps a gateway host to the official API it belongs to. These
// sites only support L2 (balance).
var officialKinds = map[string]Kind{
	"openrouter.ai":       KindOpenRouter,
	"api.deepseek.com":    KindDeepSeek,
	"api.siliconflow.cn":  KindSilicon,
	"api.siliconflow.com": KindSilicon,
	"api.moonshot.cn":     KindMoonshot,
	"api.moonshot.ai":     KindMoonshot,
	"open.bigmodel.cn":    KindZAI,
	"api.z.ai":            KindZAI,
	"api.minimax.chat":    KindMiniMax,
	"api.minimaxi.com":    KindMiniMax,
}

// hostOf strips scheme and port from an origin.
func hostOf(origin string) string {
	if i := strings.Index(origin, "://"); i >= 0 {
		origin = origin[i+3:]
	}
	if i := strings.LastIndex(origin, ":"); i >= 0 {
		origin = origin[:i]
	}
	return strings.ToLower(origin)
}

// officialKind reports the official kind for a host, if any.
func officialKind(origin string) (Kind, bool) {
	k, ok := officialKinds[hostOf(origin)]
	return k, ok
}

// errRedirect marks a refused cross-origin redirect, which is fatal: the site
// tried to move our key somewhere else.
var errRedirect = errors.New("relay: redirect refused")

// statusError carries a non-2xx status so Detect can tell "not this kind of
// site" (404) from "this key is not accepted" (401/403).
type statusError struct {
	code   int
	origin string
	keyID  string
}

func (e *statusError) Error() string {
	if e.code == http.StatusTooManyRequests {
		return "relay: " + e.origin + " is rate limiting requests (HTTP 429); try again later"
	}
	return "relay: " + e.origin + " returned " + http.StatusText(e.code)
}

// Unwrap keeps errors.Is working for callers that only want errRedirect.
func (e *statusError) Unwrap() error {
	if e.code == http.StatusUnauthorized || e.code == http.StatusForbidden {
		return errKeyRejected
	}
	return nil
}

// errKeyRejected means the gateway did not accept the key we sent.
var errKeyRejected = errors.New("relay: key rejected")

// errRejected means the site answered but refused the request in its body
// (new-api's {"success": false}).
var errRejected = errors.New("relay: site refused the request")

// Reason sums up a relay error in words that are safe to store and show: no
// URL, header, response body or credential, only the kind of failure.
func Reason(err error) string {
	var se *statusError
	var je *json.SyntaxError
	var te *json.UnmarshalTypeError
	var ne net.Error
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &se):
		if se.code == http.StatusUnauthorized || se.code == http.StatusForbidden {
			return fmt.Sprintf("key rejected (HTTP %d)", se.code)
		}
		if se.code == http.StatusTooManyRequests {
			return "rate limited (HTTP 429), try later"
		}
		return fmt.Sprintf("HTTP %d", se.code)
	case errors.Is(err, errRedirect):
		return "redirect to another site refused"
	case errors.Is(err, errRejected):
		return "site refused the request"
	case errors.Is(err, errTooLarge):
		return "response too large"
	case errors.As(err, &je), errors.As(err, &te):
		return "unexpected response format"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timed out"
	case errors.As(err, &ne):
		return "network error"
	case errors.Is(err, ErrUnsupported):
		return "not supported by this site"
	}
	return "unexpected response"
}

// detect implements docs/RELAY.md §3: official host first, then sub2api's
// billing endpoint, then new-api's status endpoint, then unknown.
func detect(ctx context.Context, h *HTTP, origin string, cred source.Credential) (Site, error) {
	site := Site{Origin: origin, KeyID: cred.KeyID, Kind: KindUnknown, DetectedAt: time.Now().UTC()}
	// 1. Official API by host: no request needed, L2 only.
	if kind, ok := officialKind(origin); ok {
		site.Kind = kind
		site.Layers = LayerBalance
		return site, nil
	}
	// 2. sub2api's billing endpoint answers with the effective multiplier.
	if res, err := h.get(ctx, origin, "/v1/sub2api/billing", cred, true); err == nil {
		if kind, ok := parseSub2APIBilling(res.body); ok {
			site.Kind = kind
			site.Layers = LayerRatio | LayerBalance | LayerBills
			return site, nil
		}
	} else if fatal(err) {
		return site, err
	}
	// 3. new-api's status endpoint needs no key.
	if res, err := h.get(ctx, origin, "/api/status", cred, false); err == nil {
		if kind, version, qpu, ok := parseNewAPIStatus(res.body, res.header); ok {
			site.Kind = kind
			site.Version = version
			site.QuotaPerUnit = qpu
			site.Layers = LayerRatio | LayerBalance | LayerBills
			return site, nil
		}
	} else if fatal(err) {
		return site, err
	}
	return site, nil
}

// fatal reports whether an error should stop detection. A refused redirect or a
// rejected key means the site is unusable; a plain 404 just means "not this
// kind of site".
func fatal(err error) bool {
	return errors.Is(err, errRedirect) || errors.Is(err, errKeyRejected)
}

// parseSub2APIBilling decides sub2api from a billing response: the presence of
// the effective multiplier is the marker.
func parseSub2APIBilling(body []byte) (Kind, bool) {
	var probe struct {
		EffectiveRateMultiplier *float64 `json:"effective_rate_multiplier"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return KindUnknown, false
	}
	if probe.EffectiveRateMultiplier == nil {
		return KindUnknown, false
	}
	return KindSub2API, true
}

// parseNewAPIStatus decides new-api from a status response, which carries the
// version header and the quota unit the site bills in.
func parseNewAPIStatus(body []byte, header http.Header) (Kind, string, float64, bool) {
	version := header.Get("x-new-api-version")
	var probe struct {
		Data struct {
			QuotaPerUnit *float64 `json:"quota_per_unit"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &probe)
	if version == "" && probe.Data.QuotaPerUnit == nil {
		return KindUnknown, "", 0, false
	}
	qpu := 500000.0
	if probe.Data.QuotaPerUnit != nil && *probe.Data.QuotaPerUnit > 0 {
		qpu = *probe.Data.QuotaPerUnit
	}
	return KindNewAPI, version, qpu, true
}
