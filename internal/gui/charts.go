package gui

import (
	"math"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar/mytoken/internal/model"
	"github.com/zzstar/mytoken/internal/query"
)

// niceMax rounds v up to 1, 2, 2.5 or 5 times a power of ten.
func costOfEvent(e query.AttributedEvent) string {
	if !e.Priced {
		return "—"
	}
	return fmtCost(e.Cost)
}

func niceMax(v float64) float64 {
	if v <= 0 {
		return 1
	}
	e := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if v <= m*e {
			return m * e
		}
	}
	return 10 * e
}

// tooltipBox paints a small glassy tip with lines of text near x, y, kept
// inside bounds.
func tooltipBox(p *ui.Painter, pal palette, bounds ui.Rect, x, y float32, title string, rows [][2]string, colors []ui.Color) {
	const pad, lh = 10, 18
	w := float32(0)
	tw, _ := p.MeasureText(0, ui.Span{Text: title, Size: 12, Weight: 700})
	w = tw
	for _, r := range rows {
		a, _ := p.MeasureText(0, ui.Span{Text: r[0], Size: 11.5})
		b, _ := p.MeasureText(0, ui.Span{Text: r[1], Size: 11.5, Weight: 650})
		if a+b+28 > w {
			w = a + b + 28
		}
	}
	w += pad * 2
	h := float32(pad*2+16) + float32(len(rows))*lh
	bx, by := x+14, y-h/2
	if bx+w > bounds.X+bounds.W {
		bx = x - 14 - w
	}
	if by < bounds.Y {
		by = bounds.Y
	}
	if by+h > bounds.Y+bounds.H {
		by = bounds.Y + bounds.H - h
	}
	r := ui.Rect{X: bx, Y: by, W: w, H: h}
	bg := ui.RGBA(255, 255, 255, 0.94)
	if pal.dark {
		bg = ui.RGBA(36, 32, 50, 0.94)
	}
	p.Shadow(r, 10, 0, 6, 18, 0, ui.RGBA(0, 0, 0, 0.18))
	p.Fill(r, bg, 10)
	p.Stroke(r, pal.faint, 10, 1)
	p.RichText(bx+pad, by+pad, 0, ui.Span{Text: title, Size: 12, Weight: 700, Color: pal.ink})
	for i, row := range rows {
		ry := by + pad + 20 + float32(i)*lh
		if i < len(colors) {
			p.Fill(ui.Rect{X: bx + pad, Y: ry + 4, W: 8, H: 8}, colors[i], 4)
		}
		p.RichText(bx+pad+14, ry, 0, ui.Span{Text: row[0], Size: 11.5, Color: pal.muted})
		vw, _ := p.MeasureText(0, ui.Span{Text: row[1], Size: 11.5, Weight: 650, Features: "tnum"})
		p.RichText(bx+w-pad-vw, ry, 0, ui.Span{Text: row[1], Size: 11.5, Weight: 650, Color: pal.ink, Features: "tnum"})
	}
}

// classRows lists a Tokens value per class, for tooltips.
func classRows(t model.Tokens) ([][2]string, []ui.Color) {
	var rows [][2]string
	var cols []ui.Color
	for i := len(classes) - 1; i >= 0; i-- {
		k := classes[i]
		if v := k.get(t); v > 0 {
			rows = append(rows, [2]string{tr(k.key), fmtTokens(v)})
			cols = append(cols, k.color)
		}
	}
	return rows, cols
}

