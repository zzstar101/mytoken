package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/source"
)

// Version is the user-agent version; set by the CLI package at startup.
var Version = "dev"

const (
	// Timeout is the per-request budget for every call to a gateway.
	Timeout = 15 * time.Second
	// MaxBody caps a response body so a hostile or misbehaving gateway cannot
	// exhaust memory.
	MaxBody = 8 << 20
)

// HTTP is the only way relay talks to a gateway. It enforces the privacy
// contract of docs/RELAY.md §2: one origin, a same-origin redirect policy, a
// timeout, a body cap, and error strings that never carry a key or a query
// value.
type HTTP struct {
	// Base is the client used for requests; nil means http.DefaultClient.
	Base *http.Client
}

// NewHTTP builds the transport with the contract's limits. base may be nil.
func NewHTTP(base *http.Client) *HTTP {
	return &HTTP{Base: base}
}

// result is one response, with the status kept so callers can tell "not this
// kind of site" (404) from "this key is not accepted" (401/403).
type result struct {
	body   []byte
	header http.Header
}

// get performs the request. path must be absolute; query values are kept out of
// every error string.
func (h *HTTP) get(ctx context.Context, origin, path string, cred source.Credential, bearer bool) (result, error) {
	var out result
	if h == nil {
		return out, errors.New("relay: http not configured")
	}
	if !strings.HasPrefix(path, "/") {
		return out, fmt.Errorf("relay: path must be absolute, got %q", redactPath(path))
	}
	base, err := url.Parse(origin)
	if err != nil || base.Host == "" {
		return out, fmt.Errorf("relay: bad origin %s", origin)
	}
	ref, err := url.Parse(path)
	if err != nil {
		return out, fmt.Errorf("relay: bad path %q", redactPath(path))
	}
	u := base.ResolveReference(ref)
	if u.Scheme != "http" && u.Scheme != "https" {
		return out, fmt.Errorf("relay: unsupported scheme in %s", origin)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return out, fmt.Errorf("relay: build request for %s: %w", origin, err)
	}
	req.Header.Set("User-Agent", "MyToken/"+Version)
	if bearer {
		req.Header.Set("Authorization", "Bearer "+cred.Secret.Reveal())
	}
	base0 := h.Base
	if base0 == nil {
		base0 = http.DefaultClient
	}
	client := *base0
	client.Timeout = Timeout
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("relay: too many redirects")
		}
		// The contract forbids following a redirect off the site: a gateway
		// must not be able to point us at a third party with our key.
		if to := originOf(req.URL.String()); !sameOrigin(to, origin) {
			return fmt.Errorf("relay: redirect to %s refused: %w", to, errRedirect)
		}
		return nil
	}
	res, err := client.Do(req)
	if err != nil {
		return out, wrapDoError(origin, req.URL.Path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxBody+1))
	if err != nil {
		return out, fmt.Errorf("relay: read %s: %w", origin, err)
	}
	if len(body) > MaxBody {
		return out, fmt.Errorf("relay: response from %s exceeds %d bytes", origin, MaxBody)
	}
	out.body, out.header = body, res.Header
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return out, &statusError{code: res.StatusCode, origin: origin, keyID: cred.KeyID}
	}
	return out, nil
}

// Get issues a GET request against one origin and returns the body.
func (h *HTTP) Get(ctx context.Context, origin, path string, cred source.Credential, bearer bool) ([]byte, error) {
	res, err := h.get(ctx, origin, path, cred, bearer)
	return res.body, err
}

// wrapDoError reduces a transport error to origin + path: no key, no query.
func wrapDoError(origin, path string, err error) error {
	return fmt.Errorf("relay: GET %s: %w", origin+redactPath(path), err)
}

// originOf extracts scheme://host[:port] from a URL, lowercased.
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// sameOrigin compares two origins case-insensitively.
func sameOrigin(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "/"), strings.TrimSuffix(b, "/"))
}

// redactPath keeps a path but drops anything that could carry a secret in a
// query string.
func redactPath(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		return path[:i]
	}
	return path
}
