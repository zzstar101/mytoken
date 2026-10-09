package gui

import (
	"sort"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar/mytoken/internal/model"
	"github.com/zzstar/mytoken/internal/query"
)

func (s *State) sessionsPage(c *ui.Context, pal palette) {
	rows := filterRows(s.sessions.Rows, s.search)
	pageHeader(c, pal, "sessions", trf("sessionsN", s.sessions.Total), func() {
		if segmented(c, pal, &s.sortIdx, tr("recent"), tr("mostTokens"), tr("mostCost")) {
			s.loadSessions()
		}
	})
	ui.Row(c).Grow(1).Basis(0).Padding(8, 8, 8, 8).Gap(12).Children(func() {
		// The list.
		pane(c, pal).Width(380).FillHeight().Shrink(0).Padding(10).Gap(8).Children(func() {
			ui.SearchField(c, &s.search).FillWidth().Label(tr("search"))
			if len(rows) == 0 {
				ui.Column(c).Grow(1).Center().Children(func() {
					if s.search != "" {
						emptyState(c, pal, artPicks, tr("noResults"), tr("noResultsSub"))
					} else {
						emptyState(c, pal, artStaff, tr("noSessions"), tr("noSessionsSub"))
					}
				})
				return
			}
			s.list.Key = func(i int) any {
				if i < len(rows) {
					return string(rows[i].Harness) + "/" + rows[i].SessionID
				}
				return i
			}
			ui.List(c, &s.list, len(rows), func(i int) {
				r := rows[i]
				key := string(r.Harness) + "/" + r.SessionID
				s.sessionRow(c, pal, r, s.sel == key, func() { s.Select(r.Harness, r.SessionID) })
			}).Grow(1).Basis(0)
		})
		// The detail.
		ui.Column(c).Grow(1).Basis(0).FillHeight().Children(func() {
			if s.sel == "" {
				pane(c, pal).Fill().Center().Children(func() {
					emptyState(c, pal, artStage, tr("pickSession"), tr("pickSub"))
				})
				return
			}
			s.detailView(c, pal)
		})
	})
}

// sessionRow is one session in a list: harness mark, title, tokens, and a
// thin class bar.
func (s *State) sessionRow(c *ui.Context, pal palette, r query.SessionRow, selected bool, onClick func()) {
	col := harnessColor(r.Harness)
	row := ui.Row(c).Padding(9, 12, 9, 10).Gap(11).Radius(12).AlignItems(ui.Center).Cursor(ui.CursorPointer).Role(ui.RoleButton).Transition(hoverFade)
	switch {
	case selected:
		row.Background(pal.raised).Border(1, pal.edge).Shadow(0, 4, 14, -4, pal.lift)
		// A colored edge on the left, like a lit fader.
		row.Draw(func(p *ui.Painter, rr ui.Rect) {
			p.Fill(ui.Rect{X: rr.X + 1, Y: rr.Y + 12, W: 3, H: rr.H - 24}, col, 2)
		})
	case row.Hovered():
		row.Background(pal.hover)
	}
	row.Children(func() {
		harnessBadge(c, r.Harness, 32)
		ui.Column(c).Grow(1).Basis(0).Gap(4).Children(func() {
			title := r.Title
			if title == "" {
				title = tr("untitled")
			}
			ui.Text(c, title).FontSize(13).FontWeight(600).TextColor(pal.ink).SingleLine().Ellipsis("…")
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, r.Harness.DisplayName()).FontSize(11).TextColor(col).FontWeight(600).SingleLine().Shrink(0)
				if r.Project != "" {
					ui.Text(c, "· "+baseName(r.Project)).FontSize(11).TextColor(pal.muted).SingleLine().Ellipsis("…").Shrink(1)
				}
				if len(r.Breakdown) > 0 {
					ui.Text(c, "· "+r.Breakdown[0].Model).FontSize(11).TextColor(pal.muted).SingleLine().Ellipsis("…").Shrink(1)
				}
				if r.Children > 0 {
					ui.Row(c).Gap(2).AlignItems(ui.Center).Shrink(0).Children(func() {
						ui.Icon(c, icBranch).FontSize(11).TextColor(pal.muted)
						ui.Textf(c, "%d", r.Children).FontSize(11).TextColor(pal.muted)
					})
				}
			})
		})
		ui.Column(c).Gap(1).AlignItems(ui.End).Shrink(0).Children(func() {
			bigNumber(c, pal, fmtTokens(r.Tokens.Total()), 21)
			ui.Text(c, fmtAgo(s.Hooks.Now(), r.UpdatedAt)).FontSize(10.5).TextColor(pal.muted).FontFeatures("tnum")
		})
	})
	if row.Clicked() && onClick != nil {
		onClick()
	}
}

