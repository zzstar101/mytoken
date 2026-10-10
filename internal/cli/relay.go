package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/reconcile"
	"github.com/zzstar101/mytoken/internal/relay"
)

// This file implements `mytoken relay list|enable|disable|sync` and
// `mytoken reconcile` (docs/RELAY.md §6).
//
// Every relay command goes through the reconciliation facade
// (internal/reconcile.Service: Sites / Enable / Disable / Sync / Report) — the
// CLI never stitches store and relay calls together itself, and the GUI uses the
// same facade. app.App owns the instance (internal/app/relays.go); the CLI only
// borrows it. relayService below is the narrow slice of that facade these
// commands use, so relay_test.go can drive them with a fake.
//
// Privacy rules that hold for every path below: a key is never printed, logged
// or serialized. The CLI only ever sees source.KeyID (a truncated SHA-256) and
// values the facade already redacted; errors are printed with %v, which keeps
// source.Secret's [redacted] marker.
//
// `relay list` and `reconcile` never touch the network; only `enable` (to
// identify the site) and `sync` do, and both refuse to run under --offline.

// errRelayUnwired is reported when the reconciliation facade is not wired into
// this build yet.
var errRelayUnwired = errors.New("relay data is not available yet: the reconciliation service is not wired into this build")

func init() {
	// The relay transport identifies itself with the same version string as the
	// CLI (internal/relay/http.go sets User-Agent).
	relay.Version = Version
}

// relayService is the slice of internal/reconcile.Service the relay commands
// use. Enable, Disable, Sync and Report all take a concrete origin and key ID;
// the CLI resolves an <origin|provider> target into one via selectSites.
type relayService interface {
	Sites(ctx context.Context) ([]reconcile.SiteStatus, error)
	Enable(ctx context.Context, origin, keyID string, layers relay.Layer) (reconcile.SiteStatus, error)
	Disable(ctx context.Context, origin, keyID string) error
	Sync(ctx context.Context, origin, keyID string) error
	Report(ctx context.Context, origin, keyID string, from, to time.Time) (reconcile.Report, error)
}

// relayEnv is everything the relay commands touch, injected so they can be
// tested without a store, a network or credentials.
type relayEnv struct {
	Now     func() time.Time
	Service relayService
}

// openRelayEnv opens the index and binds the reconciliation facade to it. Relay
// commands never need a price refresh, so they always use app.OpenLocal: the
// only network calls they can make are the ones `enable` and `sync` issue to the
// site the user turned on (docs/RELAY.md §2).
func openRelayEnv() (*relayEnv, func() error, error) {
	a, e := app.OpenLocal()
	if e != nil {
		return nil, nil, e
	}
	env := &relayEnv{Now: time.Now}
	bindRelayService(env, a)
	return env, a.Close, nil
}

// bindRelayService wires env.Service to the facade app.App already built
// (internal/app/relays.go). Constructing that facade performs no I/O, so
// `relay list` and `reconcile` stay offline; the credential sources and the
// price-rule appender live in the app layer, not here.
func bindRelayService(env *relayEnv, a *app.App) {
	if a == nil || a.Relays == nil {
		return
	}
	env.Service = a.Relays
}

func (e *relayEnv) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}
	return e.Now()
}

func (e *relayEnv) service() (relayService, error) {
	if e.Service == nil {
		return nil, errRelayUnwired
	}
	return e.Service, nil
}

// --- command dispatch ------------------------------------------------------

func runRelay(ctx context.Context, args []string, out, errout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errout, relayUsage)
		return 2
	}
	switch args[0] {
	case "list":
		return relayList(ctx, args[1:], out, errout)
	case "enable":
		return relayEnable(ctx, args[1:], out, errout)
	case "disable":
		return relayDisable(ctx, args[1:], out, errout)
	case "sync":
		return relaySync(ctx, args[1:], out, errout)
	}
	fmt.Fprintln(errout, "unknown relay subcommand:", args[0], "(want list, enable, disable or sync)")
	return 2
}

func runReconcile(ctx context.Context, args []string, out, errout io.Writer) int {
	req, code := parseReconcile(args, errout)
	if code != 0 {
		return code
	}
	env, closeEnv, e := openRelayEnv()
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	defer func() { _ = closeEnv() }()
	return reconcileRun(ctx, req, env, out, errout)
}

// --- relay list ------------------------------------------------------------

type relayListReq struct{ format outputFormat }

