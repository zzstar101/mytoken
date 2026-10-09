package cli

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// TestMain pins the process time zone so the "no --timezone" cases have a
// deterministic answer; every time-zone-sensitive case passes --timezone.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, e := time.LoadLocation(name)
	if e != nil {
		t.Fatalf("LoadLocation(%q): %v", name, e)
	}
	return loc
}

func TestParseTimeSpec(t *testing.T) {
	shanghai := mustLocation(t, "Asia/Shanghai")
	now := time.Date(2026, 3, 10, 15, 4, 5, 0, shanghai)
	cases := []struct{ in, want string }{
		{"2026-01-15", "2026-01-15T00:00:00+08:00"},
		{"20260115", "2026-01-15T00:00:00+08:00"},
		{"2026-01-15T00:00:00Z", "2026-01-15T00:00:00Z"},
		{"2026-01-15T08:00:00+08:00", "2026-01-15T08:00:00+08:00"},
		{"7d", "2026-03-03T15:04:05+08:00"},
		{"12h", "2026-03-10T03:04:05+08:00"},
		{"2w", "2026-02-24T15:04:05+08:00"},
		{"1m", "2026-02-10T15:04:05+08:00"}, // m is a calendar month
		{"90s", "2026-03-10T15:02:35+08:00"},
	}
	for _, c := range cases {
		got, e := parseTimeSpec(c.in, now, shanghai)
		if e != nil {
			t.Errorf("parseTimeSpec(%q) error: %v", c.in, e)
			continue
		}
		if s := got.Format(time.RFC3339); s != c.want {
			t.Errorf("parseTimeSpec(%q) = %s, want %s", c.in, s, c.want)
		}
	}
	for _, bad := range []string{"", "nonsense", "7x", "-1d", "1.5d", "7 d", "2026-13-45"} {
		if v, e := parseTimeSpec(bad, now, shanghai); e == nil {
			t.Errorf("parseTimeSpec(%q) = %v, want error", bad, v)
		}
	}
}

func TestParseLast(t *testing.T) {
	ny := mustLocation(t, "America/New_York")
	now := time.Date(2026, 2, 4, 10, 30, 0, 0, time.UTC) // Wednesday
	cases := []struct {
		last, loc, from string
	}{
		{"today", "UTC", "2026-02-04T00:00:00Z"},
		{"week", "UTC", "2026-02-02T00:00:00Z"}, // Monday
		{"month", "UTC", "2026-02-01T00:00:00Z"},
		{"7d", "UTC", "2026-01-28T10:30:00Z"},
		{"2w", "UTC", "2026-01-21T10:30:00Z"},
		{"3m", "UTC", "2025-11-04T10:30:00Z"},
		{"12h", "UTC", "2026-02-03T22:30:00Z"},
		{"today", "America/New_York", "2026-02-04T05:00:00Z"}, // 00:00 EST == 05:00 UTC
		{"month", "America/New_York", "2026-02-01T05:00:00Z"}, // 00:00 EST
		{"Today", "UTC", "2026-02-04T00:00:00Z"},              // case-insensitive
	}
	for _, c := range cases {
		loc := time.UTC
		if c.loc == "America/New_York" {
			loc = ny
		}
		rng, e := parseLast(c.last, now, loc)
		if e != nil {
			t.Errorf("parseLast(%q, %s) error: %v", c.last, c.loc, e)
			continue
		}
		if got := stamp(rng.From); got != c.from {
			t.Errorf("parseLast(%q, %s) from = %s, want %s", c.last, c.loc, got, c.from)
		}
		if want := stamp(now); stamp(rng.To) != want {
			t.Errorf("parseLast(%q, %s) to = %s, want %s", c.last, c.loc, stamp(rng.To), want)
		}
	}
	for _, bad := range []string{"yesterday", "0d", "", "1.5d", "7x"} {
		if _, e := parseLast(bad, now, time.UTC); e == nil {
			t.Errorf("parseLast(%q) = nil error, want error", bad)
		}
	}
}

