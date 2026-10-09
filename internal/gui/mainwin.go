package gui

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar/mytoken/internal/model"
	"github.com/zzstar/mytoken/internal/query"
)

// MainView is the main window's content.
func (s *State) MainView(c *ui.Context) {
	pal := applyTheme(c)
	c.Root().Background(ui.Transparent)
	ui.Box(c).Fill().Draw(aurora(pal)).Children(func() {
		ui.Row(c).Fill().Padding(10).Gap(10).Children(func() {
			s.sidebar(c, pal)
			ui.Column(c).Grow(1).Basis(0).Fill().Children(func() {
				switch s.page {
				case "sessions":
					s.sessionsPage(c, pal)
				case "ranking":
					s.rankingPage(c, pal)
				case "projects":
					s.projectsPage(c, pal)
				case "settings":
					s.settingsPage(c, pal)
				default:
					s.overviewPage(c, pal)
				}
			})
		})
		// The window drags by its top edge, as a hidden title bar's would.
		ui.Box(c).Absolute().Top(0).Left(240).Right(0).Height(30).DragWindow()
	})
}

func (s *State) sidebar(c *ui.Context, pal palette) {
	bar := c.TitleBar()
	top := float32(18)
	if bar.Height > 0 {
		top = bar.Height + 8
	}
	pane(c, pal).Width(220).FillHeight().Shrink(0).Padding(top, 12, 14, 12).Gap(6).Children(func() {
		ui.Column(c).Padding(4, 8, 14, 8).Gap(2).DragWindow().Children(func() {
			logo(c, pal, 22)
			ui.Text(c, "token × harness × model").FontSize(11).TextColor(pal.muted).LetterSpacing(0.3)
		})
		items := []struct {
			id  string
			ic  *ui.SVG
			col ui.Color
		}{{"overview", icOverview, Tomori}, {"sessions", icSessions, Anon}, {"ranking", icRanking, Rana}, {"projects", icProjects, Soyo}, {"settings", icSettings, Taki}}
		for _, it := range items {
			on := s.page == it.id
			row := ui.Row(c.Key(it.id)).Padding(8, 10).Gap(10).Radius(10).AlignItems(ui.Center).Cursor(ui.CursorPointer).Role(ui.RoleTab).Label(tr(it.id)).Transition(hoverFade)
			if on {
				row.Background(it.col.Alpha(0.16))
			} else if row.Hovered() {
				row.Background(pal.hover)
			}
			row.Children(func() {
				ui.Box(c).Size(26, 26).Radius(8).Center().Background(it.col.Alpha(map[bool]float32{true: 1, false: 0.14}[on])).Children(func() {
					col := it.col
					if on {
						col = ui.RGB(255, 255, 255)
					}
					ui.Icon(c, it.ic).FontSize(15).TextColor(col)
				})
				w := 550
				if on {
					w = 700
				}
				ui.Text(c, tr(it.id)).FontSize(13.5).FontWeight(w).TextColor(pal.ink)
				ui.Spacer(c)
				if it.id == "sessions" && s.sessions.Total > 0 {
					ui.Text(c, fmtInt(int64(s.sessions.Total))).FontSize(11).TextColor(pal.muted).FontFeatures("tnum")
				}
			})
			if row.Clicked() {
				s.page = it.id
			}
		}
		ui.Spacer(c)
		s.todayMini(c, pal)
		s.scanStatus(c, pal)
	})
}

// todayMini is the sidebar's little "today" card.
func (s *State) todayMini(c *ui.Context, pal palette) {
	t := s.today.Totals
	ui.Column(c).Padding(12).Gap(6).Radius(12).Background(pal.well).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Gap(6).Children(func() {
			ui.Icon(c, icSparkle).FontSize(12).TextColor(Soyo)
			ui.Text(c, tr("today")).FontSize(11.5).FontWeight(650).TextColor(pal.muted)
			ui.Spacer(c)
			ui.Text(c, fmtCostOf(t.CostUSD, t.Requests, t.Unpriced)).FontSize(11.5).FontWeight(650).TextColor(pal.muted).FontFeatures("tnum")
		})
		bigNumber(c, pal, fmtTokens(t.Tokens.Total()), 20)
		classBar(c, pal, t.Tokens, 6)
	})
}

