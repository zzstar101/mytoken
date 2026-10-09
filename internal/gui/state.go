package gui

import (
	"context"
	"sync"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// Hooks are what the GUI asks of the rest of the app; any may be nil.
type Hooks struct {
	// Progress reports the scan: sources done and total.
	Progress func() (done, total int)
	// Rebuild drops the index and rescans.
	Rebuild func()
	// OpenAtLogin reads and sets launch at login.
	OpenAtLogin    func() bool
	SetOpenAtLogin func(bool)
	// DataDir is shown in the settings.
	DataDir string
	// OpenMain shows the main window; Quit quits (the tray panel's buttons).
	OpenMain func()
	Quit     func()
	// Settings edits pricing; nil hides the pricing cards.
	Settings query.Settings
	// Now is the clock (tests pin it).
	Now func() time.Time
}

// State is the GUI's state, shared by the main window and the tray panel.
// Its fields are only touched on the UI thread, through post.
type State struct {
	Q     query.Service
	Hooks Hooks
	// post runs fn on the UI thread and redraws (win.Update); sync loads
	// data inline, for tests and snapshots.
	post func(fn func())
	sync bool

	page string
	span int

	ov       Overview
	ovLoaded bool
	today    Overview // the tray panel's: always SpanToday
	err      error

	sessions   SessionPage
	sortIdx    int
	search     string
	list       ui.ListState
	sel        string // harness/id
	detail     SessionDetail
	openKids   bool
	detailBusy bool

	rankTab int
	pr      pricingState

	mu      sync.Mutex
	loading map[string]bool
	cancel  func()
}

// NewState makes the GUI state over q. post runs a function on the UI
// thread (mygo's Window.Update); nil means run inline and load synchronously.
func NewState(q query.Service, hooks Hooks, post func(func())) *State {
	s := &State{Q: q, Hooks: hooks, post: post, page: "overview", span: int(Span7), loading: map[string]bool{}}
	if s.Hooks.Now == nil {
		s.Hooks.Now = time.Now
	}
	if post == nil {
		s.sync = true
		s.post = func(fn func()) { fn() }
	}
	return s
}

// Start loads everything and reloads whenever new data is committed.
func (s *State) Start() {
	s.Reload()
	if s.sync {
		return
	}
	ch, cancel := s.Q.Subscribe()
	s.cancel = cancel
	go func() {
		for range ch {
			s.post(func() { s.Reload() })
		}
	}()
	// The day turns over, and the tray's last-24h moves on: refresh each minute.
	go func() {
		for range time.Tick(time.Minute) {
			s.post(func() { s.Reload() })
		}
	}()
}

// Stop stops listening for changes.
func (s *State) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

// Reload reloads the data of every view.
func (s *State) Reload() {
	s.loadOverview()
	s.loadToday()
	s.loadSessions()
	if s.sel != "" {
		s.loadDetail(s.detail.Row.Harness, s.detail.Row.SessionID)
	}
	if s.Hooks.Settings != nil {
		s.loadPricing()
	}
}

// run runs load off the UI thread (or inline) and applies its result. Loads
// of one kind do not overlap: a newer one waits its turn by being dropped
// and the next notification catches up.
func (s *State) run(kind string, load func(ctx context.Context) func()) {
	if s.sync {
		apply := load(context.Background())
		apply()
		return
	}
	s.mu.Lock()
	if s.loading[kind] {
		s.mu.Unlock()
		// Try again shortly so the latest choice wins.
		time.AfterFunc(150*time.Millisecond, func() { s.post(func() { s.run(kind, load) }) })
		return
	}
	s.loading[kind] = true
	s.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		apply := load(ctx)
		s.mu.Lock()
		s.loading[kind] = false
		s.mu.Unlock()
		s.post(apply)
	}()
}

func (s *State) loadOverview() {
	span := Span(s.span)
	now := s.Hooks.Now()
	s.run("overview", func(ctx context.Context) func() {
		ov, err := loadOverview(ctx, s.Q, span, now)
		return func() {
			if Span(s.span) == span {
				s.ov, s.err, s.ovLoaded = ov, err, true
			}
		}
	})
}

func (s *State) loadToday() {
	now := s.Hooks.Now()
	s.run("today", func(ctx context.Context) func() {
		ov, _ := loadOverview(ctx, s.Q, SpanToday, now)
		return func() { s.today = ov }
	})
}

var sortKeys = []string{query.SortRecent, query.SortTokens, query.SortCost}

func (s *State) loadSessions() {
	key := sortKeys[s.sortIdx]
	s.run("sessions", func(ctx context.Context) func() {
		page, err := loadSessions(ctx, s.Q, key)
		return func() {
			if err == nil && sortKeys[s.sortIdx] == key {
				s.sessions = page
			}
		}
	})
}

func (s *State) loadDetail(h model.Harness, id string) {
	key := string(h) + "/" + id
	s.detailBusy = true
	s.run("detail", func(ctx context.Context) func() {
		d, err := loadSession(ctx, s.Q, h, id)
		return func() {
			if s.sel == key {
				s.detailBusy = false
				if err == nil {
					s.detail = d
				}
			}
		}
	})
}

// Select opens a session in the sessions page.
func (s *State) Select(h model.Harness, id string) {
	key := string(h) + "/" + id
	if s.sel == key {
		return
	}
	s.sel = key
	s.openKids = false
	s.loadDetail(h, id)
}

// SetPage switches the main window's page ("overview", "sessions", …).
func (s *State) SetPage(p string) { s.page = p }
