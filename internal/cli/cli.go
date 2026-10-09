// Package cli serves the non-GUI commands.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/query"
)

func Run(args []string) int { return run(args, os.Stdout, os.Stderr) }
func run(args []string, out, errout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errout, usage)
		return 2
	}
	ctx := context.Background()
	switch args[0] {
	case "version", "help":
		if len(args) != 1 {
			fmt.Fprint(errout, usage)
			return 2
		}
		if args[0] == "version" {
			fmt.Fprintln(out, Version)
		} else {
			fmt.Fprint(out, usage)
		}
		return 0
	case "sessions":
		return runSessions(ctx, args[1:], out, errout)
	case "doctor":
		return runDoctor(ctx, args[1:], out, errout)
	case "scan":
		return runScan(ctx, args[1:], out, errout)
	case "prices":
		return runPrices(ctx, args[1:], out, errout)
	case "stats":
		return runStats(ctx, args[1:], out, errout)
	default:
		fmt.Fprintln(errout, "unknown command:", args[0])
		return 2
	}
}

func runStats(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(errout)
	var cf commonFlags
	addCommonFlags(fs, &cf, "7d")
	by := fs.String("by", "session", "grouping")
	if e := cf.parse(fs, args); e != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errout, "usage: mytoken stats [--since DATE|DUR] [--until DATE|DUR] [--last WINDOW] [--by GROUP] [--timezone TZ] [--offline] [--no-cost] [--format table|json|csv]")
		return 2
	}
	switch *by {
	case "session", "provider", "model", "project", "harness", "day":
	default:
		fmt.Fprintln(errout, "invalid --by value:", *by, "(want session, provider, model, project, harness or day)")
		return 2
	}
	opts, e := cf.resolve(time.Now())
	if e != nil {
		fmt.Fprintln(errout, e)
		return 2
	}
	return runInLocation(opts.loc, func() int { return statsCommand(ctx, opts, *by, out, errout) })
}

func statsCommand(ctx context.Context, opts options, by string, out, errout io.Writer) int {
	a, e := openApp(opts.offline)
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	defer a.Close()
	f := query.Filter{Range: opts.rng}
	total, e := a.Query.Totals(ctx, f)
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	rows, e := statsRows(ctx, a, by, f)
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	unpriced, e := a.Store.UnpricedModels(ctx, opts.rng.From, a.Pricing.HasPrice)
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	if unpriced == nil {
		unpriced = []string{}
	}
	switch opts.format {
	case formatJSON:
		var payload any = statsReport{Totals: total, By: by, Rows: rows, UnpricedModels: unpriced}
		if opts.noCost {
			if payload, e = stripCost(payload); e != nil {
				fmt.Fprintln(errout, e)
				return 1
			}
		}
		e = json.NewEncoder(out).Encode(payload)
	case formatCSV:
		e = writeStatsCSV(out, rows, opts.noCost)
	default:
		e = writeStatsTable(out, total, rows, opts.noCost)
	}
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	if opts.format == formatTable && len(unpriced) > 0 {
		labels := make([]string, len(unpriced))
		for i, name := range unpriced {
			labels[i] = strconv.Quote(name)
		}
		fmt.Fprintf(out, "Unpriced models (excluded from computed cost): %s\n", strings.Join(labels, ", "))
	}
	return 0
}

// statsRows runs the query for one --by view and returns it as a concrete
// slice, so every format (including CSV) sees the same rows.
func statsRows(ctx context.Context, a *app.App, by string, f query.Filter) (any, error) {
	switch by {
	case "session":
		rows, _, e := a.Query.Sessions(ctx, f, query.SortRecent, 0, 0)
		if rows == nil {
			rows = []query.SessionRow{}
		}
		return rows, e
	case "provider":
		rows, e := a.Query.ByProvider(ctx, f)
		if rows == nil {
			rows = []query.Bucket{}
		}
		return rows, e
	case "model":
		rows, e := a.Query.ByModel(ctx, f)
		if rows == nil {
			rows = []query.Bucket{}
		}
		return rows, e
	case "project":
		rows, e := a.Query.ByProject(ctx, f)
		if rows == nil {
			rows = []query.Bucket{}
		}
		return rows, e
	case "harness":
		rows, e := a.Query.ByHarness(ctx, f)
		if rows == nil {
			rows = []query.Bucket{}
		}
		return rows, e
	case "day":
		rows, e := a.Query.Daily(ctx, f)
		if rows == nil {
			rows = []query.Point{}
		}
		return rows, e
	}
	return nil, fmt.Errorf("invalid --by value: %s", by)
}
