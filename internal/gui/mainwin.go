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
		ui.Row(c).Fill().Padding(10).Gap(14).Children(func() {
			s.sidebar(c, pal)
			// Keyed by page, so that every page rises in afresh.
			ui.Column(c.Key("page-" + s.page)).Grow(1).Basis(0).Fill().
				Transition(ui.ElementTransition{Duration: 320 * time.Millisecond, Enter: &ui.Motion{Y: 10}}).
				Children(func() {
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

// navItems are the sidebar's pages, each with its member.
var navItems = []struct {
	id  string
	ic  *ui.SVG
	col ui.Color
}{{"overview", icOverview, Tomori}, {"sessions", icSessions, Anon}, {"ranking", icRanking, Rana}, {"projects", icProjects, Soyo}, {"settings", icSettings, Taki}}

func (s *State) sidebar(c *ui.Context, pal palette) {
	bar := c.TitleBar()
	top := float32(20)
	if bar.Height > 0 {
		top = bar.Height + 10
	}
	pane(c, pal).Width(214).FillHeight().Shrink(0).Padding(top, 12, 14, 12).Gap(6).Children(func() {
		ui.Column(c).Padding(2, 8, 20, 8).Gap(5).DragWindow().Children(func() {
			logo(c, pal, 22)
			ui.Text(c, "token × harness × model").FontSize(10.5).TextColor(pal.muted).LetterSpacing(0.5)
		})
		nav := ui.Column(c).Gap(2)
		nav.Children(func() {
			// One raised pill glides behind the current page.
			var rows []ui.Element
			cur := -1
			for i, it := range navItems {
				on := s.page == it.id
				if on {
					cur = i
				}
				row := ui.Row(c.Key(it.id)).Height(38).Padding(0, 12).Gap(11).Radius(12).AlignItems(ui.Center).
					Cursor(ui.CursorPointer).Role(ui.RoleTab).Label(tr(it.id)).Transition(hoverFade)
				if !on && row.Hovered() {
					row.Background(pal.hover)
				}
				row.Children(func() {
					col := pal.muted
					if on {
						col = it.col
					}
					ui.Icon(c, it.ic).FontSize(16).TextColor(col)
					w, ink := 550, pal.ink.Alpha(0.78)
					if on {
						w, ink = 680, pal.ink
					}
					ui.Text(c, tr(it.id)).FontSize(13.5).FontWeight(w).TextColor(ink)
					ui.Spacer(c)
					if it.id == "sessions" && s.sessions.Total > 0 {
						ui.Text(c, fmtInt(int64(s.sessions.Total))).FontSize(11).TextColor(pal.muted).FontFeatures("tnum")
					}
					if on {
						ui.Box(c).Size(10, 12).Shrink(0).Draw(func(p *ui.Painter, r ui.Rect) {
							pick(p, r.X+r.W/2, r.Y+r.H/2, 11, -0.35, it.col)
						})
					}
				})
				if row.Clicked() {
					s.page = it.id
				}
				rows = append(rows, row)
			}
			glider(nav, rows, cur, 12, pal.raised, pal.edge, pal.lift)
		})
		ui.Spacer(c)
		s.todayMini(c, pal)
		s.scanStatus(c, pal)
	})
}

// todayMini is the sidebar's little "today" card.
func (s *State) todayMini(c *ui.Context, pal palette) {
	t := s.today.Totals
	ui.Column(c).Padding(14, 14, 12, 14).Gap(4).Radius(14).Background(pal.well).Border(1, pal.faint).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Gap(6).Children(func() {
			kicker(c, pal, tr("today"))
			ui.Spacer(c)
			ui.Text(c, fmtCostOf(t.CostUSD, t.Requests, t.Unpriced)).FontSize(11.5).FontWeight(650).TextColor(Anon.Mix(pal.ink, 0.15)).FontFeatures("tnum")
		})
		bigNumber(c, pal, fmtTokens(t.Tokens.Total()), 32)
		classBar(c, pal, t.Tokens, 4).Margin(4, 0, 0, 0)
	})
}

// scanStatus says whether the index is current; while a scan runs the five
// marks bounce like a level meter.
func (s *State) scanStatus(c *ui.Context, pal palette) {
	done, total := 0, 0
	if s.Hooks.Progress != nil {
		done, total = s.Hooks.Progress()
	}
	live := total > 0 && done < total
	ui.Row(c).Padding(10, 8, 0, 8).Gap(9).AlignItems(ui.Center).Children(func() {
		bangs(c, 11, live)
		if live {
			ui.Textf(c, "%s %d/%d", tr("scanning"), done, total).FontSize(11).TextColor(pal.muted).FontFeatures("tnum")
			c.After(500 * time.Millisecond)
		} else {
			ui.Text(c, tr("upToDate")).FontSize(11).TextColor(pal.muted)
		}
	})
}

// pageTitles are the pages' display titles, set in the serif in either
// language, like a magazine's section heads.
var pageTitles = map[string]string{"overview": "Overview", "sessions": "Sessions", "ranking": "Rankings", "projects": "Projects", "settings": "Settings"}

// pageHeader is a page's title with a kicker above it and optional
// controls on the right.
func pageHeader(c *ui.Context, pal palette, page, sub string, right func()) {
	ui.Row(c).Padding(12, 6, 6, 6).AlignItems(ui.End).Gap(12).DragWindow().Children(func() {
		ui.Column(c).Gap(0).Children(func() {
			k := tr(page)
			if Lang() != "zh" { // the serif title already says it
				k = ""
			}
			if sub != "" {
				if k != "" {
					k += "  ·  "
				}
				k += sub
			}
			kicker(c, pal, k)
			ui.Text(c, pageTitles[page]).Font(serif).Italic().FontSize(46).TextColor(pal.ink).LetterSpacing(-0.8).SingleLine()
		})
		ui.Spacer(c)
		if right != nil {
			ui.Box(c).Padding(0, 0, 8, 0).Children(right)
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

// rise wraps the i-th block of a page so that blocks drift up in turn.
func rise(c *ui.Context, i int, body func()) ui.Element {
	return ui.Column(c.Key(fmt.Sprint("rise", i))).Gap(14).
		Transition(ui.ElementTransition{Duration: time.Duration(380+i*120) * time.Millisecond, Enter: &ui.Motion{Y: 18}}).
		Children(body)
}

func (s *State) overviewPage(c *ui.Context, pal palette) {
	ov := s.ov
	now := s.Hooks.Now()
	sub := now.Format("Monday, January 2")
	if Lang() == "zh" {
		sub = now.Format("2006 年 1 月 2 日 ") + []string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}[now.Weekday()]
	}
	pageHeader(c, pal, "overview", sub, func() { s.spanSwitch(c, pal) })
	ui.Scroll(c).Grow(1).Children(func() {
		ui.Column(c).Padding(8, 6, 18, 6).Gap(14).Children(func() {
			s.unpricedBanner(c, pal)
			rise(c, 0, func() { s.hero(c, pal, ov) })
			rise(c, 1, func() {
				ui.Row(c).Gap(14).AlignItems(ui.Stretch).Children(func() {
					card(c, pal, tr("trend"), func() { classLegend(c, pal) }, func() {
						stackedBars(c.Key(fmt.Sprint("bars", ov.Span)), pal, ov.Daily, false, 230)
					}).Grow(1).Basis(0)
					card(c, pal, tr("composition"), nil, func() {
						ui.Column(c).Gap(16).AlignItems(ui.Center).Children(func() {
							donut(c.Key(fmt.Sprint("donut", ov.Span)), pal, ov.Totals.Tokens, 150)
							ui.Column(c).FillWidth().Gap(7).Children(func() {
								total := float64(ov.Totals.Tokens.Total())
								for _, k := range classes {
									v := k.get(ov.Totals.Tokens)
									ui.Row(c.Key(k.key)).Gap(8).AlignItems(ui.Center).Children(func() {
										dot(c, k.color, 7)
										ui.Text(c, tr(k.key)).FontSize(12).TextColor(pal.ink.Alpha(0.85))
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
			})
			rise(c, 2, func() {
				ui.Row(c).Gap(14).AlignItems(ui.Stretch).Children(func() {
					card(c, pal, tr("activity"), func() { heatLegend(c, pal) }, func() {
						heatmap(c, pal, ov.Heat, 132)
						ui.Row(c).Gap(28).Children(func() {
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
						s.bucketList(c, pal, topN(ov.Models, 6, "…"), 6, true, false)
					}).Width(330).Shrink(0)
				})
			})
			rise(c, 3, func() {
				ui.Row(c).Gap(14).AlignItems(ui.Stretch).Children(func() {
					card(c, pal, tr("byHarness"), nil, func() {
						s.bucketList(c, pal, ov.Harnesses, 6, false, true)
					}).Grow(1).Basis(0)
					card(c, pal, tr("byProvider"), nil, func() {
						s.bucketList(c, pal, topN(ov.Providers, 5, "…"), 6, false, false)
					}).Grow(1).Basis(0)
				})
			})
			rise(c, 4, func() { s.recentCard(c, pal, ov.Active) })
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
	ui.Column(c).Gap(2).Children(func() {
		ui.Text(c, label).FontSize(10.5).TextColor(pal.muted)
		ui.Text(c, value).FontSize(13).FontWeight(680).TextColor(pal.ink).FontFeatures("tnum")
	})
}

func classLegend(c *ui.Context, pal palette) {
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		for _, k := range classes {
			ui.Row(c.Key(k.key)).Gap(5).AlignItems(ui.Center).Children(func() {
				dot(c, k.color, 6)
				ui.Text(c, tr(k.key)).FontSize(11).TextColor(pal.muted)
			})
		}
	})
}

// hero is the overview's headline: the span's tokens counting up in large
// serif, three more figures beside them, and the days drawn as one melody
// along the bottom.
func (s *State) hero(c *ui.Context, pal palette, ov Overview) {
	t, pv := ov.Totals, ov.Prev
	pane(c, pal).Clip().Children(func() {
		ui.Row(c).Padding(22, 28, 4, 28).Gap(0).AlignItems(ui.Stretch).Children(func() {
			ui.Column(c).Grow(1).Basis(0).Gap(2).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					dot(c, Tomori, 7)
					kicker(c, pal, tr("tokens")+"  ·  "+tr(spanKeys[clampSpan(s.span)]))
				})
				countUp(c, pal, float64(t.Tokens.Total()), func(v float64) string { return fmtTokens(int64(v)) }, 84)
				ui.Row(c).Gap(12).AlignItems(ui.Center).Height(18).Children(func() {
					delta(c, pal, float64(t.Tokens.Total()), float64(pv.Tokens.Total()))
					ui.Text(c, fmtInt(t.Tokens.Total())+" tokens").FontSize(11).TextColor(pal.muted).FontFeatures("tnum")
				})
			})
			metric := func(key string, col ui.Color, value string, cur, prev float64, foot string) {
				ui.Box(c.Key("rule"+key)).Width(1).Margin(6, 0, 10, 0).Background(pal.faint)
				ui.Column(c.Key(key)).Width(168).Padding(4, 0, 0, 24).Gap(6).Children(func() {
					ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
						dot(c, col, 6)
						kicker(c, pal, tr(key))
					})
					bigNumber(c, pal, value, 40)
					ui.Row(c).Height(16).AlignItems(ui.Center).Children(func() {
						if prev > 0 {
							delta(c, pal, cur, prev)
						} else if foot != "" {
							ui.Text(c, foot).FontSize(11).TextColor(pal.muted).FontFeatures("tnum").SingleLine()
						}
					})
				})
			}
			metric("cost", Anon, fmtCostOf(t.CostUSD, t.Requests, t.Unpriced), t.CostUSD, pv.CostUSD, "")
			metric("requests", Rana, fmtTokens(t.Requests), float64(t.Requests), float64(pv.Requests), trf("sessionsN", t.Sessions))
			metric("cacheHit", Taki, fmtPct(t.CacheHit), t.CacheHit, pv.CacheHit, tr("cacheRead")+" "+fmtTokens(t.Tokens.CacheRead))
		})
		heroWave(c.Key(fmt.Sprint("wave", ov.Span)), pal, ov.Daily, 92)
	})
}

func clampSpan(i int) int {
	if i < 0 || i >= len(spanKeys) {
		return 1
	}
	return i
}

// bucketList shows ranked buckets with a share bar each.
func (s *State) bucketList(c *ui.Context, pal palette, bs []query.Bucket, max int, showCost, harnesses bool) {
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
			if harnesses {
				col = harnessColor(model.Harness(b.Key))
			}
			ui.Column(c.Key(b.Key)).Gap(5).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					if harnesses {
						harnessMark(c, model.Harness(b.Key), 15)
					} else {
						dot(c, col, 8)
					}
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
				rankBar(c, pal, float64(b.Tokens.Total())/float64(top), col, 4)
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
