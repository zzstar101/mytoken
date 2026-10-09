package gui

import "github.com/egoist/mygo/ui"

// IconView draws the app icon filling its window: a deep indigo squircle
// holding five exclamation marks in the members' colors, rising like a bar
// chart — "MyToken!!!!!" and a usage graph at once.
func IconView(c *ui.Context) {
	c.Root().Background(ui.Transparent)
	ui.Box(c).Fill().Draw(paintIcon)
}

func paintIcon(p *ui.Painter, r ui.Rect) {
	s := r.W
	if r.H < s {
		s = r.H
	}
	// macOS icons sit inside a 10% margin on the canvas.
	m := s * 0.1
	b := ui.Rect{X: r.X + m, Y: r.Y + m, W: s - 2*m, H: s - 2*m}
	rad := b.W * 0.225
	p.Shadow(b, rad, 0, b.W*0.02, b.W*0.05, 0, ui.RGBA(20, 10, 60, 0.35))
	p.FillGradient(b, ui.LinearGradient{From: ui.Hex("#3A2F7A"), To: ui.Hex("#161230"), Angle: 160, Oklab: true}, rad)
	// Glow of the stage lights behind the marks.
	p.Clip(b, rad, func() {
		softGlow(p, b.X+b.W*0.25, b.Y+b.H*0.3, b.W*0.4, Taki.Alpha(0.45))
		softGlow(p, b.X+b.W*0.8, b.Y+b.H*0.75, b.W*0.35, Anon.Alpha(0.25))
		softGlow(p, b.X+b.W*0.45, b.Y+b.H*1.0, b.W*0.4, Tomori.Alpha(0.3))
		// Glassy sheen over the top half.
		p.FillGradient(ui.Rect{X: b.X, Y: b.Y, W: b.W, H: b.H * 0.5}, ui.LinearGradient{From: ui.RGBA(255, 255, 255, 0.14), To: ui.RGBA(255, 255, 255, 0), Angle: 180}, 0)
	})

	cols := []ui.Color{Tomori, Anon, Rana, Soyo, Taki}
	heights := []float32{0.20, 0.31, 0.26, 0.40, 0.50}
	n := float32(len(cols))
	padX := b.W * 0.17
	gap := b.W * 0.045
	w := (b.W - 2*padX - gap*(n-1)) / n
	base := b.Y + b.H*0.80
	dot := w
	for i, col := range cols {
		x := b.X + padX + float32(i)*(w+gap)
		h := b.H * heights[i]
		top := base - dot - w*0.55 - h
		bar := ui.Rect{X: x, Y: top, W: w, H: h}
		p.Shadow(bar, w/2, 0, 0, w*0.6, 0, col.Alpha(0.45))
		p.FillGradient(bar, ui.LinearGradient{From: col.Mix(ui.RGB(255, 255, 255), 0.25), To: col, Angle: 180, Oklab: true}, w/2)
		d := ui.Rect{X: x, Y: base - dot, W: w, H: dot}
		p.Shadow(d, w/2, 0, 0, w*0.6, 0, col.Alpha(0.45))
		p.Fill(d, col, w/2)
	}
	// A small sparkle, the lead singer's star.
	star(p, b.X+b.W*0.24, b.Y+b.H*0.22, b.W*0.06, ui.RGBA(255, 255, 255, 0.9))
}

// softGlow paints a blurred disc. A Shadow shows only outside its box, so
// the box sits far off to the side and the shadow is offset back into place.
func softGlow(p *ui.Painter, cx, cy, r float32, col ui.Color) {
	core := r * 0.45
	const off = 10000
	p.Shadow(ui.Rect{X: cx - core - off, Y: cy - core, W: 2 * core, H: 2 * core}, core, off, 0, r*0.9, 0, col)
}
