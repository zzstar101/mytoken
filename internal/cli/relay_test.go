package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/reconcile"
	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/source"
)

// sentinelKey stands in for a real credential. Nothing the relay commands print
// may ever contain it — that is the privacy contract of docs/RELAY.md §2, and
// TestRelayPrivacy asserts it for every format and every error path.
const sentinelKey = "sk-SENTINEL-must-never-be-printed-9f3a"

func f64(v float64) *float64 { return &v }

// fakeRelay is a relayService with canned answers. It also records what the
// commands asked for, so the tests can check the flag plumbing.
type fakeRelay struct {
	statuses []reconcile.SiteStatus
	report   reconcile.Report

	sitesErr   error
	enableErr  error
	disableErr error
	syncErr    error
	reportErr  error

	enableResult reconcile.SiteStatus

	calls         []string
	layers        relay.Layer
	target, keyID string
	from, to      time.Time
}

func (f *fakeRelay) Sites(context.Context) ([]reconcile.SiteStatus, error) {
	f.calls = append(f.calls, "sites")
	return f.statuses, f.sitesErr
}

func (f *fakeRelay) Enable(_ context.Context, origin, keyID string, layers relay.Layer) (reconcile.SiteStatus, error) {
	f.calls = append(f.calls, "enable")
	f.target, f.keyID, f.layers = origin, keyID, layers
	return f.enableResult, f.enableErr
}

func (f *fakeRelay) Disable(_ context.Context, origin, keyID string) error {
	f.calls = append(f.calls, "disable")
	f.target, f.keyID = origin, keyID
	return f.disableErr
}

func (f *fakeRelay) Sync(_ context.Context, origin, keyID string) error {
	f.calls = append(f.calls, "sync")
	f.target, f.keyID = origin, keyID
	return f.syncErr
}

func (f *fakeRelay) Report(_ context.Context, origin, keyID string, from, to time.Time) (reconcile.Report, error) {
	f.calls = append(f.calls, "report")
	f.target, f.keyID, f.from, f.to = origin, keyID, from, to
	return f.report, f.reportErr
}

func fakeEnv(svc relayService) *relayEnv {
	return &relayEnv{
		Now:     func() time.Time { return time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC) },
		Service: svc,
	}
}

// testStatuses returns one enabled site with everything filled in, and one
// candidate the user never turned on.
func testStatuses() []reconcile.SiteStatus {
	return []reconcile.SiteStatus{
		{
			Site: relay.Site{
				Origin: "https://api.example.com", KeyID: "0123456789ab", Kind: relay.KindNewAPI, Version: "v0.9.1",
				Layers: relay.LayerRatio | relay.LayerBalance | relay.LayerBills, Providers: []string{"openai", "anthropic"},
			},
			Providers: []string{"openai", "anthropic"},
			HasKey:    true,
			LastSync:  time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC),
			Balance: &relay.Balance{
				Origin: "https://api.example.com", KeyID: "0123456789ab",
				At: time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC), RemainingUSD: f64(12.5), UsedUSD: f64(7.25),
			},
		},
		{
			Site: relay.Site{Origin: "https://relay.example.net", Kind: relay.KindUnknown},
		},
	}
}

// testReport is a facade report. FormulaUSD is not a facade field: the CLI
// sums the category lines (1.2 + 0.6 = 1.8), and Coverage is the oldest bill
// the site still holds.
func testReport() reconcile.Report {
	return reconcile.Report{
		Origin: "https://api.example.com", KeyID: "0123456789ab",
		From: time.Date(2026, 1, 27, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC),
		Coverage: time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC),
		LocalUSD: 1.5, ChargedUSD: 2.0, ImpliedMultiplier: f64(1.1111),
		Lines: []reconcile.Line{
			{Category: reconcile.Matched, Count: 10, LocalUSD: 1.2, FormulaUSD: 1.2, ChargedUSD: 1.2},
			{Category: reconcile.PriceDiff, Count: 2, LocalUSD: 0.3, FormulaUSD: 0.6, ChargedUSD: 0.8, Note: "gpt-5.2"},
		},
		ByModel: []reconcile.ModelLine{
			{Model: "gpt-5.2", Count: 12, LocalUSD: 1.5, FormulaUSD: 1.8, ChargedUSD: 2.0},
		},
		ByDay: []reconcile.DayLine{
			{Day: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), Count: 7, LocalUSD: 1.0, FormulaUSD: 1.1, ChargedUSD: 1.2,
				MultiplierDiffUSD: f64(0.1), UsageDiffUSD: f64(0.05)},
			{Day: time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC), Count: 5, LocalUSD: 0.5, FormulaUSD: 0.7, ChargedUSD: 0.8, Unpriced: true},
		},
		Balance: &relay.Balance{RemainingUSD: f64(12.5), UsedUSD: f64(7.25)},
	}
}