func resolveFor(t *testing.T, defaultSince string, now time.Time, args ...string) (options, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var cf commonFlags
	addCommonFlags(fs, &cf, defaultSince)
	if e := cf.parse(fs, args); e != nil {
		return options{}, e
	}
	return cf.resolve(now)
}

func TestResolve(t *testing.T) {
	now := time.Date(2026, 2, 4, 10, 30, 0, 0, time.UTC) // Wednesday
	cases := []struct {
		name     string
		def      string
		args     []string
		from, to string
		format   outputFormat
		offline  bool
		noCost   bool
		err      string
	}{
		{name: "stats default since 7d", def: "7d", from: "2026-01-28T10:30:00Z"},
		{name: "sessions default all time", def: ""},
		{name: "since compact date", def: "", args: []string{"--since", "20260201"}, from: "2026-02-01T00:00:00Z"},
		{name: "until date keeps default since", def: "7d", args: []string{"--until", "2026-02-01"}, from: "2026-01-28T10:30:00Z", to: "2026-02-01T00:00:00Z"},
		{name: "until duration", def: "", args: []string{"--until", "12h"}, to: "2026-02-03T22:30:00Z"},
		{name: "since and until", def: "", args: []string{"--since", "2026-01-01", "--until", "2026-02-01"}, from: "2026-01-01T00:00:00Z", to: "2026-02-01T00:00:00Z"},
		{name: "last today", def: "7d", args: []string{"--last", "today"}, from: "2026-02-04T00:00:00Z", to: "2026-02-04T10:30:00Z"},
		{name: "last week", def: "7d", args: []string{"--last", "week"}, from: "2026-02-02T00:00:00Z", to: "2026-02-04T10:30:00Z"},
		{name: "last month", def: "7d", args: []string{"--last", "month"}, from: "2026-02-01T00:00:00Z", to: "2026-02-04T10:30:00Z"},
		{name: "last 7d", def: "7d", args: []string{"--last", "7d"}, from: "2026-01-28T10:30:00Z", to: "2026-02-04T10:30:00Z"},
		{name: "last 2w", def: "", args: []string{"--last", "2w"}, from: "2026-01-21T10:30:00Z", to: "2026-02-04T10:30:00Z"},
		{name: "last 3m", def: "", args: []string{"--last", "3m"}, from: "2025-11-04T10:30:00Z", to: "2026-02-04T10:30:00Z"},
		{name: "timezone changes date parsing", def: "", args: []string{"--timezone", "Asia/Shanghai", "--since", "2026-02-01"}, from: "2026-01-31T16:00:00Z"},
		{name: "timezone changes last month", def: "", args: []string{"--timezone", "Asia/Shanghai", "--last", "month"}, from: "2026-01-31T16:00:00Z", to: "2026-02-04T10:30:00Z"},
		{name: "new york last today", def: "", args: []string{"--timezone", "America/New_York", "--last", "today"}, from: "2026-02-04T05:00:00Z", to: "2026-02-04T10:30:00Z"},
		{name: "format csv", def: "", args: []string{"--format", "csv"}, format: formatCSV},
		{name: "format json", def: "", args: []string{"--format", "json"}, format: formatJSON},
		{name: "json alias", def: "", args: []string{"--json"}, format: formatJSON},
		{name: "json alias for json", def: "", args: []string{"--json", "--format", "json"}, format: formatJSON},
		{name: "offline and no cost", def: "", args: []string{"--offline", "--no-cost"}, offline: true, noCost: true},
		{name: "last with since", def: "7d", args: []string{"--last", "7d", "--since", "30d"}, err: "mutually exclusive"},
		{name: "last with until", def: "7d", args: []string{"--last", "today", "--until", "2026-01-01"}, err: "--until"},
		{name: "last with since and until", def: "7d", args: []string{"--last", "today", "--since", "1d", "--until", "2026-01-01"}, err: "--since/--until"},
		{name: "empty range", def: "", args: []string{"--since", "2026-02-02", "--until", "2026-02-01"}, err: "empty time range"},
		{name: "bad since", def: "", args: []string{"--since", "nope"}, err: "invalid date or duration"},
		{name: "bad until", def: "", args: []string{"--until", "nope"}, err: "--until: invalid date or duration"},
		{name: "bad last", def: "", args: []string{"--last", "yesterday"}, err: "invalid --last"},
		{name: "zero last", def: "", args: []string{"--last", "0d"}, err: "invalid --last"},
		{name: "bad timezone", def: "", args: []string{"--timezone", "Mars/Olympus"}, err: "invalid --timezone"},
		{name: "bad format", def: "", args: []string{"--format", "xml"}, err: "invalid --format"},
		{name: "json with csv", def: "", args: []string{"--json", "--format", "csv"}, err: "cannot be combined"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, e := resolveFor(t, c.def, now, c.args...)
			if c.err != "" {
				if e == nil {
					t.Fatalf("resolve(%v) = %+v, want error containing %q", c.args, got, c.err)
				}
				if !strings.Contains(e.Error(), c.err) {
					t.Fatalf("resolve(%v) error = %q, want it to contain %q", c.args, e, c.err)
				}
				return
			}
			if e != nil {
				t.Fatalf("resolve(%v) error: %v", c.args, e)
			}
			if got := stamp(got.rng.From); got != c.from {
				t.Errorf("from = %q, want %q", got, c.from)
			}
			if got := stamp(got.rng.To); got != c.to {
				t.Errorf("to = %q, want %q", got, c.to)
			}
			wantFormat := c.format
			if wantFormat == "" {
				wantFormat = formatTable
			}
			if got.format != wantFormat {
				t.Errorf("format = %q, want %q", got.format, wantFormat)
			}
			if got.offline != c.offline || got.noCost != c.noCost {
				t.Errorf("offline=%v noCost=%v, want %v/%v", got.offline, got.noCost, c.offline, c.noCost)
			}
		})
	}
}

