package cli

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // ship the IANA database so --timezone works without host zoneinfo

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/query"
)

// outputFormat is the value of --format.
type outputFormat string

const (
	formatTable outputFormat = "table"
	formatJSON  outputFormat = "json"
	formatCSV   outputFormat = "csv"
)

// commonFlags are the flags shared by stats and sessions. Dates are parsed in
// --timezone (default local), and durations are always counted backwards from
// now: 7d means "seven days ago".
type commonFlags struct {
	since    string
	until    string
	last     string
	timezone string
	offline  bool
	noCost   bool
	format   string
	json     bool

	set map[string]bool // flags the user actually passed
}

// addCommonFlags registers the shared flags on fs. defaultSince is the value
// --since falls back to when the user passes neither --since nor --last
// ("" means unbounded, i.e. all time).
func addCommonFlags(fs *flag.FlagSet, cf *commonFlags, defaultSince string) {
	fs.StringVar(&cf.since, "since", defaultSince, "range start: YYYY-MM-DD, YYYYMMDD, RFC3339 or a duration")
	fs.StringVar(&cf.until, "until", "", "range end, exclusive: YYYY-MM-DD, YYYYMMDD, RFC3339 or a duration")
	fs.StringVar(&cf.last, "last", "", "recent window: today, week, month or Nd|Nw|Nm|Nh|Ns")
	fs.StringVar(&cf.timezone, "timezone", "", "IANA time zone for dates and --by day (default local)")
	fs.BoolVar(&cf.offline, "offline", false, "never use the network")
	fs.BoolVar(&cf.noCost, "no-cost", false, "omit cost columns/fields")
	fs.StringVar(&cf.format, "format", "table", "output format: table, json or csv")
	fs.BoolVar(&cf.json, "json", false, "alias for --format json")
}

// parse parses args and remembers which flags were set, so resolve can tell an
// explicit value from a default (needed for the --last/--since exclusion).
func (cf *commonFlags) parse(fs *flag.FlagSet, args []string) error {
	return cf.parseFlags(fs, args)
}

// parseFlags is parse for callers that already split flags from positional
// arguments (see splitArgs).
func (cf *commonFlags) parseFlags(fs *flag.FlagSet, flags []string) error {
	if e := fs.Parse(flags); e != nil {
		return e
	}
	cf.set = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { cf.set[f.Name] = true })
	return nil
}

// splitArgs separates flags from positional arguments so a command accepts its
// target before or after the options: Go's flag package stops parsing at the
// first positional argument, which would make `reconcile <origin> --by day` a
// usage error. bools names the flags that take no value; any other flag written
// as --name consumes the next argument as its value. A lone "--" ends flag
// parsing, and everything after it is positional.
func splitArgs(args []string, bools map[string]bool) (flags, positional []string, err error) {
	terminated := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if terminated || arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		if arg == "--" {
			terminated = true
			continue
		}
		flags = append(flags, arg)
		if strings.Contains(arg, "=") || bools[flagName(arg)] {
			continue
		}
		if i+1 >= len(args) {
			return nil, nil, fmt.Errorf("flag needs an argument: %s", arg)
		}
		i++
		flags = append(flags, args[i])
	}
	return flags, positional, nil
}

// flagName strips the leading dashes and any =value from a flag argument.
func flagName(arg string) string {
	name := strings.TrimLeft(arg, "-")
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	return name
}

// outputFlags are the output-only flags, for commands that take no time range
// (relay list). Commands that must not omit cost reject --no-cost themselves.
type outputFlags struct {
	format string
	json   bool

	set map[string]bool // flags the user actually passed
}

// addOutputFlags registers the output flags on fs.
func addOutputFlags(fs *flag.FlagSet, of *outputFlags) {
	fs.StringVar(&of.format, "format", "table", "output format: table, json or csv")
	fs.BoolVar(&of.json, "json", false, "alias for --format json")
}

// parse parses args and remembers which flags were set, so resolve can tell an
// explicit --format from the default.
func (of *outputFlags) parse(fs *flag.FlagSet, args []string) error {
	return of.parseFlags(fs, args)
}

// parseFlags is parse for callers that already split flags from positional
// arguments (see splitArgs).
func (of *outputFlags) parseFlags(fs *flag.FlagSet, flags []string) error {
	if e := fs.Parse(flags); e != nil {
		return e
	}
	of.set = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { of.set[f.Name] = true })
	return nil
}

// resolve turns the output flags into one output format.
func (of *outputFlags) resolve() (outputFormat, error) {
	return resolveFormat(of.format, of.json, of.set["format"])
}

// resolveFormat merges --format and --json into a single format. --json is an
// alias for --format json and is only rejected when it contradicts an explicit
// --format.
func resolveFormat(format string, jsonFlag, formatSet bool) (outputFormat, error) {
	name := format
	if name == "" {
		name = string(formatTable)
	}
	if jsonFlag {
		if formatSet && name != string(formatJSON) {
			return "", fmt.Errorf("--json cannot be combined with --format %s", name)
		}
		name = string(formatJSON)
	}
	switch outputFormat(name) {
	case formatTable, formatJSON, formatCSV:
		return outputFormat(name), nil
	default:
		return "", fmt.Errorf("invalid --format %q (want table, json or csv)", name)
	}
}

// options is the resolved form of commonFlags.
type options struct {
	loc     *time.Location
	rng     query.Range
	format  outputFormat
	offline bool
	noCost  bool
}