// runEnv runs a command body against a fake environment.
func runEnv(fn func(context.Context, *relayEnv, io.Writer, io.Writer) int, env *relayEnv) (string, string, int) {
	var out, errout bytes.Buffer
	code := fn(context.Background(), env, &out, &errout)
	return out.String(), errout.String(), code
}

// --- parsing and dispatch --------------------------------------------------

// TestRelayDispatchAndParsing drives run() directly: every case fails before a
// store is opened, which is exactly the property we want (an argument error must
// never touch the user's index).
func TestRelayDispatchAndParsing(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		code   int
		errHas string
	}{
		{name: "relay without subcommand", args: []string{"relay"}, code: 2, errHas: "Usage: mytoken relay"},
		{name: "unknown subcommand", args: []string{"relay", "bogus"}, code: 2, errHas: "unknown relay subcommand"},
		{name: "list rejects positional", args: []string{"relay", "list", "extra"}, code: 2, errHas: "Usage: mytoken relay list"},
		{name: "list rejects bad format", args: []string{"relay", "list", "--format", "xml"}, code: 2, errHas: `invalid --format "xml"`},
		{name: "list rejects json plus csv", args: []string{"relay", "list", "--json", "--format", "csv"}, code: 2, errHas: "--json cannot be combined"},
		{name: "enable without target", args: []string{"relay", "enable"}, code: 2, errHas: "Usage: mytoken relay enable"},
		{name: "enable refuses offline", args: []string{"relay", "enable", "https://api.example.com", "--offline"}, code: 2, errHas: "needs the network"},
		{name: "enable rejects unknown layer", args: []string{"relay", "enable", "https://api.example.com", "--layers", "ratio,tokens"}, code: 2, errHas: "unknown layer"},
		{name: "enable rejects empty layer", args: []string{"relay", "enable", "https://api.example.com", "--layers", "ratio,,bills"}, code: 2, errHas: "empty layer name"},
		{name: "disable without target", args: []string{"relay", "disable"}, code: 2, errHas: "Usage: mytoken relay disable"},
		{name: "sync refuses offline", args: []string{"relay", "sync", "--offline"}, code: 2, errHas: "needs the network"},
		{name: "sync rejects two targets", args: []string{"relay", "sync", "a", "b"}, code: 2, errHas: "Usage: mytoken relay sync"},
		{name: "reconcile rejects unknown by", args: []string{"reconcile", "--by", "site"}, code: 2, errHas: "invalid --by value"},
		{name: "reconcile rejects no-cost", args: []string{"reconcile", "--no-cost"}, code: 2, errHas: "--no-cost is not supported"},
		{name: "reconcile rejects two targets", args: []string{"reconcile", "a", "b"}, code: 2, errHas: "Usage: mytoken reconcile"},
		{name: "reconcile rejects last plus since", args: []string{"reconcile", "--last", "week", "--since", "7d"}, code: 2, errHas: "mutually exclusive"},
		{name: "reconcile rejects empty range", args: []string{"reconcile", "--since", "2026-02-01", "--until", "2026-01-01"}, code: 2, errHas: "empty time range"},
		{name: "reconcile rejects bad timezone", args: []string{"reconcile", "--timezone", "Mars/Olympus"}, code: 2, errHas: "invalid --timezone"},
		{name: "unknown command", args: []string{"relai"}, code: 2, errHas: "unknown command"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, errout, code := runCLI(t, tc.args...)
			if code != tc.code {
				t.Fatalf("code=%d want %d (stderr=%s)", code, tc.code, errout)
			}
			if !strings.Contains(errout, tc.errHas) {
				t.Fatalf("stderr %q does not contain %q", errout, tc.errHas)
			}
			if out != "" {
				t.Fatalf("stdout should stay empty on a usage error, got %q", out)
			}
		})
	}
}

func TestParseRelayListAcceptsOffline(t *testing.T) {
	// relay list never uses the network, but scripts still pass --offline.
	req, code := parseRelayList([]string{"--offline", "--json"}, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if req.format != formatJSON {
		t.Fatalf("format=%q want json", req.format)
	}
}

func TestParseRelayEnable(t *testing.T) {
	all := relay.LayerRatio | relay.LayerBalance | relay.LayerBills
	cases := []struct {
		name   string
		args   []string
		layers relay.Layer
		target string
		keyID  string
	}{
		{name: "defaults to all three layers", args: []string{"https://api.example.com"}, layers: all, target: "https://api.example.com"},
		{name: "single layer", args: []string{"api.example.com", "--layers", "balance"}, layers: relay.LayerBalance, target: "api.example.com"},
		{name: "comma list with spaces", args: []string{"https://api.example.com", "--layers", " bills , ratio "},
			layers: relay.LayerBills | relay.LayerRatio, target: "https://api.example.com"},
		{name: "key id selector", args: []string{"https://api.example.com", "--key-id", "0123456789ab"},
			layers: all, target: "https://api.example.com", keyID: "0123456789ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, code := parseRelayEnable(tc.args, &bytes.Buffer{})
			if code != 0 {
				t.Fatalf("code=%d", code)
			}
			if req.target != tc.target || req.keyID != tc.keyID || req.layers != tc.layers {
				t.Fatalf("got target=%q keyID=%q layers=%v, want %q %q %v",
					req.target, req.keyID, req.layers, tc.target, tc.keyID, tc.layers)
			}
		})
	}
}