// stamp renders an instant in UTC so expectations are location-independent.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// seed installs a fresh MYTOKEN_HOME holding events, then closes the store.
func seed(t *testing.T, events ...model.UsageEvent) {
	t.Helper()
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	a, e := app.OpenLocal()
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Store.Commit(t.Context(), model.Codex, "test", harness.Batch{Events: events}, nil); e != nil {
		a.Close()
		t.Fatal(e)
	}
	if e = a.Close(); e != nil {
		t.Fatal(e)
	}
}

func event(dedup, modelName string, at time.Time, tokens model.Tokens) model.UsageEvent {
	return model.UsageEvent{Harness: model.Codex, DedupKey: dedup, SessionID: "s1", Model: modelName, Timestamp: at, Tokens: tokens}
}

func runCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errout bytes.Buffer
	code := run(args, &out, &errout)
	return out.String(), errout.String(), code
}

type statsJSON struct {
	Totals struct {
		Requests int64 `json:"requests"`
	} `json:"totals"`
	Rows []struct {
		Key    string       `json:"key"`
		Label  string       `json:"label"`
		Tokens model.Tokens `json:"tokens"`
		T      time.Time    `json:"t"`
	} `json:"rows"`
	UnpricedModels []string `json:"unpricedModels"`
}

func decodeStats(t *testing.T, out string) statsJSON {
	t.Helper()
	var data statsJSON
	if e := json.Unmarshal([]byte(out), &data); e != nil {
		t.Fatalf("bad JSON: %v\n%s", e, out)
	}
	return data
}

