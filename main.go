// Command mytoken is MyToken!!!!!: a tray-resident tracker of the tokens AI
// coding harnesses spend, per session × provider × model.
//
//	mytoken                 run the app (tray + main window)
//	mytoken stats [--json]  print usage without a window
//	mytoken scan            index the logs once
//	mytoken help            list every command (see docs/CLI.md)
package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/cli"
	"github.com/zzstar101/mytoken/internal/gui"
	"github.com/zzstar101/mytoken/internal/paths"
)

func main() {
	// `mygo build` cannot pass -ldflags, but it embeds mygo.json's version.
	if cli.Version == "dev" {
		if v := mygo.App.Version(); v != "" {
			cli.Version = v
		}
	}
	if len(os.Args) > 1 && os.Args[1] != "--hidden" {
		os.Exit(cli.Run(os.Args[1:]))
	}
	a, err := app.Open()
	if err != nil {
		log.Fatalf("mytoken: %v", err)
	}
	defer a.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := a.Scanner.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("mytoken: scanner: %v", err)
		}
	}()

	dataDir, _ := paths.DataDir()
	var main, panel *mygo.Window
	var state *gui.State

	// Both windows show the same state; a change redraws both.
	post := func(fn func()) {
		if main == nil {
			return
		}
		main.Update(fn)
		if panel != nil {
			panel.Update(func() {})
		}
	}
	openMain := func() {
		if panel != nil {
			panel.Hide()
		}
		main.Show()
		main.Focus()
	}
	hooks := gui.Hooks{
		Progress: a.Scanner.Progress,
		Rebuild: func() {
			if err := a.Scanner.Rebuild(ctx); err != nil {
				log.Printf("mytoken: rebuild: %v", err)
			}
		},
		OpenAtLogin:    mygo.App.OpenAtLogin,
		SetOpenAtLogin: func(on bool) { _ = mygo.App.SetOpenAtLogin(on) },
		DataDir:        dataDir,
		Settings:       a.Settings,
		OpenMain:       func() { main.Update(openMain) },
		Quit:           func() { mygo.App.Quit() },
	}
	if l := mygo.App.Locale(); os.Getenv("MYTOKEN_LANG") == "" && l != "" {
		if strings.HasPrefix(strings.ToLower(l), "zh") {
			gui.SetLang("zh")
		} else {
			gui.SetLang("en")
		}
	}

	mygo.App.WhenReady(func() {
		hidden := mygo.App.WasOpenedAtLogin() || len(os.Args) > 1
		main = mygo.NewWindow(mygo.WindowOptions{
			Title:         "MyToken!!!!!",
			Width:         1280,
			Height:        860,
			MinWidth:      980,
			MinHeight:     640,
			Hidden:        hidden,
			TitleBarStyle: mygo.TitleBarHidden,
			StateKey:      "main",
			Content:       ui.View(func(c *ui.Context) { state.MainView(c) }),
		})
		// Closing the main window keeps the app in the tray.
		main.OnClose(func(e *mygo.CloseEvent) {
			e.PreventDefault()
			main.Hide()
		})

		panel = mygo.NewWindow(mygo.WindowOptions{
			Title:           "MyToken!!!!!",
			Width:           gui.TrayWidth,
			Height:          gui.TrayHeight,
			Hidden:          true,
			Frameless:       true,
			DisableResize:   true,
			DisableMinimize: true,
			DisableMaximize: true,
			AlwaysOnTop:     true,
			SkipTaskbar:     true,
			Transparent:     true,
			Vibrancy:        mygo.VibrancyMenu,
			Content:         ui.View(func(c *ui.Context) { state.TrayView(c) }),
		})
		panel.OnBlur(func() { panel.Hide() })

		state = gui.NewState(a.Query, hooks, post)
		state.Start()

		tray, err := mygo.NewTray(mygo.TrayOptions{Icon: trayIcon(), IconIsTemplate: true, ToolTip: "MyToken!!!!!"})
		if err != nil {
			// No tray (e.g. Linux without appindicator): the window is the app.
			log.Printf("mytoken: tray: %v", err)
			main.Show()
			return
		}
		mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory)
		tray.OnClick(func() {
			if panel.IsVisible() {
				panel.Hide()
				return
			}
			b := tray.Bounds()
			x := b.X + b.Width/2 - gui.TrayWidth/2
			y := b.Y + b.Height + 6
			if b.Y > 300 { // taskbar at the bottom (Windows): open upwards
				y = b.Y - gui.TrayHeight - 6
			}
			panel.SetPosition(x, y)
			panel.Show()
			panel.Focus()
		})
		mygo.App.OnActivate(func(hasVisible bool) {
			if !hasVisible {
				openMain()
			}
		})
		// An accessory app isn't brought forward on launch: do it ourselves.
		if !hidden {
			openMain()
		}
	})
	if err := mygo.App.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "mytoken:", err)
		os.Exit(1)
	}
}

// trayIcon draws the template icon: three rising bars, like a tiny chart.
func trayIcon() []byte {
	const n = 32
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	bar := func(x0, x1, top int) {
		for y := top; y < 28; y++ {
			for x := x0; x < x1; x++ {
				// Round the top corners a little.
				if y == top && (x == x0 || x == x1-1) {
					continue
				}
				img.Set(x, y, color.NRGBA{A: 255})
			}
		}
	}
	bar(4, 10, 17)
	bar(13, 19, 10)
	bar(22, 28, 4)
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
