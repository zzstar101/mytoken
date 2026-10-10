package gui

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/reconcile"
	"github.com/zzstar101/mytoken/internal/relay"
)

// Relays is what the reconciliation page asks of the app: *reconcile.Service
// satisfies it. Sites and Report are offline; Enable and Sync reach the site
// (and only it), Disable keeps what was fetched.
type Relays interface {
	Sites(ctx context.Context) ([]reconcile.SiteStatus, error)
	Enable(ctx context.Context, origin, keyID string, layers relay.Layer) (reconcile.SiteStatus, error)
	Disable(ctx context.Context, origin, keyID string) error
	Sync(ctx context.Context, origin, keyID string) error
	Report(ctx context.Context, origin, keyID string, from, to time.Time) (reconcile.Report, error)
}

// allLayers is what the page's switch turns on: ratios, balance and bills.
const allLayers = relay.LayerRatio | relay.LayerBalance | relay.LayerBills

// relayColor is the page's own: between Tomori's sky and Rana's green.
var relayColor = Tomori.Mix(Rana, 0.5)

// relayState backs the reconciliation page.
type relayState struct {
	loaded bool
	sites  []reconcile.SiteStatus
	err    error
	sel    string // siteKey of the selected site

	want   string // siteKey|span of the report wanted
	have   string // ... of the report shown
	report reconcile.Report
	repErr error

	busy    map[string]bool // sites with a network call running
	message string
}

func siteKey(origin, keyID string) string { return origin + "|" + keyID }

// relayRange is the report's range for a span; "all" is everything to date.
func relayRange(sp Span, now time.Time) (time.Time, time.Time) {
	r := rangeOf(sp, now)
	if r.From.IsZero() {
		day := now.Local()
		return time.Unix(0, 0), time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, time.Local)
	}
	return r.From, r.To
}

func (s *State) loadRelays() {
	rs := s.Hooks.Relays
	if rs == nil {
		return
	}
	s.run("relays", func(ctx context.Context) func() {
		sites, err := rs.Sites(ctx)
		return func() {
			s.rl.sites, s.rl.err, s.rl.loaded = sites, err, true
			if s.rl.selected() != nil {
				return
			}
			s.rl.sel = ""
			// Pick the first site turned on, else the first.
			for _, st := range sites {
				if st.Layers != 0 {
					s.rl.sel = siteKey(st.Origin, st.KeyID)
					return
				}
			}
			if len(sites) > 0 {
				s.rl.sel = siteKey(sites[0].Origin, sites[0].KeyID)
			}
		}
	})
}

// ensureRelays loads the site list once per reload; safe to call per frame.
func (s *State) ensureRelays() {
	if s.Hooks.Relays != nil && !s.rl.loaded {
		s.rl.loaded = true
		s.loadRelays()
	}
}

// balanceSites lists the relays turned on that have a balance to show.
func (s *State) balanceSites() []reconcile.SiteStatus {
	s.ensureRelays()
	var on []reconcile.SiteStatus
	for _, st := range s.rl.sites {
		if st.Layers != 0 && st.Balance != nil {
			on = append(on, st)
		}
	}
	return on
}

// trayBalances is the panel's line of balances for relays turned on.
func (s *State) trayBalances(c *ui.Context, pal palette) {
	on := s.balanceSites()
	if len(on) == 0 {
		return
	}
	ui.Row(c).Gap(6).Children(func() {
		for i, st := range on {
			if i >= 2 {
				break
			}
			chip := ui.Row(c.Key("bal"+siteKey(st.Origin, st.KeyID))).Gap(6).Padding(6, 9).Radius(9).AlignItems(ui.Center).
				Background(pal.well).Border(1, pal.edge).Grow(1).Basis(0).Cursor(ui.CursorPointer).Role(ui.RoleButton).Transition(hoverFade)
			if chip.Hovered() {
				chip.Background(pal.hover)
			}
			chip.Children(func() {
				dot(c, relayColor, 6)
				ui.Text(c, hostOf(st.Origin)).FontSize(11).TextColor(pal.muted).SingleLine().Ellipsis("…").Grow(1).Basis(0)
				ui.Text(c, balanceText(st.Balance)).FontSize(12).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum")
			})
			if chip.Clicked() {
				s.page = "relays"
				s.rl.sel = siteKey(st.Origin, st.KeyID)
				if s.Hooks.OpenMain != nil {
					s.Hooks.OpenMain()
				}
			}
		}
	})
}