func TestParseReconcileRange(t *testing.T) {
	req, code := parseReconcile([]string{"https://api.example.com", "--by", "day", "--last", "week"}, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if req.target != "https://api.example.com" || req.by != "day" {
		t.Fatalf("got target=%q by=%q", req.target, req.by)
	}
	if req.rng.To.IsZero() || !req.rng.From.Before(req.rng.To) {
		t.Fatalf("--last week should produce a bounded window, got %+v", req.rng)
	}

	// The default is --since 7d with an open end.
	req, code = parseReconcile(nil, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if req.by != "category" {
		t.Fatalf("default --by=%q want category", req.by)
	}
	if age := time.Since(req.rng.From); age < 6*24*time.Hour || age > 8*24*time.Hour {
		t.Fatalf("default --since should be about 7 days ago, got %s", age)
	}
	if !req.rng.To.IsZero() {
		t.Fatalf("default range should be open ended, got to=%s", req.rng.To)
	}
}

// --- relay list ------------------------------------------------------------

func TestRelayListTable(t *testing.T) {
	env := fakeEnv(&fakeRelay{statuses: testStatuses()})
	out, errout, code := runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayListRun(ctx, relayListReq{format: formatTable}, e, o, er)
	}, env)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	for _, want := range []string{"ORIGIN", "KIND", "KEY", "api.example.com", "newapi", "0123456789ab",
		"ratio,balance,bills", "openai,anthropic", "2026-02-03T04:05:06Z", "12.5000", "relay.example.net", "off"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table output missing %q:\n%s", want, out)
		}
	}
}

func TestRelayListJSON(t *testing.T) {
	env := fakeEnv(&fakeRelay{statuses: testStatuses()})
	out, errout, code := runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayListRun(ctx, relayListReq{format: formatJSON}, e, o, er)
	}, env)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	var data relayListReport
	if e := json.Unmarshal([]byte(out), &data); e != nil {
		t.Fatalf("bad JSON: %v\n%s", e, out)
	}
	if len(data.Sites) != 2 {
		t.Fatalf("want 2 sites, got %d: %s", len(data.Sites), out)
	}
	first := data.Sites[0]
	if first.Origin != "https://api.example.com" || first.Kind != "newapi" || !first.Enabled || !first.HasKey {
		t.Fatalf("unexpected first site: %+v", first)
	}
	if strings.Join(first.Layers, ",") != "ratio,balance,bills" {
		t.Fatalf("layers=%v", first.Layers)
	}
	if first.LastSync == nil || !first.LastSync.Equal(time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)) {
		t.Fatalf("lastSync=%v", first.LastSync)
	}
	if first.Balance == nil || first.Balance.RemainingUSD == nil || *first.Balance.RemainingUSD != 12.5 {
		t.Fatalf("balance=%+v", first.Balance)
	}
	second := data.Sites[1]
	if second.Enabled || second.HasKey || second.Layers == nil || len(second.Layers) != 0 {
		t.Fatalf("candidate row should be disabled with an empty layers array: %+v (%s)", second, out)
	}
}

func TestRelayListCSV(t *testing.T) {
	env := fakeEnv(&fakeRelay{statuses: testStatuses()})
	out, errout, code := runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayListRun(ctx, relayListReq{format: formatCSV}, e, o, er)
	}, env)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	recs := parseCSV(t, out)
	if len(recs) != 3 {
		t.Fatalf("want header + 2 rows, got %d: %s", len(recs), out)
	}
	for i, name := range relayListCSVHeader {
		if recs[0][i] != name {
			t.Fatalf("column %d: got %q want %q", i, recs[0][i], name)
		}
	}
	row := recs[1]
	if row[0] != "https://api.example.com" || row[1] != "0123456789ab" || row[2] != "true" || row[3] != "newapi" {
		t.Fatalf("unexpected row: %v", row)
	}
	if row[6] != "ratio,balance,bills" || row[8] != "2026-02-03T04:05:06Z" {
		t.Fatalf("unexpected row: %v", row)
	}
	if row[10] != "12.500000" || row[11] != "7.250000" || row[12] != "false" {
		t.Fatalf("money columns should be plain six-decimal numbers: %v", row)
	}
	if recs[2][5] != "false" || recs[2][6] != "" {
		t.Fatalf("candidate row: %v", recs[2])
	}
}