func parseRelayList(args []string, errout io.Writer) (relayListReq, int) {
	var req relayListReq
	fs := flag.NewFlagSet("relay list", flag.ContinueOnError)
	fs.SetOutput(errout)
	var of outputFlags
	addOutputFlags(fs, &of)
	// relay list never uses the network; the flag is accepted (and redundant)
	// so scripts can pass --offline unconditionally.
	fs.Bool("offline", false, "always on: relay list never uses the network")
	flags, positional, e := splitArgs(args, map[string]bool{"offline": true, "json": true})
	if e != nil {
		fmt.Fprintln(errout, e)
		return req, 2
	}
	if e := of.parseFlags(fs, flags); e != nil {
		return req, 2
	}
	if len(positional) != 0 {
		fmt.Fprint(errout, relayListUsage)
		return req, 2
	}
	format, e := of.resolve()
	if e != nil {
		fmt.Fprintln(errout, e)
		return req, 2
	}
	req.format = format
	return req, 0
}

func relayList(ctx context.Context, args []string, out, errout io.Writer) int {
	req, code := parseRelayList(args, errout)
	if code != 0 {
		return code
	}
	env, closeEnv, e := openRelayEnv()
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	defer func() { _ = closeEnv() }()
	return relayListRun(ctx, req, env, out, errout)
}

func relayListRun(ctx context.Context, req relayListReq, env *relayEnv, out, errout io.Writer) int {
	rows, e := relayRows(ctx, env)
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	if e = writeRelayList(out, req.format, rows); e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	return 0
}

// relayRows asks the service for every site it can see and turns that into the
// table/JSON/CSV view. Nothing here touches the network.
func relayRows(ctx context.Context, env *relayEnv) ([]relaySiteRow, error) {
	svc, e := env.service()
	if e != nil {
		return nil, e
	}
	statuses, e := svc.Sites(ctx)
	if e != nil {
		return nil, e
	}
	rows := make([]relaySiteRow, 0, len(statuses))
	for _, st := range statuses {
		rows = append(rows, siteRowFrom(st))
	}
	// The service sorts too, but the CLI must not depend on that: table, JSON
	// and CSV output has to be reproducible.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Origin != rows[j].Origin {
			return rows[i].Origin < rows[j].Origin
		}
		return rows[i].KeyID < rows[j].KeyID
	})
	return rows, nil
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// matchesTarget reports whether a site answers to the user's <origin|provider>
// argument: the origin itself (case-insensitive, trailing slash ignored), or one
// of the providers its credentials were found under, or the bare host. An empty target means
// every site.
func matchesTarget(st reconcile.SiteStatus, target string) bool {
	if target == "" {
		return true
	}
	t := normalizeTarget(target)
	if strings.EqualFold(normalizeTarget(st.Site.Origin), t) {
		return true
	}
	// A bare host ("api.example.com" or "api.example.com:3000") names its site.
	if u, err := url.Parse(st.Site.Origin); err == nil && u.Host != "" && strings.EqualFold(u.Host, t) {
		return true
	}
	for _, p := range st.Providers {
		if strings.EqualFold(strings.TrimSpace(p), t) {
			return true
		}
	}
	for _, p := range st.Site.Providers {
		_, name := relay.SplitProvider(p)
		if strings.EqualFold(strings.TrimSpace(p), t) || strings.EqualFold(strings.TrimSpace(name), t) {
			return true
		}
	}
	return false
}

