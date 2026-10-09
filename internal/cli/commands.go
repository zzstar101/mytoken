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
)

// Version is set at build time with -ldflags -X.
var Version = "dev"

const usage = `Usage: mytoken <command> [options]
  scan
  stats [--json] [--since Nd|YYYY-MM-DD] [--by session|provider|model|project|day|harness]
  sessions [--limit N] [--json]
  doctor [--json]
  version
  help
  prices [list|import-ccswitch]
  prices set --provider NAME [--model NAME] [--multiplier N] [--input N]
             [--output N] [--cache-read N] [--cache-write N]

stats defaults: --since 7d --by session
sessions defaults: --limit 50 (0 means all); most recently updated first.
Exit status: 0 success, 1 operational error, 2 invalid arguments.
`

func runSessions(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	fs.SetOutput(errout)
	limit := fs.Int("limit", 50, "maximum sessions (0 for all)")
	js := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *limit < 0 {
		fmt.Fprintln(errout, "usage: mytoken sessions [--limit N] [--json]; N must be nonnegative")
		return 2
	}
	a, err := app.OpenLocal()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer a.Close()
	rows, total, err := a.Query.Sessions(ctx, query.Filter{}, query.SortRecent, *limit, 0)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if rows == nil {
		rows = []query.SessionRow{}
	}
	if *js {
		err = json.NewEncoder(out).Encode(struct {
			Total    int                `json:"total"`
			Sessions []query.SessionRow `json:"sessions"`
		}{total, rows})
	} else {
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "HARNESS\tSESSION\tUPDATED\tREQUESTS\tTOKENS\tCOST USD\tTITLE")
		for _, v := range rows {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%.4f\t%s\n", v.Harness, v.SessionID, v.UpdatedAt.Format(time.RFC3339), v.Requests, v.Tokens.Total(), v.CostUSD, v.Title)
		}
		err = w.Flush()
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