func TestRelayListSortsAndReportsServiceErrors(t *testing.T) {
	// The service is expected to sort, but the CLI must not depend on it.
	env := fakeEnv(&fakeRelay{statuses: []reconcile.SiteStatus{
		{Site: relay.Site{Origin: "https://z.example.net"}},
		{Site: relay.Site{Origin: "https://a.example.com"}},
	}})
	out, _, code := runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayListRun(ctx, relayListReq{format: formatJSON}, e, o, er)
	}, env)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var data relayListReport
	if e := json.Unmarshal([]byte(out), &data); e != nil {
		t.Fatal(e)
	}
	if data.Sites[0].Origin != "https://a.example.com" || data.Sites[1].Origin != "https://z.example.net" {
		t.Fatalf("rows are not sorted by origin: %+v", data.Sites)
	}

	env = fakeEnv(&fakeRelay{sitesErr: errors.New("boom")})
	out, errout, code := runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayListRun(ctx, relayListReq{format: formatTable}, e, o, er)
	}, env)
	if code != 1 || !strings.Contains(errout, "boom") || out != "" {
		t.Fatalf("service error should exit 1 with the message on stderr, got code=%d out=%q err=%q", code, out, errout)
	}
}

// --- mutations -------------------------------------------------------------

func TestRelayEnableRun(t *testing.T) {
	svc := &fakeRelay{
		statuses: testStatuses(),
		enableResult: reconcile.SiteStatus{
			Site: relay.Site{Origin: "https://api.example.com", KeyID: "0123456789ab", Kind: relay.KindNewAPI,
				Layers: relay.LayerRatio | relay.LayerBalance},
			HasKey: true,
		},
	}
	req, code := parseRelayEnable([]string{"https://api.example.com", "--layers", "ratio,balance", "--key-id", "0123456789ab"}, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	out, errout, code := runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayEnableRun(ctx, req, e, o, er)
	}, fakeEnv(svc))
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	if want := "enabled https://api.example.com (newapi) key 0123456789ab layers ratio,balance\n"; out != want {
		t.Fatalf("out=%q want %q", out, want)
	}
	if svc.target != "https://api.example.com" || svc.keyID != "0123456789ab" {
		t.Fatalf("service got target=%q keyID=%q", svc.target, svc.keyID)
	}
	if svc.layers != relay.LayerRatio|relay.LayerBalance {
		t.Fatalf("service got layers=%v", svc.layers)
	}

	// A failed enable is an operational error, and the site is left untouched.
	failing := &fakeRelay{statuses: testStatuses(), enableErr: errors.New("relay: cannot identify site")}
	out, errout, code = runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayEnableRun(ctx, req, e, o, er)
	}, fakeEnv(failing))
	if code != 1 || !strings.Contains(errout, "cannot identify site") || out != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
	}

	// A target nothing matches never reaches the service.
	unknown := &fakeRelay{statuses: testStatuses()}
	out, errout, code = runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayEnableRun(ctx, relayEnableReq{target: "https://nope.example", layers: relay.LayerRatio}, e, o, er)
	}, fakeEnv(unknown))
	if code != 1 || out != "" || !strings.Contains(errout, "no relay site with a key matches") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
	}
	if len(unknown.calls) != 1 { // Sites only: nothing was enabled
		t.Fatalf("service calls = %v", unknown.calls)
	}

	// Several keys match: the user has to pick one, and nothing is enabled yet.
	ambiguous := &fakeRelay{statuses: []reconcile.SiteStatus{
		{Site: relay.Site{Origin: "https://api.example.com", KeyID: "aaaaaaaaaaaa", Kind: relay.KindNewAPI}, HasKey: true},
		{Site: relay.Site{Origin: "https://api.example.com", KeyID: "bbbbbbbbbbbb", Kind: relay.KindNewAPI}, HasKey: true},
	}}
	out, errout, code = runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayEnableRun(ctx, relayEnableReq{target: "https://api.example.com", layers: relay.LayerRatio}, e, o, er)
	}, fakeEnv(ambiguous))
	if code != 1 || out != "" || !strings.Contains(errout, "pass --key-id") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
	}
	if len(ambiguous.calls) != 1 {
		t.Fatalf("service calls = %v", ambiguous.calls)
	}

	// A provider name is a valid target for enable.
	out, errout, code = runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayEnableRun(ctx, relayEnableReq{target: "claude-code", layers: relay.LayerRatio}, e, o, er)
	}, fakeEnv(&fakeRelay{
		statuses: []reconcile.SiteStatus{{
			Site:      relay.Site{Origin: "https://xlabapi.com", KeyID: "80548a357f0d", Kind: relay.KindNewAPI},
			Providers: []string{"claude-code"},
			HasKey:    true,
		}},
		enableResult: reconcile.SiteStatus{
			Site: relay.Site{Origin: "https://xlabapi.com", KeyID: "80548a357f0d", Kind: relay.KindNewAPI,
				Layers: relay.LayerRatio},
			HasKey: true,
		},
	}))
	if code != 0 || out != "enabled https://xlabapi.com (newapi) key 80548a357f0d layers ratio\n" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
	}
}

