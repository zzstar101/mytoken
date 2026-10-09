// Package app wires the backend and registered parsers.
package app

import (
	"context"
	"github.com/zzstar/mytoken/internal/attrib"
	_ "github.com/zzstar/mytoken/internal/harness/claude"
	_ "github.com/zzstar/mytoken/internal/harness/codex"
	_ "github.com/zzstar/mytoken/internal/harness/dsh"
	_ "github.com/zzstar/mytoken/internal/harness/pi"
	"github.com/zzstar/mytoken/internal/paths"
	"github.com/zzstar/mytoken/internal/pricing"
	"github.com/zzstar/mytoken/internal/query"
	"github.com/zzstar/mytoken/internal/scan"
	"github.com/zzstar/mytoken/internal/store"
	"sync"
	"time"
)

type App struct {
	cancel   context.CancelFunc
	workers  sync.WaitGroup
	Store    *store.Store
	Scanner  *scan.Scanner
	Query    query.Service
	Resolver *attrib.Resolver
	Pricing  *pricing.Pricer
}

func Open() (*App, error) {
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
	a.Scanner = scan.New(st, resolver, prices)
	a.Query = query.NewService(st)
	if e = st.RecomputeCosts(context.Background(), prices.Cost); e != nil {
		a.Close()
		return nil, e
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
			_ = st.RecomputeCosts(ctx, prices.Cost)
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
