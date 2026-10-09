package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/reconcile"
	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/source"
)

// newRelays builds the relay facade shared by the GUI and the CLI. Building
// it does no network I/O; only Enable, Sync and a started Run do, and only
// for sites the user turned on.
func newRelays(a *App) *reconcile.Service {
	home, _ := os.UserHomeDir()
	creds := relayCredentials(
		source.NewCCSwitch(filepath.Join(home, ".cc-switch", "cc-switch.db")),
		source.NewHarnessConfig(home),
	)
	return reconcile.NewService(a.Store, creds, relay.NewHTTP(nil), relayRules{a.Settings})
}

// credentialer is a source that can list the keys it holds.
type credentialer interface {
	Credentials(ctx context.Context) ([]source.Credential, error)
}

// relayCredentials merges the keys of every source, first source first on
// the same (origin, key). A source that fails is skipped unless all do.
func relayCredentials(srcs ...credentialer) func(context.Context) ([]source.Credential, error) {
	return func(ctx context.Context) ([]source.Credential, error) {
		var out []source.Credential
		var errs []error
		seen := map[[2]string]bool{}
		for _, s := range srcs {
			cs, err := s.Credentials(ctx)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, c := range cs {
				k := [2]string{c.Origin, c.KeyID}
				if seen[k] {
					continue
				}
				seen[k] = true
				out = append(out, c)
			}
		}
		if len(errs) == len(srcs) && len(errs) > 0 {
			return nil, errors.Join(errs...)
		}
		return out, nil
	}
}

// relayRules adapts the settings to reconcile.RulesAppender: the origin is
// already in each rule's Source ("relay:<origin>").
type relayRules struct{ s query.Settings }

func (r relayRules) AppendRelayRules(ctx context.Context, _ string, rules []pricing.Rule) error {
	return r.s.AppendRelayRules(ctx, rules)
}

// StartRelaySync keeps the sites turned on in sync until Close. The GUI
// calls it; one-shot CLI commands do not.
func (a *App) StartRelaySync() {
	if a.Relays == nil || a.relayCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.relayCancel = cancel
	a.workers.Add(1)
	go func() {
		defer a.workers.Done()
		a.Relays.Run(ctx)
	}()
}