func TestRelayDisableRun(t *testing.T) {
	svc := &fakeRelay{statuses: testStatuses()}
	out, errout, code := runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayDisableRun(ctx, relayDisableReq{target: "https://api.example.com"}, e, o, er)
	}, fakeEnv(svc))
	if code != 0 || out != "disabled https://api.example.com\n" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
	}
	if svc.target != "https://api.example.com" {
		t.Fatalf("service got target=%q", svc.target)
	}

	failing := &fakeRelay{statuses: testStatuses(), disableErr: errors.New("relay: no site matches")}
	out, errout, code = runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayDisableRun(ctx, relayDisableReq{target: "https://api.example.com"}, e, o, er)
	}, fakeEnv(failing))
	if code != 1 || !strings.Contains(errout, "no site matches") || out != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
	}

	// An unknown target is rejected before the service is called.
	unknown := &fakeRelay{statuses: testStatuses()}
	out, errout, code = runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayDisableRun(ctx, relayDisableReq{target: "https://nope.example"}, e, o, er)
	}, fakeEnv(unknown))
	if code != 1 || out != "" || !strings.Contains(errout, "no relay site matches") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
	}
	if len(unknown.calls) != 1 {
		t.Fatalf("service calls = %v", unknown.calls)
	}
}

func TestRelaySyncRun(t *testing.T) {
	svc := &fakeRelay{statuses: testStatuses()}
	out, errout, code := runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relaySyncRun(ctx, relaySyncReq{}, e, o, er)
	}, fakeEnv(svc))
	if code != 0 || out != "synced https://api.example.com\n" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
	}
	if svc.target != "https://api.example.com" || svc.keyID != "0123456789ab" {
		t.Fatalf("service got target=%q keyID=%q", svc.target, svc.keyID)
	}

	// Nothing enabled and no target: nothing to do, but not an error.
	em := &fakeRelay{statuses: testStatuses()[1:]} // candidate without layers
	out, errout, code = runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relaySyncRun(ctx, relaySyncReq{}, e, o, er)
	}, fakeEnv(em))
	if code != 0 || !strings.Contains(out, "no relay sites are enabled") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
	}
	if len(em.calls) != 1 {
		t.Fatalf("service calls = %v", em.calls)
	}

	// A target that is not enabled is an error.
	out, errout, code = runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relaySyncRun(ctx, relaySyncReq{target: "https://api.example.com"}, e, o, er)
	}, fakeEnv(&fakeRelay{statuses: testStatuses()[1:]}))
	if code != 1 || out != "" || !strings.Contains(errout, "no enabled relay site matches") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
	}

	out, _, code = runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relaySyncRun(ctx, relaySyncReq{target: "https://api.example.com"}, e, o, er)
	}, fakeEnv(&fakeRelay{statuses: testStatuses()}))
	if code != 0 || out != "synced https://api.example.com\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}

	failing := &fakeRelay{statuses: testStatuses(), syncErr: errors.New("relay: 502 from https://api.example.com/api/status")}
	out, errout, code = runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relaySyncRun(ctx, relaySyncReq{}, e, o, er)
	}, fakeEnv(failing))
	if code != 1 || out != "" || !strings.Contains(errout, "502") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
	}
}