func (r *relayState) selected() *reconcile.SiteStatus {
	for i := range r.sites {
		if siteKey(r.sites[i].Origin, r.sites[i].KeyID) == r.sel {
			return &r.sites[i]
		}
	}
	return nil
}

// loadReport fetches the selected site's report for the current span.
func (s *State) loadReport(st reconcile.SiteStatus) {
	rs := s.Hooks.Relays
	if rs == nil {
		return
	}
	want := s.rl.want
	from, to := relayRange(Span(s.span), s.Hooks.Now())
	s.run("relay-report", func(ctx context.Context) func() {
		rep, err := rs.Report(ctx, st.Origin, st.KeyID, from, to)
		return func() {
			if s.rl.want != want {
				return // the selection or span moved on; a newer load follows
			}
			s.rl.report, s.rl.repErr, s.rl.have = rep, err, want
		}
	})
}

// relayOp runs a network call for one site, then reloads the sites, the
// report and — since gateway ratios may have moved the prices — the rest.
func (s *State) relayOp(st reconcile.SiteStatus, done string, op func(ctx context.Context, rs Relays) error) {
	rs := s.Hooks.Relays
	k := siteKey(st.Origin, st.KeyID)
	if rs == nil || s.rl.busy[k] {
		return
	}
	if s.rl.busy == nil {
		s.rl.busy = map[string]bool{}
	}
	s.rl.busy[k] = true
	s.run("relay-op:"+k, func(ctx context.Context) func() {
		err := op(ctx, rs)
		return func() {
			delete(s.rl.busy, k)
			s.rl.message = done
			if err != nil {
				s.rl.message = tr("failed") + ": " + err.Error()
			}
			s.rl.loaded, s.rl.want = false, ""
			s.Reload()
		}
	})
}

func (s *State) relaysPage(c *ui.Context, pal palette) {
	pageHeader(c, pal, "relays", tr("relaysSub"), func() { s.spanSwitch(c, pal) })
	s.ensureRelays()
	sel := s.rl.selected()
	if sel != nil && sel.Layers != 0 {
		want := s.rl.sel + "|" + spanKeys[clampSpan(s.span)]
		if s.rl.want != want {
			s.rl.want = want
			s.loadReport(*sel)
		}
	}
	ui.Scroll(c).Grow(1).Children(func() {
		ui.Row(c).Padding(8, 8, 16, 8).Gap(14).AlignItems(ui.Start).Children(func() {
			ui.Column(c).Width(330).Shrink(0).Gap(12).Children(func() {
				rise(c, 0, func() { s.relayPrivacy(c, pal) })
				rise(c, 1, func() { s.siteList(c, pal) })
			})
			ui.Column(c).Grow(1).Basis(0).Gap(14).Children(func() {
				switch {
				case s.rl.err != nil:
					pane(c, pal).Padding(40).Children(func() {
						emptyState(c, pal, artPicks, tr("failed"), s.rl.err.Error())
					})
				case len(s.rl.sites) == 0:
					pane(c, pal).Padding(40).Children(func() {
						emptyState(c, pal, artStage, tr("noRelays"), tr("noRelaysSub"))
					})
				case sel == nil:
				case sel.Layers == 0:
					s.relayOff(c, pal, *sel)
				default:
					s.relayReport(c, pal, *sel)
				}
			})
		})
	})
}

// relayPrivacy says plainly what turning a site on does.
func (s *State) relayPrivacy(c *ui.Context, pal palette) {
	ui.Column(c).Padding(14, 16).Gap(6).Radius(16).Background(relayColor.Alpha(0.10)).Border(1, relayColor.Alpha(0.28)).Children(func() {
		ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, icShield).FontSize(14).TextColor(relayColor)
			ui.Text(c, tr("relayPrivacy")).FontSize(12.5).FontWeight(680).TextColor(pal.ink)
		})
		ui.Text(c, tr("relayPrivacySub")).FontSize(11.5).TextColor(pal.muted).LineHeight(1.45)
		if s.rl.message != "" {
			ui.Text(c, s.rl.message).FontSize(11.5).TextColor(pal.ink.Alpha(0.8)).Margin(4, 0, 0, 0)
		}
	})
}