// stackedBars draws one bar per point, stacked by token class, with a y
// axis, sparse date labels and a hover tooltip.
func stackedBars(c *ui.Context, pal palette, pts []query.Point, hourly bool, height float32) ui.Element {
	el := ui.Box(c).FillWidth().Height(height)
	hx, _, hover := el.PointerPosition()
	grow := entrance(el, 700*time.Millisecond)
	el.Draw(func(p *ui.Painter, r ui.Rect) {
		if len(pts) == 0 {
			return
		}
		var mx float64
		for _, pt := range pts {
			mx = math.Max(mx, float64(pt.Tokens.Total()))
		}
		top := niceMax(mx)
		left, bottom := float32(44), float32(22)
		if hourly {
			left = 0
		}
		plot := ui.Rect{X: r.X + left, Y: r.Y + 6, W: r.W - left, H: r.H - bottom - 6}
		// Grid and y labels.
		for i := 0; i <= 4; i++ {
			y := plot.Y + plot.H - plot.H*float32(i)/4
			if i > 0 {
				p.Line(plot.X, y, plot.X+plot.W, y, 1, pal.faint)
			}
			if !hourly && (mx > 0 || i == 0) {
				s := fmtTokens(int64(top * float64(i) / 4))
				w, _ := p.MeasureText(0, ui.Span{Text: s, Size: 10.5})
				p.RichText(r.X+left-8-w, y-7, 0, ui.Span{Text: s, Size: 10.5, Color: pal.muted, Features: "tnum"})
			}
		}
		p.Line(plot.X, plot.Y+plot.H, plot.X+plot.W, plot.Y+plot.H, 1, pal.faint.Alpha(0.16))
		n := len(pts)
		slot := plot.W / float32(n)
		bw := slot * 0.5
		if bw > 22 {
			bw = 22
		}
		if bw < 2 {
			bw = slot * 0.8
		}
		hi := -1
		if hover {
			hi = int((hx - left) / slot)
			if hi < 0 || hi >= n {
				hi = -1
			}
		}
		// Label every k-th bar.
		every := int(math.Ceil(float64(n) / 8))
		for i, pt := range pts {
			cx := plot.X + slot*(float32(i)+0.5)
			if hi == i {
				p.Fill(ui.Rect{X: cx - slot/2, Y: plot.Y, W: slot, H: plot.H}, pal.hover, 6)
			}
			y := plot.Y + plot.H
			total := float64(pt.Tokens.Total())
			barH := float32(total/top) * plot.H * grow
			if total > 0 && barH < 2 {
				barH = 2
			}
			// Draw the whole bar rounded, clipping the stack inside it.
			br := ui.Rect{X: cx - bw/2, Y: y - barH, W: bw, H: barH}
			rad := float32(math.Min(float64(bw)/2, 5))
			p.Clip(br, rad, func() {
				yy := y
				for _, k := range classes {
					v := float64(k.get(pt.Tokens))
					if v == 0 {
						continue
					}
					h := float32(v/total) * barH
					col := k.color
					if hi >= 0 && hi != i {
						col = col.Alpha(0.45)
					}
					p.Fill(ui.Rect{X: br.X, Y: yy - h, W: bw, H: h + 0.5}, col, 0)
					yy -= h
				}
			})
			if i%every == 0 || i == n-1 && n <= 8 {
				lab := pt.Day.Format("1/2")
				if hourly {
					lab = pt.Day.Format("15h")
				}
				w, _ := p.MeasureText(0, ui.Span{Text: lab, Size: 10.5})
				p.RichText(cx-w/2, plot.Y+plot.H+6, 0, ui.Span{Text: lab, Size: 10.5, Color: pal.muted, Features: "tnum"})
			}
		}
		if hi >= 0 {
			pt := pts[hi]
			title := pt.Day.Format("2006-01-02 Mon")
			if hourly {
				title = pt.Day.Format("01-02 15:00")
			}
			rows, cols := classRows(pt.Tokens)
			rows = append([][2]string{{tr("tokens"), fmtTokens(pt.Tokens.Total())}, {tr("cost"), fmtCost(pt.CostUSD)}}, rows...)
			cols = append([]ui.Color{ui.Transparent, ui.Transparent}, cols...)
			tooltipBox(p, pal, r, plot.X+slot*(float32(hi)+0.5), plot.Y+plot.H/2, title, rows, cols)
		}
	})
	return el
}

