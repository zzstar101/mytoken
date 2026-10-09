package gui

import (
	"math"
	"strings"
	"time"

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

// aurora paints the window's backdrop: a calm field with a few large, soft
// pools of the members' colors for the glass panes above to pick up.
func aurora(pal palette) func(p *ui.Painter, r ui.Rect) {
	return func(p *ui.Painter, r ui.Rect) {
		p.Fill(r, pal.base, 0)
		big := float32(math.Max(float64(r.W), float64(r.H)))
		a := float32(0.16)
		if pal.dark {
			a = 0.22
		}
		softGlow(p, r.X+r.W*0.08, r.Y+r.H*0.02, big*0.42, Tomori.Alpha(a))
		softGlow(p, r.X+r.W*0.92, r.Y+r.H*0.10, big*0.38, Taki.Alpha(a*0.8))
		softGlow(p, r.X+r.W*0.78, r.Y+r.H*1.02, big*0.40, Anon.Alpha(a*0.7))
		softGlow(p, r.X+r.W*0.20, r.Y+r.H*0.98, big*0.30, Rana.Alpha(a*0.5))
	}
}

// pick fills a guitar pick of height h centred at x, y and turned by rot
// radians: a wide rounded top tapering to a blunt tip, drawn point down.
func pick(p *ui.Painter, x, y, h, rot float32, c ui.Color) {
	sn, cs := float32(math.Sin(float64(rot))), float32(math.Cos(float64(rot)))
	at := func(u, v float32) (float32, float32) {
		u, v = u*h, v*h
		return x + u*cs - v*sn, y + u*sn + v*cs
	}
	path := new(ui.Path)
	path.MoveTo(at(0, -0.46))
	// Down the right side to the tip, then back up the mirrored left side.
	c1x, c1y := at(0.62, -0.50)
	c2x, c2y := at(0.38, 0.22)
	tx, ty := at(0, 0.50)
	path.CubeTo(c1x, c1y, c2x, c2y, tx, ty)
	c1x, c1y = at(-0.38, 0.22)
	c2x, c2y = at(-0.62, -0.50)
	tx, ty = at(0, -0.46)
	path.CubeTo(c1x, c1y, c2x, c2y, tx, ty)
	p.FillPath(path.Close(), c)
}

// hoverFade eases row and pill backgrounds as the pointer comes and goes.
var hoverFade = ui.ElementTransition{Colors: true, Duration: 140 * time.Millisecond}

// entrance is 0 on the frame el first appears and then eases to 1, so charts
// grow in when a page opens. (Animate on its own starts at its target, and
// with reduced motion this jumps straight to 1.)
func entrance(el ui.Element, d time.Duration) float32 {
	var target float32 = 1
	if b := el.Bounds(); b.W == 0 && b.H == 0 {
		target = 0 // not laid out last frame: new
	}
	return el.Animate("entrance", target, d)
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

// pane is a floating pane of glass with a lit rim and a soft shadow.
func pane(c *ui.Context, pal palette) ui.Element {
	return ui.Column(c).Radius(20).Material(glass.Glass{Style: glass.Regular, Tint: pal.pane}).
		Border(1, pal.edge).Shadow(0, 12, 32, -8, pal.lift)
}

// card is a titled pane.
func card(c *ui.Context, pal palette, title string, trailing func(), body func()) ui.Element {
	return pane(c, pal).Padding(18, 20).Gap(14).Children(func() {
		if title != "" || trailing != nil {
			ui.Row(c).AlignItems(ui.Center).Gap(8).Children(func() {
				kicker(c, pal, title)
				ui.Spacer(c)
				if trailing != nil {
					trailing()
				}
			})
		}
		body()
	})
}

// kicker is a small tracked-out label over a section, in capitals where the
// script has them.
func kicker(c *ui.Context, pal palette, s string) ui.Element {
	return ui.Text(c, strings.ToUpper(s)).FontSize(11).FontWeight(650).TextColor(pal.muted).LetterSpacing(0.9).SingleLine()
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

// logo sets "MyToken" in the display face, followed by five exclamation
// marks drawn as the band: one bar and dot per member.
func logo(c *ui.Context, pal palette, size float32) ui.Element {
	return ui.Row(c).AlignItems(ui.End).Gap(size * 0.12).Shrink(0).Children(func() {
		ui.Text(c, "MyToken").Font(display).FontSize(size * 1.06).FontWeight(800).TextColor(pal.ink).LetterSpacing(-0.4).SingleLine()
		bangs(c, size*0.86, false).Margin(0, 0, size*0.26, 0)
	})
}

// bangs draws the five exclamation marks at height h. Live, the bars bounce
// like a level meter, each to its own beat.
func bangs(c *ui.Context, h float32, live bool) ui.Element {
	w := h * 0.17
	gap := h * 0.12
	el := ui.Box(c).Size(5*w+4*gap, h).Shrink(0)
	var t float32
	if live {
		t = el.Loop("meter", 2400*time.Millisecond, ui.Linear)
	}
	return el.Draw(func(p *ui.Painter, r ui.Rect) {
		for i, col := range []ui.Color{Tomori, Anon, Rana, Soyo, Taki} {
			x := r.X + float32(i)*(w+gap)
			full := r.H * 0.66
			bar := full
			if live {
				ph := float64(t)*2*math.Pi*float64(i%3+2) + float64(i)*1.7
				bar = full * float32(0.42+0.58*math.Abs(math.Sin(ph)))
			}
			p.Fill(ui.Rect{X: x, Y: r.Y + full - bar, W: w, H: bar}, col, w/2)
			p.FillPath(new(ui.Path).Circle(x+w/2, r.Y+r.H-w/2, w/2), col)
		}
	})
}

// bigNumber is a large figure in the display face, its unit (万, M, %…)
// set small beside it.
func bigNumber(c *ui.Context, pal palette, s string, size float32) ui.Element {
	return ui.RichText(c, figure(pal, s, size)...).SingleLine()
}

// figure splits a formatted value into spans: a leading currency sign and
// the digits in the display face, a trailing unit small in the interface face.
func figure(pal palette, s string, size float32) []ui.Span {
	lead, num, unit := splitFigure(s)
	var spans []ui.Span
	if lead != "" {
		spans = append(spans, ui.Span{Text: lead, Font: display, Size: size * 0.6, Weight: 600, Color: pal.muted})
	}
	spans = append(spans, ui.Span{Text: num, Font: display, Size: size, Weight: 700, Color: pal.ink, LetterSpacing: -0.03 * size, Features: "tnum"})
	if unit != "" {
		spans = append(spans, ui.Span{Text: " " + unit, Size: size * 0.34, Weight: 650, Color: pal.muted})
	}
	return spans
}

// splitFigure cuts "$1.5K" into "$", "1.5" and "K", and "2162万" into "",
// "2162" and "万".
func splitFigure(s string) (lead, num, unit string) {
	i := 0
	for i < len(s) && !isFigure(s[i]) {
		i++
	}
	j := i
	for j < len(s) && isFigure(s[j]) {
		j++
	}
	if j == i {
		return "", s, ""
	}
	return s[:i], s[i:j], strings.TrimSpace(s[j:])
}

func isFigure(b byte) bool { return b >= '0' && b <= '9' || b == '.' || b == ',' }

// countUp is a figure that counts up from zero when it first appears.
func countUp(c *ui.Context, pal palette, v float64, format func(float64) string, size float32) ui.Element {
	el := ui.Box(c).Shrink(0)
	f := entrance(el, 1100*time.Millisecond)
	return el.Children(func() {
		bigNumber(c, pal, format(v*float64(f)), size)
	})
}

// segmented is a pill switcher whose raised thumb slides to the choice.
func segmented(c *ui.Context, pal palette, sel *int, labels ...string) bool {
	changed := false
	box := ui.Row(c).Padding(3).Gap(2).Radius(99).Background(pal.well).Border(1, pal.faint)
	box.Children(func() {
		var segs []ui.Element
		for i, l := range labels {
			on := *sel == i
			b := ui.Row(c.Key(l)).Padding(5, 13).Radius(99).Cursor(ui.CursorPointer).Label(l).Role(ui.RoleButton).Transition(hoverFade)
			if !on && b.Hovered() {
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
			segs = append(segs, b)
		}
		glider(box, segs, *sel, 99, pal.raised, ui.Transparent, ui.RGBA(0, 0, 0, 0.12))
	})
	return changed
}

// glider paints a raised pill behind items[i], as laid out last frame, in
// parent's own drawing (so under the items), easing it from choice to choice.
func glider(parent ui.Element, items []ui.Element, i int, radius float32, fill, edge, shadow ui.Color) {
	if i < 0 || i >= len(items) {
		return
	}
	pb, b := parent.Bounds(), items[i].Bounds()
	if b.W == 0 {
		return
	}
	const d = 320 * time.Millisecond
	x := parent.AnimateWith("gx", b.X-pb.X, d, ui.EaseOut)
	y := parent.AnimateWith("gy", b.Y-pb.Y, d, ui.EaseOut)
	w := parent.AnimateWith("gw", b.W, d, ui.EaseOut)
	h := parent.AnimateWith("gh", b.H, d, ui.EaseOut)
	parent.Draw(func(p *ui.Painter, r ui.Rect) {
		rr := ui.Rect{X: r.X + x, Y: r.Y + y, W: w, H: h}
		p.Shadow(rr, radius, 0, 2, 8, 0, shadow)
		p.Fill(rr, fill, radius)
		if edge != ui.Transparent {
			p.Stroke(rr, edge, radius, 1)
		}
	})
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

// art is the drawing above an empty state's copy.
type art int

const (
	artStaff art = iota // five notes on a staff: nothing recorded yet
	artPicks            // five dropped picks: a search found nothing
	artStage            // a spotlight and stars: nothing yet today
)

// emptyState is a centered illustration with a title and a line of copy. It
// rises in gently when it appears.
func emptyState(c *ui.Context, pal palette, kind art, title, sub string) {
	band := []ui.Color{Tomori, Anon, Rana, Soyo, Taki}
	ui.Column(c).FillWidth().Center().Gap(10).Padding(24).
		Transition(ui.ElementTransition{Duration: 280 * time.Millisecond, Enter: &ui.Motion{Y: 10}}).
		Children(func() {
			ui.Box(c).Size(132, 84).Draw(func(p *ui.Painter, r ui.Rect) {
				switch kind {
				case artPicks:
					// One pick per member, scattered with a soft drop shadow.
					for i, sp := range [][3]float32{{0.15, 0.62, -0.5}, {0.35, 0.36, 0.3}, {0.54, 0.68, -0.15}, {0.73, 0.40, 0.6}, {0.88, 0.70, -0.35}} {
						x, y := r.X+r.W*sp[0], r.Y+r.H*sp[1]
						pick(p, x+1, y+2, 24, sp[2], ui.RGBA(0, 0, 0, 0.07))
						pick(p, x, y, 24, sp[2], band[i].Alpha(0.85))
					}
					star(p, r.X+r.W*0.55, r.Y+10, 5, Soyo)
				case artStage:
					// An empty stage: one warm spotlight and five stars.
					cx, cy := r.X+r.W*0.5, r.Y+r.H*0.55
					softGlow(p, cx, cy, 34, Soyo.Alpha(0.35))
					star(p, cx, cy, 13, Soyo.Alpha(0.9))
					for i, sp := range [][3]float32{{0.14, 0.30, 6}, {0.28, 0.80, 4}, {0.80, 0.22, 7}, {0.90, 0.66, 4.5}, {0.66, 0.88, 3.5}} {
						star(p, r.X+r.W*sp[0], r.Y+r.H*sp[1], sp[2], band[i])
					}
				default:
					// Five notes on a staff, one per member.
					for i := 0; i < 5; i++ {
						y := r.Y + 22 + float32(i)*10
						p.Line(r.X+4, y, r.X+r.W-4, y, 1, pal.faint)
					}
					for i, col := range band {
						x := r.X + 18 + float32(i)*23
						y := r.Y + 62 - float32((i*7)%4)*9
						note(p, x, y, 26, col)
					}
					star(p, r.X+r.W-8, r.Y+8, 6, Soyo)
					pick(p, r.X+8, r.Y+10, 12, -0.4, Anon.Alpha(0.7))
				}
			})
			ui.Text(c, title).FontSize(15).FontWeight(700).TextColor(pal.ink).TextAlign(ui.Center)
			ui.Text(c, sub).FontSize(12).TextColor(pal.muted).TextAlign(ui.Center).MaxWidth(320)
		})
}
