// Package harness defines the parser contract (docs/SPEC.md §4) and a registry.
package harness

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zzstar101/mytoken/internal/model"
)

// Source is one discovered log file or database.
type Source struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // "jsonl", "json", "jsonl.zstd", "sqlite"
}

// Cursor is the incremental read state of a Source, persisted by the store.
type Cursor struct {
	Offset      int64     `json:"offset"` // bytes consumed (end of last complete line)
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"modTime"`
	Fingerprint string    `json:"fingerprint"` // sha1 of the first 4KB; change => reread from 0
	Extra       string    `json:"extra,omitempty"`
}

// Batch is what one Parse call produces.
type Batch struct {
	Events   []model.UsageEvent
	Sessions []model.SessionMeta
	Next     Cursor
}

// Parser reads one harness's local logs. Implementations must be safe to call
// from a single scan goroutine (no concurrent calls on the same Source).
type Parser interface {
	Harness() model.Harness
	// Roots returns directories to watch (env vars and ~ already expanded).
	// Non-existent roots are allowed and simply ignored.
	Roots() []string
	Discover(ctx context.Context) ([]Source, error)
	// Parse reads src incrementally from cur to the current end. Parsers that
	// cannot read incrementally (whole-file JSON, zstd) reparse fully and rely on
	// DedupKey; they still must return an accurate Next.
	Parse(ctx context.Context, src Source, cur Cursor) (Batch, error)
}

var (
	mu       sync.RWMutex
	registry = map[model.Harness]Parser{}
)

// Register adds a parser; call from init() of each harness package.
func Register(p Parser) {
	mu.Lock()
	defer mu.Unlock()
	registry[p.Harness()] = p
}

// All returns registered parsers sorted by harness id.
func All() []Parser {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Parser, 0, len(registry))
	for _, p := range registry {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Harness() < out[j].Harness() })
	return out
}

// Get returns the parser for h.
func Get(h model.Harness) (Parser, bool) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := registry[h]
	return p, ok
}

// Home returns the user's home directory ("" if unknown).
func Home() string {
	h, _ := os.UserHomeDir()
	return h
}

// EnvOr returns $key if set, else filepath.Join(Home(), rel...).
func EnvOr(key string, rel ...string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return filepath.Join(append([]string{Home()}, rel...)...)
}

// Title normalizes a first user message into a session title: whitespace
// collapsed, at most 60 runes.
func Title(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= 60 {
		return s
	}
	r := []rune(s)
	return string(r[:60])
}