// heroWave draws the days as one smooth melody across the hero's foot: a
// line in the band's colors over five faint staff lines, rising in when the
// page opens, with the day under the pointer called out.
func heroWave(c *ui.Context, pal palette, pts []query.Point, height float32) ui.Element {
	el := ui.Box(c).FillWidth().Height(height)
	hx, _, hover := el.PointerPosition()
	grow := entrance(el, 1000*time.Millisecond)
	el.Draw(func(p *ui.Painter, r ui.Rect) {
		for i := 0; i < 5; i++ {
			y := r.Y + 14 + (r.H-18)*float32(i)/4
			p.Line(r.X, y, r.X+r.W, y, 1, pal.faint.Alpha(0.6))
		}
		n := len(pts)
		if n < 2 {
			return
		}
		var mx float64
		for _, pt := range pts {
			mx = math.Max(mx, float64(pt.Tokens.Total()))
		}
		if mx == 0 {
			mx = 1
		}
		// The line runs from edge to edge; days sit at even steps.
		pad := float32(28)
		at := func(i int) (float32, float32) {
			v := float32(float64(pts[i].Tokens.Total())/mx) * grow
			return r.X + pad + (r.W-2*pad)*float32(i)/float32(n-1), r.Y + r.H - 4 - (r.H-22)*v
		}
		line := new(ui.Path)
		area := new(ui.Path)
		x0, y0 := at(0)
		line.MoveTo(r.X, y0).LineTo(x0, y0)
		area.MoveTo(r.X, r.Y+r.H).LineTo(r.X, y0).LineTo(x0, y0)
		for i := 1; i < n; i++ {
			px, py := at(i - 1)
			x, y := at(i)
			m := (px + x) / 2
			line.CubeTo(m, py, m, y, x, y)
			area.CubeTo(m, py, m, y, x, y)
		}
		xe, ye := at(n - 1)
		line.LineTo(r.X+r.W, ye)
		area.LineTo(r.X+r.W, ye).LineTo(r.X+r.W, r.Y+r.H).Close()
		p.FillPathGradient(area, ui.LinearGradient{From: Tomori.Alpha(0.16), To: Taki.Alpha(0.16), Angle: 90, Oklab: true})
		p.FillPathGradient(area, ui.LinearGradient{From: pal.base.Alpha(0), To: pal.base.Alpha(0.35), Angle: 180})
		p.StrokePathGradient(line, 2.2, ui.LinearGradient{From: Tomori, To: Anon, Angle: 90, Oklab: true})
		hi := n - 1
		if hover {
			hi = int(math.Round(float64((hx - pad) / ((r.W - 2*pad) / float32(n-1)))))
			hi = max(0, min(n-1, hi))
		}
		x, y := at(hi)
		if hover {
			p.Line(x, r.Y+8, x, r.Y+r.H, 1, pal.ink.Alpha(0.18))
		}
		softGlow(p, x, y, 14, Anon.Alpha(0.35))
		p.FillPath(new(ui.Path).Circle(x, y, 4.5), Anon)
		p.FillPath(new(ui.Path).Circle(x, y, 2), ui.RGB(255, 255, 255))
		if hover {
			pt := pts[hi]
			tooltipBox(p, pal, r, x, r.Y+r.H/2, pt.Day.Format("2006-01-02 Mon"),
				[][2]string{{tr("tokens"), fmtTokens(pt.Tokens.Total())}, {tr("cost"), fmtCost(pt.CostUSD)}}, nil)
		}
		_ = xe
	})
	return el
}

// miniBars draws compact bars with no axes (the tray's 24 hours).
func miniBars(c *ui.Context, pal palette, pts []query.Point, height float32) ui.Element {
	el := ui.Box(c).FillWidth().Height(height)
	hx, _, hover := el.PointerPosition()
	grow := entrance(el, 600*time.Millisecond)
	el.Draw(func(p *ui.Painter, r ui.Rect) {
		n := len(pts)
		if n == 0 {
			return
		}
		var mx float64
		for _, pt := range pts {
			mx = math.Max(mx, float64(pt.Tokens.Total()))
		}
		if mx == 0 {
			mx = 1
		}
		plotH := r.H - 16
		slot := r.W / float32(n)
		bw := slot * 0.56
		hi := -1
		if hover {
			hi = int(hx / slot)
		}
		// A quiet hour is a dot on the floor; a busy one rises as a level
		// meter in the band's colors.
		for i, pt := range pts {
			cx := r.X + slot*(float32(i)+0.5)
			x := cx - bw/2
			h := float32(float64(pt.Tokens.Total())/mx) * plotH * grow
			if h <= 0 {
				p.FillPath(new(ui.Path).Circle(cx, r.Y+plotH-1.5, 1.5), pal.faint)
				continue
			}
			if h < bw {
				h = bw
			}
			g := ui.LinearGradient{From: Tomori, To: Taki, Angle: 180, Oklab: true}
			if i == hi {
				g = ui.LinearGradient{From: Anon, To: Soyo, Angle: 180, Oklab: true}
			} else if hi >= 0 {
				g = ui.LinearGradient{From: Tomori.Alpha(0.5), To: Taki.Alpha(0.5), Angle: 180, Oklab: true}
			}
			p.FillGradient(ui.Rect{X: x, Y: r.Y + plotH - h, W: bw, H: h}, g, bw/2)
		}
		for _, i := range []int{0, n / 2, n - 1} {
			lab := pts[i].Day.Format("15:00")
			w, _ := p.MeasureText(0, ui.Span{Text: lab, Size: 10})
			x := r.X + slot*(float32(i)+0.5) - w/2
			x = float32(math.Max(float64(r.X), math.Min(float64(x), float64(r.X+r.W-w))))
			p.RichText(x, r.Y+plotH+3, 0, ui.Span{Text: lab, Size: 10, Color: pal.muted, Features: "tnum"})
		}
		if hi >= 0 && hi < n {
			pt := pts[hi]
			tooltipBox(p, pal, r, r.X+slot*(float32(hi)+0.5), r.Y+plotH/2, pt.Day.Format("15:00"),
				[][2]string{{tr("tokens"), fmtTokens(pt.Tokens.Total())}, {tr("cost"), fmtCost(pt.CostUSD)}}, nil)
		}
	})
	return el
}