func (s *State) scanStatus(c *ui.Context, pal palette) {
	done, total := 0, 0
	if s.Hooks.Progress != nil {
		done, total = s.Hooks.Progress()
	}
	ui.Row(c).Padding(8, 6, 0, 6).Gap(8).AlignItems(ui.Center).Children(func() {
		if total > 0 && done < total {
			ui.Spinner(c).Size(12, 12)
			ui.Textf(c, "%s %d/%d", tr("scanning"), done, total).FontSize(11).TextColor(pal.muted).FontFeatures("tnum")
			c.After(500 * time.Millisecond)
		} else {
			dot(c, Rana, 7)
			ui.Text(c, tr("upToDate")).FontSize(11).TextColor(pal.muted)
		}
	})
}

// pageHeader is a page's title row with optional controls on the right.
func pageHeader(c *ui.Context, pal palette, title, sub string, right func()) {
	ui.Row(c).Padding(14, 8, 4, 8).AlignItems(ui.End).Gap(12).DragWindow().Children(func() {
		ui.Column(c).Gap(2).Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, title).FontSize(26).FontWeight(800).TextColor(pal.ink).LetterSpacing(-0.4)
				flourish(c)
			})
			if sub != "" {
				ui.Text(c, sub).FontSize(12).TextColor(pal.muted)
			}
		})
		ui.Spacer(c)
		if right != nil {
			right()
		}
	})
}

func (s *State) spanSwitch(c *ui.Context, pal palette) {
	labels := make([]string, len(spanKeys))
	for i, k := range spanKeys {
		labels[i] = tr(k)
	}
	if segmented(c, pal, &s.span, labels...) {
		s.loadOverview()
	}
}

func (s *State) overviewPage(c *ui.Context, pal palette) {
	ov := s.ov
	now := s.Hooks.Now()
	sub := now.Format("2006-01-02 Monday")
	if Lang() == "zh" {
		sub = now.Format("2006 年 1 月 2 日 ") + []string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}[now.Weekday()]
	}
	pageHeader(c, pal, tr("overview"), sub, func() { s.spanSwitch(c, pal) })
	ui.Scroll(c).Grow(1).Children(func() {
		ui.Column(c).Padding(8, 8, 16, 8).Gap(12).Children(func() {
			s.unpricedBanner(c, pal)
			s.kpis(c, pal, ov)
			ui.Row(c).Gap(12).AlignItems(ui.Stretch).Children(func() {
				card(c, pal, tr("trend"), func() { classLegend(c, pal) }, func() {
					stackedBars(c.Key(fmt.Sprint("bars", ov.Span)), pal, ov.Daily, false, 230)
				}).Grow(1).Basis(0)
				card(c, pal, tr("composition"), nil, func() {
					ui.Column(c).Gap(14).AlignItems(ui.Center).Children(func() {
						donut(c.Key(fmt.Sprint("donut", ov.Span)), pal, ov.Totals.Tokens, 150)
						ui.Column(c).FillWidth().Gap(6).Children(func() {
							total := float64(ov.Totals.Tokens.Total())
							for _, k := range classes {
								v := k.get(ov.Totals.Tokens)
								ui.Row(c.Key(k.key)).Gap(8).AlignItems(ui.Center).Children(func() {
									dot(c, k.color, 8)
									ui.Text(c, tr(k.key)).FontSize(12).TextColor(pal.ink)
									ui.Spacer(c)
									ui.Text(c, fmtTokens(v)).FontSize(12).FontWeight(650).TextColor(pal.ink).FontFeatures("tnum")
									share := 0.0
									if total > 0 {
										share = float64(v) / total
									}
									ui.Text(c, fmtPct(share)).FontSize(11).TextColor(pal.muted).FontFeatures("tnum").Width(44).TextAlign(ui.End)
								})
							}
						})
					})
				}).Width(290).Shrink(0)
			})
			ui.Row(c).Gap(12).AlignItems(ui.Stretch).Children(func() {
				card(c, pal, tr("activity"), func() { heatLegend(c, pal) }, func() {
					heatmap(c, pal, ov.Heat, 132)
					ui.Row(c).Gap(18).Children(func() {
						if b, ok := busiest(ov.Heat); ok {
							miniStat(c, pal, tr("busiestDay"), b.Day.Format("01-02")+" · "+fmtTokens(b.Tokens.Total()))
						}
						miniStat(c, pal, tr("streak"), trf("days", streak(ov.Heat)))
						if days := activeDays(ov.Heat); days > 0 {
							miniStat(c, pal, tr("perDay"), fmtTokens(sumTokens(ov.Heat)/int64(days)))
						}
					})
				}).Grow(1).Basis(0)
				card(c, pal, tr("topModels"), nil, func() {
					s.bucketList(c, pal, topN(ov.Models, 6, "…"), 6, true)
				}).Width(330).Shrink(0)
			})
			ui.Row(c).Gap(12).AlignItems(ui.Stretch).Children(func() {
				card(c, pal, tr("byHarness"), nil, func() {
					s.bucketList(c, pal, ov.Harnesses, 6, false)
				}).Grow(1).Basis(0)
				card(c, pal, tr("byProvider"), nil, func() {
					s.bucketList(c, pal, topN(ov.Providers, 5, "…"), 6, false)
				}).Grow(1).Basis(0)
			})
			s.recentCard(c, pal, ov.Active)
		})
	})
}

