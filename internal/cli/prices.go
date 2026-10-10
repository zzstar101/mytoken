package cli

import (
	"context"
	"flag"
	"fmt"
	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/query"
	"io"
	"strconv"
)

func runPrices(ctx context.Context, args []string, out, errout io.Writer) int {
	command := "list"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	var rule query.PriceRule
	if command == "set" {
		fs := flag.NewFlagSet("prices set", flag.ContinueOnError)
		fs.SetOutput(errout)
		fs.StringVar(&rule.Provider, "provider", "", "provider name")
		fs.StringVar(&rule.Model, "model", "", "model name")
		fs.Float64Var(&rule.Multiplier, "multiplier", 1, "cost multiplier")
		for _, field := range []struct {
			name string
			dest **float64
		}{{"input", &rule.Input}, {"output", &rule.Output}, {"cache-read", &rule.CacheRead}, {"cache-write", &rule.CacheWrite}} {
			dest := field.dest
			fs.Func(field.name, "USD per million tokens", func(raw string) error {
				v, err := strconv.ParseFloat(raw, 64)
				if err == nil {
					*dest = &v
				}
				return err
			})
		}
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if fs.NArg() != 0 || rule.Provider == "" {
			fmt.Fprintln(errout, "prices set requires --provider")
			return 2
		}
		rule.Source = "user"
	} else if command != "list" && command != "import-ccswitch" || len(args) != 0 {
		fmt.Fprintln(errout, "usage: mytoken prices [list|import-ccswitch|set --provider X [--model Y] [--multiplier N] [--input N] [--output N] [--cache-read N] [--cache-write N]]")
		return 2
	}
	a, err := app.Open()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer a.Close()
	switch command {
	case "import-ccswitch":
		n, err := a.Settings.ImportCCSwitch(ctx)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		fmt.Fprintf(out, "Imported %d cc-switch price rules\n", n)
	case "set":
		rules, err := a.Settings.PriceRules(ctx)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		next := []query.PriceRule{}
		for _, r := range rules {
			if r.Source == "cc-switch" || r.Provider != rule.Provider || r.Model != rule.Model {
				next = append(next, r)
			}
		}
		next = append(next, rule)
		if err = a.Settings.SetPriceRules(ctx, next); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		fmt.Fprintln(out, "Price rule saved")
	case "list":
		rules, err := a.Settings.PriceRules(ctx)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		w := newTable(out)
		fmt.Fprintln(w, "PROVIDER\tMODEL\tMULTIPLIER\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\tSOURCE")
		rate := func(v *float64) string {
			if v == nil {
				return "catalog"
			}
			return strconv.FormatFloat(*v, 'g', -1, 64)
		}
		for _, r := range rules {
			provider, model := r.Provider, r.Model
			if provider == "" {
				provider = "*"
			}
			if model == "" {
				model = "*"
			}
			m := r.Multiplier
			if m == 0 {
				m = 1
			}
			fmt.Fprintf(w, "%s\t%s\t%g\t%s\t%s\t%s\t%s\t%s\n", provider, model, m, rate(r.Input), rate(r.Output), rate(r.CacheRead), rate(r.CacheWrite), r.Source)
		}
		if err = w.Flush(); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
	}
	return 0
}
