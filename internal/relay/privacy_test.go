package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/source"
)

// sentinelKey stands in for a real gateway key. Every test asserts it never
// appears in an error string, a status field or any other output, and that the
// only thing that ever reaches a request is this exact header.
const sentinelKey = "sk-sentinel-DO-NOT-LOG-0000"

// forbid fails the test when the sentinel key appears anywhere in a message.
func forbid(t *testing.T, what, msg string) {
	t.Helper()
	if strings.Contains(msg, sentinelKey) {
		t.Fatalf("key leaked into %s: %s", what, msg)
	}
}

// forbidErr fails when the sentinel key appears in an error.
func forbidErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		forbid(t, "error", err.Error())
	}
}

// gateway is a fake site: it records the requests it was sent, answers from a
// table of handlers, and keeps the Authorization header only long enough to
// check it against the sentinel.
type gateway struct {
	mu   sync.Mutex
	seen []string // "METHOD path", never the Authorization header
	auth []string
	h    map[string]http.HandlerFunc
}

func newGateway() *gateway {
	return &gateway{h: map[string]http.HandlerFunc{}}
}

func (g *gateway) handle(path string, fn http.HandlerFunc) *gateway { g.h[path] = fn; return g }

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.seen = append(g.seen, r.Method+" "+r.URL.Path)
	g.auth = append(g.auth, r.Header.Get("Authorization"))
	g.mu.Unlock()
	if h, ok := g.h[r.URL.Path]; ok {
		h(w, r)
		return
	}
	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
}

// requests returns the paths the gateway was asked for.
func (g *gateway) requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string{}, g.seen...)
}

// authorizations reports, per request, whether the sentinel key was sent as a
// bearer token — without ever printing the key itself.
func (g *gateway) authorizations() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.auth))
	for i, a := range g.auth {
		switch {
		case a == "":
			out[i] = "none"
		case a == "Bearer "+sentinelKey:
			out[i] = "sentinel"
		case strings.HasPrefix(a, "Bearer "):
			out[i] = "other"
		default:
			out[i] = "malformed"
		}
	}
	return out
}

// site starts the gateway and returns its origin plus a transport for it.
func (g *gateway) site(t *testing.T) (origin string, h *HTTP) {
	t.Helper()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return strings.TrimSuffix(srv.URL, "/"), NewHTTP(srv.Client())
}

// cred builds a credential for a gateway origin.
func cred(origin string) source.Credential {
	return source.Credential{Origin: origin, KeyID: source.KeyID(sentinelKey), Provider: "XLAB", Secret: source.NewSecret(sentinelKey)}
}

// jsonBody writes a JSON body with the given status.
func jsonBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- privacy contract -------------------------------------------------------

func TestSecretRedactsEveryPath(t *testing.T) {
	s := source.NewSecret(sentinelKey)
	forbid(t, "String", s.String())
	forbid(t, "GoString", fmt.Sprintf("%#v", s))
	forbid(t, "Format", fmt.Sprintf("%v", s))
	forbid(t, "Format-quoted", fmt.Sprintf("%q", s))
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	forbid(t, "MarshalJSON", string(b))
	if got := s.String(); got != "[redacted]" {
		t.Fatalf("String = %q, want [redacted]", got)
	}
	if got := s.Reveal(); got != sentinelKey {
		t.Fatalf("Reveal = %q", got)
	}
}