func normalizeTarget(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

// ambiguousTarget refuses to guess when a target names several sites or keys:
// it lists the candidates (origins and key IDs only, never keys) so the user can
// pick one with --key-id.
func ambiguousTarget(errout io.Writer, target string, sites []reconcile.SiteStatus) int {
	fmt.Fprintf(errout, "%d relay sites match %q; pass --key-id to pick one:\n", len(sites), target)
	for _, st := range sites {
		fmt.Fprintf(errout, "  %s  key %s\n", st.Site.Origin, dash(st.Site.KeyID))
	}
	return 1
}

// selectSites resolves an <origin|provider> target against the facade's own
// site list. The facade's Enable, Disable, Sync and Report all take a concrete
// origin and key ID (internal/reconcile/sync.go), so selecting the site is the
// CLI's job; no detection or credential merging happens here.
func selectSites(ctx context.Context, svc relayService, target, keyID string, needEnabled bool) ([]reconcile.SiteStatus, error) {
	statuses, e := svc.Sites(ctx)
	if e != nil {
		return nil, e
	}
	out := make([]reconcile.SiteStatus, 0, len(statuses))
	for _, st := range statuses {
		if !matchesTarget(st, target) {
			continue
		}
		if needEnabled && st.Site.Layers == 0 {
			continue
		}
		if keyID != "" && st.Site.KeyID != keyID {
			continue
		}
		out = append(out, st)
	}
	return out, nil
}

// --- relay enable ----------------------------------------------------------

type relayEnableReq struct {
	target string
	keyID  string
	layers relay.Layer
}

func parseRelayEnable(args []string, errout io.Writer) (relayEnableReq, int) {
	var req relayEnableReq
	fs := flag.NewFlagSet("relay enable", flag.ContinueOnError)
	fs.SetOutput(errout)
	offline := fs.Bool("offline", false, "refused: enable needs the network")
	layersFlag := fs.String("layers", "", "layers to enable: ratio,balance,bills (default all three)")
	keyID := fs.String("key-id", "", "pick one key when several credentials match")
	flags, positional, e := splitArgs(args, map[string]bool{"offline": true})
	if e != nil {
		fmt.Fprintln(errout, e)
		return req, 2
	}
	if e := fs.Parse(flags); e != nil {
		return req, 2
	}
	if len(positional) != 1 {
		fmt.Fprint(errout, relayEnableUsage)
		return req, 2
	}
	if *offline {
		fmt.Fprintln(errout, "relay enable needs the network; --offline is only valid for relay list and reconcile")
		return req, 2
	}
	layers, e := parseLayers(*layersFlag)
	if e != nil {
		fmt.Fprintln(errout, e)
		return req, 2
	}
	req.target = positional[0]
	req.keyID = *keyID
	req.layers = layers
	return req, 0
}

// parseLayers accepts a comma-separated subset of ratio, balance and bills. An
// empty spec means all three layers; an unknown name is an error rather than a
// silently dropped layer.
func parseLayers(spec string) (relay.Layer, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return relay.LayerRatio | relay.LayerBalance | relay.LayerBills, nil
	}
	var out relay.Layer
	for _, name := range strings.Split(spec, ",") {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "ratio":
			out |= relay.LayerRatio
		case "balance":
			out |= relay.LayerBalance
		case "bills":
			out |= relay.LayerBills
		case "":
			return 0, fmt.Errorf("invalid --layers %q: empty layer name", spec)
		default:
			return 0, fmt.Errorf("invalid --layers %q: unknown layer %q (want ratio, balance or bills)", spec, strings.TrimSpace(name))
		}
	}
	if out == 0 {
		return 0, fmt.Errorf("invalid --layers %q (want ratio, balance, bills or a comma-separated combination)", spec)
	}
	return out, nil
}

func relayEnable(ctx context.Context, args []string, out, errout io.Writer) int {
	req, code := parseRelayEnable(args, errout)
	if code != 0 {
		return code
	}
	env, closeEnv, e := openRelayEnv()
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	defer func() { _ = closeEnv() }()
	return relayEnableRun(ctx, req, env, out, errout)
}

func relayEnableRun(ctx context.Context, req relayEnableReq, env *relayEnv, out, errout io.Writer) int {
	svc, e := env.service()
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	// The service identifies the site before it persists anything, so a site
	// that cannot be enabled never leaves a half-configured row behind. It needs
	// a concrete origin and key ID, so resolve <origin|provider> first: only a
	// site we actually hold a key for can be enabled.
	usable := make([]reconcile.SiteStatus, 0, 1)
	all, e := selectSites(ctx, svc, req.target, req.keyID, false)
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	for _, st := range all {
		if st.HasKey || st.Site.KeyID != "" {
			usable = append(usable, st)
		}
	}
	switch {
	case len(usable) == 0:
		fmt.Fprintf(errout, "no relay site with a key matches %q; run `mytoken relay list`\n", req.target)
		return 1
	case len(usable) > 1:
		return ambiguousTarget(errout, req.target, usable)
	}
	st, e := svc.Enable(ctx, usable[0].Site.Origin, usable[0].Site.KeyID, req.layers)
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	fmt.Fprintf(out, "enabled %s (%s) key %s layers %s\n",
		st.Site.Origin, dash(string(st.Site.Kind)), dash(st.Site.KeyID), layersText(st.Site.Layers.LayerNames()))
	return 0
}

// --- relay disable ---------------------------------------------------------

type relayDisableReq struct {
	target string
	keyID  string
}

