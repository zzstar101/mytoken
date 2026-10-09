// Package cli serves the non-GUI commands.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/zzstar/mytoken/internal/app"
	"github.com/zzstar/mytoken/internal/query"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

func Run(args []string) int { return run(args, os.Stdout, os.Stderr) }
func run(args []string, out, errout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errout, "usage: mytoken stats [--json] [--since 7d|30d|YYYY-MM-DD] [--by session|provider|model|project|day] | scan")
		return 2
	}
	ctx := context.Background()
	switch args[0] {
	case "scan":
		if len(args) > 1 {
			fmt.Fprintln(errout, "usage: mytoken scan")
			return 2
		}
		a, e := app.Open()
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		defer a.Close()
		start := time.Now()
		e = a.Scanner.Scan(ctx)
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		tot, e := a.Query.Totals(ctx, query.Filter{})
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		done, total := a.Scanner.Progress()
		fmt.Fprintf(out, "Scanned %d/%d sources: %d events, %d sessions in %s\n", done, total, tot.Requests, tot.Sessions, time.Since(start).Round(time.Millisecond))
		return 0
	case "prices":
		return runPrices(ctx, args[1:], out, errout)
	case "stats":
		fs := flag.NewFlagSet("stats", flag.ContinueOnError)
		fs.SetOutput(errout)
		jsonOutput := fs.Bool("json", false, "JSON output")
		since := fs.String("since", "7d", "date or duration")
		by := fs.String("by", "session", "grouping")
		if e := fs.Parse(args[1:]); e != nil || fs.NArg() != 0 {
			return 2
		}
		from, e := parseSince(*since)
		if e != nil {
			fmt.Fprintln(errout, e)
			return 2
		}
		a, e := app.Open()
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		defer a.Close()
		f := query.Filter{Range: query.Range{From: from}}
		total, e := a.Query.Totals(ctx, f)
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		var rows any
		switch *by {
		case "session":
			rows, _, e = a.Query.Sessions(ctx, f, query.SortRecent, 0, 0)
		case "provider":
			rows, e = a.Query.ByProvider(ctx, f)
		case "model":
			rows, e = a.Query.ByModel(ctx, f)
		case "project":
			rows, e = a.Query.ByProject(ctx, f)
		case "day":
			rows, e = a.Query.Daily(ctx, f)
		default:
			fmt.Fprintln(errout, "invalid --by value:", *by)
			return 2
		}
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		unpriced, e := a.Store.UnpricedModels(ctx, from, a.Pricing.HasPrice)
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		if *jsonOutput {
			e = json.NewEncoder(out).Encode(struct {
				Totals         query.Totals `json:"totals"`
				By             string       `json:"by"`
				Rows           any          `json:"rows"`
				UnpricedModels []string     `json:"unpricedModels"`
			}{total, *by, rows, unpriced})
			if e != nil {
				fmt.Fprintln(errout, e)
				return 1
			}
			return 0
		}
		fmt.Fprintf(out, "Requests: %d  Sessions: %d  Tokens: %d  Cost: $%.4f  Cache hit: %.1f%%  Unpriced: %d\n", total.Requests, total.Sessions, total.Tokens.Total(), total.CostUSD, total.CacheHit*100, total.Unpriced)
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tREQUESTS\tTOKENS\tCOST USD\tUNPRICED")
		switch values := rows.(type) {
		case []query.Bucket:
			for _, v := range values {
				fmt.Fprintf(w, "%s\t%d\t%d\t%.4f\t%d\n", v.Label, v.Requests, v.Tokens.Total(), v.CostUSD, v.Unpriced)
			}
		case []query.SessionRow:
			for _, v := range values {
				fmt.Fprintf(w, "%s/%s\t%d\t%d\t%.4f\t%d\n", v.Harness, v.SessionID, v.Requests, v.Tokens.Total(), v.CostUSD, v.Unpriced)
			}
		case []query.Point:
			for _, v := range values {
				fmt.Fprintf(w, "%s\t-\t%d\t%.4f\n", v.Day.Format("2006-01-02"), v.Tokens.Total(), v.CostUSD)
			}
		}
		_ = w.Flush()
		if len(unpriced) > 0 {
			labels := make([]string, len(unpriced))
			for i, name := range unpriced {
				labels[i] = strconv.Quote(name)
			}
			fmt.Fprintf(out, "Unpriced models (excluded from computed cost): %s\n", strings.Join(labels, ", "))
		}
		return 0
	default:
		fmt.Fprintln(errout, "unknown command:", args[0])
		return 2
	}
}
func parseSince(s string) (time.Time, error) {
	now := time.Now()
	if strings.HasSuffix(s, "d") {
		n, e := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if e != nil || n < 0 {
			return time.Time{}, errors.New("--since must be Nd or YYYY-MM-DD")
		}
		return now.AddDate(0, 0, -n), nil
	}
	day, e := time.ParseInLocation("2006-01-02", s, time.Local)
	if e != nil {
		return time.Time{}, errors.New("--since must be Nd or YYYY-MM-DD")
	}
	return day, nil
}
