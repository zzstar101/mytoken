// Package scan incrementally indexes registered harness sources.
package scan

import (
	"context"
	"errors"
	"fmt"
	"github.com/fsnotify/fsnotify"
	"github.com/zzstar101/mytoken/internal/attrib"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/store"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

type Scanner struct {
	st          *store.Store
	resolver    *attrib.Resolver
	pricer      *pricing.Pricer
	parsers     []harness.Parser
	mu          sync.Mutex
	scanMu      sync.Mutex
	done, total int
	lastError   error
	workers     int
	interval    time.Duration
	debounce    time.Duration
}

func New(st *store.Store, resolver *attrib.Resolver, pricer *pricing.Pricer, parsers ...harness.Parser) *Scanner {
	if len(parsers) == 0 {
		parsers = harness.All()
	}
	workers := runtime.NumCPU()
	// Parsing many large JSONL files concurrently multiplies decoder buffers. A
	// bounded pool keeps peak RSS predictable while retaining parallel IO.
	if workers > 4 {
		workers = 4
	}
	return &Scanner{st: st, resolver: resolver, pricer: pricer, parsers: parsers, workers: workers, interval: 2 * time.Minute, debounce: 250 * time.Millisecond}
}
func (s *Scanner) Progress() (done, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done, s.total
}
func (s *Scanner) LastError() error                     { s.mu.Lock(); defer s.mu.Unlock(); return s.lastError }
func (s *Scanner) Subscribe() (<-chan struct{}, func()) { return s.st.Subscribe() }

type source struct {
	parser harness.Parser
	src    harness.Source
	cur    harness.Cursor
	size   int64
}

func (s *Scanner) Scan(ctx context.Context) (err error) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	defer func() {
		s.mu.Lock()
		s.lastError = err
		s.mu.Unlock()
	}()
	if s.resolver != nil {
		if e := s.resolver.Refresh(ctx); e != nil {
			return e
		}
	}
	restoreIndexes, err := s.st.DeferEmptyIndexes(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, restoreIndexes()) }()
	var work []source
	var errs []error
	for _, p := range s.parsers {
		srcs, e := p.Discover(ctx)
		if e != nil {
			errs = append(errs, fmt.Errorf("discover %s: %w", p.Harness(), e))
			continue
		}
		for _, src := range srcs {
			if err := ctx.Err(); err != nil {
				return err
			}
			cur, err := s.st.Cursor(ctx, p.Harness(), src.Path)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			var size int64
			if info, err := os.Stat(src.Path); err == nil {
				size = info.Size()
			}
			work = append(work, source{p, src, cur, size})
		}
	}
	// Start large logs first to avoid a long single-worker tail.
	sort.SliceStable(work, func(i, j int) bool { return work[i].size > work[j].size })
	s.mu.Lock()
	s.done = 0
	s.total = len(work)
	s.mu.Unlock()
	ch := make(chan source)
	type result struct {
		write *store.Write
		err   error
	}
	results := make(chan result, s.workers+1)
	var wg sync.WaitGroup
	n := s.workers
	if n < 1 {
		n = 1
	}
	if n > len(work) {
		n = len(work)
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range ch {
				if ctx.Err() != nil {
					return
				}
				write, err := s.scanOne(ctx, item)
				results <- result{write, err}
				s.mu.Lock()
				s.done++
				s.mu.Unlock()
			}
		}()
	}
	go func() {
	dispatch:
		for _, item := range work {
			select {
			case <-ctx.Done():
				break dispatch
			case ch <- item:
			}
		}
		close(ch)
		wg.Wait()
		close(results)
	}()
	var pending []store.Write
	eventCount := 0
	flush := func() {
		if len(pending) == 0 {
			return
		}
		if err := s.st.CommitMany(ctx, pending); err != nil {
			errs = append(errs, err)
		}
		pending = nil
		eventCount = 0
	}
	for r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
		}
		if r.write != nil {
			pending = append(pending, *r.write)
			eventCount += len(r.write.Batch.Events)
			if len(pending) >= 32 || eventCount >= 8192 {
				flush()
			}
		}
	}
	flush()
	if e := s.st.NormalizeProjects(ctx); e != nil {
		errs = append(errs, e)
	}
	if s.pricer != nil {
		if e := s.st.EnsurePrices(ctx, s.pricer.Fingerprint(), s.pricer.Evaluate); e != nil {
			errs = append(errs, e)
		}
	}
	if e := ctx.Err(); e != nil {
		errs = append(errs, e)
	}
	return errors.Join(errs...)
}
func (s *Scanner) scanOne(ctx context.Context, item source) (*store.Write, error) {
	h := item.parser.Harness()
	cur := item.cur
	b, e := item.parser.Parse(ctx, item.src, cur)
	if e != nil {
		return nil, fmt.Errorf("parse %s %s: %w", h, item.src.Path, e)
	}
	if len(b.Events) == 0 && len(b.Sessions) == 0 && b.Next == cur {
		return nil, nil
	}
	resolved := make([]store.Resolution, len(b.Events))
	for i, event := range b.Events {
		if event.Harness == "" {
			event.Harness = h
		}
		provider, kind := "", model.AttribInferred
		if s.resolver != nil {
			provider, kind = s.resolver.Resolve(ctx, event)
		} else if event.Provider != "" {
			provider, kind = event.Provider, model.AttribLog
		} else {
			provider = attrib.Infer(event.Model)
		}
		event.Provider = provider
		var cost float64
		priced := event.CostUSD != nil
		if s.pricer != nil {
			cost, priced = s.pricer.Evaluate(event)
		} else if event.CostUSD != nil {
			cost = *event.CostUSD
		}
		resolved[i] = store.Resolution{Provider: provider, Attrib: kind, Cost: cost, Priced: priced}
	}
	return &store.Write{Harness: h, Path: item.src.Path, Batch: b, Resolutions: resolved}, nil
}
func (s *Scanner) Rebuild(ctx context.Context) error {
	if e := s.st.Rebuild(ctx); e != nil {
		return e
	}
	return s.Scan(ctx)
}
func (s *Scanner) Run(ctx context.Context) error {
	watcher, e := fsnotify.NewWatcher()
	if e != nil {
		return e
	}
	defer watcher.Close()
	watched := map[string]bool{}
	var addRecursive func(string)
	addRecursive = func(path string) {
		if watched[path] {
			return
		}
		fi, e := os.Stat(path)
		if e != nil || !fi.IsDir() {
			return
		}
		if e = watcher.Add(path); e != nil {
			return
		}
		watched[path] = true
		entries, e := os.ReadDir(path)
		if e != nil {
			return
		}
		for _, entry := range entries {
			if entry.IsDir() {
				addRecursive(filepath.Join(path, entry.Name()))
			}
		}
	}
	watchRoots := func() {
		for _, p := range s.parsers {
			for _, root := range p.Roots() {
				addRecursive(root)
			}
		}
		if s.resolver != nil {
			if home, e := os.UserHomeDir(); e == nil {
				for _, name := range []string{".claude", ".codex"} {
					path := filepath.Join(home, name)
					if !watched[path] {
						if e := watcher.Add(path); e == nil {
							watched[path] = true
						}
					}
				}
			}
		}
	}
	watchRoots()
	if e = s.Scan(ctx); e != nil {
		return e
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	var timer *time.Timer
	var debounced <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if ev.Has(fsnotify.Create) {
				addRecursive(ev.Name)
			}
			if timer == nil {
				timer = time.NewTimer(s.debounce)
				debounced = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(s.debounce)
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
		case <-debounced:
			debounced = nil
			timer = nil
			_ = s.Scan(ctx)
		case <-ticker.C:
			watchRoots()
			_ = s.Scan(ctx)
		}
	}
}
