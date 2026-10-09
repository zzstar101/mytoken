package gui

import (
	"context"
	"fmt"
	"time"

	"github.com/zzstar101/mytoken/internal/reconcile"
	"github.com/zzstar101/mytoken/internal/relay"
)

// demoRelays is a made-up set of relays for snapshots and the demo mode.
type demoRelays struct {
	now   time.Time
	sites []reconcile.SiteStatus
}

// NewDemoRelays returns relays with one new-api site on, a sub2api site on
// and one site off.
func NewDemoRelays(now time.Time) Relays {
	f := func(v float64) *float64 { return &v }
	return &demoRelays{now: now, sites: []reconcile.SiteStatus{
		{Site: relay.Site{Origin: "https://api.kami-cn.dev", KeyID: "3f9a01c2b7de", Kind: relay.KindNewAPI, Layers: allLayers, Providers: []string{"kami-cn"}},
			Providers: []string{"kami-cn"}, HasKey: true, LastSync: now.Add(-4 * time.Minute),
			Balance: &relay.Balance{At: now.Add(-4 * time.Minute), RemainingUSD: f(37.42)}},
		{Site: relay.Site{Origin: "https://nerv-base.io", KeyID: "a1b2c3d4e5f6", Kind: relay.KindSub2API, Layers: allLayers, Providers: []string{"nerv-base"}},
			Providers: []string{"nerv-base"}, HasKey: true, LastSync: now.Add(-2 * time.Hour),
			Balance: &relay.Balance{At: now.Add(-2 * time.Hour), RemainingUSD: f(112.8)}},
		{Site: relay.Site{Origin: "https://relay.example.org", KeyID: "0f0f0f0f0f0f", Providers: []string{"example"}},
			Providers: []string{"example"}, HasKey: true},
	}}
}

func (d *demoRelays) Sites(context.Context) ([]reconcile.SiteStatus, error) {
	return append([]reconcile.SiteStatus(nil), d.sites...), nil
}

func (d *demoRelays) find(origin, keyID string) (*reconcile.SiteStatus, error) {
	for i := range d.sites {
		if d.sites[i].Origin == origin && d.sites[i].KeyID == keyID {
			return &d.sites[i], nil
		}
	}
	return nil, fmt.Errorf("no relay %s", origin)
}

func (d *demoRelays) Enable(_ context.Context, origin, keyID string, layers relay.Layer) (reconcile.SiteStatus, error) {
	st, err := d.find(origin, keyID)
	if err != nil {
		return reconcile.SiteStatus{}, err
	}
	st.Layers, st.Kind, st.LastSync = layers, relay.KindNewAPI, d.now
	return *st, nil
}

func (d *demoRelays) Disable(_ context.Context, origin, keyID string) error {
	st, err := d.find(origin, keyID)
	if err == nil {
		st.Layers = 0
	}
	return err
}

func (d *demoRelays) Sync(_ context.Context, origin, keyID string) error {
	st, err := d.find(origin, keyID)
	if err == nil {
		st.LastSync = d.now
	}
	return err
}

func (d *demoRelays) Report(_ context.Context, origin, keyID string, from, to time.Time) (reconcile.Report, error) {
	st, err := d.find(origin, keyID)
	if err != nil {
		return reconcile.Report{}, err
	}
	days := int(to.Sub(from).Hours()/24 + 0.5)
	if days > 30 {
		days = 30
	}
	if from.Before(to.AddDate(0, 0, -days)) {
		from = to.AddDate(0, 0, -days)
	}
	scale := float64(days) / 7
	mult := 0.312
	rep := reconcile.Report{Origin: origin, KeyID: keyID, From: from, To: to, Coverage: st.LastSync, Balance: st.Balance, ImpliedMultiplier: &mult,
		Lines: []reconcile.Line{
			{Category: reconcile.Matched, Count: int64(1830 * scale), LocalUSD: 41.20 * scale, FormulaUSD: 41.18 * scale, ChargedUSD: 41.31 * scale},
			{Category: reconcile.PriceDiff, Count: int64(212 * scale), LocalUSD: 6.10 * scale, FormulaUSD: 6.10 * scale, ChargedUSD: 7.02 * scale, Note: ""},
			{Category: reconcile.TokenSemantics, Count: int64(64 * scale), LocalUSD: 2.40 * scale, FormulaUSD: 2.40 * scale, ChargedUSD: 2.05 * scale},
			{Category: reconcile.BillOnly, Count: int64(31 * scale), ChargedUSD: 1.84 * scale},
			{Category: reconcile.EventOnly, Count: int64(18 * scale), LocalUSD: 0.62 * scale},
			{Category: reconcile.Refund, Count: 2, ChargedUSD: -0.35},
		},
		ByModel: []reconcile.ModelLine{
			{Model: "claude-sonnet-4-5", Count: int64(1400 * scale), LocalUSD: 31.5 * scale, ChargedUSD: 32.9 * scale},
			{Model: "claude-opus-4-1", Count: int64(260 * scale), LocalUSD: 12.2 * scale, ChargedUSD: 13.1 * scale},
			{Model: "gpt-5-codex", Count: int64(330 * scale), LocalUSD: 5.1 * scale, ChargedUSD: 5.2 * scale},
			{Model: "claude-haiku-4-5", Count: int64(150 * scale), LocalUSD: 1.4 * scale, ChargedUSD: 1.0 * scale},
		},
	}
	for _, l := range rep.Lines {
		rep.LocalUSD += l.LocalUSD
		rep.ChargedUSD += l.ChargedUSD
	}
	for i := 0; i < days; i++ {
		day := time.Date(from.Year(), from.Month(), from.Day()+i, 0, 0, 0, 0, time.Local)
		w := 0.6 + 0.4*float64((i*7)%5)/4
		local := rep.LocalUSD / float64(days) * w
		charged := local * (1 + 0.02*float64((i*3)%7-2))
		md, ud := (charged-local)*0.7, (charged-local)*0.3
		rep.ByDay = append(rep.ByDay, reconcile.DayLine{Day: day, Count: int64(300 * w), LocalUSD: local, ChargedUSD: charged, MultiplierDiffUSD: &md, UsageDiffUSD: &ud})
	}
	return rep, nil
}