// TestTimezoneDayGrouping checks that --timezone drives both date parsing and
// the --by day buckets: the same two events land in one UTC day but two
// Shanghai days.
func TestTimezoneDayGrouping(t *testing.T) {
	seed(t,
		event("a", "gpt-5", time.Date(2026, 1, 1, 20, 0, 0, 0, time.UTC), model.Tokens{Input: 20}),
		event("b", "gpt-5", time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC), model.Tokens{Input: 10}),
	)
	base := []string{"stats", "--by", "day", "--since", "2026-01-01", "--until", "2026-01-03", "--offline", "--format", "json"}

	out, errout, code := runCLI(t, append([]string{"stats", "--timezone", "Asia/Shanghai"}, base[1:]...)...)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	data := decodeStats(t, out)
	if len(data.Rows) != 2 {
		t.Fatalf("want 2 shanghai days, got %d: %s", len(data.Rows), out)
	}
	if got := data.Rows[0].T.Format(time.RFC3339); got != "2026-01-01T00:00:00+08:00" {
		t.Errorf("first day = %s, want 2026-01-01T00:00:00+08:00", got)
	}
	if got := data.Rows[0].Tokens.Input; got != 10 {
		t.Errorf("2026-01-01 input = %d, want 10 (the 10:00Z event)", got)
	}
	if got := data.Rows[1].T.Format(time.RFC3339); got != "2026-01-02T00:00:00+08:00" {
		t.Errorf("second day = %s, want 2026-01-02T00:00:00+08:00", got)
	}
	if got := data.Rows[1].Tokens.Input; got != 20 {
		t.Errorf("2026-01-02 input = %d, want 20 (the 20:00Z event)", got)
	}

	out, errout, code = runCLI(t, append([]string{"stats", "--timezone", "UTC"}, base[1:]...)...)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	data = decodeStats(t, out)
	if len(data.Rows) != 2 || data.Rows[0].Tokens.Input != 30 || data.Rows[1].Tokens.Input != 0 {
		t.Fatalf("UTC grouping = %+v, want both events on 2026-01-01: %s", data.Rows, out)
	}
	if got := data.Rows[0].T.Format(time.RFC3339); got != "2026-01-01T00:00:00Z" {
		t.Errorf("UTC first day = %s, want 2026-01-01T00:00:00Z", got)
	}
}

// TestRangeBounds checks the half-open range: --since includes the event,
// --until excludes it.
func TestRangeBounds(t *testing.T) {
	at := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	seed(t, event("a", "gpt-5", at, model.Tokens{Input: 20}))
	cases := []struct {
		name string
		args []string
		want int64
	}{
		{"since includes", []string{"--since", "2026-01-15T12:00:00Z"}, 1},
		{"until excludes", []string{"--since", "2026-01-01T00:00:00Z", "--until", "2026-01-15T12:00:00Z"}, 0},
		{"window contains", []string{"--timezone", "UTC", "--since", "2026-01-14", "--until", "2026-01-16"}, 1},
		{"window before", []string{"--timezone", "UTC", "--since", "2026-01-01", "--until", "2026-01-02"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"stats", "--offline", "--format", "json"}, c.args...)
			out, errout, code := runCLI(t, args...)
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, errout)
			}
			if got := decodeStats(t, out).Totals.Requests; got != c.want {
				t.Errorf("requests = %d, want %d (%s)", got, c.want, out)
			}
		})
	}
	t.Run("empty range is rejected", func(t *testing.T) {
		_, errout, code := runCLI(t, "stats", "--since", "2026-01-15", "--until", "2026-01-15", "--offline")
		if code != 2 || !strings.Contains(errout, "empty time range") {
			t.Fatalf("code=%d stderr=%q", code, errout)
		}
	})
}

// TestLastWindow checks --last ends at now, so an event that just happened is
// inside the window while --until 1h keeps it out.
func TestLastWindow(t *testing.T) {
	seed(t, event("a", "gpt-5", time.Now(), model.Tokens{Input: 20}))
	for _, c := range []struct {
		args []string
		want int64
	}{
		{[]string{"--last", "1h"}, 1},
		{[]string{"--last", "today"}, 1},
		{[]string{"--until", "1h"}, 0},
	} {
		args := append([]string{"stats", "--offline", "--format", "json"}, c.args...)
		out, errout, code := runCLI(t, args...)
		if code != 0 {
			t.Fatalf("%v: code=%d stderr=%s", c.args, code, errout)
		}
		if got := decodeStats(t, out).Totals.Requests; got != c.want {
			t.Errorf("%v: requests = %d, want %d", c.args, got, c.want)
		}
	}
}