// sparkline draws a smooth line with a fading fill.
func sparkline(c *ui.Context, vals []float64, col ui.Color, height float32) ui.Element {
	return ui.Box(c).FillWidth().Height(height).Draw(func(p *ui.Painter, r ui.Rect) {
		if len(vals) < 2 {
			return
		}
		mx := 0.0
		for _, v := range vals {
			mx = math.Max(mx, v)
		}
		if mx == 0 {
			mx = 1
		}
		pt := func(i int) (float32, float32) {
			return r.X + r.W*float32(i)/float32(len(vals)-1), r.Y + 2 + (r.H-4)*(1-float32(vals[i]/mx))
		}
		line := new(ui.Path)
		area := new(ui.Path)
		x0, y0 := pt(0)
		line.MoveTo(x0, y0)
		area.MoveTo(x0, r.Y+r.H).LineTo(x0, y0)
		for i := 1; i < len(vals); i++ {
			px, py := pt(i - 1)
			x, y := pt(i)
			mx := (px + x) / 2
			line.CubeTo(mx, py, mx, y, x, y)
			area.CubeTo(mx, py, mx, y, x, y)
		}
		xe, _ := pt(len(vals) - 1)
		area.LineTo(xe, r.Y+r.H).Close()
		p.FillPathGradient(area, ui.LinearGradient{From: col.Alpha(0.35), To: col.Alpha(0), Angle: 180})
		p.StrokePath(line, 2, col)
		xl, yl := pt(len(vals) - 1)
		p.FillPath(new(ui.Path).Circle(xl, yl, 3.5), col)
		p.FillPath(new(ui.Path).Circle(xl, yl, 1.6), ui.RGB(255, 255, 255))
	})
}

// arc appends a ring segment from angle a0 to a1 (radians, 0 = up, clockwise).
func arc(cx, cy, ro, ri float32, a0, a1 float64) *ui.Path {
	steps := int(math.Max(4, math.Ceil((a1-a0)/(math.Pi/48))))
	path := new(ui.Path)
	at := func(rad float32, a float64) (float32, float32) {
		return cx + rad*float32(math.Sin(a)), cy - rad*float32(math.Cos(a))
	}
	x, y := at(ro, a0)
	path.MoveTo(x, y)
	for i := 1; i <= steps; i++ {
		x, y = at(ro, a0+(a1-a0)*float64(i)/float64(steps))
		path.LineTo(x, y)
	}
	for i := steps; i >= 0; i-- {
		x, y = at(ri, a0+(a1-a0)*float64(i)/float64(steps))
		path.LineTo(x, y)
	}
	return path.Close()
}

