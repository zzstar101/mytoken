// Package gui is MyToken!!!!!'s native interface: the main window and the
// tray panel, drawn with MyGo's ui package (no WebView).
//
// Look: Liquid Glass panes floating over a soft "aurora" painted in the five
// MyGO!!!!! member colors, which double as the chart palette.
package gui

import (
	"github.com/egoist/mygo/ui"
	"github.com/zzstar/mytoken/internal/model"
)

// The five member colors, slightly deepened so that they read on white.
var (
	Tomori = ui.Hex("#5AA9D6") // 高松燈  — sky blue
	Anon   = ui.Hex("#FF7A92") // 千早愛音 — pink
	Rana   = ui.Hex("#4FC27A") // 要楽奈  — green
	Soyo   = ui.Hex("#F0B83C") // 長崎そよ — honey
	Taki   = ui.Hex("#7B6FD0") // 椎名立希 — violet
)

// Band is the chart palette, in the order series are given colors.
var Band = []ui.Color{Tomori, Anon, Rana, Soyo, Taki,
	ui.Hex("#3E8FB8"), ui.Hex("#E25C79"), ui.Hex("#34A262"), ui.Hex("#D69A22"), ui.Hex("#5F54B3")}

// bandAt returns the i-th palette color, cycling.
func bandAt(i int) ui.Color { return Band[((i%len(Band))+len(Band))%len(Band)] }

// class is a token class with its color.
type class struct {
	key   string
	color ui.Color
	get   func(model.Tokens) int64
}

// classes lists the token classes in drawing order: one member per class.
var classes = []class{
	{"input", Tomori, func(t model.Tokens) int64 { return t.Input }},
	{"output", Anon, func(t model.Tokens) int64 { return t.Output }},
	{"cacheRead", Rana, func(t model.Tokens) int64 { return t.CacheRead }},
	{"cacheWrite", Soyo, func(t model.Tokens) int64 { return t.CacheWrite }},
	{"reasoning", Taki, func(t model.Tokens) int64 { return t.Reasoning }},
}

// harnessColor gives each harness a stable member color.
func harnessColor(h model.Harness) ui.Color {
	switch h {
	case model.ClaudeCode:
		return Soyo
	case model.Codex:
		return Tomori
	case model.DSH:
		return Taki
	case model.Pi:
		return Rana
	case model.Gemini:
		return ui.Hex("#3E8FB8")
	case model.OpenCode, model.Crush:
		return ui.Hex("#5F54B3")
	}
	return Anon
}

// palette is one appearance's colors beyond the ui.Theme.
type palette struct {
	dark bool
	// base is painted under the aurora; ink and muted color text; faint
	// colors hairlines and chart grids; well fills recessed tracks.
	base, ink, muted, faint, well ui.Color
	// pane tints the glass so that text on it stays legible.
	pane  ui.Color
	hover ui.Color
}

func paletteFor(dark bool) palette {
	if dark {
		return palette{
			dark:  true,
			base:  ui.Hex("#121019"),
			ink:   ui.Hex("#F3F0FA"),
			muted: ui.RGBA(235, 230, 250, 0.58),
			faint: ui.RGBA(255, 255, 255, 0.08),
			well:  ui.RGBA(255, 255, 255, 0.06),
			pane:  ui.RGBA(28, 24, 40, 0.45),
			hover: ui.RGBA(255, 255, 255, 0.07),
		}
	}
	return palette{
		base:  ui.Hex("#F6F3FA"),
		ink:   ui.Hex("#231F33"),
		muted: ui.RGBA(35, 31, 51, 0.55),
		faint: ui.RGBA(35, 31, 51, 0.08),
		well:  ui.RGBA(35, 31, 51, 0.05),
		pane:  ui.RGBA(255, 255, 255, 0.50),
		hover: ui.RGBA(35, 31, 51, 0.05),
	}
}

// applyTheme sets MyToken's theme on c and returns its palette.
func applyTheme(c *ui.Context) palette {
	base := c.Theme()
	pal := paletteFor(base.Dark)
	t := *base
	t.Accent = Taki
	t.AccentHover = Taki.Mix(ui.RGB(255, 255, 255), 0.12)
	t.AccentPressed = Taki.Mix(ui.RGB(0, 0, 0), 0.12)
	t.AccentText = ui.RGB(255, 255, 255)
	t.Text = pal.ink
	t.TextMuted = pal.muted
	t.Background = pal.base
	t.Radius = 10
	t.Font = "SF Pro Rounded, system-ui"
	if pal.dark {
		t.Surface = ui.RGBA(255, 255, 255, 0.08)
		t.SurfaceHover = ui.RGBA(255, 255, 255, 0.12)
		t.SurfacePressed = ui.RGBA(255, 255, 255, 0.16)
		t.Border = ui.RGBA(255, 255, 255, 0.10)
	} else {
		t.Surface = ui.RGBA(255, 255, 255, 0.70)
		t.SurfaceHover = ui.RGBA(255, 255, 255, 0.90)
		t.SurfacePressed = ui.RGBA(235, 232, 245, 1)
		t.Border = ui.RGBA(35, 31, 51, 0.10)
	}
	c.SetTheme(&t)
	return pal
}