func (s *State) detailView(c *ui.Context, pal palette) {
	d := s.detail
	r := d.Row
	if r.SessionID == "" {
		pane(c, pal).Fill().Center().Children(func() { ui.Spinner(c) })
		return
	}
	col := harnessColor(r.Harness)
	ui.Scroll(c).Fill().Children(func() {
		ui.Column(c).Gap(12).Padding(0, 0, 12, 0).Children(func() {
			// Header: who, what, when, then the figures and the mix.
			pane(c, pal).Padding(20, 24, 22, 24).Gap(12).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					harnessBadge(c, r.Harness, 24)
					chip(c, r.Harness.DisplayName(), col)
					if r.Children > 0 {
						chip(c, trf("childrenN", r.Children), Taki)
					}
					if r.Tokens.Total() > 50_000_000 {
						chip(c, tr("heavy"), Anon)
					}
					ui.Spacer(c)
					ui.Text(c, r.SessionID).FontSize(10.5).TextColor(pal.muted.Alpha(0.8)).FontFeatures("tnum").SingleLine().Ellipsis("…").MaxWidth(220)
				})
				title := r.Title
				if title == "" {
					title = tr("untitled")
				}
				ui.Text(c, title).FontSize(22).FontWeight(720).TextColor(pal.ink).MaxLines(2)
				ui.Row(c).Gap(18).Children(func() {
					if r.Project != "" {
						metaItem(c, pal, icFolder, shortPath(r.Project))
					}
					metaItem(c, pal, icClock, fmtWhen(r.StartedAt)+" → "+fmtWhen(r.UpdatedAt))
				})
				ui.Box(c).Height(1).FillWidth().Margin(6, 0, 4, 0).Background(pal.faint)
				den := r.Tokens.Input + r.Tokens.CacheRead + r.Tokens.CacheWrite
				hit := 0.0
				if den > 0 {
					hit = float64(r.Tokens.CacheRead) / float64(den)
				}
				ui.Row(c).AlignItems(ui.Stretch).Children(func() {
					figures := []struct {
						key   string
						col   ui.Color
						value string
					}{
						{"tokens", Tomori, fmtTokens(r.Tokens.Total())},
						{"cost", Anon, fmtCostOf(r.CostUSD, r.Requests, r.Unpriced)},
						{"requests", Rana, fmtInt(r.Requests)},
						{"cacheHit", Taki, fmtPct(hit)},
					}
					for i, f := range figures {
						if i > 0 {
							ui.Box(c.Key("rule"+f.key)).Width(1).Margin(4, 18, 4, 0).Background(pal.faint)
						}
						ui.Column(c.Key(f.key)).Grow(1).Basis(0).Gap(4).Children(func() {
							ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
								dot(c, f.col, 6)
								kicker(c, pal, tr(f.key))
							})
							bigNumber(c, pal, f.value, 38)
						})
					}
				})
				ui.Column(c).Gap(10).Margin(8, 0, 0, 0).Children(func() {
					ui.Row(c).AlignItems(ui.Center).Children(func() {
						kicker(c, pal, tr("composition"))
						ui.Spacer(c)
						classLegend(c, pal)
					})
					classBar(c, pal, r.Tokens, 10)
					ui.Row(c).Gap(22).Children(func() {
						for _, k := range classes {
							if v := k.get(r.Tokens); v > 0 {
								miniStat(c.Key(k.key), pal, tr(k.key), fmtTokens(v))
							}
						}
					})
				})
			})
			// Provider × model.
			models := detailModels(d.Events, r.Breakdown)
			card(c, pal, tr("breakdown"), nil, func() {
				s.breakdownTable(c, pal, r.Breakdown, models, r.Tokens.Total())
			})
			// Timeline.
			if len(d.Events) > 1 {
				card(c, pal, tr("timeline"), func() {
					ui.Text(c, trf("requestsN", fmtInt(int64(len(d.Events))))).FontSize(11).TextColor(pal.muted)
				}, func() {
					timeline(c.Key("tl"+s.sel), pal, d.Events, models, 170)
				})
			}
			// Subagents.
			if len(d.Children) > 0 {
				card(c, pal, tr("subagents"), func() {
					chev := ui.Icon(c, icChevron).FontSize(14).TextColor(pal.muted).Cursor(ui.CursorPointer)
					if !s.openKids {
						chev.Rotate(-90)
					}
					if chev.Clicked() {
						s.openKids = !s.openKids
					}
				}, func() {
					kids := d.Children
					n := len(kids)
					if !s.openKids && n > 3 {
						n = 3
					}
					var top int64 = 1
					for _, k := range kids {
						if v := k.Tokens.Total(); v > top {
							top = v
						}
					}
					for i := 0; i < n; i++ {
						k := kids[i]
						ui.Column(c.Key(k.SessionID)).Gap(5).Padding(4, 0).Children(func() {
							ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
								ui.Icon(c, icBranch).FontSize(12).TextColor(Taki)
								t := k.Title
								if t == "" {
									t = k.SessionID
								}
								ui.Text(c, t).FontSize(12.5).TextColor(pal.ink).SingleLine().Ellipsis("…").Shrink(1)
								ui.Spacer(c)
								if len(k.Breakdown) > 0 {
									ui.Text(c, k.Breakdown[0].Model).FontSize(11).TextColor(pal.muted)
								}
								ui.Text(c, fmtTokens(k.Tokens.Total())).FontSize(12.5).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum")
							})
							rankBar(c, pal, float64(k.Tokens.Total())/float64(top), Taki, 5)
						})
					}
					if len(kids) > 3 {
						more := ui.Text(c, map[bool]string{true: "▲", false: "▼ +" + fmtInt(int64(len(kids)-3))}[s.openKids]).FontSize(11.5).TextColor(Taki).Cursor(ui.CursorPointer)
						if more.Clicked() {
							s.openKids = !s.openKids
						}
					}
				})
			}
		})
	})
}

