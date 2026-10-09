package clinetask

import (
	"context"
	"sort"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// RealDataSummary is the result of a full Discover+Parse sweep over a parser's
// real roots. The harness packages' MYTOKEN_REALDATA-gated tests use it to log a
// totals table without duplicating the aggregation, and to assert the shape of
// what was read.
type RealDataSummary struct {
	Roots        []string
	Files        int
	Events       []model.UsageEvent
	Sessions     []model.SessionMeta
	Deduped      []model.UsageEvent // last copy per DedupKey, in first-seen order
	DiscoverDur  time.Duration
	ParseDur     time.Duration
	TotalDur     time.Duration
	TotalTokens  model.Tokens
	TotalCostUSD float64
}

// ProviderModelRow is one row of the per-provider/model totals table.
type ProviderModelRow struct {
	Provider string
	Model    string
	Requests int
	Tokens   model.Tokens
	CostUSD  float64
}

// Rows returns the per-provider/model totals, sorted by provider then model.
func (s *RealDataSummary) Rows() []ProviderModelRow {
	type key struct{ provider, model string }
	agg := map[key]*ProviderModelRow{}
	order := make([]key, 0, len(s.Deduped))
	for _, e := range s.Deduped {
		k := key{e.Provider, e.Model}
		r, ok := agg[k]
		if !ok {
			r = &ProviderModelRow{Provider: e.Provider, Model: e.Model}
			agg[k] = r
			order = append(order, k)
		}
		r.Requests++
		r.Tokens = r.Tokens.Add(e.Tokens)
		if e.CostUSD != nil {
			r.CostUSD += *e.CostUSD
		}
	}
	out := make([]ProviderModelRow, 0, len(order))
	for _, k := range order {
		out = append(out, *agg[k])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// SessionsWithParent counts the sessions that link to another session.
func (s *RealDataSummary) SessionsWithParent() int {
	n := 0
	for _, sess := range s.Sessions {
		if sess.ParentID != "" {
			n++
		}
	}
	return n
}

// SweepRealData runs Discover then Parse over every source p reports and
// aggregates the result. It is deliberately free of testing and logging
// dependencies so the production package stays importable by the binary.
func SweepRealData(ctx context.Context, p harness.Parser) (*RealDataSummary, error) {
	start := time.Now()
	roots := p.Roots()
	srcs, err := p.Discover(ctx)
	if err != nil {
		return nil, err
	}

	parseStart := time.Now()
	s := &RealDataSummary{Roots: roots, Files: len(srcs), DiscoverDur: time.Since(start)}
	for _, src := range srcs {
		b, err := p.Parse(ctx, src, harness.Cursor{})
		if err != nil {
			return s, err
		}
		s.Events = append(s.Events, b.Events...)
		s.Sessions = append(s.Sessions, b.Sessions...)
	}
	s.ParseDur = time.Since(parseStart)
	s.TotalDur = time.Since(start)

	// Deduplicate exactly like the store does: last copy wins.
	last := map[string]model.UsageEvent{}
	order := make([]string, 0, len(s.Events))
	for _, e := range s.Events {
		if _, ok := last[e.DedupKey]; !ok {
			order = append(order, e.DedupKey)
		}
		last[e.DedupKey] = e
	}
	for _, k := range order {
		e := last[k]
		s.Deduped = append(s.Deduped, e)
		s.TotalTokens = s.TotalTokens.Add(e.Tokens)
		if e.CostUSD != nil {
			s.TotalCostUSD += *e.CostUSD
		}
	}
	return s, nil
}