func parseRelayDisable(args []string, errout io.Writer) (relayDisableReq, int) {
	var req relayDisableReq
	fs := flag.NewFlagSet("relay disable", flag.ContinueOnError)
	fs.SetOutput(errout)
	keyID := fs.String("key-id", "", "pick one key when several sites match")
	flags, positional, e := splitArgs(args, map[string]bool{})
	if e != nil {
		fmt.Fprintln(errout, e)
		return req, 2
	}
	if e := fs.Parse(flags); e != nil {
		return req, 2
	}
	if len(positional) != 1 {
		fmt.Fprint(errout, relayDisableUsage)
		return req, 2
	}
	req.target = positional[0]
	req.keyID = *keyID
	return req, 0
}

func relayDisable(ctx context.Context, args []string, out, errout io.Writer) int {
	req, code := parseRelayDisable(args, errout)
	if code != 0 {
		return code
	}
	env, closeEnv, e := openRelayEnv()
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	defer func() { _ = closeEnv() }()
	return relayDisableRun(ctx, req, env, out, errout)
}

func relayDisableRun(ctx context.Context, req relayDisableReq, env *relayEnv, out, errout io.Writer) int {
	svc, e := env.service()
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	// Disable keeps the site row (and its history) and only clears the enabled
	// layers, so the service reports nothing back: the origin is what the user
	// asked for.
	sites, e := selectSites(ctx, svc, req.target, req.keyID, false)
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	if len(sites) == 0 {
		fmt.Fprintf(errout, "no relay site matches %q; run `mytoken relay list`\n", req.target)
		return 1
	}
	if len(sites) > 1 {
		// Disabling is destructive: never guess which of several matching sites
		// or keys the user meant.
		return ambiguousTarget(errout, req.target, sites)
	}
	for _, st := range sites {
		if e := svc.Disable(ctx, st.Site.Origin, st.Site.KeyID); e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		fmt.Fprintf(out, "disabled %s\n", st.Site.Origin)
	}
	return 0
}

// --- relay sync ------------------------------------------------------------

type relaySyncReq struct {
	target string
	keyID  string
}

func parseRelaySync(args []string, errout io.Writer) (relaySyncReq, int) {
	var req relaySyncReq
	fs := flag.NewFlagSet("relay sync", flag.ContinueOnError)
	fs.SetOutput(errout)
	offline := fs.Bool("offline", false, "refused: sync needs the network")
	keyID := fs.String("key-id", "", "pick one key when several sites match")
	flags, positional, e := splitArgs(args, map[string]bool{"offline": true})
	if e != nil {
		fmt.Fprintln(errout, e)
		return req, 2
	}
	if e := fs.Parse(flags); e != nil {
		return req, 2
	}
	if len(positional) > 1 {
		fmt.Fprint(errout, relaySyncUsage)
		return req, 2
	}
	if *offline {
		fmt.Fprintln(errout, "relay sync needs the network; --offline is only valid for relay list and reconcile")
		return req, 2
	}
	if len(positional) == 1 {
		req.target = positional[0]
	}
	req.keyID = *keyID
	return req, 0
}

func relaySync(ctx context.Context, args []string, out, errout io.Writer) int {
	req, code := parseRelaySync(args, errout)
	if code != 0 {
		return code
	}
	env, closeEnv, e := openRelayEnv()
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	defer func() { _ = closeEnv() }()
	return relaySyncRun(ctx, req, env, out, errout)
}

func relaySyncRun(ctx context.Context, req relaySyncReq, env *relayEnv, out, errout io.Writer) int {
	svc, e := env.service()
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	sites, e := selectSites(ctx, svc, req.target, req.keyID, true)
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	if len(sites) == 0 {
		if req.target == "" {
			fmt.Fprintln(out, "no relay sites are enabled; run `mytoken relay enable <origin|provider>` first")
			return 0
		}
		fmt.Fprintf(errout, "no enabled relay site matches %q; run `mytoken relay list`\n", req.target)
		return 1
	}
	// One site failing (a dead relay, a revoked key) must not keep the others
	// from syncing: try all, report each, and fail at the end.
	code := 0
	for _, st := range sites {
		name := st.Site.Origin
		if len(sites) > 1 {
			name += " (" + st.Site.KeyID + ")"
		}
		if e := svc.Sync(ctx, st.Site.Origin, st.Site.KeyID); e != nil {
			fmt.Fprintf(errout, "%s: %v\n", name, e)
			code = 1
			continue
		}
		fmt.Fprintf(out, "synced %s\n", name)
	}
	return code
}

// --- reconcile -------------------------------------------------------------

type reconcileReq struct {
	target string
	keyID  string
	by     string
	format outputFormat
	rng    query.Range
}