func parseCSV(t *testing.T, out string) [][]string {
	t.Helper()
	recs, e := csv.NewReader(strings.NewReader(out)).ReadAll()
	if e != nil {
		t.Fatalf("bad CSV: %v\n%s", e, out)
	}
	return recs
}

func TestCSVOutput(t *testing.T) {
	seed(t, event("a", "gpt-5", time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), model.Tokens{Input: 20, Output: 5}))

	t.Run("stats by model", func(t *testing.T) {
		out, errout, code := runCLI(t, "stats", "--by", "model", "--timezone", "UTC", "--since", "2026-01-01", "--until", "2026-02-01", "--offline", "--format", "csv")
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		recs := parseCSV(t, out)
		if len(recs) != 2 {
			t.Fatalf("want header + 1 row, got %v", recs)
		}
		if !reflect.DeepEqual(recs[0], bucketCSVHeader) {
			t.Errorf("header = %v, want %v", recs[0], bucketCSVHeader)
		}
		row := recs[1]
		if row[0] != "gpt-5" || row[1] != "gpt-5" || row[2] != "1" || row[3] != "1" {
			t.Errorf("row = %v", row)
		}
		if !reflect.DeepEqual(row[4:10], []string{"25", "20", "5", "0", "0", "0"}) {
			t.Errorf("token columns = %v", row[4:10])
		}
		if _, e := strconv.ParseFloat(row[10], 64); e != nil {
			t.Errorf("cost column %q is not a plain number", row[10])
		}
	})
	t.Run("stats by day", func(t *testing.T) {
		out, errout, code := runCLI(t, "stats", "--by", "day", "--timezone", "UTC", "--since", "2026-01-15", "--until", "2026-01-16", "--offline", "--format", "csv")
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		recs := parseCSV(t, out)
		if !reflect.DeepEqual(recs[0], dayCSVHeader) {
			t.Errorf("header = %v, want %v", recs[0], dayCSVHeader)
		}
		if len(recs) != 2 || recs[1][0] != "2026-01-15T00:00:00Z" || recs[1][1] != "25" {
			t.Errorf("rows = %v", recs)
		}
	})
	t.Run("sessions", func(t *testing.T) {
		out, errout, code := runCLI(t, "sessions", "--offline", "--format", "csv")
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		recs := parseCSV(t, out)
		if !reflect.DeepEqual(recs[0], sessionCSVHeader) {
			t.Errorf("header = %v, want %v", recs[0], sessionCSVHeader)
		}
		if len(recs) != 2 || recs[1][0] != "codex" || recs[1][1] != "s1" || recs[1][3] != "1" {
			t.Fatalf("rows = %v", recs)
		}
		if _, e := time.Parse(time.RFC3339, recs[1][2]); e != nil {
			t.Errorf("updated_at %q is not RFC3339: %v", recs[1][2], e)
		}
	})
	t.Run("no cost drops the column", func(t *testing.T) {
		out, errout, code := runCLI(t, "stats", "--by", "model", "--timezone", "UTC", "--since", "2026-01-01", "--until", "2026-02-01", "--offline", "--format", "csv", "--no-cost")
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		recs := parseCSV(t, out)
		want := csvHeaderFor(bucketCSVHeader, true)
		if !reflect.DeepEqual(recs[0], want) {
			t.Errorf("header = %v, want %v", recs[0], want)
		}
		if len(recs[1]) != len(want) {
			t.Errorf("row width = %d, want %d: %v", len(recs[1]), len(want), recs[1])
		}
	})
}