// TestRelayTargetResolution covers the <origin|provider> matching the CLI does
// before calling the facade, which only accepts a concrete (origin, keyID).
func TestRelayTargetResolution(t *testing.T) {
	two := []reconcile.SiteStatus{
		{
			Site:      relay.Site{Origin: "https://a.example.com", KeyID: "aaaaaaaaaaaa", Kind: relay.KindNewAPI, Layers: relay.LayerRatio},
			Providers: []string{"claude-code"},
			HasKey:    true,
		},
		{
			Site:      relay.Site{Origin: "https://b.example.com", KeyID: "bbbbbbbbbbbb", Kind: relay.KindSub2API, Layers: relay.LayerRatio},
			Providers: []string{"claude-code"},
			HasKey:    true,
		},
	}

	// A provider name that names two sites is ambiguous for the mutations: the
	// candidates are listed and nothing is changed.
	for _, tc := range []struct {
		name string
		body func(context.Context, *relayEnv, io.Writer, io.Writer) int
	}{
		{"enable", func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
			return relayEnableRun(ctx, relayEnableReq{target: "claude-code", layers: relay.LayerRatio}, e, o, er)
		}},
		{"disable", func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
			return relayDisableRun(ctx, relayDisableReq{target: "claude-code"}, e, o, er)
		}},
	} {
		svc := &fakeRelay{statuses: two}
		out, errout, code := runEnv(tc.body, fakeEnv(svc))
		if code != 1 || out != "" {
			t.Fatalf("%s: code=%d out=%q err=%q", tc.name, code, out, errout)
		}
		for _, want := range []string{"2 relay sites match", "https://a.example.com", "https://b.example.com", "aaaaaaaaaaaa", "bbbbbbbbbbbb", "pass --key-id"} {
			if !strings.Contains(errout, want) {
				t.Fatalf("%s: stderr should list %q:\n%s", tc.name, want, errout)
			}
		}
		if len(svc.calls) != 1 { // Sites only: no mutation happened
			t.Fatalf("%s: service calls = %v", tc.name, svc.calls)
		}
	}

	// reconcile aggregates every matching enabled site instead of guessing.
	agg := &fakeRelay{statuses: two, report: testReport()}
	out, errout, code := runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return reconcileRun(ctx, reconcileReq{target: "claude-code", by: "category", format: formatJSON}, e, o, er)
	}, fakeEnv(agg))
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	var data reconcileJSON
	if e := json.Unmarshal([]byte(out), &data); e != nil {
		t.Fatalf("bad JSON: %v\n%s", e, out)
	}
	if len(data.Reports) != 2 {
		t.Fatalf("want two reports, got %d: %s", len(data.Reports), out)
	}
	if len(agg.calls) != 3 { // Sites + one Report per site
		t.Fatalf("service calls = %v", agg.calls)
	}

	// The origin match ignores case and a trailing slash, and the resolved
	// (origin, keyID) pair — not the user's spelling — reaches the service.
	spelled := "HTTPS://API.Example.COM/"
	svc := &fakeRelay{statuses: testStatuses()}
	if out, errout, code := runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayDisableRun(ctx, relayDisableReq{target: spelled}, e, o, er)
	}, fakeEnv(svc)); code != 0 || out != "disabled https://api.example.com\n" {
		t.Fatalf("disable: code=%d out=%q err=%q", code, out, errout)
	}
	if svc.target != "https://api.example.com" || svc.keyID != "0123456789ab" {
		t.Fatalf("disable: service got target=%q keyID=%q", svc.target, svc.keyID)
	}

	rep := &fakeRelay{statuses: testStatuses(), report: testReport()}
	if _, errout, code := runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return reconcileRun(ctx, reconcileReq{target: "https://API.example.com", by: "category", format: formatTable}, e, o, er)
	}, fakeEnv(rep)); code != 0 {
		t.Fatalf("reconcile: code=%d err=%q", code, errout)
	}
	if rep.target != "https://api.example.com" || rep.keyID != "0123456789ab" {
		t.Fatalf("reconcile: service got target=%q keyID=%q", rep.target, rep.keyID)
	}
}

// TestRelayUnwired pins the behaviour while internal/reconcile is not wired in:
// every command reports the same error and exits 1 instead of panicking.
func TestRelayUnwired(t *testing.T) {
	env := fakeEnv(nil)
	cases := []struct {
		name string
		fn   func(context.Context, *relayEnv, io.Writer, io.Writer) int
	}{
		{name: "list", fn: func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
			return relayListRun(ctx, relayListReq{format: formatTable}, e, o, er)
		}},
		{name: "enable", fn: func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
			return relayEnableRun(ctx, relayEnableReq{target: "https://api.example.com"}, e, o, er)
		}},
		{name: "disable", fn: func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
			return relayDisableRun(ctx, relayDisableReq{target: "https://api.example.com"}, e, o, er)
		}},
		{name: "sync", fn: func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
			return relaySyncRun(ctx, relaySyncReq{}, e, o, er)
		}},
		{name: "reconcile", fn: func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
			return reconcileRun(ctx, reconcileReq{by: "category", format: formatTable}, e, o, er)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, errout, code := runEnv(tc.fn, env)
			if code != 1 {
				t.Fatalf("code=%d want 1 (out=%q err=%q)", code, out, errout)
			}
			if !strings.Contains(errout, "not available yet") {
				t.Fatalf("stderr=%q", errout)
			}
			if out != "" {
				t.Fatalf("stdout=%q", out)
			}
		})
	}
}

// --- reconcile -------------------------------------------------------------

func TestReconcileTableViews(t *testing.T) {
	cases := []struct {
		by     string
		header string
		want   []string
	}{
		{by: "category", header: "CATEGORY", want: []string{"matched", "price-diff", "gpt-5.2"}},
		{by: "model", header: "MODEL", want: []string{"gpt-5.2", "12"}},
		{by: "day", header: "DAY", want: []string{"2026-02-01", "2026-02-02", "true"}},
	}
	for _, tc := range cases {
		t.Run(tc.by, func(t *testing.T) {
			env := fakeEnv(&fakeRelay{statuses: testStatuses(), report: testReport()})
			out, errout, code := runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
				return reconcileRun(ctx, reconcileReq{by: tc.by, format: formatTable}, e, o, er)
			}, env)
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, errout)
			}
			for _, want := range append([]string{"site https://api.example.com", "local $1.500000", "charged $2.000000", "implied x1.1111", tc.header}, tc.want...) {
				if !strings.Contains(out, want) {
					t.Fatalf("table (%s) missing %q:\n%s", tc.by, want, out)
				}
			}
		})
	}
}