func (s *State) siteList(c *ui.Context, pal palette) {
	now := s.Hooks.Now()
	for _, st := range s.rl.sites {
		k := siteKey(st.Origin, st.KeyID)
		on, picked, busy := st.Layers != 0, k == s.rl.sel, s.rl.busy[k]
		card := pane(c.Key("site-"+k), pal).Padding(14, 16).Gap(8).Cursor(ui.CursorPointer).Transition(hoverFade)
		if picked {
			card.Border(1.5, relayColor.Alpha(0.75))
		} else if card.Hovered() {
			card.Border(1, relayColor.Alpha(0.4))
		}
		if !st.HasKey && !on {
			card.Opacity(0.62)
		}
		card.Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				dot(c, siteDot(pal, st), 8)
				ui.Text(c, hostOf(st.Origin)).FontSize(14).FontWeight(700).TextColor(pal.ink).SingleLine().Ellipsis("…").Grow(1).Basis(0)
				if st.Kind != "" && st.Kind != relay.KindUnknown {
					chipOn(c, pal, kindLabel(st.Kind), relayColor)
				}
			})
			if len(st.Providers) > 0 {
				ui.Text(c, strings.Join(st.Providers, " · ")).FontSize(11).TextColor(pal.muted).SingleLine().Ellipsis("…")
			}
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				ui.Column(c).Grow(1).Basis(0).Gap(2).Children(func() {
					switch {
					case busy:
						ui.Text(c, tr("relaySyncing")).FontSize(11).TextColor(relayColor)
						c.After(400 * time.Millisecond)
					case st.LastError != "":
						ui.Text(c, st.LastError).FontSize(11).TextColor(Anon).SingleLine().Ellipsis("…").Tooltip(st.LastError)
					case !st.HasKey:
						ui.Text(c, tr("relayNoKey")).FontSize(11).TextColor(pal.muted)
					case !st.LastSync.IsZero():
						ui.Text(c, trf("relaySynced", fmtAgo(now, st.LastSync))).FontSize(11).TextColor(pal.muted)
					default:
						ui.Text(c, tr("relayOff")).FontSize(11).TextColor(pal.muted)
					}
					if b := st.Balance; b != nil && on {
						ui.Text(c, balanceText(b)).FontSize(15).FontWeight(720).TextColor(pal.ink).FontFeatures("tnum")
					}
				})
				if on && st.HasKey {
					btn := ui.Box(c).Size(28, 28).Radius(9).Center().
						Cursor(ui.CursorPointer).Tooltip(tr("relaySyncNow")).Label(tr("relaySyncNow")).Transition(hoverFade).Disabled(busy)
					if btn.Hovered() {
						btn.Background(pal.hover)
					}
					btn.Children(func() { ui.Icon(c, icRefresh).FontSize(14).TextColor(pal.muted) })
					if btn.Clicked() {
						s.relayOp(st, trf("relaySyncedNow", hostOf(st.Origin)), func(ctx context.Context, rs Relays) error {
							return rs.Sync(ctx, st.Origin, st.KeyID)
						})
					}
				}
				want := on
				sw := ui.Switch(c.Key("sw-"+k), &want).Disabled(busy || (!st.HasKey && !on)).Label(tr("relayEnable"))
				if sw.Changed() {
					s.rl.sel = k
					if want {
						s.relayOp(st, trf("relayEnabled", hostOf(st.Origin)), func(ctx context.Context, rs Relays) error {
							_, err := rs.Enable(ctx, st.Origin, st.KeyID, allLayers)
							return err
						})
					} else {
						s.relayOp(st, trf("relayDisabled", hostOf(st.Origin)), func(ctx context.Context, rs Relays) error {
							return rs.Disable(ctx, st.Origin, st.KeyID)
						})
					}
				}
			})
		})
		if card.Clicked() {
			s.rl.sel = k
		}
	}
}

func siteDot(pal palette, st reconcile.SiteStatus) ui.Color {
	switch {
	case st.LastError != "":
		return Anon
	case st.Layers != 0:
		return Rana
	}
	return pal.faint
}

