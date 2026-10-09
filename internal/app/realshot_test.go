package app

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/zzstar/mytoken/internal/gui"
)

// TestRealShots renders the GUI over the real index at MYTOKEN_HOME, so the
// screens can be reviewed on this machine's data without a window:
//
//	MYTOKEN_HOME=/tmp/mt-home MYTOKEN_REALSHOTS=/tmp/mt-real go test ./internal/app -run RealShots
func TestRealShots(t *testing.T) {
	dir := os.Getenv("MYTOKEN_REALSHOTS")
	if dir == "" {
		t.Skip("set MYTOKEN_REALSHOTS=dir")
	}
	a, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	os.MkdirAll(dir, 0o755)
	gui.SetLang("zh")
	s := gui.NewState(a.Query, gui.Hooks{Progress: a.Scanner.Progress}, nil)
	s.Start()
	for _, page := range []string{"overview", "sessions", "ranking", "projects", "settings"} {
		s.SetPage(page)
		shot(t, filepath.Join(dir, page+".png"), s.MainView, 1280, 860, true)
	}
	shot(t, filepath.Join(dir, "tray.png"), s.TrayView, gui.TrayWidth, gui.TrayHeight, false)
}

func shot(t *testing.T, path string, view func(*ui.Context), w, h int, main bool) {
	tt := ui.NewTester(view, w, h)
	if main {
		tt.SetTitleBar(ui.TitleBar{Height: 28, Left: 78})
	}
	tt.SetPreferences(ui.Preferences{ReduceMotion: true})
	for i := 0; i < 20; i++ {
		tt.Frame()
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, tt.Image()); err != nil {
		t.Fatal(err)
	}
}