func activeDays(days []query.Point) int {
	n := 0
	for _, d := range days {
		if d.Tokens.Total() > 0 {
			n++
		}
	}
	return n
}

func sumTokens(days []query.Point) int64 {
	var n int64
	for _, d := range days {
		n += d.Tokens.Total()
	}
	return n
}

func miniStat(c *ui.Context, pal palette, label, value string) {
	ui.Column(c).Gap(1).Children(func() {
		ui.Text(c, label).FontSize(10.5).TextColor(pal.muted)
		ui.Text(c, value).FontSize(13).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum")
	})
}

func classLegend(c *ui.Context, pal palette) {
	ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
		for _, k := range classes {
			ui.Row(c.Key(k.key)).Gap(4).AlignItems(ui.Center).Children(func() {
				dot(c, k.color, 7)
				ui.Text(c, tr(k.key)).FontSize(11).TextColor(pal.muted)
			})
		}
	})
}

// kpis is the row of four headline tiles.
func (s *State) kpis(c *ui.Context, pal palette, ov Overview) {
	t, pv := ov.Totals, ov.Prev
	series := func(f func(query.Point) float64) []float64 {
		v := make([]float64, len(ov.Daily))
		for i, p := range ov.Daily {
			v[i] = f(p)
		}
		return v
	}
	hits := series(func(p query.Point) float64 {
		den := p.Tokens.Input + p.Tokens.CacheRead + p.Tokens.CacheWrite
		if den == 0 {
			return 0
		}
		return float64(p.Tokens.CacheRead) / float64(den)
	})
	tiles := []struct {
		key       string
		col       ui.Color
		value     string
		cur, prev float64
		spark     []float64
		foot      string
	}{
		{"tokens", Tomori, fmtTokens(t.Tokens.Total()), float64(t.Tokens.Total()), float64(pv.Tokens.Total()), series(func(p query.Point) float64 { return float64(p.Tokens.Total()) }), fmtInt(t.Tokens.Total())},
		{"cost", Anon, fmtCostOf(t.CostUSD, t.Requests, t.Unpriced), t.CostUSD, pv.CostUSD, series(func(p query.Point) float64 { return p.CostUSD }), ""},
		{"requests", Rana, fmtTokens(t.Requests), float64(t.Requests), float64(pv.Requests), nil, trf("sessionsN", t.Sessions)},
		{"cacheHit", Taki, fmtPct(t.CacheHit), t.CacheHit, pv.CacheHit, hits, tr("cacheRead") + " " + fmtTokens(t.Tokens.CacheRead)},
	}
	ui.Row(c).Gap(12).Children(func() {
		for _, k := range tiles {
			pane(c.Key(k.key), pal).Grow(1).Basis(0).Padding(14, 16).Gap(4).Children(func() {
				ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
					ui.Box(c).Size(8, 8).Radius(3).Background(k.col)
					ui.Text(c, tr(k.key)).FontSize(12).FontWeight(650).TextColor(pal.muted)
				})
				bigNumber(c, pal, k.value, 28)
				ui.Row(c).Height(16).AlignItems(ui.Center).Children(func() {
					if k.prev > 0 {
						delta(c, pal, k.cur, k.prev)
					} else if k.foot != "" {
						ui.Text(c, k.foot).FontSize(11).TextColor(pal.muted).FontFeatures("tnum").SingleLine()
					}
				})
				if k.spark != nil {
					sparkline(c, k.spark, k.col, 34)
				} else {
					ui.Box(c).Height(34).FillWidth().Children(func() {
						ui.Text(c, k.foot).FontSize(11).TextColor(pal.muted)
					})
				}
			})
		}
	})
}

