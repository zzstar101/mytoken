package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/zzstar101/mytoken/internal/model"
)

// Secret holds a credential in memory only. Every formatting path redacts it,
// so a Secret cannot leak into logs, error strings or JSON output by accident.
type Secret struct {
	v string
}

// NewSecret wraps a key that a source read from a harness or cc-switch config.
func NewSecret(key string) Secret { return Secret{v: key} }

// Reveal returns the key itself. Callers must only use it to build a request
// and must never store, log or serialize the result.
func (s Secret) Reveal() string {
	secretReads.Add(1)
	return s.v
}

// String implements fmt.Stringer and always redacts.
func (s Secret) String() string { return redacted }

// GoString implements fmt.GoStringer and always redacts.
func (s Secret) GoString() string { return redacted }

// MarshalJSON emits the redaction marker instead of the key.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// Format implements fmt.Formatter and always redacts.
func (s Secret) Format(f fmt.State, verb rune) {
	switch verb {
	case 's', 'v', 'q':
		writeRedacted(f)
	default:
		writeRedacted(f)
	}
}

const redacted = "[redacted]"

func writeRedacted(f fmt.State) {
	_, _ = f.Write([]byte(redacted))
}

// KeyID is the stable, non-reversible identifier persisted instead of a key:
// hex(sha256("mytoken-key:" + key))[:12].
func KeyID(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("mytoken-key:" + key))
	return hex.EncodeToString(sum[:])[:12]
}

// Origin normalizes a base URL to scheme://host[:port], lowercased and without
// a path. It reports false for anything unusable (no scheme, no host, or a
// scheme that is not http/https).
// IsLocalProxy reports whether base points at cc-switch's local proxy, which
// forwards to whichever provider is selected in cc-switch: it says nothing
// about the gateway a request reached.
func IsLocalProxy(base string) bool {
	origin, ok := Origin(base)
	return ok && (origin == LocalProxyOrigin || origin == "http://localhost:15721")
}

func Origin(rawURL string) (string, bool) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", false
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", false
	}
	host := strings.ToLower(u.Host)
	if host == "" || strings.Contains(host, "%") {
		return "", false
	}
	return strings.ToLower(u.Scheme) + "://" + host, true
}

// Credential is one key that a harness or cc-switch already sends to a site.
// Secret is the only field holding the key and it redacts itself everywhere.
type Credential struct {
	Origin   string        // https://host[:port], lowercased, no path
	KeyID    string        // KeyID(secret)
	Provider string        // provider name as attribution sees it ("XLAB")
	Harness  model.Harness // "" when the source is app-wide
	Source   string        // "cc-switch", "harness-config"
	Secret   Secret
}

// Credentials is an optional Source capability. Implementations read keys on
// each call and must not cache them beyond the call.
type Credentials interface {
	Credentials(ctx context.Context) ([]Credential, error)
}

// secretReads counts how often a raw key was revealed, so tests can assert the
// only place that happens is a request builder.
var secretReads atomic.Int64

// SecretReveals reports how many times a key was revealed in this process. It
// exists for the privacy tests and nothing else.
func SecretReveals() int64 { return secretReads.Load() }