// relayOff is the right side for a site not turned on: what it would do.
func (s *State) relayOff(c *ui.Context, pal palette, st reconcile.SiteStatus) {
	pane(c, pal).Padding(36, 40).Gap(14).Children(func() {
		emptyState(c, pal, artPicks, hostOf(st.Origin), tr("relayOffSub"))
		for _, l := range []string{"relayLayerRatio", "relayLayerBalance", "relayLayerBills"} {
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				dot(c, relayColor, 6)
				ui.Text(c, tr(l)).FontSize(12.5).TextColor(pal.ink.Alpha(0.85))
			})
		}
	})
}

func (s *State) relayReport(c *ui.Context, pal palette, st reconcile.SiteStatus) {
	rep := s.rl.report
	// While another span of the same site loads, the old report stays.
	if s.rl.have == "" || !strings.HasPrefix(s.rl.have, s.rl.sel+"|") {
		pane(c, pal).Padding(40).Children(func() { emptyState(c, pal, artStaff, tr("loading"), "") })
		return
	}
	if s.rl.repErr != nil {
		pane(c, pal).Padding(40).Children(func() { emptyState(c, pal, artPicks, tr("failed"), s.rl.repErr.Error()) })
		return
	}
	rise(c, 0, func() { relayHero(c, pal, st, rep) })
	if len(rep.Lines) == 0 && len(rep.ByModel) == 0 {
		pane(c, pal).Padding(36).Children(func() { emptyState(c, pal, artStaff, tr("noBills"), tr("noBillsSub")) })
		return
	}
	rise(c, 1, func() {
		card(c, pal, tr("relayWaterfall"), func() { waterLegend(c, pal) }, func() {
			waterfall(c.Key("water-"+s.rl.have), pal, rep, 210)
		})
	})
	rise(c, 2, func() {
		ui.Row(c).Gap(14).AlignItems(ui.Stretch).Children(func() {
			card(c, pal, tr("relayCategories"), nil, func() { categoryTable(c, pal, rep.Lines) }).Grow(1).Basis(0)
			card(c, pal, tr("relayByModel"), nil, func() { modelTable(c, pal, rep.ByModel) }).Grow(1).Basis(0)
		})
	})
	if len(rep.ByDay) > 1 {
		rise(c, 3, func() {
			card(c, pal, tr("relayByDay"), func() { waterLegend(c, pal) }, func() {
				dayPairs(c.Key("days-"+s.rl.have), pal, rep.ByDay, 150)
			})
		})
	}
}

// relayHero is the answer in one line: what we think it cost, what the
// site charged, and the gap.
func relayHero(c *ui.Context, pal palette, st reconcile.SiteStatus, rep reconcile.Report) {
	pane(c, pal).Padding(22, 26).Gap(16).Children(func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Text(c, hostOf(st.Origin)).Font(display).FontSize(20).FontWeight(750).TextColor(pal.ink)
			if st.Kind != "" && st.Kind != relay.KindUnknown {
				chipOn(c, pal, kindLabel(st.Kind), relayColor)
			}
			ui.Spacer(c)
			if !rep.Coverage.IsZero() {
				ui.Text(c, trf("relayCoverage", fmtWhen(rep.Coverage))).FontSize(11).TextColor(pal.muted)
			}
		})
		ui.Row(c).Gap(34).AlignItems(ui.End).Children(func() {
			heroFigure(c, pal, tr("relayLocal"), rep.LocalUSD, pal.ink.Alpha(0.85))
			heroFigure(c, pal, tr("relayCharged"), rep.ChargedUSD, relayColor)
			diff := rep.ChargedUSD - rep.LocalUSD
			ui.Column(c).Gap(4).Children(func() {
				kicker(c, pal, tr("relayDiff"))
				col, sign := Rana, "−"
				if diff > 0 {
					col, sign = Anon, "+"
				}
				if math.Abs(diff) < 0.005 {
					col, sign = pal.muted, "±"
				}
				ui.Text(c, sign+fmtCost(math.Abs(diff))).Font(display).FontSize(30).FontWeight(750).TextColor(col).FontFeatures("tnum")
				if rep.LocalUSD > 0 {
					ui.Text(c, signedPct(diff/rep.LocalUSD)).FontSize(11.5).FontWeight(650).TextColor(col).FontFeatures("tnum")
				}
			})
			ui.Spacer(c)
			ui.Column(c).Gap(8).AlignItems(ui.End).Children(func() {
				if m := rep.ImpliedMultiplier; m != nil {
					miniStat(c, pal, tr("relayImplied"), trimNum(*m, 3)+"×")
				}
				if b := rep.Balance; b != nil {
					miniStat(c, pal, tr("relayBalance"), balanceText(b))
				}
			})
		})
	})
}