// resolve validates the flags and turns them into a query range plus output
// options. now is passed in so the resolution is testable.
func (cf *commonFlags) resolve(now time.Time) (options, error) {
	o := options{loc: time.Local, offline: cf.offline, noCost: cf.noCost}
	if cf.timezone != "" {
		loc, e := time.LoadLocation(cf.timezone)
		if e != nil {
			return o, fmt.Errorf("invalid --timezone %q: %w", cf.timezone, e)
		}
		o.loc = loc
	}
	format, e := resolveFormat(cf.format, cf.json, cf.set["format"])
	if e != nil {
		return o, e
	}
	o.format = format
	if cf.set["last"] && (cf.set["since"] || cf.set["until"]) {
		other := "--since"
		switch {
		case cf.set["since"] && cf.set["until"]:
			other = "--since/--until"
		case cf.set["until"]:
			other = "--until"
		}
		return o, fmt.Errorf("--last and %s are mutually exclusive", other)
	}
	if cf.set["last"] {
		rng, e := parseLast(cf.last, now, o.loc)
		if e != nil {
			return o, e
		}
		o.rng = rng
		return o, nil
	}
	var from, to time.Time
	if cf.set["since"] || cf.since != "" {
		v, e := parseTimeSpec(cf.since, now, o.loc)
		if e != nil {
			return o, fmt.Errorf("--since: %w", e)
		}
		from = v
	}
	if cf.set["until"] {
		v, e := parseTimeSpec(cf.until, now, o.loc)
		if e != nil {
			return o, fmt.Errorf("--until: %w", e)
		}
		to = v
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		return o, fmt.Errorf("empty time range: --since %s is not before --until %s",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	o.rng = query.Range{From: from, To: to}
	return o, nil
}

// parseTimeSpec accepts YYYY-MM-DD, YYYYMMDD, RFC3339 or a duration such as
// 7d, 12h, 2w or 3m. Dates are midnight in loc; durations count back from now.
func parseTimeSpec(s string, now time.Time, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if v, e := time.Parse(layout, s); e == nil {
			return v, nil
		}
	}
	for _, layout := range []string{"2006-01-02", "20060102"} {
		if v, e := time.ParseInLocation(layout, s, loc); e == nil {
			return v, nil
		}
	}
	if d, ok := parseDurationSpec(s); ok {
		return d.before(now), nil
	}
	return time.Time{}, fmt.Errorf("invalid date or duration %q (want YYYY-MM-DD, YYYYMMDD, RFC3339, 7d, 12h or 2w)", s)
}

// durationSpec is a single number plus a unit. m is a calendar month, not a
// minute; minutes are written in seconds (90s) or hours.
type durationSpec struct {
	n    int
	unit byte // s, h, d, w or m
}

func parseDurationSpec(s string) (durationSpec, bool) {
	if len(s) < 2 {
		return durationSpec{}, false
	}
	unit := s[len(s)-1]
	switch unit {
	case 's', 'h', 'd', 'w', 'm':
	default:
		return durationSpec{}, false
	}
	n, e := strconv.Atoi(s[:len(s)-1])
	if e != nil || n < 0 {
		return durationSpec{}, false
	}
	return durationSpec{n: n, unit: unit}, true
}

// before returns the instant n units before t. d, w and m are calendar units so
// they land on the same wall-clock time across daylight-saving changes.
func (d durationSpec) before(t time.Time) time.Time {
	switch d.unit {
	case 's':
		return t.Add(-time.Duration(d.n) * time.Second)
	case 'h':
		return t.Add(-time.Duration(d.n) * time.Hour)
	case 'd':
		return t.AddDate(0, 0, -d.n)
	case 'w':
		return t.AddDate(0, 0, -7*d.n)
	case 'm':
		return t.AddDate(0, -d.n, 0)
	}
	return t
}

// parseLast turns --last into a half-open window that ends at now: the start is
// the beginning of the local day/week/month, or now minus the duration.
func parseLast(s string, now time.Time, loc *time.Location) (query.Range, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "today":
		return query.Range{From: startOfDay(now, loc), To: now}, nil
	case "week":
		day := startOfDay(now, loc)
		offset := (int(day.Weekday()) + 6) % 7 // Monday is day 0
		return query.Range{From: day.AddDate(0, 0, -offset), To: now}, nil
	case "month":
		t := now.In(loc)
		return query.Range{From: time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc), To: now}, nil
	}
	d, ok := parseDurationSpec(v)
	if !ok {
		return query.Range{}, fmt.Errorf("invalid --last %q (want today, week, month or a duration like 7d, 2w, 3m)", s)
	}
	from := d.before(now)
	if !from.Before(now) {
		return query.Range{}, fmt.Errorf("invalid --last %q: the window is empty", s)
	}
	return query.Range{From: from, To: now}, nil
}

func startOfDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// openApp opens the index. offline skips the price refresh worker, which is the
// only network access the CLI performs (internal/app/app.go open()).
func openApp(offline bool) (*app.App, error) {
	if offline {
		return app.OpenLocal()
	}
	return app.Open()
}

// runInLocation runs fn with time.Local set to loc. query.Daily and
// query.Hourly bucket by time.Local (internal/query/service.go floor()), so
// this is what makes --timezone affect --by day without touching internal/query.
func runInLocation(loc *time.Location, fn func() int) int {
	if loc == nil || loc == time.Local {
		return fn()
	}
	prev := time.Local
	time.Local = loc
	defer func() { time.Local = prev }()
	return fn()
}
