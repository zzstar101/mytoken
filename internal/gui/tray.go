package gui

import (
	"github.com/egoist/mygo/ui"
)

// TrayWidth and TrayHeight are the tray panel's size.
const TrayWidth, TrayHeight = 340, 520

// TrayView is the menu-bar panel: today at a glance.
func (s *State) TrayView(c *ui.Context) {
	pal := applyTheme(c)
	t := s.today
	c.Root().Background(ui.Transparent)
	ui.Box(c).Fill().Radius(16).Clip().Draw(aurora(pal)).Children(func() {
		pane(c, pal).Fill().Padding(18, 18, 14, 18).Gap(14).Children(func() {
			ui.Row(c).AlignItems(ui.Center).Children(func() {
				logo(c, pal, 17)
				ui.Spacer(c)
				ui.Text(c, s.Hooks.Now().Format("Mon · Jan 2")).Font(serif).Italic().FontSize(14).TextColor(pal.muted)
			})
			// Today, large.
			ui.Column(c).Gap(6).Children(func() {
				ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
					bangs(c, 10, t.Totals.Tokens.Total() > 0)
					kicker(c, pal, tr("today"))
				})
				ui.Row(c).AlignItems(ui.End).Gap(10).Children(func() {
					countUp(c, pal, float64(t.Totals.Tokens.Total()), func(v float64) string { return fmtTokens(int64(v)) }, 58)
					ui.Spacer(c)
					ui.Column(c).AlignItems(ui.End).Gap(2).Padding(0, 0, 8, 0).Children(func() {
						ui.RichText(c, figure(pal, fmtCostOf(t.Totals.CostUSD, t.Totals.Requests, t.Totals.Unpriced), 26)...)
						ui.Text(c, trf("requestsN", fmtInt(t.Totals.Requests))+" · "+tr("cacheHit")+" "+fmtPct(t.Totals.CacheHit)).FontSize(10.5).TextColor(pal.muted)
					})
				})
				classBar(c, pal, t.Totals.Tokens, 5)
			})
			if t.Totals.Tokens.Total() == 0 {
				ui.Column(c).Grow(1).Center().Children(func() {
					emptyState(c, pal, artStage, tr("nothingToday"), tr("nothingTodaySub"))
				})
			} else {
				ui.Column(c).Gap(8).Children(func() {
					kicker(c, pal, tr("last24h"))
					miniBars(c, pal, t.Hourly, 62)
				})
				ui.Column(c).Gap(8).Children(func() {
					kicker(c, pal, tr("topModels"))
					s.bucketList(c, pal, t.Models, 3, true, false)
				})
				// What is playing now, one line each.
				ui.Column(c).Gap(4).Grow(1).Basis(0).Clip().Children(func() {
					if len(t.Active) == 0 {
						return
					}
					kicker(c, pal, tr("activeNow"))
					for i, r := range t.Active {
						if i >= 2 {
							break
						}
						row := ui.Row(c.Key(string(r.Harness)+r.SessionID)).Gap(8).Padding(5, 6).Radius(8).AlignItems(ui.Center).Cursor(ui.CursorPointer).Role(ui.RoleButton).Transition(hoverFade)
						if row.Hovered() {
							row.Background(pal.hover)
						}
						row.Children(func() {
							harnessMark(c, r.Harness, 15)
							title := r.Title
							if title == "" {
								title = tr("untitled")
							}
							ui.Text(c, title).FontSize(12.5).FontWeight(600).TextColor(pal.ink).SingleLine().Ellipsis("…").Grow(1).Basis(0)
							ui.Text(c, fmtTokens(r.Tokens.Total())).FontSize(12).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum")
						})
						if row.Clicked() {
							s.page = "sessions"
							s.Select(r.Harness, r.SessionID)
							if s.Hooks.OpenMain != nil {
								s.Hooks.OpenMain()
							}
						}
					}
				})
			}
			ui.Row(c).Gap(8).Children(func() {
				b := trayButton(c, pal, icWindow, tr("openMain"), true).Grow(1)
				if b.Clicked() && s.Hooks.OpenMain != nil {
					s.Hooks.OpenMain()
				}
				q := trayButton(c, pal, icPower, "", false).Tooltip(tr("quit")).Label(tr("quit"))
				if q.Clicked() && s.Hooks.Quit != nil {
					s.Hooks.Quit()
				}
			})
		})
	})
}

// trayButton is the panel's footer button: the main one in ink, the
// other a quiet square.
func trayButton(c *ui.Context, pal palette, ic *ui.SVG, label string, primary bool) ui.Element {
	b := ui.Row(c.Key(label)).Padding(9, 12).Gap(7).Radius(11).Center().Cursor(ui.CursorPointer).Role(ui.RoleButton).Label(label).Transition(hoverFade)
	fg := pal.ink
	switch {
	case primary:
		fg = pal.base
		b.Background(pal.ink.Alpha(map[bool]float32{true: 0.84, false: 0.94}[b.Hovered()])).Shadow(0, 4, 12, -4, pal.lift)
	case b.Hovered():
		b.Background(pal.hover).Border(1, pal.edge)
	default:
		b.Background(pal.well).Border(1, pal.edge)
	}
	return b.Children(func() {
		ui.Icon(c, ic).FontSize(13).TextColor(fg)
		if label != "" {
			ui.Text(c, label).FontSize(12.5).FontWeight(650).TextColor(fg)
		}
	})
}
