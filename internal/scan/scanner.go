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
	"strings"
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
	// minGap is the least time from one watched-change scan to the next:
	// agents append to their logs all the time, and each scan stats every
	// source, so back-to-back scans would keep a core busy.
	minGap time.Duration
	// dirs holds the directories of the sources the last scan found, which
	// Run watches with the directories between them and their roots.
	dirs map[string]bool
	// watching counts the directories Run watches, for tests.
	watching int
}

// maxWatchDirs bounds the directories Run watches. On macOS each watched
// directory costs a descriptor for it and each file in it, and a harness
// home can hold a whole program (~/.hermes/hermes-agent: 150K files): the
// periodic scan covers whatever is past the bound.
const maxWatchDirs = 2048

// newDirBudget bounds how deep Run follows a directory created under a
// watched one, such as a new session folder, before the next scan.
const newDirBudget = 32

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
	return &Scanner{st: st, resolver: resolver, pricer: pricer, parsers: parsers, workers: workers, interval: 2 * time.Minute, debounce: 250 * time.Millisecond, minGap: 2 * time.Second}
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
	dirs := map[string]bool{}
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
				if info.IsDir() {
					dirs[src.Path] = true
				} else {
					dirs[filepath.Dir(src.Path)] = true
				}
			}
			work = append(work, source{p, src, cur, size})
		}
	}
	// Start large logs first to avoid a long single-worker tail.
	sort.SliceStable(work, func(i, j int) bool { return work[i].size > work[j].size })
	s.mu.Lock()
	s.done = 0
	s.total = len(work)
	s.dirs = dirs
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
	watch := func(path string) bool {
		if watched[path] {
			return true
		}
		if len(watched) >= maxWatchDirs {
			return false
		}
		if fi, e := os.Stat(path); e != nil || !fi.IsDir() {
			return false
		}
		if watcher.Add(path) != nil {
			return false
		}
		watched[path] = true
		return true
	}
	// watchNew follows a directory created under a watched one a few
	// levels down: a new session folder, not a program being installed.
	watchNew := func(path string) {
		budget := newDirBudget
		var walk func(string)
		walk = func(dir string) {
			if budget <= 0 || !watch(dir) {
				return
			}
			budget--
			entries, e := os.ReadDir(dir)
			if e != nil {
				return
			}
			for _, entry := range entries {
				if entry.IsDir() {
					walk(filepath.Join(dir, entry.Name()))
				}
			}
		}
		walk(path)
	}
	// watchRoots watches each root, and the directories from it down to
	// each source found: never a whole root, which may hold anything.
	watchRoots := func() {
		var roots []string
		for _, p := range s.parsers {
			roots = append(roots, p.Roots()...)
		}
		s.mu.Lock()
		dirs := make([]string, 0, len(s.dirs))
		for dir := range s.dirs {
			dirs = append(dirs, dir)
		}
		s.mu.Unlock()
		for _, dir := range watchDirs(roots, dirs) {
			watch(dir)
		}
		defer func() {
			s.mu.Lock()
			s.watching = len(watched)
			s.mu.Unlock()
		}()
		if s.resolver != nil {
			if home, e := os.UserHomeDir(); e == nil {
				for _, name := range []string{".claude", ".codex"} {
					watch(filepath.Join(home, name))
				}
			}
		}
	}
	watchRoots()
	// A source that fails to parse must not stop watching the others.
	if s.Scan(ctx) != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	watchRoots()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	last := time.Now()
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
				watchNew(ev.Name)
			}
			// The first change arms the timer and later ones ride on it:
			// re-arming on each would never scan while an agent writes on.
			if timer == nil {
				timer = time.NewTimer(s.debounce)
				debounced = timer.C
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
		case <-debounced:
			if wait := s.minGap - time.Since(last); wait > 0 {
				timer.Reset(wait)
				continue
			}
			debounced = nil
			timer = nil
			_ = s.Scan(ctx)
			last = time.Now()
		case <-ticker.C:
			_ = s.Scan(ctx)
			watchRoots()
			last = time.Now()
		}
	}
}

// watchDirs returns the directories to watch, roots first: each root, and
// for each source directory the directories from it up to its root.
func watchDirs(roots, dirs []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(dir string) {
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	clean := make([]string, 0, len(roots))
	for _, root := range roots {
		if root != "" {
			root = filepath.Clean(root)
			clean = append(clean, root)
			add(root)
		}
	}
	dirs = append([]string(nil), dirs...)
	sort.Strings(dirs)
	for _, dir := range dirs {
		dir = filepath.Clean(dir)
		var chain []string
		for _, root := range clean {
			rel, e := filepath.Rel(root, dir)
			if e != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			for d := filepath.Dir(dir); len(d) > len(root); d = filepath.Dir(d) {
				chain = append(chain, d)
			}
		}
		for i := len(chain) - 1; i >= 0; i-- {
			add(chain[i])
		}
		add(dir)
	}
	return out
}