func heroFigure(c *ui.Context, pal palette, label string, usd float64, col ui.Color) {
	ui.Column(c).Gap(4).Children(func() {
		kicker(c, pal, label)
		ui.Text(c, fmtCost(usd)).Font(display).FontSize(30).FontWeight(750).TextColor(col).FontFeatures("tnum")
	})
}

func signedPct(f float64) string {
	if f >= 0 {
		return "+" + trimNum(f*100, 1) + "%"
	}
	return "−" + trimNum(-f*100, 1) + "%"
}

func fmtSigned(v float64) string {
	if v < 0 {
		return "−" + fmtCost(-v)
	}
	return "+" + fmtCost(v)
}

func balanceText(b *relay.Balance) string {
	switch {
	case b.Unlimited:
		return tr("relayUnlimited")
	case b.RemainingUSD != nil:
		return inCurrency(fmtCost(*b.RemainingUSD), b.Currency)
	case b.UsedUSD != nil:
		return trf("relayUsed", inCurrency(fmtCost(*b.UsedUSD), b.Currency))
	}
	return "—"
}

// inCurrency swaps the dollar sign for the currency a balance is quoted in.
// Amounts are not converted: DeepSeek's balance stays in yuan.
func inCurrency(s, currency string) string {
	switch currency {
	case "", "USD":
		return s
	case "CNY":
		return strings.Replace(s, "$", "¥", 1)
	}
	return strings.Replace(s, "$", "", 1) + " " + currency
}

func hostOf(origin string) string {
	if i := strings.Index(origin, "://"); i >= 0 {
		return origin[i+3:]
	}
	return origin
}

func kindLabel(k relay.Kind) string {
	switch k {
	case relay.KindNewAPI:
		return "new-api"
	case relay.KindSub2API:
		return "sub2api"
	case relay.KindOpenRouter:
		return "OpenRouter"
	case relay.KindDeepSeek:
		return "DeepSeek"
	case relay.KindSilicon:
		return "SiliconFlow"
	case relay.KindMoonshot:
		return "Moonshot"
	case relay.KindZAI:
		return "Z.ai"
	case relay.KindMiniMax:
		return "MiniMax"
	}
	return string(k)
}

// categoryColor colors a reconciliation category.
func categoryColor(pal palette, k reconcile.Category) ui.Color {
	switch k {
	case reconcile.Matched:
		return Rana
	case reconcile.PriceDiff:
		return Soyo
	case reconcile.TokenSemantics:
		return Taki
	case reconcile.BillOnly:
		return Anon
	case reconcile.EventOnly:
		return Tomori
	case reconcile.Refund:
		return relayColor
	}
	return pal.muted
}

func waterLegend(c *ui.Context, pal palette) {
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		for _, it := range []struct {
			key string
			col ui.Color
		}{{"relayLocal", pal.ink.Alpha(0.45)}, {"relayCharged", relayColor}} {
			ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
				dot(c, it.col, 7)
				ui.Text(c, tr(it.key)).FontSize(11).TextColor(pal.muted)
			})
		}
	})
}