// donut draws the token-class mix with the total in the middle.
func donut(c *ui.Context, pal palette, t model.Tokens, size float32) ui.Element {
	el := ui.Box(c).Size(size, size).Shrink(0)
	hx, hy, hover := el.PointerPosition()
	sweep := entrance(el, 900*time.Millisecond)
	el.Draw(func(p *ui.Painter, r ui.Rect) {
		cx, cy := r.X+r.W/2, r.Y+r.H/2
		ro := r.W / 2
		ri := ro * 0.68
		total := float64(t.Total())
		p.FillPath(arc(cx, cy, ro, ri, 0, 2*math.Pi-0.0001), pal.well)
		hi := -1
		if hover {
			dx, dy := float64(hx-r.W/2), float64(hy-r.H/2)
			d := math.Hypot(dx, dy)
			if d >= float64(ri) && d <= float64(ro) {
				a := math.Atan2(dx, -dy)
				if a < 0 {
					a += 2 * math.Pi
				}
				acc := 0.0
				for i, k := range classes {
					f := float64(k.get(t)) / total
					if a >= acc*2*math.Pi && a < (acc+f)*2*math.Pi {
						hi = i
					}
					acc += f
				}
			}
		}
		if total > 0 {
			acc := 0.0
			gap := 0.025
			for i, k := range classes {
				f := float64(k.get(t)) / total
				if f <= 0 {
					continue
				}
				a0 := acc * 2 * math.Pi * float64(sweep)
				a1 := (acc + f) * 2 * math.Pi * float64(sweep)
				acc += f
				if a1-a0 > gap*2 {
					a0 += gap / 2
					a1 -= gap / 2
				}
				o := ro
				if i == hi {
					o += 4
				}
				p.FillPath(arc(cx, cy, o, ri, a0, a1), k.color)
			}
		}
		// Center label.
		big, small := fmtTokens(t.Total()), tr("tokens")
		if hi >= 0 {
			k := classes[hi]
			big, small = fmtPct(float64(k.get(t))/total), tr(k.key)
		}
		fig := figure(pal, big, size*0.2)
		w, h := p.MeasureText(0, fig...)
		p.RichText(cx-w/2, cy-h/2-6, 0, fig...)
		w2, _ := p.MeasureText(0, ui.Span{Text: small, Size: 11})
		p.RichText(cx-w2/2, cy+h/2-4, 0, ui.Span{Text: small, Size: 11, Color: pal.muted})
	})
	return el
}

// heatLevel maps a value to 0..4 against quartiles of the nonzero days.
func heatColor(pal palette, v, mx float64) ui.Color {
	if v <= 0 {
		return pal.well
	}
	f := math.Sqrt(v / mx)
	switch {
	case f < 0.25:
		return Tomori.Alpha(0.35)
	case f < 0.5:
		return Tomori.Mix(Taki, 0.4).Alpha(0.65)
	case f < 0.75:
		return Taki.Alpha(0.85)
	}
	return Anon
}