// TestHTTPErrorsCarryNoKey walks every failure mode the transport can produce
// and asserts the key is absent from the error text.
func TestHTTPErrorsCarryNoKey(t *testing.T) {
	ctx := context.Background()
	h := NewHTTP(&http.Client{Timeout: 2 * time.Second})

	if _, err := NewHTTP(nil).Get(ctx, "https://x.invalid", "/api/usage/token/", cred("https://x.invalid"), true); err == nil {
		t.Error("an unreachable host should fail (nil base = default client)")
	} else {
		forbidErr(t, err)
	}
	if _, err := h.Get(ctx, "https://x.invalid", "api/usage/token/?key="+sentinelKey, cred("https://x.invalid"), true); err == nil {
		t.Error("relative path should fail")
	} else {
		forbidErr(t, err)
	}
	if _, err := h.Get(ctx, "https://x.invalid", "/api/usage/token/", cred("https://x.invalid"), true); err == nil {
		t.Error("unreachable origin should fail")
	} else {
		forbidErr(t, err)
	}
	if _, err := h.Get(ctx, "ftp://x.invalid", "/api/usage/token/", cred("ftp://x.invalid"), true); err == nil {
		t.Error("bad scheme should fail")
	} else {
		forbidErr(t, err)
	}

	// A failing response must be reported as origin + status, never with the
	// key, and never with the query string that carried it.
	g := newGateway().handle("/api/status", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	origin, gh := g.site(t)
	_, err := gh.Get(ctx, origin, "/api/status?key="+sentinelKey, cred(origin), true)
	if err == nil {
		t.Fatal("500 should fail")
	}
	forbidErr(t, err)
	if !strings.Contains(err.Error(), origin) {
		t.Fatalf("error %q should name the origin", err)
	}
	if strings.Contains(err.Error(), "key=") {
		t.Fatalf("error %q kept the query string", err)
	}
}

func TestHTTPRejectsCrossOriginRedirect(t *testing.T) {
	ctx := context.Background()
	// The second server is where a hostile site would try to move our key.
	victim := newGateway().handle("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+sentinelKey {
			t.Error("the key reached a third-party origin")
		}
		jsonBody(w, 200, map[string]any{"data": map[string]any{"quota_per_unit": 500000}})
	})
	victimOrigin, _ := victim.site(t)
	g := newGateway().handle("/api/status", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, victimOrigin+"/api/status", http.StatusFound)
	})
	origin, h := g.site(t)
	_, err := h.Get(ctx, origin, "/api/status", cred(origin), true)
	if err == nil {
		t.Fatal("cross-origin redirect was followed")
	}
	if !errors.Is(err, errRedirect) {
		t.Fatalf("err = %v, want errRedirect", err)
	}
	forbidErr(t, err)
}

func TestHTTPSameOriginRedirectFollowed(t *testing.T) {
	ctx := context.Background()
	g := newGateway()
	g.handle("/api/status", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/status/v2", http.StatusFound)
	})
	g.handle("/api/status/v2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-new-api-version", "v0.0.1")
		jsonBody(w, 200, map[string]any{"data": map[string]any{"quota_per_unit": 500000}})
	})
	origin, h := g.site(t)
	site, err := Detect(ctx, h, origin, cred(origin))
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if site.Kind != KindNewAPI {
		t.Fatalf("kind = %v, want newapi", site.Kind)
	}
}

func TestHTTPCapsOversizedBody(t *testing.T) {
	ctx := context.Background()
	g := newGateway().handle("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		// More than MaxBody bytes of junk must be refused, not buffered.
		chunk := strings.Repeat("a", 64<<10)
		for i := 0; i < (MaxBody/(64<<10))+2; i++ {
			_, _ = io.WriteString(w, chunk)
		}
	})
	origin, h := g.site(t)
	_, err := h.Get(ctx, origin, "/api/status", cred(origin), true)
	if err == nil {
		t.Fatal("oversized body should fail")
	}
	forbidErr(t, err)
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want a size complaint", err)
	}
}

// TestBearerSentExactlyOnce checks the key is only ever attached where the
// client asked for it: new-api's status endpoint is probed without a key, the
// authenticated ones get it.
func TestBearerSentExactlyWhereAsked(t *testing.T) {
	ctx := context.Background()
	g := newGateway()
	g.handle("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-new-api-version", "v0.0.1")
		jsonBody(w, 200, map[string]any{"data": map[string]any{"quota_per_unit": 500000}})
	})
	g.handle("/api/usage/token/", func(w http.ResponseWriter, r *http.Request) {
		jsonBody(w, 200, map[string]any{"data": map[string]any{"total_granted": 500000, "total_used": 100000, "total_available": 400000}})
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
	if _, err := c.Balance(ctx); err != nil {
		t.Fatalf("balance: %v", err)
	}
	// Detect probes the sub2api marker with the key, the status endpoint
	// without it, then Balance sends the key again.
	want := []string{"sentinel", "none", "sentinel"}
	got := g.authorizations()
	if len(got) != len(want) {
		t.Fatalf("authorizations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request %d sent %q, want %q", i, got[i], want[i])
		}
	}
}