// waterfall walks from the local estimate to what was charged: one floating
// bar per category for the money it adds or takes away.
func waterfall(c *ui.Context, pal palette, rep reconcile.Report, height float32) ui.Element {
	type step struct {
		label    string
		from, to float64
		col      ui.Color
		total    bool
	}
	steps := []step{{tr("relayLocal"), 0, rep.LocalUSD, pal.ink.Alpha(0.45), true}}
	run := rep.LocalUSD
	for _, l := range rep.Lines {
		d := l.ChargedUSD - l.LocalUSD
		if math.Abs(d) < 1e-9 {
			continue
		}
		steps = append(steps, step{tr("cat-" + string(l.Category)), run, run + d, categoryColor(pal, l.Category), false})
		run += d
	}
	steps = append(steps, step{tr("relayCharged"), 0, rep.ChargedUSD, relayColor, true})
	// The differences are small next to the totals, so the axis starts just
	// under the lowest running total; the two total bars are cut short.
	top, low := math.Inf(-1), math.Inf(1)
	for _, s := range steps {
		for _, v := range []float64{s.from, s.to} {
			if s.total && v == 0 {
				continue
			}
			top, low = math.Max(top, v), math.Min(low, v)
		}
	}
	pad := math.Max((top-low)*0.6, top*0.01)
	if pad <= 0 {
		pad = 1
	}
	top += pad * 0.4
	low = math.Max(0, low-pad)
	if low < (top-low)*0.5 {
		low = 0
	}
	el := ui.Box(c).FillWidth().Height(height)
	g := float64(entrance(el, 700*time.Millisecond))
	hx, _, hover := el.PointerPosition()
	return el.Draw(func(p *ui.Painter, r ui.Rect) {
		const labelH = 30
		plot := ui.Rect{X: r.X, Y: r.Y + 18, W: r.W, H: r.H - labelH - 18}
		y := func(v float64) float32 {
			return plot.Y + plot.H*float32((top-math.Max(v, low))/(top-low))
		}
		base := plot.Y + plot.H
		p.Line(plot.X, base, plot.X+plot.W, base, 1, pal.faint)
		slot := plot.W / float32(len(steps))
		bw := min(slot*0.56, 64)
		hi := -1
		if hover {
			hi = int((hx - plot.X) / slot)
		}
		for i, s := range steps {
			x := plot.X + slot*float32(i) + (slot-bw)/2
			a, b := s.from, s.from+(s.to-s.from)*g
			if s.total {
				a, b = low, low+(s.to-low)*g
			}
			y0, y1 := y(math.Max(a, b)), y(math.Min(a, b))
			col := s.col
			if hi >= 0 && hi != i {
				col = col.Alpha(0.45)
			}
			p.Fill(ui.Rect{X: x, Y: y0, W: bw, H: max(y1-y0, 2)}, col, 5)
			if s.total && low > 0 {
				// a break in the bar: the axis does not start at zero
				for k := float32(0); k < 2; k++ {
					by := base - 10 - k*5
					p.Line(x-2, by+2, x+bw+2, by-2, 2, pal.pane)
				}
			}
			if i < len(steps)-1 {
				// a thin connector from this bar's end to the next one's start
				ny := y(s.to)
				p.Line(x+bw, ny, x+slot, ny, 1, pal.faint)
			}
			txt := fmtCost(s.to)
			if !s.total {
				txt = fmtSigned(s.to - s.from)
			}
			tw, _ := p.MeasureText(0, ui.Span{Text: txt, Size: 11, Weight: 650})
			p.RichText(x+bw/2-tw/2, y0-16, 0, ui.Span{Text: txt, Size: 11, Weight: 650, Color: pal.ink.Alpha(0.8)})
			lw, _ := p.MeasureText(slot-4, ui.Span{Text: s.label, Size: 11})
			p.RichText(plot.X+slot*float32(i)+(slot-lw)/2, plot.Y+plot.H+8, slot-4, ui.Span{Text: s.label, Size: 11, Color: pal.muted})
		}
	})
}

// categoryTable lists the categories with counts, money and notes.
func categoryTable(c *ui.Context, pal palette, lines []reconcile.Line) {
	for i, l := range lines {
		if i > 0 {
			ui.Divider(c)
		}
		ui.Column(c).Gap(3).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				dot(c, categoryColor(pal, l.Category), 8)
				ui.Text(c, tr("cat-"+string(l.Category))).FontSize(13).FontWeight(650).TextColor(pal.ink)
				ui.Text(c, fmtInt(l.Count)).FontSize(11).TextColor(pal.muted).FontFeatures("tnum")
				ui.Spacer(c)
				ui.Text(c, fmtCost(l.LocalUSD)+" → "+fmtCost(l.ChargedUSD)).FontSize(12).FontWeight(620).TextColor(pal.ink.Alpha(0.85)).FontFeatures("tnum")
			})
			note := l.Note
			if note == "" {
				note = tr("catsub-" + string(l.Category))
			}
			ui.Text(c, note).FontSize(11).TextColor(pal.muted).LineHeight(1.4).Margin(0, 0, 0, 16)
		})
	}
}