func metaItem(c *ui.Context, pal palette, ic *ui.SVG, text string) {
	ui.Row(c).Gap(5).AlignItems(ui.Center).Shrink(1).Children(func() {
		ui.Icon(c, ic).FontSize(12).TextColor(pal.muted)
		ui.Text(c, text).FontSize(12).TextColor(pal.muted).SingleLine().Ellipsis("…")
	})
}

// detailModels orders a session's models, biggest first, so colors agree
// between the breakdown and the timeline.
func detailModels(evs []query.AttributedEvent, bd []query.ProviderModel) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range bd {
		if !seen[b.Model] {
			seen[b.Model] = true
			out = append(out, b.Model)
		}
	}
	extra := map[string]int64{}
	for _, e := range evs {
		if !seen[e.Model] {
			extra[e.Model] += e.Tokens.Total()
		}
	}
	var rest []string
	for m := range extra {
		rest = append(rest, m)
	}
	sort.Slice(rest, func(i, j int) bool { return extra[rest[i]] > extra[rest[j]] })
	return append(out, rest...)
}

func (s *State) breakdownTable(c *ui.Context, pal palette, bd []query.ProviderModel, models []string, total int64) {
	if len(bd) == 0 {
		ui.Text(c, tr("noData")).FontSize(12).TextColor(pal.muted)
		return
	}
	colOf := func(m string) ui.Color {
		for i, x := range models {
			if x == m {
				return bandAt(i)
			}
		}
		return pal.muted
	}
	ui.Column(c).Gap(10).Children(func() {
		for _, b := range bd {
			col := colOf(b.Model)
			ui.Column(c.Key(b.Provider + "|" + b.Model)).Gap(6).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					dot(c, col, 9)
					ui.Text(c, b.Model).FontSize(13).FontWeight(650).TextColor(pal.ink).SingleLine().Ellipsis("…").Shrink(1)
					ui.Text(c, "@ "+b.Provider).FontSize(12).TextColor(pal.muted).SingleLine()
					attribChip(c, pal, b.Attrib)
					ui.Spacer(c)
					ui.Text(c, trf("requestsN", fmtInt(b.Requests))).FontSize(11).TextColor(pal.muted)
					ui.Text(c, fmtCostOf(b.CostUSD, b.Requests, b.Unpriced)).FontSize(12).TextColor(pal.muted).FontFeatures("tnum").Width(64).TextAlign(ui.End)
					ui.Text(c, fmtTokens(b.Tokens.Total())).FontSize(13).FontWeight(750).TextColor(pal.ink).FontFeatures("tnum").Width(70).TextAlign(ui.End)
				})
				ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
					classBar(c, pal, b.Tokens, 6).Grow(1).Basis(0)
					share := 0.0
					if total > 0 {
						share = float64(b.Tokens.Total()) / float64(total)
					}
					ui.Text(c, fmtPct(share)).FontSize(11).TextColor(pal.muted).FontFeatures("tnum").Width(44).TextAlign(ui.End)
				})
			})
		}
	})
}

// attribChip tells how sure the provider is; nothing for the log's own word.
func attribChip(c *ui.Context, pal palette, a model.AttribSource) {
	if a == model.AttribLog || a == "" {
		return
	}
	col := map[model.AttribSource]ui.Color{
		model.AttribCCSwitch: Rana,
		model.AttribConfig:   Tomori,
		model.AttribUserRule: Taki,
		model.AttribInferred: Soyo,
	}[a]
	chip(c, attribLabel(a), col)
}