// bucketList shows ranked buckets with a share bar each.
func (s *State) bucketList(c *ui.Context, pal palette, bs []query.Bucket, max int, showCost bool) {
	if len(bs) == 0 {
		ui.Text(c, tr("noData")).FontSize(12).TextColor(pal.muted)
		return
	}
	var top int64
	for _, b := range bs {
		if v := b.Tokens.Total(); v > top {
			top = v
		}
	}
	var all int64
	for _, b := range bs {
		all += b.Tokens.Total()
	}
	ui.Column(c).Gap(10).Children(func() {
		for i, b := range bs {
			if i >= max {
				break
			}
			col := bandAt(i)
			if h := model.Harness(b.Key); h.DisplayName() != b.Key {
				col = harnessColor(h)
			}
			ui.Column(c.Key(b.Key)).Gap(5).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					dot(c, col, 8)
					label := b.Label
					if label == "" {
						label = b.Key
					}
					ui.Text(c, label).FontSize(12.5).FontWeight(600).TextColor(pal.ink).SingleLine().Ellipsis("…").Shrink(1)
					ui.Spacer(c)
					if showCost {
						ui.Text(c, fmtCostOf(b.CostUSD, b.Requests, b.Unpriced)).FontSize(11).TextColor(pal.muted).FontFeatures("tnum")
					}
					ui.Text(c, fmtTokens(b.Tokens.Total())).FontSize(12.5).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum")
				})
				rankBar(c, pal, float64(b.Tokens.Total())/float64(top), col, 6)
			})
		}
	})
}

// recentCard lists the latest sessions; a click opens one.
func (s *State) recentCard(c *ui.Context, pal palette, rows []query.SessionRow) {
	card(c, pal, tr("recent"), nil, func() {
		if len(rows) == 0 {
			emptyState(c, pal, artStaff, tr("noSessions"), tr("noSessionsSub"))
			return
		}
		ui.Column(c).Gap(2).Children(func() {
			for _, r := range rows {
				s.sessionRow(c.Key(string(r.Harness)+r.SessionID), pal, r, false, func() {
					s.page = "sessions"
					s.Select(r.Harness, r.SessionID)
				})
			}
		})
	})
}

// unpricedBanner nudges towards mapping models the price list doesn't know.
func (s *State) unpricedBanner(c *ui.Context, pal palette) {
	n := len(s.pr.unpriced)
	if n == 0 || s.ov.Totals.Unpriced == 0 {
		return
	}
	ui.Row(c).Gap(10).Padding(9, 14).Radius(12).AlignItems(ui.Center).Background(Soyo.Alpha(0.14)).Children(func() {
		dot(c, Soyo, 8)
		ui.Text(c, trf("unpricedBanner", n)).FontSize(12.5).TextColor(pal.ink).Grow(1).Basis(0)
		ui.Text(c, tr("fixIt")).FontSize(12.5).FontWeight(650).TextColor(Soyo.Mix(pal.ink, 0.35)).Cursor(ui.CursorPointer).Role(ui.RoleButton).OnClick(func() { s.SetPage("settings") })
	})
}
