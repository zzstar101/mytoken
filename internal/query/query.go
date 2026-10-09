// Package query is the read API shared by the UI and the CLI (docs/SPEC.md §5).
// The types and the Service interface are frozen; astra implements Service
// (see NewService in service.go) on top of internal/store.
package query

import (
	"context"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

// Range is a half-open local-time interval [From, To). Zero values mean unbounded.
type Range struct {
	From, To time.Time
}

// Filter narrows every query. Empty slices mean "all".
type Filter struct {
	Range     Range
	Harnesses []model.Harness
	Providers []string
	Models    []string
	Project   string
}

// Totals is the aggregate over a filter.
type Totals struct {
	Tokens   model.Tokens `json:"tokens"`
	CostUSD  float64      `json:"costUsd"`
	Requests int64        `json:"requests"`
	Sessions int64        `json:"sessions"`
	// Unpriced counts requests with neither a log cost nor a known price;
	// CostUSD excludes them. The UI shows "—" when Unpriced == Requests.
	Unpriced int64 `json:"unpriced"`
	// CacheHit = CacheRead / (Input + CacheRead + CacheWrite); 0 when denominator is 0.
	CacheHit float64 `json:"cacheHit"`
}

// Point is one bucket of a time series. Day is the local start of the day (Daily)
// or of the hour (Hourly).
type Point struct {
	Day     time.Time    `json:"t"`
	Tokens  model.Tokens `json:"tokens"`
	CostUSD float64      `json:"costUsd"`
}

// Bucket is one row of a ranking. Key is the stable id, Label the display name.
type Bucket struct {
	Key      string       `json:"key"`
	Label    string       `json:"label"`
	Tokens   model.Tokens `json:"tokens"`
	CostUSD  float64      `json:"costUsd"`
	Requests int64        `json:"requests"`
	Sessions int64        `json:"sessions"`
	Unpriced int64        `json:"unpriced"` // see Totals.Unpriced
}

// ProviderModel is a session's usage split by provider × model.
type ProviderModel struct {
	Provider string             `json:"provider"`
	Model    string             `json:"model"`
	Attrib   model.AttribSource `json:"attrib"`
	Tokens   model.Tokens       `json:"tokens"`
	CostUSD  float64            `json:"costUsd"`
	Requests int64              `json:"requests"`
	Unpriced int64              `json:"unpriced"` // see Totals.Unpriced
}

// SessionRow is a session with its aggregates. For a parent session, Tokens,
// CostUSD and Requests INCLUDE its children (subagents); Breakdown too.
type SessionRow struct {
	model.SessionMeta
	Tokens    model.Tokens    `json:"tokens"`
	CostUSD   float64         `json:"costUsd"`
	Requests  int64           `json:"requests"`
	Unpriced  int64           `json:"unpriced"` // see Totals.Unpriced
	Children  int             `json:"children"`
	Breakdown []ProviderModel `json:"breakdown"` // sorted by Tokens.Total() desc
}

// AttributedEvent is a usage event with its resolved provider and cost.
type AttributedEvent struct {
	model.UsageEvent
	ResolvedProvider string             `json:"resolvedProvider"`
	Attrib           model.AttribSource `json:"attrib"`
	Cost             float64            `json:"cost"`
	Priced           bool               `json:"priced"` // false: no log cost and no known price
}

// Session sort keys.
const (
	SortRecent = "recent" // UpdatedAt desc (default)
	SortTokens = "tokens" // Tokens.Total() desc
	SortCost   = "cost"   // CostUSD desc
)

// Service is the read API. Sessions lists only top-level sessions (ParentID == "")
// whose events intersect the filter.
type Service interface {
	Totals(ctx context.Context, f Filter) (Totals, error)
	Daily(ctx context.Context, f Filter) ([]Point, error)  // one point per local day in range, zero-filled
	Hourly(ctx context.Context, f Filter) ([]Point, error) // one point per local hour in range, zero-filled
	ByProvider(ctx context.Context, f Filter) ([]Bucket, error)
	ByModel(ctx context.Context, f Filter) ([]Bucket, error)
	ByProject(ctx context.Context, f Filter) ([]Bucket, error)
	ByHarness(ctx context.Context, f Filter) ([]Bucket, error)
	Sessions(ctx context.Context, f Filter, sort string, limit, offset int) (rows []SessionRow, total int, err error)
	// Session returns the session, its children, and its own events (not children's), time-ordered.
	Session(ctx context.Context, h model.Harness, id string) (SessionRow, []SessionRow, []AttributedEvent, error)
	// Subscribe notifies (throttled to ≤1/s) whenever new data is committed.
	Subscribe() (<-chan struct{}, func())
}
