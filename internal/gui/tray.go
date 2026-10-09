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
		pane(c, pal).Fill().Padding(16).Gap(12).Children(func() {
			ui.Row(c).AlignItems(ui.Center).Children(func() {
				logo(c, pal, 17)
				ui.Spacer(c)
				ui.Text(c, s.Hooks.Now().Format("01-02 Mon")).FontSize(11).TextColor(pal.muted)
			})
			// Today.
			ui.Column(c).Gap(4).Children(func() {
				ui.Text(c, tr("today")).FontSize(11.5).FontWeight(650).TextColor(pal.muted)
				ui.Row(c).AlignItems(ui.End).Gap(10).Children(func() {
					bigNumber(c, pal, fmtTokens(t.Totals.Tokens.Total()), 34)
					ui.Text(c, fmtCostOf(t.Totals.CostUSD, t.Totals.Requests, t.Totals.Unpriced)).FontSize(15).FontWeight(700).TextColor(Anon).FontFeatures("tnum").Padding(0, 0, 5, 0)
					ui.Spacer(c)
					ui.Column(c).AlignItems(ui.End).Gap(1).Padding(0, 0, 4, 0).Children(func() {
						ui.Text(c, trf("requestsN", fmtInt(t.Totals.Requests))).FontSize(11).TextColor(pal.muted)
						ui.Text(c, tr("cacheHit")+" "+fmtPct(t.Totals.CacheHit)).FontSize(11).TextColor(pal.muted)
					})
				})
				classBar(c, pal, t.Totals.Tokens, 7)
			})
			if t.Totals.Tokens.Total() == 0 {
				ui.Column(c).Grow(1).Center().Children(func() {
					emptyState(c, pal, artStage, tr("nothingToday"), tr("nothingTodaySub"))
				})
			} else {
				// Last 24 hours.
				ui.Column(c).Gap(6).Children(func() {
					ui.Text(c, tr("last24h")).FontSize(11.5).FontWeight(650).TextColor(pal.muted)
					miniBars(c, pal, t.Hourly, 74)
				})
				// Top models today.
				ui.Column(c).Gap(6).Children(func() {
					ui.Text(c, tr("topModels")).FontSize(11.5).FontWeight(650).TextColor(pal.muted)
					s.bucketList(c, pal, t.Models, 3, true, false)
				})
				// Active sessions.
				if len(t.Active) > 0 {
					ui.Column(c).Gap(2).Grow(1).Basis(0).Clip().Children(func() {
						ui.Text(c, tr("activeNow")).FontSize(11.5).FontWeight(650).TextColor(pal.muted)
						for i, r := range t.Active {
							if i >= 2 {
								break
							}
							s.sessionRow(c.Key(string(r.Harness)+r.SessionID), pal, r, false, func() {
								s.page = "sessions"
								s.Select(r.Harness, r.SessionID)
								if s.Hooks.OpenMain != nil {
									s.Hooks.OpenMain()
								}
							})
						}
					})
				} else {
					ui.Spacer(c)
				}
			}
			ui.Row(c).Gap(8).Children(func() {
				b := trayButton(c, pal, icWindow, tr("openMain"), Taki).Grow(1)
				if b.Clicked() && s.Hooks.OpenMain != nil {
					s.Hooks.OpenMain()
				}
				q := trayButton(c, pal, icPower, tr("quit"), pal.muted)
				if q.Clicked() && s.Hooks.Quit != nil {
					s.Hooks.Quit()
				}
			})
		})
	})
}

func trayButton(c *ui.Context, pal palette, ic *ui.SVG, label string, col ui.Color) ui.Element {
	b := ui.Row(c.Key(label)).Padding(9, 12).Gap(6).Radius(10).Center().Cursor(ui.CursorPointer).Role(ui.RoleButton).Label(label).Transition(hoverFade)
	if b.Hovered() {
		b.Background(col.Alpha(0.22))
	} else {
		b.Background(col.Alpha(0.12))
	}
	return b.Children(func() {
		ui.Icon(c, ic).FontSize(13).TextColor(col)
		ui.Text(c, label).FontSize(12.5).FontWeight(650).TextColor(pal.ink)
	})
}
