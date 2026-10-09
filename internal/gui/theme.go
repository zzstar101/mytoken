// Package gui is MyToken!!!!!'s native interface: the main window and the
// tray panel, drawn with MyGo's ui package (no WebView).
//
// Look: Liquid Glass panes floating over a soft "aurora" painted in the five
// MyGO!!!!! member colors, which double as the chart palette.
package gui

import (
	"embed"
	"sync"

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
	case model.OpenCode:
		return ui.Hex("#5F54B3")
	case model.Crush:
		return ui.Hex("#E25C79")
	case model.Roo:
		return ui.Hex("#E0577A")
	case model.Kilo:
		return ui.Hex("#D99A2B")
	}
	return Anon
}

// Harness marks are the tools' own logos (from the MIT-licensed
// @lobehub/icons set, see brand/README.md). Colored logos are shown in their
// colors; one-color ones take the harness color. Crush has no square mark,
// so it gets a drawn heart in the app's line style.
//
//go:embed brand/*.svg
var brandFS embed.FS

type brandMark struct {
	svg     *ui.SVG
	colored bool
	tile    bool // the logo is its own rounded tile
}

var harnessIcons = func() map[model.Harness]brandMark {
	load := func(name string) *ui.SVG {
		b, err := brandFS.ReadFile("brand/" + name + ".svg")
		if err != nil {
			panic(err)
		}
		return ui.MustParseSVG(b)
	}
	return map[model.Harness]brandMark{
		model.ClaudeCode: {load("claudecode-color"), true, false},
		model.Codex:      {load("openai"), false, false},
		model.Gemini:     {load("gemini-color"), true, false},
		model.DSH:        {load("deepseek-color"), true, false},
		model.OpenCode:   {load("opencode"), false, false},
		model.Cline:      {load("cline"), false, false},
		model.Roo:        {load("roocode"), false, false},
		model.Kilo:       {load("kilocode"), false, false},
		model.Pi:         {load("pi"), false, false},
		model.Crush:      {icon(`<path d="M12 19.5s-7.5-4.4-7.5-10A4.2 4.2 0 0 1 12 7a4.2 4.2 0 0 1 7.5 2.5c0 5.6-7.5 10-7.5 10z"/>`), false, false},
	}
}()

// harnessMark draws h's logo at size points, or a dot for an unknown harness.
func harnessMark(c *ui.Context, h model.Harness, size float32) ui.Element {
	m, ok := harnessIcons[h]
	switch {
	case !ok:
		return dot(c, harnessColor(h), size*0.6)
	case m.colored:
		return ui.Image(c, m.svg).Size(size, size).Shrink(0)
	}
	return ui.Icon(c, m.svg).FontSize(size).TextColor(harnessColor(h)).Shrink(0)
}

// harnessBadge is harnessMark on a tinted rounded square, for list rows;
// logos that are tiles already fill the square themselves.
func harnessBadge(c *ui.Context, h model.Harness, box float32) ui.Element {
	if m := harnessIcons[h]; m.tile {
		return ui.Image(c, m.svg).Size(box, box).Shrink(0)
	}
	return ui.Box(c).Size(box, box).Radius(box * 0.3).Center().Shrink(0).Background(harnessColor(h).Alpha(0.16)).Children(func() {
		harnessMark(c, h, box*0.56)
	})
}

// serif is the display face for big figures and page titles: Instrument
// Serif (SIL OFL, see fonts/OFL.txt), embedded so it looks the same anywhere.
const serif = "Instrument Serif"

//go:embed fonts/*.ttf
var fontFS embed.FS

var fontsOnce sync.Once

// loadFonts registers the embedded faces once. A failure only means the
// system serif stands in.
func loadFonts() {
	fontsOnce.Do(func() {
		for _, name := range []string{"InstrumentSerif-Regular.ttf", "InstrumentSerif-Italic.ttf"} {
			if b, err := fontFS.ReadFile("fonts/" + name); err == nil {
				_ = ui.RegisterFont(b, serif)
			}
		}
	})
}

// palette is one appearance's colors beyond the ui.Theme.
type palette struct {
	dark bool
	// base is painted under the stage lights; ink and muted color text;
	// faint colors hairlines and chart grids; well fills recessed tracks.
	base, ink, muted, faint, well ui.Color
	// pane tints the glass so that text on it stays legible; edge is the
	// glass's lit rim and lift the shadow it casts.
	pane, edge, lift ui.Color
	hover            ui.Color
	// raised is the solid face of a selected pill or row.
	raised ui.Color
}

func paletteFor(dark bool) palette {
	if dark {
		return palette{
			dark:   true,
			base:   ui.Hex("#0B0A10"),
			ink:    ui.Hex("#F4F2F8"),
			muted:  ui.RGBA(240, 236, 250, 0.54),
			faint:  ui.RGBA(255, 255, 255, 0.075),
			well:   ui.RGBA(255, 255, 255, 0.055),
			pane:   ui.RGBA(24, 22, 33, 0.52),
			edge:   ui.RGBA(255, 255, 255, 0.085),
			lift:   ui.RGBA(0, 0, 0, 0.38),
			hover:  ui.RGBA(255, 255, 255, 0.055),
			raised: ui.RGBA(255, 255, 255, 0.10),
		}
	}
	return palette{
		base:   ui.Hex("#F2EFE9"),
		ink:    ui.Hex("#17161C"),
		muted:  ui.RGBA(23, 22, 28, 0.55),
		faint:  ui.RGBA(23, 22, 28, 0.075),
		well:   ui.RGBA(23, 22, 28, 0.05),
		pane:   ui.RGBA(255, 255, 255, 0.56),
		edge:   ui.RGBA(255, 255, 255, 0.80),
		lift:   ui.RGBA(60, 45, 30, 0.08),
		hover:  ui.RGBA(23, 22, 28, 0.04),
		raised: ui.RGBA(255, 255, 255, 0.92),
	}
}

// applyTheme sets MyToken's theme on c and returns its palette.
func applyTheme(c *ui.Context) palette {
	loadFonts()
	base := c.Theme()
	pal := paletteFor(base.Dark)
	t := *base
	accent := Tomori.Mix(ui.RGB(0, 0, 0), 0.08)
	t.Accent = accent
	t.AccentHover = accent.Mix(ui.RGB(255, 255, 255), 0.12)
	t.AccentPressed = accent.Mix(ui.RGB(0, 0, 0), 0.12)
	t.AccentText = ui.RGB(255, 255, 255)
	t.Text = pal.ink
	t.TextMuted = pal.muted
	t.Background = pal.base
	t.Radius = 10
	t.Font = "SF Pro Text, system-ui"
	if pal.dark {
		t.Surface = ui.RGBA(255, 255, 255, 0.07)
		t.SurfaceHover = ui.RGBA(255, 255, 255, 0.11)
		t.SurfacePressed = ui.RGBA(255, 255, 255, 0.15)
		t.Border = ui.RGBA(255, 255, 255, 0.10)
	} else {
		t.Surface = ui.RGBA(255, 255, 255, 0.72)
		t.SurfaceHover = ui.RGBA(255, 255, 255, 0.92)
		t.SurfacePressed = ui.RGBA(236, 232, 226, 1)
		t.Border = ui.RGBA(23, 22, 28, 0.10)
	}
	c.SetTheme(&t)
	return pal
}
