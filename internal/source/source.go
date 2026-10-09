// Package source defines optional, read-only provider data sources: cc-switch's
// local database and the harness config files that say which provider a tool is
// currently pointing at. Sources feed attribution and price import, and 0.2
// relay reconciliation (which gateways a user has, their base URLs, whether a
// key is configured). A source that is missing or unreadable is skipped, never
// fatal: attribution must work with no data source at all.
package source

import (
	"context"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/pricing"
)

// ProviderInfo describes one provider a source knows about. Keys are never
// returned or persisted: HasKey only reports whether one is configured, and
// callers that need the key read it from the source at that moment.
type ProviderInfo struct {
	Name    string        `json:"name"`
	Harness model.Harness `json:"harness,omitempty"`
	App     string        `json:"app,omitempty"` // source-native app id ("claude", "codex")
	BaseURL string        `json:"baseUrl,omitempty"`
	HasKey  bool          `json:"hasKey"`
	Current bool          `json:"current,omitempty"`
	Source  string        `json:"source"` // Name() of the source that reported it
}

// Status is a snapshot of a source's state, for doctor and the GUI. Err is the
// last load problem, which is informational: a broken source never stops
// attribution.
type Status struct {
	Name      string    `json:"name"`
	Path      string    `json:"path,omitempty"`
	Exists    bool      `json:"exists"`
	ReadAt    time.Time `json:"readAt,omitzero"`
	Mod       time.Time `json:"-"`
	Err       string    `json:"err,omitempty"`
	Providers int       `json:"providers"`
	Matches   int       `json:"matches"`
	// Detail is a short human summary chosen by the source. Implementations
	// must never put credentials in it.
	Detail string `json:"detail,omitempty"`
}

// CurrentConfig is one source's view of the provider a harness is pointing at
// right now. The resolver maps Base to a provider name and appends it to the
// provider_configs timeline when it changed.
type CurrentConfig struct {
	Harness  model.Harness
	Base     string
	Provider string
	// SeedAtZero marks a selection that predates MyToken's own timeline: it is
	// written at time 0 and only when the harness has no entries yet, so
	// historical events still attribute to it (cc-switch's is_current).
	SeedAtZero bool
}

// Source is one optional provider data source.
type Source interface {
	Name() string
	Status() Status
	// Load refreshes the source. A missing or unreadable source records the
	// problem in Status and returns nil.
	Load(ctx context.Context) error
	// Close releases the source's handles.
	Close() error
}

// Matcher is implemented by sources that can attribute a single request
// (cc-switch's proxy request log).
type Matcher interface {
	Match(e model.UsageEvent) (provider string, ok bool)
}

// Configs is implemented by sources that know what is selected right now, for
// backfilling the provider_configs timeline.
type Configs interface {
	CurrentConfigs() []CurrentConfig
}

// Providers is implemented by sources that can list the providers they know
// about (0.2 relay reconciliation).
type Providers interface {
	Providers() []ProviderInfo
}

// Pricing is implemented by sources that can contribute price rules
// (cc-switch's model_pricing table and provider multipliers). Rules carry the
// source's name in Source and leave Multiplier at 0 where it is not set.
type Pricing interface {
	PriceRules(ctx context.Context) ([]pricing.Rule, error)
}
