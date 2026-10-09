package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"text/tabwriter"
	"time"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/source"
)

// Version is set at build time with -ldflags -X.
var Version = "dev"

const usage = `Usage: mytoken <command> [options]
  scan
  stats [options] [--by session|provider|model|project|day|harness]
  sessions [--limit N] [options]
  doctor [--json]
  version
  help
  prices [list|import-ccswitch]
  prices set --provider NAME [--model NAME] [--multiplier N] [--input N]
             [--output N] [--cache-read N] [--cache-write N]

options for stats and sessions:
  --since DATE|DUR    range start (stats default 7d; sessions default all time)
  --until DATE|DUR    range end, not included
  --last WINDOW       today|week|month or 7d/2w/3m/12h/30s (ends now);
                      exclusive with --since/--until
  --timezone TZ       IANA zone for date parsing and --by day (default local)
  --offline           never use the network (default: refresh prices first)
  --no-cost           leave out cost columns and costUsd fields
  --format FMT        table (default), json or csv
  --json              alias for --format json

DATE is YYYY-MM-DD, YYYYMMDD or RFC3339. DUR is Ns, Nh, Nd, Nw or Nm
(Nm is a calendar month; minutes are written 90s).
stats defaults: --since 7d --by session
sessions defaults: --limit 50 (0 means all); most recently updated first.
Exit status: 0 success, 1 operational error, 2 invalid arguments.
`

func runSessions(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	fs.SetOutput(errout)
	limit := fs.Int("limit", 50, "maximum sessions (0 for all)")
	var cf commonFlags
	addCommonFlags(fs, &cf, "")
	if err := cf.parse(fs, args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *limit < 0 {
		fmt.Fprintln(errout, "usage: mytoken sessions [--limit N] [--since DATE|DUR] [--until DATE|DUR] [--last WINDOW] [--timezone TZ] [--offline] [--no-cost] [--format table|json|csv]; N must be nonnegative")
		return 2
	}
	opts, err := cf.resolve(time.Now())
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	return runInLocation(opts.loc, func() int { return sessionsCommand(ctx, opts, *limit, out, errout) })
}

func sessionsCommand(ctx context.Context, opts options, limit int, out, errout io.Writer) int {
	a, err := openApp(opts.offline)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer a.Close()
	rows, total, err := a.Query.Sessions(ctx, query.Filter{Range: opts.rng}, query.SortRecent, limit, 0)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if rows == nil {
		rows = []query.SessionRow{}
	}
	switch opts.format {
	case formatJSON:
		var payload any = sessionsReport{Total: total, Sessions: rows}
		if opts.noCost {
			if payload, err = stripCost(payload); err != nil {
				fmt.Fprintln(errout, err)
				return 1
			}
		}
		err = json.NewEncoder(out).Encode(payload)
	case formatCSV:
		err = writeSessionsCSV(out, rows, opts.noCost)
	default:
		err = writeSessionsTable(out, rows, opts.noCost)
	}
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}

type doctorRoot struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}
type doctorHarness struct {
	Harness string       `json:"harness"`
	Roots   []doctorRoot `json:"roots"`
	Sources int64        `json:"sources"`
	Events  int64        `json:"events"`
}
type doctorReport struct {
	DataDir           string          `json:"dataDir"`
	DBPath            string          `json:"dbPath"`
	DBBytes           int64           `json:"dbBytes"`
	PricingSource     string          `json:"pricingSource"`
	PricingFetchedAt  *time.Time      `json:"pricingFetchedAt"`
	PricingAgeSeconds *int64          `json:"pricingAgeSeconds"`
	Harnesses         []doctorHarness `json:"harnesses"`
	ProviderSources   []doctorSource  `json:"providerSources"`
}

