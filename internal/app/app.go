// Package app wires the backend and registered parsers.
package app

import (
	"context"
	"github.com/zzstar101/mytoken/internal/attrib"
	_ "github.com/zzstar101/mytoken/internal/harness/claude"
	_ "github.com/zzstar101/mytoken/internal/harness/cline"
	_ "github.com/zzstar101/mytoken/internal/harness/codex"
	_ "github.com/zzstar101/mytoken/internal/harness/crush"
	_ "github.com/zzstar101/mytoken/internal/harness/dsh"
	_ "github.com/zzstar101/mytoken/internal/harness/gemini"
	_ "github.com/zzstar101/mytoken/internal/harness/kilo"
	_ "github.com/zzstar101/mytoken/internal/harness/opencode"
	_ "github.com/zzstar101/mytoken/internal/harness/pi"
	_ "github.com/zzstar101/mytoken/internal/harness/roo"
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/scan"
	"github.com/zzstar101/mytoken/internal/store"
	"sync"
	"time"
)

type App struct {
	cancel   context.CancelFunc
	workers  sync.WaitGroup
	Store    *store.Store
	Scanner  *scan.Scanner
	Settings query.Settings
	Query    query.Service
	Resolver *attrib.Resolver
	Pricing  *pricing.Pricer
}

func Open() (*App, error) { return open(true) }

// OpenLocal uses the loaded offline prices without starting a network worker.
func OpenLocal() (*App, error) { return open(false) }

func open(refresh bool) (*App, error) {
	dir, e := paths.DataDir()
	if e != nil {
		return nil, e
	}
	db, e := paths.DBPath()
	if e != nil {
		return nil, e
	}
	st, e := store.Open(db)
	if e != nil {
		return nil, e
	}
	resolver, e := attrib.New(st)
	if e != nil {
		st.Close()
		return nil, e
	}
	prices := pricing.New(dir)
	a := &App{Store: st, Resolver: resolver, Pricing: prices}
	a.Settings = query.NewSettings(st, prices)
	if e = query.LoadPricingSettings(context.Background(), st, prices); e != nil {
		a.Close()
		return nil, e
	}
	a.Scanner = scan.New(st, resolver, prices)
	a.Query = query.NewService(st)
	if e = st.EnsurePrices(context.Background(), prices.Fingerprint(), prices.Evaluate); e != nil {
		a.Close()
		return nil, e
	}
	if !refresh {
		return a, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.workers.Add(1)
	go func() {
		defer a.workers.Done()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			// Refresh failure leaves the loaded offline table available.
			_ = prices.Refresh(ctx)
			if ctx.Err() != nil {
				return
			}
			_ = st.EnsurePrices(ctx, prices.Fingerprint(), prices.Evaluate)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return a, nil
}
func (a *App) Close() error {
	if a.cancel != nil {
		a.cancel()
		a.workers.Wait()
	}
	if a.Resolver != nil {
		_ = a.Resolver.Close()
	}
	if a.Store != nil {
		return a.Store.Close()
	}
	return nil
}