// heatmap draws one column per week (Monday on top), like a contribution graph.
func heatmap(c *ui.Context, pal palette, days []query.Point, height float32) ui.Element {
	el := ui.Box(c).FillWidth().Height(height)
	hx, hy, hover := el.PointerPosition()
	el.Draw(func(p *ui.Painter, r ui.Rect) {
		if len(days) == 0 {
			return
		}
		mx := 0.0
		for _, d := range days {
			mx = math.Max(mx, float64(d.Tokens.Total()))
		}
		top := float32(16)
		labW := float32(22)
		gap := float32(3)
		// Cells are sized by height; as many recent weeks as fit are shown.
		cell := (r.H-top)/7 - gap
		weeks := (len(days) + 6) / 7
		if fit := int((r.W - labW) / (cell + gap)); fit < weeks && fit > 0 {
			days = days[(weeks-fit)*7:]
			weeks = fit
		}
		if c := (r.W-labW)/float32(weeks) - gap; c < cell {
			cell = c
		}
		ox := r.X + labW + ((r.W-labW)-float32(weeks)*(cell+gap))/2
		var hit *query.Point
		var hitX, hitY float32
		lastMonth := -1
		lastLabX := float32(-1e9)
		for i := range days {
			d := &days[i]
			w, wd := i/7, i%7
			x := ox + float32(w)*(cell+gap)
			y := r.Y + top + float32(wd)*(cell+gap)
			if wd == 0 && int(d.Day.Month()) != lastMonth {
				lastMonth = int(d.Day.Month())
				// A month stub in the first column would collide with the next.
				if x-lastLabX >= 28 && (w > 0 || d.Day.Day() <= 17) {
					lastLabX = x
					lab := d.Day.Format("Jan")
					if Lang() == "zh" {
						lab = d.Day.Format("1月")
					}
					p.RichText(x, r.Y, 0, ui.Span{Text: lab, Size: 10, Color: pal.muted})
				}
			}
			cr := ui.Rect{X: x, Y: y, W: cell, H: cell}
			p.Fill(cr, heatColor(pal, float64(d.Tokens.Total()), mx), cell*0.28)
			if hover && hx+r.X >= x && hx+r.X < x+cell+gap && hy+r.Y >= y && hy+r.Y < y+cell+gap {
				hit, hitX, hitY = d, x+cell/2, y+cell/2
				p.Stroke(cr, pal.ink.Alpha(0.6), cell*0.28, 1.5)
			}
		}
		wdLabels := []string{"Mon", "", "Wed", "", "Fri", "", ""}
		if Lang() == "zh" {
			wdLabels = []string{"一", "", "三", "", "五", "", ""}
		}
		for i, l := range wdLabels {
			if l != "" {
				p.RichText(r.X, r.Y+top+float32(i)*(cell+gap)+cell/2-7, 0, ui.Span{Text: l, Size: 10, Color: pal.muted})
			}
		}
		if hit != nil {
			tooltipBox(p, pal, r, hitX, hitY, hit.Day.Format("2006-01-02 Mon"),
				[][2]string{{tr("tokens"), fmtTokens(hit.Tokens.Total())}, {tr("cost"), fmtCost(hit.CostUSD)}}, nil)
		}
	})
	return el
}

// heatLegend is the less…more scale under the heatmap.
func heatLegend(c *ui.Context, pal palette) {
	ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
		ui.Text(c, tr("less")).FontSize(10.5).TextColor(pal.muted)
		for _, f := range []float64{0, 0.04, 0.2, 0.5, 1} {
			ui.Box(c).Size(10, 10).Radius(3).Background(heatColor(pal, f, 1))
		}
		ui.Text(c, tr("more")).FontSize(10.5).TextColor(pal.muted)
	})
}

// rankBar draws a rounded progress track for a share in 0..1.
func rankBar(c *ui.Context, pal palette, frac float64, col ui.Color, height float32) ui.Element {
	el := ui.Box(c).FillWidth().Height(height)
	g := entrance(el, 600*time.Millisecond)
	return el.Draw(func(p *ui.Painter, r ui.Rect) {
		p.Fill(r, pal.well, r.H/2)
		w := r.W * g * float32(frac)
		if w > 0 && w < r.H {
			w = r.H
		}
		if w > 0 {
			p.FillGradient(ui.Rect{X: r.X, Y: r.Y, W: w, H: r.H}, ui.LinearGradient{From: col.Alpha(0.75), To: col, Angle: 90}, r.H/2)
		}
	})
}

// classBar is a single horizontal bar split by token class.
func classBar(c *ui.Context, pal palette, t model.Tokens, height float32) ui.Element {
	return ui.Box(c).FillWidth().Height(height).Draw(func(p *ui.Painter, r ui.Rect) {
		p.Fill(r, pal.well, r.H/2)
		total := float64(t.Total())
		if total == 0 {
			return
		}
		p.Clip(r, r.H/2, func() {
			x := r.X
			for _, k := range classes {
				w := float32(float64(k.get(t))/total) * r.W
				if w <= 0 {
					continue
				}
				p.Fill(ui.Rect{X: x, Y: r.Y, W: w + 0.5, H: r.H}, k.color, 0)
				x += w
			}
		})
	})
}