func TestReconcileJSON(t *testing.T) {
	env := fakeEnv(&fakeRelay{statuses: testStatuses(), report: testReport()})
	out, errout, code := runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return reconcileRun(ctx, reconcileReq{by: "day", format: formatJSON}, e, o, er)
	}, env)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	var data reconcileJSON
	if e := json.Unmarshal([]byte(out), &data); e != nil {
		t.Fatalf("bad JSON: %v\n%s", e, out)
	}
	if len(data.Reports) != 1 {
		t.Fatalf("want 1 report, got %d: %s", len(data.Reports), out)
	}
	rep := data.Reports[0]
	if rep.Origin != "https://api.example.com" || rep.ChargedUSD != 2.0 || rep.ImpliedMultiplier == nil || *rep.ImpliedMultiplier != 1.1111 {
		t.Fatalf("report=%+v", rep)
	}
	if len(rep.Lines) != 2 || len(rep.ByModel) != 1 || len(rep.ByDay) != 2 {
		t.Fatalf("detail slices: %+v", rep)
	}
	if rep.ByDay[0].MultiplierDiffUSD == nil || *rep.ByDay[0].MultiplierDiffUSD != 0.1 || rep.ByDay[0].UsageDiffUSD == nil {
		t.Fatalf("day line: %+v", rep.ByDay[0])
	}
	if !rep.ByDay[1].Unpriced {
		t.Fatalf("second day should be unpriced: %+v", rep.ByDay[1])
	}
}

func TestReconcileCSV(t *testing.T) {
	cases := []struct {
		by     string
		header []string
		rows   int
	}{
		{by: "category", header: reconcileCategoryCSVHeader, rows: 2},
		{by: "model", header: reconcileModelCSVHeader, rows: 1},
		{by: "day", header: reconcileDayCSVHeader, rows: 2},
	}
	for _, tc := range cases {
		t.Run(tc.by, func(t *testing.T) {
			env := fakeEnv(&fakeRelay{statuses: testStatuses(), report: testReport()})
			out, errout, code := runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
				return reconcileRun(ctx, reconcileReq{by: tc.by, format: formatCSV}, e, o, er)
			}, env)
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, errout)
			}
			recs := parseCSV(t, out)
			if len(recs) != tc.rows+1 {
				t.Fatalf("want header + %d rows, got %d: %s", tc.rows, len(recs), out)
			}
			for i, name := range tc.header {
				if recs[0][i] != name {
					t.Fatalf("column %d: got %q want %q", i, recs[0][i], name)
				}
			}
			if recs[1][2] != "matched" && tc.by == "category" {
				t.Fatalf("first row should be the matched category: %v", recs[1])
			}
			if tc.by == "day" && recs[1][2] != "2026-02-01T00:00:00Z" {
				t.Fatalf("day column should be RFC3339: %v", recs[1])
			}
		})
	}
}

func TestReconcileEmptyAndErrors(t *testing.T) {
	// Nothing enabled: a friendly message, exit 0.
	env := fakeEnv(&fakeRelay{})
	out, errout, code := runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return reconcileRun(ctx, reconcileReq{by: "category", format: formatTable}, e, o, er)
	}, env)
	if code != 0 || !strings.Contains(out, "no relay sites are enabled") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
	}

	// A target the service does not know is an error.
	out, errout, code = runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return reconcileRun(ctx, reconcileReq{target: "https://nope.example", by: "category", format: formatTable}, e, o, er)
	}, fakeEnv(&fakeRelay{}))
	if code != 1 || !strings.Contains(errout, "no enabled relay site matches") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
	}

	// An empty report still serializes as valid JSON.
	out, errout, code = runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return reconcileRun(ctx, reconcileReq{by: "category", format: formatJSON}, e, o, er)
	}, fakeEnv(&fakeRelay{}))
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errout)
	}
	var data reconcileJSON
	if e := json.Unmarshal([]byte(out), &data); e != nil {
		t.Fatalf("bad JSON: %v\n%s", e, out)
	}
	if len(data.Reports) != 0 {
		t.Fatalf("an empty view must serialize as an empty list: %s", out)
	}

	// A provider name resolves to the site that holds its credential, and a
	// reconcile without a target covers every enabled site (JSON is a list).
	twoSites := &fakeRelay{statuses: testStatuses(), report: testReport()}
	out, errout, code = runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return reconcileRun(ctx, reconcileReq{target: "openai", by: "category", format: formatJSON}, e, o, er)
	}, fakeEnv(twoSites))
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errout)
	}
	if twoSites.target != "https://api.example.com" || twoSites.keyID != "0123456789ab" {
		t.Fatalf("service got target=%q keyID=%q", twoSites.target, twoSites.keyID)
	}

	all := &fakeRelay{statuses: testStatuses(), report: testReport()}
	out, errout, code = runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return reconcileRun(ctx, reconcileReq{by: "category", format: formatJSON}, e, o, er)
	}, fakeEnv(all))
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errout)
	}
	if e := json.Unmarshal([]byte(out), &data); e != nil {
		t.Fatalf("bad JSON: %v\n%s", e, out)
	}
	if len(data.Reports) != 1 { // only the first site is enabled
		t.Fatalf("want one report, got %d: %s", len(data.Reports), out)
	}

	// A failing reconcile exits 1.
	out, errout, code = runEnv(func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return reconcileRun(ctx, reconcileReq{by: "category", format: formatTable}, e, o, er)
	}, fakeEnv(&fakeRelay{statuses: testStatuses(), reportErr: errors.New("relay: reconcile failed")}))
	if code != 1 || out != "" || !strings.Contains(errout, "reconcile failed") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errout)
	}
}