func modelTable(c *ui.Context, pal palette, rows []reconcile.ModelLine) {
	top := 0.0
	for _, m := range rows {
		top = math.Max(top, math.Max(m.LocalUSD, m.ChargedUSD))
	}
	if top == 0 {
		top = 1
	}
	for i, m := range rows {
		if i >= 8 {
			ui.Text(c, trf("relayMore", len(rows)-i)).FontSize(11).TextColor(pal.muted)
			break
		}
		ui.Column(c).Gap(4).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, m.Model).FontSize(12.5).FontWeight(620).TextColor(pal.ink).SingleLine().Ellipsis("…").Grow(1).Basis(0)
				ui.Text(c, fmtCost(m.LocalUSD)+" → "+fmtCost(m.ChargedUSD)).FontSize(11.5).TextColor(pal.ink.Alpha(0.8)).FontFeatures("tnum")
			})
			rankBar(c, pal, m.LocalUSD/top, pal.ink.Alpha(0.35), 3)
			rankBar(c, pal, m.ChargedUSD/top, relayColor, 3)
		})
	}
}

// dayPairs draws each day's local estimate and charge side by side.
func dayPairs(c *ui.Context, pal palette, days []reconcile.DayLine, height float32) ui.Element {
	top := 0.0
	for _, d := range days {
		top = math.Max(top, math.Max(d.LocalUSD, d.ChargedUSD))
	}
	top = niceMax(top)
	el := ui.Box(c).FillWidth().Height(height)
	g := entrance(el, 650*time.Millisecond)
	hx, hy, hover := el.PointerPosition()
	return el.Draw(func(p *ui.Painter, r ui.Rect) {
		const labelH = 18
		plot := ui.Rect{X: r.X, Y: r.Y, W: r.W, H: r.H - labelH}
		slot := plot.W / float32(len(days))
		bw := min(slot*0.32, 14)
		hi := -1
		if hover {
			hi = max(0, min(int((hx-plot.X)/slot), len(days)-1))
		}
		bar := func(x float32, v float64, col ui.Color) {
			h := plot.H * g * float32(v/top)
			if v > 0 && h < 2 {
				h = 2
			}
			p.Fill(ui.Rect{X: x, Y: plot.Y + plot.H - h, W: bw, H: h}, col, 3)
		}
		step := max(1, len(days)/8)
		for i, d := range days {
			x := plot.X + slot*float32(i) + slot/2
			a, b := pal.ink.Alpha(0.35), relayColor
			if hi >= 0 && hi != i {
				a, b = pal.ink.Alpha(0.15), relayColor.Alpha(0.4)
			}
			bar(x-bw-1, d.LocalUSD, a)
			bar(x+1, d.ChargedUSD, b)
			if i%step == 0 {
				lbl := d.Day.Format("1/2")
				lw, _ := p.MeasureText(0, ui.Span{Text: lbl, Size: 10})
				p.RichText(x-lw/2, plot.Y+plot.H+4, 0, ui.Span{Text: lbl, Size: 10, Color: pal.muted})
			}
		}
		if hi >= 0 {
			d := days[hi]
			rows := [][2]string{{tr("relayLocal"), fmtCost(d.LocalUSD)}, {tr("relayCharged"), fmtCost(d.ChargedUSD)}}
			cols := []ui.Color{pal.ink.Alpha(0.45), relayColor}
			if d.MultiplierDiffUSD != nil {
				rows = append(rows, [2]string{tr("relayMultDiff"), fmtSigned(*d.MultiplierDiffUSD)})
				cols = append(cols, Soyo)
			}
			if d.UsageDiffUSD != nil {
				rows = append(rows, [2]string{tr("relayUsageDiff"), fmtSigned(*d.UsageDiffUSD)})
				cols = append(cols, Taki)
			}
			tooltipBox(p, pal, r, plot.X+slot*float32(hi)+slot/2, hy, d.Day.Format("2006-01-02"), rows, cols)
		}
	})
}

var icScale = icon(`<path d="M12 3v18M7 21h10M5 7h14"/><path d="m5 7-3 7a3 3 0 0 0 6 0zM19 7l-3 7a3 3 0 0 0 6 0z"/>`)

var icShield = icon(`<path d="M12 3 5 6v5c0 4.4 3 8.3 7 10 4-1.7 7-5.6 7-10V6z"/><path d="m9 12 2 2 4-4"/>`)

func init() {
	for k, v := range relayStrs {
		strs[k] = v
	}
}