func parseReconcile(args []string, errout io.Writer) (reconcileReq, int) {
	var req reconcileReq
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	fs.SetOutput(errout)
	var cf commonFlags
	addCommonFlags(fs, &cf, "7d")
	by := fs.String("by", "category", "grouping: category, model or day")
	keyID := fs.String("key-id", "", "pick one key when several sites match")
	flags, positional, e := splitArgs(args, map[string]bool{"offline": true, "no-cost": true, "json": true})
	if e != nil {
		fmt.Fprintln(errout, e)
		return req, 2
	}
	if e := cf.parseFlags(fs, flags); e != nil {
		return req, 2
	}
	if len(positional) > 1 {
		fmt.Fprint(errout, reconcileUsage)
		return req, 2
	}
	switch *by {
	case "category", "model", "day":
	default:
		fmt.Fprintln(errout, "invalid --by value:", *by, "(want category, model or day)")
		return req, 2
	}
	opts, e := cf.resolve(time.Now())
	if e != nil {
		fmt.Fprintln(errout, e)
		return req, 2
	}
	if opts.noCost {
		fmt.Fprintln(errout, "--no-cost is not supported by reconcile; the whole view is about money")
		return req, 2
	}
	if len(positional) == 1 {
		req.target = positional[0]
	}
	req.keyID = *keyID
	req.by = *by
	req.format = opts.format
	req.rng = opts.rng
	return req, 0
}

func reconcileRun(ctx context.Context, req reconcileReq, env *relayEnv, out, errout io.Writer) int {
	svc, e := env.service()
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	sites, e := selectSites(ctx, svc, req.target, req.keyID, true)
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	if len(sites) == 0 {
		// Nothing is enabled to compare against. With a target this is the
		// user's mistake (exit 1); with no target it is simply an empty view.
		if req.target != "" {
			fmt.Fprintf(errout, "no enabled relay site matches %q; run `mytoken relay list`\n", req.target)
			return 1
		}
		if req.format == formatTable {
			fmt.Fprintln(out, "no relay sites are enabled; run `mytoken relay enable <origin|provider>` first")
			return 0
		}
	}
	reports := make([]reconcileReport, 0, len(sites))
	for _, st := range sites {
		rep, e := svc.Report(ctx, st.Site.Origin, st.Site.KeyID, req.rng.From, req.rng.To)
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		reports = append(reports, reportFrom(rep, req.rng.From, req.rng.To))
	}
	if e := writeReconcile(out, req.format, req.by, reports); e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	return 0
}

// --- usage -----------------------------------------------------------------

const relayUsage = `Usage: mytoken relay <subcommand> [options]
  relay list [--format table|json|csv] [--json] [--offline]
      sites this build can see: enabled ones, and candidates found in
      cc-switch / harness configs. Never uses the network.
  relay enable <origin|provider> [--layers ratio,balance,bills] [--key-id ID]
      identify the site and start syncing it (default: all three layers).
      Needs the network; --offline is refused.
  relay disable <origin|provider> [--key-id ID]
      stop syncing a site; the row is kept so relay list still shows it.
  relay sync [<origin|provider>] [--key-id ID]
      refresh the enabled sites now. Needs the network; --offline is refused.

A key is never stored or printed: only its key ID (a truncated hash) appears.
Relay sites stay off until you enable them one by one.
`

const relayListUsage = `Usage: mytoken relay list [--format table|json|csv] [--json] [--offline]
Exit status: 0 success, 1 operational error, 2 invalid arguments.
`

const relayEnableUsage = `Usage: mytoken relay enable <origin|provider> [--layers ratio,balance,bills] [--key-id ID]
Exit status: 0 success, 1 operational error, 2 invalid arguments.
`

const relayDisableUsage = `Usage: mytoken relay disable <origin|provider> [--key-id ID]
Exit status: 0 success, 1 operational error, 2 invalid arguments.
`

const relaySyncUsage = `Usage: mytoken relay sync [<origin|provider>] [--key-id ID]
Exit status: 0 success, 1 operational error, 2 invalid arguments.
`

const reconcileUsage = `Usage: mytoken reconcile [<origin|provider>] [--since DATE|DUR] [--until DATE|DUR] [--last WINDOW]
                        [--by category|model|day] [--timezone TZ]
                        [--format table|json|csv] [--json] [--key-id ID]
Compares what local logs say you should have paid with what the site charged.
Never uses the network. Default range: --since 7d.
Exit status: 0 success, 1 operational error, 2 invalid arguments.
`
