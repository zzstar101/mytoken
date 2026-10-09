package gui

import (
	"math"

	"github.com/egoist/mygo/plugins/glass"
	"github.com/egoist/mygo/ui"
)

// icon parses a 24×24 stroked icon drawn in currentColor.
func icon(shapes string) *ui.SVG {
	return ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">` + shapes + `</svg>`))
}

var (
	icOverview = icon(`<rect x="3" y="3" width="7" height="9" rx="2"/><rect x="14" y="3" width="7" height="5" rx="2"/><rect x="14" y="12" width="7" height="9" rx="2"/><rect x="3" y="16" width="7" height="5" rx="2"/>`)
	icSessions = icon(`<path d="M4 6h16M4 12h16M4 18h10"/>`)
	icRanking  = icon(`<path d="M8 21V11M16 21V7M12 21V3M4 21h16"/>`)
	icProjects = icon(`<path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>`)
	icSettings = icon(`<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z"/>`)
	icSearch   = icon(`<circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/>`)
	icBranch   = icon(`<path d="M6 3v12"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M18 9a9 9 0 0 1-9 9"/>`)
	icFolder   = icon(`<path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>`)
	icClock    = icon(`<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>`)
	icArrowUp  = icon(`<path d="M7 17 17 7M9 7h8v8"/>`)
	icArrowDn  = icon(`<path d="M7 7l10 10M17 9v8H9"/>`)
	icWindow   = icon(`<rect x="3" y="4" width="18" height="16" rx="3"/><path d="M3 9h18"/>`)
	icPower    = icon(`<path d="M12 3v9"/><path d="M6.4 6.4a8 8 0 1 0 11.2 0"/>`)
	icRefresh  = icon(`<path d="M21 12a9 9 0 0 1-15.5 6.2L3 16"/><path d="M3 12A9 9 0 0 1 18.5 5.8L21 8"/><path d="M3 21v-5h5M21 3v5h-5"/>`)
	icChevron  = icon(`<path d="m9 6 6 6-6 6"/>`)
	icSparkle  = icon(`<path d="M12 3l1.8 5.2L19 10l-5.2 1.8L12 17l-1.8-5.2L5 10l5.2-1.8z"/>`)
)

// aurora paints the window's backdrop: the base color and five soft blobs of
// the member colors, which the glass panes above refract.
func aurora(pal palette) func(p *ui.Painter, r ui.Rect) {
	return func(p *ui.Painter, r ui.Rect) {
		p.Fill(r, pal.base, 0)
		a := float32(0.20)
		if pal.dark {
			a = 0.22
		}
		blob := func(fx, fy, fr float32, c ui.Color, alpha float32) {
			cx, cy := r.X+r.W*fx, r.Y+r.H*fy
			rad := fr * float32(math.Max(float64(r.W), float64(r.H)))
			// A blurred circular shadow is a soft glow at the cost of one
			// draw; stacks of path rings swamp the renderer's path budget.
			core := rad * 0.45
			p.Shadow(ui.Rect{X: cx - core, Y: cy - core, W: core * 2, H: core * 2}, core, 0, 0, rad*0.9, 0, c.Alpha(alpha*1.6))
		}
		blob(0.08, 0.10, 0.42, Tomori, a)
		blob(0.92, 0.05, 0.38, Anon, a*0.9)
		blob(0.55, 0.55, 0.36, Taki, a*0.7)
		blob(0.12, 0.95, 0.40, Rana, a*0.8)
		blob(0.95, 0.90, 0.36, Soyo, a*0.9)
		// Faint music dust: tiny stars and notes, fixed in place.
		seed := uint32(42)
		rnd := func() float32 { seed = seed*1664525 + 1013904223; return float32(seed>>8) / float32(1<<24) }
		for i := 0; i < 18; i++ {
			x, y := r.X+rnd()*r.W, r.Y+rnd()*r.H
			col := bandAt(i).Alpha(0.22)
			if i%3 == 0 {
				note(p, x, y, 9+rnd()*6, col)
			} else {
				star(p, x, y, 3+rnd()*4, col)
			}
		}
	}
}

// star fills a four-pointed sparkle centered at x, y.
func star(p *ui.Painter, x, y, s float32, c ui.Color) {
	k := s * 0.28
	path := new(ui.Path).MoveTo(x, y-s).
		QuadTo(x+k*0.4, y-k*0.4, x+s, y).
		QuadTo(x+k*0.4, y+k*0.4, x, y+s).
		QuadTo(x-k*0.4, y+k*0.4, x-s, y).
		QuadTo(x-k*0.4, y-k*0.4, x, y-s).Close()
	p.FillPath(path, c)
}

// note fills an eighth note of height h whose head is at x, y.
func note(p *ui.Painter, x, y, h float32, c ui.Color) {
	r := h * 0.22
	p.FillPath(new(ui.Path).Circle(x, y, r), c)
	p.Line(x+r*0.9, y, x+r*0.9, y-h, h*0.09, c)
	flag := new(ui.Path).MoveTo(x+r*0.9, y-h).
		QuadTo(x+r*0.9+h*0.35, y-h*0.8, x+r*0.9+h*0.3, y-h*0.45).
		QuadTo(x+r*0.9+h*0.2, y-h*0.68, x+r*0.9, y-h*0.72).Close()
	p.FillPath(flag, c)
}

// pane is a floating pane of glass.
func pane(c *ui.Context, pal palette) ui.Element {
	return ui.Column(c).Radius(18).Material(glass.Glass{Style: glass.Regular, Tint: pal.pane})
}

