package gui

import (
	"os"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar/mytoken/internal/harness"
	"github.com/zzstar/mytoken/internal/query"
)

// rankingPage ranks providers, models and harnesses over the chosen span.
func (s *State) rankingPage(c *ui.Context, pal palette) {
	ov := s.ov
	pageHeader(c, pal, tr("ranking"), "", func() { s.spanSwitch(c, pal) })
	ui.Scroll(c).Grow(1).Children(func() {
		ui.Column(c).Padding(8, 8, 16, 8).Gap(12).Children(func() {
			ui.Row(c).Children(func() {
				segmented(c, pal, &s.rankTab, tr("byModel"), tr("byProvider"), tr("byHarness"))
			})
			var bs []query.Bucket
			switch s.rankTab {
			case 1:
				bs = ov.Providers
			case 2:
				bs = ov.Harnesses
			default:
				bs = ov.Models
			}
			if len(bs) == 0 {
				pane(c, pal).Padding(40).Children(func() { emptyState(c, pal, artStaff, tr("noData"), tr("noDataSub")) })
				return
			}
			// Podium for the top three, then a table.
			ui.Row(c).Gap(12).AlignItems(ui.End).Children(func() {
				order := []int{1, 0, 2}
				for _, i := range order {
					if i >= len(bs) {
						continue
					}
					b := bs[i]
					col := bandAt(i)
					h := []float32{150, 124, 108}[i]
					pane(c.Key(b.Key), pal).Grow(1).Basis(0).Height(h + 60).Padding(16).Gap(6).Children(func() {
						ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
							ui.Box(c).Size(26, 26).Radius(13).Center().Background(col).Children(func() {
								ui.Textf(c, "%d", i+1).FontSize(13).FontWeight(800).TextColor(ui.RGB(255, 255, 255))
							})
							if i == 0 {
								ui.Icon(c, icSparkle).FontSize(14).TextColor(Soyo)
							}
						})
						label := b.Label
						if label == "" {
							label = b.Key
						}
						ui.Text(c, label).FontSize(14).FontWeight(700).TextColor(pal.ink).SingleLine().Ellipsis("…")
						ui.Spacer(c)
						bigNumber(c, pal, fmtTokens(b.Tokens.Total()), []float32{30, 24, 22}[i])
						ui.Row(c).Gap(10).Children(func() {
							ui.Text(c, fmtCostOf(b.CostUSD, b.Requests, b.Unpriced)).FontSize(11.5).TextColor(pal.muted).FontFeatures("tnum")
							ui.Text(c, trf("requestsN", fmtInt(b.Requests))).FontSize(11.5).TextColor(pal.muted)
						})
						classBar(c, pal, b.Tokens, 6)
					})
				}
			})
			card(c, pal, "", func() { classLegend(c, pal) }, func() {
				s.rankTable(c, pal, bs)
			})
		})
	})
}

func (s *State) rankTable(c *ui.Context, pal palette, bs []query.Bucket) {
	var all int64
	for _, b := range bs {
		all += b.Tokens.Total()
	}
	head := func(t string, w float32) {
		ui.Text(c, t).FontSize(11).FontWeight(650).TextColor(pal.muted).Width(w).TextAlign(ui.End)
	}
	ui.Row(c).Gap(12).Padding(0, 4, 6, 4).Children(func() {
		ui.Text(c, "#").FontSize(11).TextColor(pal.muted).Width(22)
		ui.Text(c, "").Grow(1)
		head(tr("share"), 54)
		head(tr("requests"), 70)
		head(tr("cost"), 74)
		head(tr("tokens"), 80)
	})
	ui.Divider(c)
	for i, b := range bs {
		row := ui.Row(c.Key(b.Key)).Gap(12).Padding(8, 4).Radius(8).AlignItems(ui.Center).Transition(hoverFade)
		if row.Hovered() {
			row.Background(pal.hover)
		}
		row.Children(func() {
			ui.Textf(c, "%d", i+1).FontSize(12).TextColor(pal.muted).Width(22).FontFeatures("tnum")
			ui.Column(c).Grow(1).Basis(0).Gap(5).Children(func() {
				label := b.Label
				if label == "" {
					label = b.Key
				}
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					dot(c, bandAt(i), 8)
					ui.Text(c, label).FontSize(13).FontWeight(600).TextColor(pal.ink).SingleLine().Ellipsis("…")
				})
				classBar(c, pal, b.Tokens, 4)
			})
			share := 0.0
			if all > 0 {
				share = float64(b.Tokens.Total()) / float64(all)
			}
			ui.Text(c, fmtPct(share)).FontSize(12).TextColor(pal.muted).FontFeatures("tnum").Width(54).TextAlign(ui.End)
			ui.Text(c, fmtInt(b.Requests)).FontSize(12).TextColor(pal.muted).FontFeatures("tnum").Width(70).TextAlign(ui.End)
			ui.Text(c, fmtCostOf(b.CostUSD, b.Requests, b.Unpriced)).FontSize(12).TextColor(pal.ink).FontFeatures("tnum").Width(74).TextAlign(ui.End)
			ui.Text(c, fmtTokens(b.Tokens.Total())).FontSize(13).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum").Width(80).TextAlign(ui.End)
		})
	}
}

