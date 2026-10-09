package gui

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/query"
)

// countingService counts Totals calls and can hold them until released.
type countingService struct {
	query.Service
	totals atomic.Int32
	gate   chan struct{}
}

func (c *countingService) Totals(ctx context.Context, f query.Filter) (query.Totals, error) {
	c.totals.Add(1)
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return query.Totals{}, ctx.Err()
		}
	}
	return c.Service.Totals(ctx, f)
}

// uiLoop runs posted functions on one goroutine, like the window's thread.
type uiLoop struct {
	ch chan func()
	wg sync.WaitGroup
}

func newLoop() *uiLoop {
	l := &uiLoop{ch: make(chan func(), 64)}
	go func() {
		for fn := range l.ch {
			fn()
			l.wg.Done()
		}
	}()
	return l
}

func (l *uiLoop) post(fn func()) { l.wg.Add(1); l.ch <- fn }

// do runs fn on the loop and waits for it.
func (l *uiLoop) do(fn func()) {
	done := make(chan struct{})
	l.post(func() { fn(); close(done) })
	<-done
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSpanSwitchUsesCache(t *testing.T) {
	now := time.Date(2026, 5, 20, 15, 0, 0, 0, time.Local)
	q := &countingService{Service: NewDemoService(now)}
	l := newLoop()
	s := NewState(q, Hooks{Now: func() time.Time { return now }}, l.post)
	l.do(s.Reload)
	waitFor(t, func() bool {
		var ok bool
		l.do(func() { ok = s.spans[Span7].ok && s.spans[SpanToday].ok && s.shared.Heat != nil })
		return ok
	})
	l.do(func() { s.span = int(Span30); s.setSpan() })
	waitFor(t, func() bool { var ok bool; l.do(func() { ok = s.ov.Span == Span30 }); return ok })
	before := q.totals.Load()
	// Back to 7 days: cached and current, so it shows at once with no query.
	l.do(func() {
		s.span = int(Span7)
		s.setSpan()
		if s.ov.Span != Span7 || s.ov.Heat == nil || s.today.Span != SpanToday {
			t.Errorf("not composed from cache: ov %v heat %d today %v", s.ov.Span, len(s.ov.Heat), s.today.Span)
		}
	})
	time.Sleep(50 * time.Millisecond)
	if n := q.totals.Load(); n != before {
		t.Errorf("cached span queried again: %d → %d Totals calls", before, n)
	}
	// New data makes the cache stale: switching then loads again.
	l.do(func() { s.gen++; s.span = int(Span30); s.setSpan() })
	waitFor(t, func() bool { return q.totals.Load() > before })
}

func TestRunCoalesces(t *testing.T) {
	l := newLoop()
	s := NewState(nil, Hooks{}, l.post)
	release := make(chan struct{})
	var runs atomic.Int32
	var applied atomic.Int32
	for i := 1; i <= 5; i++ {
		i := int32(i)
		l.do(func() {
			s.run("k", func(context.Context) func() {
				runs.Add(1)
				if i == 1 {
					<-release
				}
				return func() { applied.Store(i) }
			})
		})
	}
	close(release)
	waitFor(t, func() bool { return applied.Load() == 5 })
	if n := runs.Load(); n != 2 {
		t.Errorf("runs = %d, want 2 (the first and the latest)", n)
	}
}

func TestSwitchCancelsUnwantedSpan(t *testing.T) {
	now := time.Date(2026, 5, 20, 15, 0, 0, 0, time.Local)
	q := &countingService{Service: NewDemoService(now), gate: make(chan struct{})}
	l := newLoop()
	s := NewState(q, Hooks{Now: func() time.Time { return now }}, l.post)
	l.do(func() { s.span = int(Span90); s.setSpan() })
	waitFor(t, func() bool { return q.totals.Load() == 1 })
	l.do(func() { s.span = int(SpanAll); s.setSpan() })
	close(q.gate)
	waitFor(t, func() bool { var ok bool; l.do(func() { ok = s.spans[SpanAll].ok }); return ok })
	l.do(func() {
		if s.spans[Span90].ok {
			t.Error("the abandoned 90-day load was applied")
		}
		if s.ov.Span != SpanAll {
			t.Errorf("ov.Span = %v", s.ov.Span)
		}
	})
}
