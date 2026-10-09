package relay

import (
	"time"

	"github.com/zzstar101/mytoken/internal/pricing"
)

// RuleSourcePrefix marks rules that came from a gateway, so they can be told
// apart from user rules and cc-switch imports.
const RuleSourcePrefix = "relay:"

// RuleSource is the Source tag rules from this gateway carry.
func RuleSource(origin string) string { return RuleSourcePrefix + origin }

// ratioChangeThreshold is the relative change below which a re-derived ratio is
// treated as the same rule: a site that jitters by a fraction of a percent must
// not grow the rule table (docs/RELAY.md §5.5).
const ratioChangeThreshold = 0.01

// RulesFromSnapshot turns a gateway's ratio snapshot into dated price rules.
// The rules apply to every provider mapped to the site and carry the
// observation time, so a later snapshot supersedes an earlier one.
func RulesFromSnapshot(site Site, snap Snapshot) []pricing.Rule {
	if len(site.Providers) == 0 {
		return nil
	}
	at := snap.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var out []pricing.Rule
	for model, r := range snap.Ratios {
		if model == "" {
			continue
		}
		in, out2, cr, cw := unitPrices(r)
		for _, provider := range site.Providers {
			out = append(out, pricing.Rule{
				Provider:   provider,
				Model:      model,
				From:       at,
				Input:      &in,
				Output:     &out2,
				CacheRead:  &cr,
				CacheWrite: &cw,
				Source:     RuleSource(site.Origin),
			})
		}
	}
	return out
}

// RulesFromBills derives ratios from the bills a site actually charged, which
// is the only source of ratios when /api/pricing is locked behind a web
// session. One rule per (model, group) is produced, using the newest bill for
// that pair.
func RulesFromBills(site Site, bills []Bill) []pricing.Rule {
	if len(site.Providers) == 0 {
		return nil
	}
	// Newest bill per (model, group) wins.
	type key struct{ model, group string }
	best := map[key]Bill{}
	for _, b := range bills {
		if b.Model == "" || b.Ratios.Model == 0 {
			continue
		}
		k := key{b.Model, b.Group}
		if prev, ok := best[k]; !ok || b.At.After(prev.At) {
			best[k] = b
		}
	}
	var out []pricing.Rule
	for k, b := range best {
		in, outp, cr, cw := unitPrices(b.Ratios)
		for _, provider := range site.Providers {
			out = append(out, pricing.Rule{
				Provider:   provider,
				Model:      k.model,
				From:       b.At,
				Input:      &in,
				Output:     &outp,
				CacheRead:  &cr,
				CacheWrite: &cw,
				Source:     RuleSource(site.Origin),
			})
		}
	}
	return out
}

// unitPrices converts a gateway's ratios into $/1M token prices. new-api's
// model_ratio of 1 means $2 per 1M input tokens (docs/RELAY.md §4.1); sub2api
// quotes plain prices, where 1 is already $1/M, and the multiplier applies on
// top of the price rules.
func unitPrices(r Ratios) (input, output, cacheRead, cacheWrite float64) {
	modelRatio := r.Model
	if modelRatio == 0 {
		modelRatio = 1
	}
	group := r.Group
	if group == 0 {
		group = 1
	}
	input = 2 * modelRatio * group
	if r.FixedPrice > 0 {
		// Per-call models are not quoted per token; the fixed price rides along
		// in the model rule and the token prices are left at zero.
		return 0, 0, 0, 0
	}
	output = input * ratioOr(r.Completion, 1)
	cacheRead = input * ratioOr(r.Cache, 1)
	cacheWrite = input * ratioOr(r.CacheCreate, 1)
	return input, output, cacheRead, cacheWrite
}

// MergeRelayRules appends the rules a gateway just produced to the ones already
// stored, replacing a stored rule only when the new one is materially
// different: a change of more than 1% in any class. This keeps the rule table
// from growing every time a site's ratio jitters.
func MergeRelayRules(old, fresh []pricing.Rule) []pricing.Rule {
	out := make([]pricing.Rule, 0, len(old)+len(fresh))
	out = append(out, old...)
	for _, f := range fresh {
		if i := indexRule(out, f); i >= 0 {
			if changed(out[i], f) {
				out[i] = f
			}
			continue
		}
		out = append(out, f)
	}
	return out
}

// indexRule finds the rule for the same selector (provider, model) and source.
func indexRule(rules []pricing.Rule, want pricing.Rule) int {
	for i, r := range rules {
		if r.Provider == want.Provider && r.Model == want.Model && r.Source == want.Source {
			return i
		}
	}
	return -1
}

// changed reports whether any price class moved by more than 1%, or whether the
// new rule adds a class the old one did not set.
func changed(old, fresh pricing.Rule) bool {
	for _, pair := range [][2]*float64{
		{old.Input, fresh.Input},
		{old.Output, fresh.Output},
		{old.CacheRead, fresh.CacheRead},
		{old.CacheWrite, fresh.CacheWrite},
	} {
		if pair[0] == nil || pair[1] == nil {
			if pair[0] != pair[1] {
				return true
			}
			continue
		}
		if relativeChange(*pair[0], *pair[1]) > ratioChangeThreshold {
			return true
		}
	}
	return false
}

// relativeChange is |new-old| / |old|, or 1 when old is zero and new is not.
func relativeChange(old, fresh float64) float64 {
	if old == 0 {
		if fresh == 0 {
			return 0
		}
		return 1
	}
	d := fresh - old
	if d < 0 {
		d = -d
	}
	return d / old
}