// projectsPage shows usage per working directory.
func (s *State) projectsPage(c *ui.Context, pal palette) {
	ov := s.ov
	pageHeader(c, pal, tr("projects"), "", func() { s.spanSwitch(c, pal) })
	ui.Scroll(c).Grow(1).Children(func() {
		ui.Column(c).Padding(8, 8, 16, 8).Gap(12).Children(func() {
			if len(ov.Projects) == 0 {
				pane(c, pal).Padding(40).Children(func() { emptyState(c, pal, artStaff, tr("noData"), tr("noDataSub")) })
				return
			}
			var top int64 = 1
			for _, b := range ov.Projects {
				if v := b.Tokens.Total(); v > top {
					top = v
				}
			}
			ui.Grid(c).Columns(2).Gap(12).Children(func() {
				for i, b := range ov.Projects {
					col := bandAt(i)
					pane(c.Key(b.Key), pal).Padding(16).Gap(8).Children(func() {
						ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
							ui.Box(c).Size(32, 32).Radius(10).Center().Shrink(0).Background(col.Alpha(0.16)).Children(func() {
								ui.Icon(c, icFolder).FontSize(16).TextColor(col)
							})
							ui.Column(c).Grow(1).Basis(0).Gap(2).Children(func() {
								name := baseName(b.Key)
								if name == "" {
									name = tr("untitled")
								}
								ui.Text(c, name).FontSize(14).FontWeight(700).TextColor(pal.ink).SingleLine().Ellipsis("…")
								ui.Text(c, shortPath(b.Key)).FontSize(11).TextColor(pal.muted).SingleLine().Ellipsis("…")
							})
							bigNumber(c, pal, fmtTokens(b.Tokens.Total()), 20)
						})
						rankBar(c, pal, float64(b.Tokens.Total())/float64(top), col, 6)
						ui.Row(c).Gap(14).Children(func() {
							miniStat(c, pal, tr("cost"), fmtCostOf(b.CostUSD, b.Requests, b.Unpriced))
							miniStat(c, pal, tr("sessions"), fmtInt(b.Sessions))
							miniStat(c, pal, tr("requests"), fmtInt(b.Requests))
						})
					})
				}
			})
		})
	})
}

// settingsPage holds the few preferences there are.
func (s *State) settingsPage(c *ui.Context, pal palette) {
	pageHeader(c, pal, tr("settings"), "", nil)
	ui.Scroll(c).Grow(1).Children(func() {
		ui.Column(c).Padding(8, 8, 16, 8).Gap(12).MaxWidth(720).Children(func() {
			card(c, pal, "", nil, func() {
				settingRow(c, pal, tr("launchAtLogin"), tr("launchAtLoginSub"), func() {
					on := s.Hooks.OpenAtLogin != nil && s.Hooks.OpenAtLogin()
					if ui.Switch(c, &on).Changed() && s.Hooks.SetOpenAtLogin != nil {
						s.Hooks.SetOpenAtLogin(on)
					}
				})
				ui.Divider(c)
				settingRow(c, pal, tr("rebuild"), tr("rebuildSub"), func() {
					if ui.Button(c, tr("rebuild")).Clicked() && s.Hooks.Rebuild != nil {
						go s.Hooks.Rebuild()
					}
				})
				if s.Hooks.DataDir != "" {
					ui.Divider(c)
					settingRow(c, pal, tr("dataDir"), s.Hooks.DataDir, nil)
				}
			})
			s.pricingCards(c, pal)
			card(c, pal, tr("sources"), nil, func() {
				ui.Text(c, tr("sourcesSub")).FontSize(12).TextColor(pal.muted)
				for _, p := range harness.All() {
					h := p.Harness()
					ui.Row(c.Key(string(h))).Gap(10).Padding(6, 0).AlignItems(ui.Center).Children(func() {
						dot(c, harnessColor(h), 9)
						ui.Text(c, h.DisplayName()).FontSize(13).FontWeight(650).TextColor(pal.ink).Width(130)
						ui.Column(c).Grow(1).Basis(0).Gap(2).Children(func() {
							for _, r := range p.Roots() {
								ok := exists(r)
								col := pal.muted
								if !ok {
									col = pal.muted.Alpha(0.5)
								}
								ui.Row(c.Key(r)).Gap(6).AlignItems(ui.Center).Children(func() {
									if ok {
										dot(c, Rana, 6)
									} else {
										dot(c, pal.faint, 6)
									}
									ui.Text(c, shortPath(r)).FontSize(11.5).TextColor(col).SingleLine().Ellipsis("…")
								})
							}
						})
					})
				}
			})
			card(c, pal, tr("about"), nil, func() {
				ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
					logo(c, pal, 20)
					ui.Text(c, "v0.1 · MIT").FontSize(12).TextColor(pal.muted)
				})
				ui.Text(c, "Built with MyGo · 春日影は、もう演奏しない").FontSize(11.5).TextColor(pal.muted)
			})
		})
	})
}

func settingRow(c *ui.Context, pal palette, title, sub string, control func()) {
	ui.Row(c.Key(title)).Gap(12).Padding(6, 0).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Grow(1).Basis(0).Gap(2).Children(func() {
			ui.Text(c, title).FontSize(13.5).FontWeight(650).TextColor(pal.ink)
			ui.Text(c, sub).FontSize(11.5).TextColor(pal.muted)
		})
		if control != nil {
			control()
		}
	})
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