func TestNoCost(t *testing.T) {
	seed(t, event("a", "gpt-5", time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), model.Tokens{Input: 20}))
	t.Run("table", func(t *testing.T) {
		out, _, code := runCLI(t, "stats", "--by", "model", "--timezone", "UTC", "--since", "2026-01-01", "--until", "2026-02-01", "--offline")
		if code != 0 {
			t.Fatal(code)
		}
		if !strings.Contains(out, "COST USD") || !strings.Contains(out, "Cost: $") {
			t.Fatalf("default table has no cost: %s", out)
		}
		out, errout, code := runCLI(t, "stats", "--by", "model", "--timezone", "UTC", "--since", "2026-01-01", "--until", "2026-02-01", "--offline", "--no-cost")
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		if strings.Contains(out, "COST USD") || strings.Contains(out, "Cost: $") {
			t.Fatalf("--no-cost table still shows cost: %s", out)
		}
		out, errout, code = runCLI(t, "sessions", "--offline", "--no-cost")
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		if strings.Contains(out, "COST USD") {
			t.Fatalf("--no-cost sessions table still shows cost: %s", out)
		}
	})
	t.Run("json", func(t *testing.T) {
		for _, args := range [][]string{
			{"stats", "--by", "session", "--offline", "--json", "--no-cost"},
			{"stats", "--by", "day", "--offline", "--json", "--no-cost"},
			{"sessions", "--offline", "--json", "--no-cost"},
		} {
			out, errout, code := runCLI(t, args...)
			if code != 0 {
				t.Fatalf("%v: code=%d stderr=%s", args, code, errout)
			}
			var v any
			if e := json.Unmarshal([]byte(out), &v); e != nil {
				t.Fatalf("%v: bad JSON: %v", args, e)
			}
			if hasCostKey(v) {
				t.Errorf("%v: JSON still contains costUsd: %s", args, out)
			}
		}
	})
}

func hasCostKey(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if k == "costUsd" || hasCostKey(x) {
				return true
			}
		}
	case []any:
		for _, x := range t {
			if hasCostKey(x) {
				return true
			}
		}
	}
	return false
}

func TestJSONAliasMatches(t *testing.T) {
	seed(t, event("a", "gpt-5", time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), model.Tokens{Input: 20, Output: 5}))
	alias, _, code := runCLI(t, "stats", "--by", "model", "--offline", "--json")
	if code != 0 {
		t.Fatal(code)
	}
	full, _, code := runCLI(t, "stats", "--by", "model", "--offline", "--format", "json")
	if code != 0 {
		t.Fatal(code)
	}
	var a, b any
	if e := json.Unmarshal([]byte(alias), &a); e != nil {
		t.Fatal(e)
	}
	if e := json.Unmarshal([]byte(full), &b); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("--json and --format json differ:\n%s\n%s", alias, full)
	}
}

// TestSessionsRange checks sessions accepts the shared range flags.
func TestSessionsRange(t *testing.T) {
	seed(t, event("a", "gpt-5", time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), model.Tokens{Input: 20}))
	out, errout, code := runCLI(t, "sessions", "--offline", "--format", "json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	var all struct {
		Total int `json:"total"`
	}
	if e := json.Unmarshal([]byte(out), &all); e != nil || all.Total != 1 {
		t.Fatalf("total = %d (%v): %s", all.Total, e, out)
	}
	out, errout, code = runCLI(t, "sessions", "--since", "2030-01-01", "--offline", "--format", "json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	var none struct {
		Total int `json:"total"`
	}
	if e := json.Unmarshal([]byte(out), &none); e != nil || none.Total != 0 {
		t.Fatalf("total = %d (%v): %s", none.Total, e, out)
	}
}

// TestUnpricedModelsEmptyJSON checks rows and unpricedModels stay arrays (not
// null) when a view has no data.
func TestEmptyViews(t *testing.T) {
	seed(t)
	for _, by := range []string{"session", "provider", "model", "project", "harness", "day"} {
		out, errout, code := runCLI(t, "stats", "--by", by, "--offline", "--json")
		if code != 0 {
			t.Fatalf("%s: code=%d stderr=%s", by, code, errout)
		}
		if !strings.Contains(out, `"rows":[]`) {
			t.Errorf("%s: rows is not an empty array: %s", by, out)
		}
		if !strings.Contains(out, `"unpricedModels":[]`) {
			t.Errorf("%s: unpricedModels is not an empty array: %s", by, out)
		}
		for _, format := range []string{"csv", "table"} {
			if _, errout, code = runCLI(t, "stats", "--by", by, "--offline", "--format", format); code != 0 {
				t.Errorf("%s/%s: code=%d stderr=%s", by, format, code, errout)
			}
		}
	}
}