var relayStrs = map[string][2]string{
	"relays":                 {"对账", "Reconcile"},
	"relaysSub":              {"本地估算 vs 中转站实扣", "Local estimate vs what the relay charged"},
	"relayPrivacy":           {"默认不联网，逐站开启", "Off by default, one site at a time"},
	"relayPrivacySub":        {"开启后只用你已经交给该站点的 Key，只访问该站点本身，只读。Key 不落盘、不写日志。", "When on, MyToken uses only the key you already gave that site, talks only to that site, and only reads. Keys are never stored or logged."},
	"noRelays":               {"没有发现中转站", "No relays found"},
	"noRelaysSub":            {"在 cc-switch 或 Claude Code / Codex 配置里设了自定义 Base URL 的站点会出现在这里", "Sites set as a custom base URL in cc-switch or Claude Code / Codex config show up here"},
	"relayOffSub":            {"打开左侧的开关后，MyToken 会识别站点类型并拉取：", "Turn it on and MyToken detects the site and fetches:"},
	"relayLayerRatio":        {"倍率与价格表，用来按站点的规则重算本地费用", "Ratios and prices, to price your usage the site's way"},
	"relayLayerBalance":      {"余额", "Balance"},
	"relayLayerBills":        {"逐条或按天的扣费记录，和本地请求一一对账", "Per-request or daily charges, matched to your requests"},
	"relayEnable":            {"开启对账", "Reconcile this site"},
	"relayEnabled":           {"已开启 %s", "Turned on %s"},
	"relayDisabled":          {"已关闭 %s，已拉取的数据保留", "Turned off %s; fetched data is kept"},
	"relaySyncNow":           {"立即同步", "Sync now"},
	"relaySyncedNow":         {"%s 已同步", "%s synced"},
	"relaySyncing":           {"正在同步…", "Syncing…"},
	"relaySynced":            {"%s同步", "Synced %s"},
	"relayNoKey":             {"配置里已找不到这个站点的 Key", "Its key is no longer in your config"},
	"relayOff":               {"未开启", "Off"},
	"relayLocal":             {"本地估算", "Local estimate"},
	"relayCharged":           {"实际扣费", "Charged"},
	"relayDiff":              {"差额", "Difference"},
	"relayImplied":           {"实际倍率", "Implied multiplier"},
	"relayBalance":           {"余额", "Balance"},
	"relayUnlimited":         {"不限额", "Unlimited"},
	"relayUsed":              {"已用 %s", "%s used"},
	"relayCoverage":          {"只核对 %s 之后（站点保留的账单从这里开始）", "Checked from %s on (the oldest charge the site kept)"},
	"relayWaterfall":         {"差额从哪来", "Where the difference comes from"},
	"relayCategories":        {"对账分类", "Categories"},
	"relayByModel":           {"按模型", "By model"},
	"relayByDay":             {"按天", "By day"},
	"relayMultDiff":          {"倍率差", "Multiplier"},
	"relayUsageDiff":         {"用量差", "Usage"},
	"relayMore":              {"还有 %d 个", "and %d more"},
	"noBills":                {"这段时间没有账单", "No bills in this span"},
	"noBillsSub":             {"同步后再看看，或者换一个时间段", "Sync, or pick another span"},
	"loading":                {"加载中…", "Loading…"},
	"cat-matched":            {"对上了", "Matched"},
	"cat-price-diff":         {"价格不同", "Price differs"},
	"cat-token-semantics":    {"口径不同", "Token semantics"},
	"cat-bill-only":          {"只有账单", "Bill only"},
	"cat-event-only":         {"只有本地记录", "Local only"},
	"cat-refund":             {"退款", "Refunds"},
	"catsub-matched":         {"同一请求，金额在 ±2% 内", "Same request, within ±2%"},
	"catsub-price-diff":      {"同一请求，站点的单价或倍率和本地不一样", "Same request, but the site's price or multiplier differs"},
	"catsub-token-semantics": {"同一请求，缓存 token 的计法不同", "Same request, cache tokens counted differently"},
	"catsub-bill-only":       {"站点扣了钱，本地没有对应的请求（可能是别的设备或工具用了同一个 Key）", "Charged, but no local request matches (another device or tool on the same key?)"},
	"catsub-event-only":      {"本地有请求，站点没有扣费记录（失败、免费、用的是别的 Key，或账单还没同步）", "A local request with no charge (failed, free, another key, or not synced yet)"},
	"catsub-refund":          {"站点退回的额度", "Quota the site gave back"},
}