// doctorSource is one provider source's health; credentials are never printed.
type doctorSource struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	ReadAt    string `json:"readAt,omitempty"`
	Providers int    `json:"providers"`
	Matches   int    `json:"matches"`
	Error     string `json:"error,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

func runDoctor(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(errout)
	js := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errout, "usage: mytoken doctor [--json]")
		return 2
	}
	dir, err := paths.DataDir()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	db, err := paths.DBPath()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	a, err := app.OpenLocal()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer a.Close()
	report := doctorReport{DataDir: dir, DBPath: db, PricingSource: "embedded", Harnesses: []doctorHarness{}}
	if info, e := os.Stat(db); e == nil {
		report.DBBytes = info.Size()
	} else {
		fmt.Fprintln(errout, e)
		return 1
	}
	if raw, e := os.ReadFile(filepath.Join(dir, "prices.json")); e == nil {
		var cache struct {
			Fetched time.Time `json:"fetched"`
		}
		if json.Unmarshal(raw, &cache) == nil && !cache.Fetched.IsZero() {
			report.PricingSource = "cache"
			report.PricingFetchedAt = &cache.Fetched
			age := int64(time.Since(cache.Fetched).Seconds())
			if age < 0 {
				age = 0
			}
			report.PricingAgeSeconds = &age
		}
	}
	for _, p := range harness.All() {
		h := doctorHarness{Harness: string(p.Harness()), Roots: []doctorRoot{}}
		for _, root := range p.Roots() {
			_, e := os.Stat(root)
			h.Roots = append(h.Roots, doctorRoot{root, e == nil})
		}
		err = a.Store.DB().QueryRowContext(ctx, "SELECT count(*) FROM cursors WHERE harness=?", p.Harness()).Scan(&h.Sources)
		if err == nil {
			err = a.Store.DB().QueryRowContext(ctx, "SELECT count(*) FROM events WHERE harness=?", p.Harness()).Scan(&h.Events)
		}
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		report.Harnesses = append(report.Harnesses, h)
	}
	for _, s := range a.Resolver.Sources() {
		st := s.Status()
		entry := doctorSource{Name: s.Name(), Path: st.Path, Exists: st.Exists, Providers: st.Providers, Matches: st.Matches, Error: st.Err, Detail: st.Detail}
		if !st.ReadAt.IsZero() {
			entry.ReadAt = st.ReadAt.UTC().Format(time.RFC3339)
		}
		if lister, ok := s.(source.Providers); ok {
			if n := len(lister.Providers()); n > 0 {
				entry.Providers = n
			}
		}
		report.ProviderSources = append(report.ProviderSources, entry)
	}
	if *js {
		err = json.NewEncoder(out).Encode(report)
	} else {
		fmt.Fprintf(out, "Data directory: %s\nDatabase: %s (%d bytes)\nPricing: %s", report.DataDir, report.DBPath, report.DBBytes, report.PricingSource)
		if report.PricingAgeSeconds != nil {
			fmt.Fprintf(out, " (age %ds)", *report.PricingAgeSeconds)
		} else {
			fmt.Fprint(out, " (age unknown)")
		}
		fmt.Fprintln(out)
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "HARNESS\tSOURCES\tEVENTS\tROOT\tEXISTS")
		for _, h := range report.Harnesses {
			for _, r := range h.Roots {
				fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%t\n", h.Harness, h.Sources, h.Events, r.Path, r.Exists)
			}
			if len(h.Roots) == 0 {
				fmt.Fprintf(w, "%s\t%d\t%d\t-\tfalse\n", h.Harness, h.Sources, h.Events)
			}
		}
		if len(report.ProviderSources) > 0 {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "PROVIDER SOURCE\tEXISTS\tPROVIDERS\tMATCHES\tREAD AT\tERROR")
			for _, s := range report.ProviderSources {
				fmt.Fprintf(w, "%s\t%t\t%d\t%d\t%s\t%s\n", s.Name, s.Exists, s.Providers, s.Matches, s.ReadAt, s.Error)
			}
		}
		err = w.Flush()
	}
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}

func runScan(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(errout)
	cpu := fs.String("cpuprofile", "", "CPU profile path")
	mem := fs.String("memprofile", "", "allocation profile path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errout, "usage: mytoken scan")
		return 2
	}
	if *cpu != "" {
		f, err := os.Create(*cpu)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		if err = pprof.StartCPUProfile(f); err != nil {
			f.Close()
			fmt.Fprintln(errout, err)
			return 1
		}
		defer func() { pprof.StopCPUProfile(); f.Close() }()
	}
	a, err := app.OpenLocal()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer a.Close()
	start := time.Now()
	if err = a.Scanner.Scan(ctx); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	var events, sessions int64
	err = a.Store.DB().QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&events)
	if err == nil {
		err = a.Store.DB().QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE parent_id='' AND EXISTS (SELECT 1 FROM events WHERE events.harness=sessions.harness AND events.session_id=sessions.session_id)").Scan(&sessions)
	}
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	done, total := a.Scanner.Progress()
	fmt.Fprintf(out, "Scanned %d/%d sources: %d events, %d sessions in %s\n", done, total, events, sessions, time.Since(start).Round(time.Millisecond))
	if *mem != "" {
		f, e := os.Create(*mem)
		if e == nil {
			runtime.GC()
			e = pprof.Lookup("allocs").WriteTo(f, 0)
			closeErr := f.Close()
			if e == nil {
				e = closeErr
			}
		}
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
	}
	return 0
}
