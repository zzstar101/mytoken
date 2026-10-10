package gui

import (
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// TestSnapshots renders every page with demo data to MYTOKEN_SNAPSHOTS
// (skipped when unset), for visual review.
func TestSnapshots(t *testing.T) {
	dir := os.Getenv("MYTOKEN_SNAPSHOTS")
	if dir == "" {
		t.Skip("set MYTOKEN_SNAPSHOTS=dir to render")
	}
	os.MkdirAll(dir, 0o755)
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	for _, lang := range []string{"zh", "en"} {
		SetLang(lang)
		for _, dark := range []bool{false, true} {
			s := NewState(NewDemoService(now), Hooks{Settings: NewDemoSettings(), Relays: NewDemoRelays(now), Now: func() time.Time { return now }, DataDir: "~/Library/Application Support/MyToken",
				Progress: func() (int, int) { return 10, 10 }}, nil)
			s.Start()
			rows := s.sessions.Rows
			for _, page := range []string{"overview", "sessions", "ranking", "projects", "relays", "settings"} {
				s.page = page
				if page == "sessions" && len(rows) > 0 {
					for _, r := range rows {
						if r.Children > 0 {
							s.Select(r.Harness, r.SessionID)
							break
						}
					}
				}
				name := lang + "-" + page
				if dark {
					name += "-dark"
				}
				if only := os.Getenv("MYTOKEN_SNAP_ONLY"); only != "" && !strings.Contains(name, only) {
					continue
				}
				shot(t, filepath.Join(dir, name+".png"), s.MainView, 1280, 860, dark)
			}
			name := lang + "-tray"
			if dark {
				name += "-dark"
			}
			if only := os.Getenv("MYTOKEN_SNAP_ONLY"); only != "" && !strings.Contains(name, only) {
				continue
			}
			shot(t, filepath.Join(dir, name+".png"), s.TrayView, TrayWidth, TrayHeight, dark)
		}
		// Empty states: a fresh install, and a search that finds nothing.
		empty := NewState(&demo{events: map[string][]query.AttributedEvent{}}, Hooks{Settings: NewDemoSettings(), Relays: &demoRelays{now: now}, Now: func() time.Time { return now }, DataDir: "~/Library/Application Support/MyToken",
			Progress: func() (int, int) { return 10, 10 }}, nil)
		empty.Start()
		views := []struct {
			name string
			prep func()
			view func(*ui.Context)
			w, h int
		}{
			{"empty-overview", func() { empty.page = "overview" }, empty.MainView, 1280, 860},
			{"empty-ranking", func() { empty.page = "ranking" }, empty.MainView, 1280, 860},
			{"empty-sessions", func() { empty.page = "sessions" }, empty.MainView, 1280, 860},
			{"empty-relays", func() { empty.page = "relays" }, empty.MainView, 1280, 860},
			{"empty-search", func() { empty.page = "sessions"; empty.search = "春日影" }, empty.MainView, 1280, 860},
			{"empty-tray", func() {}, empty.TrayView, TrayWidth, TrayHeight},
		}
		for _, v := range views {
			name := lang + "-" + v.name
			if only := os.Getenv("MYTOKEN_SNAP_ONLY"); only != "" && !strings.Contains(name, only) {
				continue
			}
			v.prep()
			shot(t, filepath.Join(dir, name+".png"), v.view, v.w, v.h, false)
		}
	}
	_ = model.ClaudeCode
}

func shot(t *testing.T, path string, view func(*ui.Context), w, h int, dark bool) {
	tt := ui.NewTester(view, w, h)
	tt.SetDark(dark)
	scale := float32(2)
	if v, err := strconv.ParseFloat(os.Getenv("MYTOKEN_SNAP_SCALE"), 32); err == nil {
		scale = float32(v)
	}
	tt.SetScale(scale)
	if w > 600 {
		tt.SetTitleBar(ui.TitleBar{Height: 66, Left: 82})
	}
	// Skip entry animations: snapshots show the settled state.
	tt.SetPreferences(ui.Preferences{ReduceMotion: true})
	for i := 0; i < 30; i++ {
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

// TestIcon writes the app icon: MYTOKEN_ICON=resources/icon.png.
func TestIcon(t *testing.T) {
	path := os.Getenv("MYTOKEN_ICON")
	if path == "" {
		t.Skip("set MYTOKEN_ICON to write the icon")
	}
	img := ui.Render(IconView, 512, 512, 2)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}