// --- privacy ---------------------------------------------------------------

// TestRelayPrivacy is the sentinel test of docs/RELAY.md §8: a key must not
// show up in any output — success or failure, table, JSON or CSV — and the CLI
// must never even unwrap it (source.SecretReveals stays flat).
func TestRelayPrivacy(t *testing.T) {
	secret := source.NewSecret(sentinelKey)

	statuses := testStatuses()
	statuses[0].Site.KeyID = source.KeyID(sentinelKey)
	statuses[0].LastError = fmt.Sprintf("relay: rejected key %s", secret)

	report := testReport()
	report.KeyID = source.KeyID(sentinelKey)

	// Success paths use a healthy service; the error paths use one whose
	// messages are built from the Secret. Both carry the sentinel-derived key ID
	// so the test proves the ID is shown while the key is not.
	okSvc := &fakeRelay{statuses: statuses, report: report}
	errSvc := &fakeRelay{
		statuses: statuses,
		report:   report,
		// Errors travel through the same %v path the CLI prints, so this is the
		// realistic leak vector: an error built from a Secret must stay redacted.
		enableErr:  fmt.Errorf("relay: key %s rejected by https://api.example.com", secret),
		syncErr:    fmt.Errorf("relay: sync with %s failed", secret),
		disableErr: fmt.Errorf("relay: cannot disable %s", secret),
		reportErr:  fmt.Errorf("relay: report for %s failed", secret),
	}

	before := source.SecretReveals()
	var outputs []string
	capture := func(name string, fn func(context.Context, *relayEnv, io.Writer, io.Writer) int) {
		out, errout, code := runEnv(fn, fakeEnv(errSvc))
		outputs = append(outputs, out, errout)
		if code == 0 {
			t.Errorf("%s: expected a failure path, got exit 0 (%s)", name, out)
		}
	}

	// Success paths: list and reconcile, in every format.
	for _, format := range []outputFormat{formatTable, formatJSON, formatCSV} {
		for _, fn := range []struct {
			name string
			body func(context.Context, *relayEnv, io.Writer, io.Writer) int
		}{
			{"list", func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
				return relayListRun(ctx, relayListReq{format: format}, e, o, er)
			}},
			{"reconcile", func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
				return reconcileRun(ctx, reconcileReq{by: "category", format: format}, e, o, er)
			}},
		} {
			out, errout, code := runEnv(fn.body, fakeEnv(okSvc))
			outputs = append(outputs, out, errout)
			if code != 0 {
				t.Fatalf("%s %s: code=%d err=%s", fn.name, format, code, errout)
			}
		}
	}

	// Error paths.
	capture("enable", func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayEnableRun(ctx, relayEnableReq{target: "https://api.example.com", layers: relay.LayerRatio}, e, o, er)
	})
	capture("disable", func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relayDisableRun(ctx, relayDisableReq{target: "https://api.example.com"}, e, o, er)
	})
	capture("sync", func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return relaySyncRun(ctx, relaySyncReq{}, e, o, er)
	})
	capture("reconcile", func(ctx context.Context, e *relayEnv, o, er io.Writer) int {
		return reconcileRun(ctx, reconcileReq{by: "category", format: formatJSON}, e, o, er)
	})

	joined := strings.Join(outputs, "\n")
	if strings.Contains(joined, sentinelKey) {
		t.Fatalf("the sentinel key leaked into CLI output:\n%s", joined)
	}
	if !strings.Contains(joined, "[redacted]") {
		t.Fatalf("expected the redaction marker in the error output:\n%s", joined)
	}
	if got := source.SecretReveals(); got != before {
		t.Fatalf("the CLI unwrapped a key %d time(s)", got-before)
	}
	if strings.Contains(joined, source.KeyID(sentinelKey)) == false {
		t.Fatalf("the key ID should still be shown:\n%s", joined)
	}
}