// card is a titled pane.
func card(c *ui.Context, pal palette, title string, trailing func(), body func()) ui.Element {
	return pane(c, pal).Padding(16, 18).Gap(12).Children(func() {
		if title != "" || trailing != nil {
			ui.Row(c).AlignItems(ui.Center).Gap(8).Children(func() {
				ui.Text(c, title).FontSize(13).FontWeight(650).TextColor(pal.muted).LetterSpacing(0.2)
				ui.Spacer(c)
				if trailing != nil {
					trailing()
				}
			})
		}
		body()
	})
}

// dot is a small round swatch.
func dot(c *ui.Context, col ui.Color, size float32) ui.Element {
	return ui.Box(c).Size(size, size).Radius(size / 2).Background(col).Shrink(0)
}

// chip is a little rounded label in a color.
func chip(c *ui.Context, text string, col ui.Color) ui.Element {
	return ui.Row(c).Padding(2, 8).Radius(99).Background(col.Alpha(0.16)).Shrink(0).Children(func() {
		ui.Text(c, text).FontSize(11).FontWeight(600).TextColor(col.Mix(ui.RGB(0, 0, 0), 0.25)).SingleLine()
	})
}

// chipOn is a chip readable on the palette's appearance.
func chipOn(c *ui.Context, pal palette, text string, col ui.Color) ui.Element {
	ink := col.Mix(ui.RGB(0, 0, 0), 0.3)
	if pal.dark {
		ink = col.Mix(ui.RGB(255, 255, 255), 0.35)
	}
	return ui.Row(c).Padding(2, 8).Radius(99).Background(col.Alpha(0.18)).Shrink(0).Children(func() {
		ui.Text(c, text).FontSize(11).FontWeight(600).TextColor(ink).SingleLine()
	})
}

// logo draws "MyToken" followed by five exclamation marks, one per member.
func logo(c *ui.Context, pal palette, size float32) ui.Element {
	return ui.RichText(c,
		ui.Span{Text: "MyToken", Weight: 800, Color: pal.ink, Size: size},
		ui.Span{Text: "!", Weight: 900, Color: Tomori, Size: size},
		ui.Span{Text: "!", Weight: 900, Color: Anon, Size: size},
		ui.Span{Text: "!", Weight: 900, Color: Rana, Size: size},
		ui.Span{Text: "!", Weight: 900, Color: Soyo, Size: size},
		ui.Span{Text: "!", Weight: 900, Color: Taki, Size: size},
	).SingleLine()
}

// bigNumber is a large tabular figure.
func bigNumber(c *ui.Context, pal palette, s string, size float32) ui.Element {
	return ui.Text(c, s).FontSize(size).FontWeight(750).TextColor(pal.ink).FontFeatures("tnum").SingleLine().LetterSpacing(-0.5)
}

// segmented is a pill switcher over glass.
func segmented(c *ui.Context, pal palette, sel *int, labels ...string) bool {
	changed := false
	ui.Row(c).Padding(3).Gap(2).Radius(99).Background(pal.well).Children(func() {
		for i, l := range labels {
			on := *sel == i
			b := ui.Row(c.Key(l)).Padding(5, 12).Radius(99).Cursor(ui.CursorPointer).Label(l).Role(ui.RoleButton)
			if on {
				b.Background(pal.pane.Alpha(0.95)).Shadow(0, 1, 3, 0, ui.RGBA(0, 0, 0, 0.12))
			} else if b.Hovered() {
				b.Background(pal.hover)
			}
			b.Children(func() {
				col := pal.muted
				if on {
					col = pal.ink
				}
				ui.Text(c, l).FontSize(12).FontWeight(600).TextColor(col).SingleLine()
			})
			if b.Clicked() && !on {
				*sel = i
				changed = true
			}
		}
	})
	return changed
}

// delta shows the change against the previous period.
func delta(c *ui.Context, pal palette, cur, prev float64) {
	if prev <= 0 {
		return
	}
	ch := (cur - prev) / prev
	col, ic := Rana, icArrowUp
	if ch < 0 {
		col, ic = Anon, icArrowDn
	}
	ui.Row(c).Gap(2).AlignItems(ui.Center).Children(func() {
		ui.Icon(c, ic).FontSize(11).TextColor(col)
		ui.Text(c, fmtPct(math.Abs(ch))).FontSize(11).FontWeight(650).TextColor(col).FontFeatures("tnum")
		ui.Text(c, " "+tr("vsPrev")).FontSize(11).TextColor(pal.muted)
	})
}

// emptyState is a centered illustration with a line of copy.
func emptyState(c *ui.Context, pal palette, title, sub string) {
	ui.Column(c).Grow(1).Center().Gap(10).Padding(24).Children(func() {
		ui.Box(c).Size(120, 84).Draw(func(p *ui.Painter, r ui.Rect) {
			// Five notes on a staff, one per member.
			for i := 0; i < 5; i++ {
				y := r.Y + 22 + float32(i)*10
				p.Line(r.X+4, y, r.X+r.W-4, y, 1, pal.faint)
			}
			for i, col := range []ui.Color{Tomori, Anon, Rana, Soyo, Taki} {
				x := r.X + 16 + float32(i)*22
				y := r.Y + 62 - float32((i*7)%4)*9
				note(p, x, y, 26, col)
			}
			star(p, r.X+r.W-8, r.Y+8, 6, Soyo)
		})
		ui.Text(c, title).FontSize(15).FontWeight(700).TextColor(pal.ink)
		ui.Text(c, sub).FontSize(12).TextColor(pal.muted).TextAlign(ui.Center)
	})
}