// timeline draws a session's requests as dots over time, sized by tokens.
func timeline(c *ui.Context, pal palette, evs []query.AttributedEvent, models []string, height float32) ui.Element {
	el := ui.Box(c).FillWidth().Height(height)
	hx, hy, hover := el.PointerPosition()
	el.Draw(func(p *ui.Painter, r ui.Rect) {
		if len(evs) == 0 {
			return
		}
		t0, t1 := evs[0].Timestamp, evs[len(evs)-1].Timestamp
		span := t1.Sub(t0).Seconds()
		if span <= 0 {
			span = 1
		}
		var mx float64
		for _, e := range evs {
			mx = math.Max(mx, float64(e.Tokens.Total()))
		}
		plot := ui.Rect{X: r.X + 6, Y: r.Y + 6, W: r.W - 12, H: r.H - 26}
		for i := 0; i <= 2; i++ {
			y := plot.Y + plot.H*float32(i)/2
			p.Line(plot.X, y, plot.X+plot.W, y, 1, pal.faint)
		}
		colOf := func(m string) ui.Color {
			for i, x := range models {
				if x == m {
					return bandAt(i)
				}
			}
			return pal.muted
		}
		// Cumulative line.
		cum := new(ui.Path)
		var acc, all float64
		for _, e := range evs {
			all += float64(e.Tokens.Total())
		}
		var best = -1
		var bestD float32 = 1e9
		for i, e := range evs {
			acc += float64(e.Tokens.Total())
			x := plot.X + plot.W*float32(e.Timestamp.Sub(t0).Seconds()/span)
			y := plot.Y + plot.H*(1-float32(acc/all))
			if i == 0 {
				cum.MoveTo(x, y)
			} else {
				cum.LineTo(x, y)
			}
			if hover {
				if d := float32(math.Abs(float64(x - (r.X + hx)))); d < bestD {
					bestD, best = d, i
				}
			}
		}
		// The running total as a soft wash behind the requests.
		area := new(ui.Path)
		acc = 0
		for i, e := range evs {
			acc += float64(e.Tokens.Total())
			x := plot.X + plot.W*float32(e.Timestamp.Sub(t0).Seconds()/span)
			y := plot.Y + plot.H*(1-float32(acc/all))
			if i == 0 {
				area.MoveTo(x, plot.Y+plot.H).LineTo(x, y)
			} else {
				area.LineTo(x, y)
			}
		}
		area.LineTo(plot.X+plot.W, plot.Y+plot.H).Close()
		p.FillPathGradient(area, ui.LinearGradient{From: Tomori.Alpha(0.10), To: Tomori.Alpha(0), Angle: 180})
		p.StrokePath(cum, 1.25, Tomori.Alpha(0.45))
		// Each request is a stem from the floor with a ringed head, like a
		// note on a staff.
		for i, e := range evs {
			x := plot.X + plot.W*float32(e.Timestamp.Sub(t0).Seconds()/span)
			v := float64(e.Tokens.Total())
			y := plot.Y + plot.H*(1-float32(v/mx))
			rad := float32(2 + 3.2*math.Sqrt(v/mx))
			col := colOf(e.Model)
			if best >= 0 && i != best && bestD < 24 {
				col = col.Alpha(0.5)
			}
			p.Line(x, plot.Y+plot.H, x, y+rad, 1, col.Alpha(0.35))
			if i == best && bestD < 24 {
				p.FillPath(new(ui.Path).Circle(x, y, rad+5), col.Alpha(0.18))
			}
			p.FillPath(new(ui.Path).Circle(x, y, rad+1.5), pal.raised)
			p.FillPath(new(ui.Path).Circle(x, y, rad), col)
		}
		for _, lab := range []struct {
			t time.Time
			a float32
		}{{t0, 0}, {t1, 1}} {
			s := lab.t.Local().Format("15:04")
			w, _ := p.MeasureText(0, ui.Span{Text: s, Size: 10.5})
			p.RichText(plot.X+(plot.W-w)*lab.a, plot.Y+plot.H+6, 0, ui.Span{Text: s, Size: 10.5, Color: pal.muted, Features: "tnum"})
		}
		if best >= 0 && bestD < 24 {
			e := evs[best]
			x := plot.X + plot.W*float32(e.Timestamp.Sub(t0).Seconds()/span)
			rows, cols := classRows(e.Tokens)
			prov := e.ResolvedProvider
			if l := attribLabel(e.Attrib); l != "" {
				prov += " · " + l
			}
			rows = append([][2]string{{prov, e.Model}, {tr("cost"), costOfEvent(e)}}, rows...)
			cols = append([]ui.Color{colOf(e.Model), ui.Transparent}, cols...)
			_ = hy
			tooltipBox(p, pal, r, x, plot.Y+plot.H/2, e.Timestamp.Local().Format("15:04:05"), rows, cols)
		}
	})
	return el
}
