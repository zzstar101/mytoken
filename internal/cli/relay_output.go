package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/reconcile"
	"github.com/zzstar101/mytoken/internal/relay"
)

// This file renders `relay list` and `reconcile`. The view types below are the
// CLI's own output contract: siteRowFrom and reportFrom convert the facade's
// types (internal/reconcile) into them, so the JSON/CSV shape does not change
// when the facade grows fields.

// relaySiteRow is one line of `relay list`: a site the index remembers, or a
// candidate the credential sources know about but the user never enabled.
// KeyID is a truncated hash and safe to print; the key itself never reaches a
// view type.
type relaySiteRow struct {
	Origin    string         `json:"origin"`
	KeyID     string         `json:"keyId"`
	HasKey    bool           `json:"hasKey"`
	Kind      string         `json:"kind"`
	Version   string         `json:"version,omitempty"`
	Enabled   bool           `json:"enabled"`
	Layers    []string       `json:"layers"`
	Providers []string       `json:"providers,omitempty"`
	LastSync  *time.Time     `json:"lastSync,omitempty"`
	LastError string         `json:"lastError,omitempty"`
	Balance   *relay.Balance `json:"balance,omitempty"`
}

// relayListReport is the --format json payload of `relay list`.
type relayListReport struct {
	Sites []relaySiteRow `json:"sites"`
}

// reconcileLine mirrors reconcile.Line: one difference category.
type reconcileLine struct {
	Category   string  `json:"category"`
	Count      int64   `json:"count"`
	LocalUSD   float64 `json:"localUsd"`
	FormulaUSD float64 `json:"formulaUsd"`
	ChargedUSD float64 `json:"chargedUsd"`
	// FormulaMissing counts charges with no computable list cost.
	FormulaMissing int64  `json:"formulaMissing,omitempty"`
	Note           string `json:"note,omitempty"`
}

// reconcileModelLine mirrors reconcile.ModelLine.
type reconcileModelLine struct {
	Model      string  `json:"model"`
	Count      int64   `json:"count"`
	LocalUSD   float64 `json:"localUsd"`
	FormulaUSD float64 `json:"formulaUsd"`
	ChargedUSD float64 `json:"chargedUsd"`
}

// reconcileDayLine mirrors reconcile.DayLine.
type reconcileDayLine struct {
	Day               time.Time `json:"day"`
	Count             int64     `json:"count"`
	LocalUSD          float64   `json:"localUsd"`
	FormulaUSD        float64   `json:"formulaUsd"`
	ChargedUSD        float64   `json:"chargedUsd"`
	MultiplierDiffUSD *float64  `json:"multiplierDiffUsd,omitempty"`
	UsageDiffUSD      *float64  `json:"usageDiffUsd,omitempty"`
	Unpriced          bool      `json:"unpriced,omitempty"`
}

// reconcileReport is the CLI view of reconcile.Report: the same numbers plus a
// FormulaUSD total (the sum of the category lines) and non-nil detail slices so
// table, JSON and CSV stay stable. It carries no key material, only KeyID.
type reconcileReport struct {
	Origin            string               `json:"origin"`
	KeyID             string               `json:"keyId"`
	From              time.Time            `json:"from"`
	To                time.Time            `json:"to"`
	Coverage          *time.Time           `json:"coverage,omitempty"`
	LocalUSD          float64              `json:"localUsd"`
	FormulaUSD        float64              `json:"formulaUsd"`
	FormulaMissing    int64                `json:"formulaMissing,omitempty"`
	ChargedUSD        float64              `json:"chargedUsd"`
	ImpliedMultiplier *float64             `json:"impliedMultiplier,omitempty"`
	Lines             []reconcileLine      `json:"lines"`
	ByModel           []reconcileModelLine `json:"byModel"`
	ByDay             []reconcileDayLine   `json:"byDay"`
	Balance           *relay.Balance       `json:"balance,omitempty"`
}

// reconcileJSON is the --format json payload of reconcile.
type reconcileJSON struct {
	Reports []reconcileReport `json:"reports"`
}

// siteRowFrom renders one facade site status as a `relay list` row.
func siteRowFrom(st reconcile.SiteStatus) relaySiteRow {
	providers := st.Providers
	if len(providers) == 0 {
		providers = relay.ProviderNames(st.Site.Providers)
	}
	row := relaySiteRow{
		Origin:    st.Site.Origin,
		KeyID:     st.Site.KeyID,
		Kind:      string(st.Site.Kind),
		Version:   st.Site.Version,
		Enabled:   st.Site.Layers != 0,
		HasKey:    st.HasKey || st.Site.KeyID != "",
		Layers:    nonNilStrings(st.Site.Layers.LayerNames()),
		Providers: providers,
		LastError: st.LastError,
		Balance:   st.Balance,
	}
	if !st.LastSync.IsZero() {
		sync := st.LastSync
		row.LastSync = &sync
	}
	return row
}

// reportFrom renders a facade report. The facade leaves From/To zero when it
// did not record the caller's range, and its Coverage is the oldest bill it
// still holds (before that, event-only is expected — docs/RELAY.md §5.4); both
// are normalized here, and FormulaUSD is the sum of the category lines.
func reportFrom(r reconcile.Report, from, to time.Time) reconcileReport {
	out := reconcileReport{
		Origin:            r.Origin,
		KeyID:             r.KeyID,
		From:              r.From,
		To:                r.To,
		LocalUSD:          r.LocalUSD,
		ChargedUSD:        r.ChargedUSD,
		ImpliedMultiplier: r.ImpliedMultiplier,
		Balance:           r.Balance,
		Lines:             make([]reconcileLine, 0, len(r.Lines)),
		ByModel:           make([]reconcileModelLine, 0, len(r.ByModel)),
		ByDay:             make([]reconcileDayLine, 0, len(r.ByDay)),
	}
	if out.From.IsZero() {
		out.From = from
	}
	if out.To.IsZero() {
		out.To = to
	}
	if !r.Coverage.IsZero() {
		coverage := r.Coverage
		out.Coverage = &coverage
	}
	for _, l := range r.Lines {
		out.FormulaUSD += l.FormulaUSD
		out.FormulaMissing += l.FormulaMissing
		out.Lines = append(out.Lines, reconcileLine{
			Category: string(l.Category), Count: l.Count, LocalUSD: l.LocalUSD,
			FormulaUSD: l.FormulaUSD, ChargedUSD: l.ChargedUSD, FormulaMissing: l.FormulaMissing, Note: l.Note,
		})
	}
	for _, l := range r.ByModel {
		out.ByModel = append(out.ByModel, reconcileModelLine{
			Model: l.Model, Count: l.Count, LocalUSD: l.LocalUSD,
			FormulaUSD: l.FormulaUSD, ChargedUSD: l.ChargedUSD,
		})
	}
	for _, l := range r.ByDay {
		out.ByDay = append(out.ByDay, reconcileDayLine{
			Day: l.Day, Count: l.Count, LocalUSD: l.LocalUSD,
			FormulaUSD: l.FormulaUSD, ChargedUSD: l.ChargedUSD,
			MultiplierDiffUSD: l.MultiplierDiffUSD, UsageDiffUSD: l.UsageDiffUSD, Unpriced: l.Unpriced,
		})
	}
	return out
}

// CSV headers are stable: new columns append to the end, and numbers are plain
// (no thousands separators, no currency symbol).
var (
	relayListCSVHeader         = []string{"origin", "key_id", "has_key", "kind", "version", "enabled", "layers", "providers", "last_sync", "last_error", "remaining_usd", "used_usd", "unlimited", "currency"}
	reconcileCategoryCSVHeader = []string{"origin", "key_id", "category", "count", "local_usd", "formula_usd", "charged_usd", "note"}
	reconcileModelCSVHeader    = []string{"origin", "key_id", "model", "count", "local_usd", "formula_usd", "charged_usd"}
	reconcileDayCSVHeader      = []string{"origin", "key_id", "day", "count", "local_usd", "formula_usd", "charged_usd", "multiplier_diff_usd", "usage_diff_usd", "unpriced"}
)

// writeRelayList renders `relay list` in the requested format.
func writeRelayList(out io.Writer, format outputFormat, rows []relaySiteRow) error {
	switch format {
	case formatJSON:
		if rows == nil {
			rows = []relaySiteRow{}
		}
		return json.NewEncoder(out).Encode(relayListReport{Sites: rows})
	case formatCSV:
		recs := make([][]string, 0, len(rows))
		for _, r := range rows {
			recs = append(recs, relaySiteCSVRow(r))
		}
		return writeCSV(out, relayListCSVHeader, recs)
	}
	w := newTable(out)
	fmt.Fprintln(w, "ORIGIN\tKIND\tKEY\tLAYERS\tPROVIDERS\tLAST SYNC\tREMAINING\tERROR")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Origin, dash(r.Kind), dash(r.KeyID), layersText(r.Layers),
			dash(strings.Join(r.Providers, ",")), syncText(r.LastSync), balanceText(r.Balance), r.LastError)
	}
	return w.Flush()
}

func relaySiteCSVRow(r relaySiteRow) []string {
	row := []string{r.Origin, r.KeyID, strconv.FormatBool(r.HasKey), r.Kind, r.Version, strconv.FormatBool(r.Enabled),
		strings.Join(r.Layers, ","), strings.Join(r.Providers, ","), "", r.LastError}
	if r.LastSync != nil {
		row[8] = r.LastSync.Format(time.RFC3339)
	}
	remaining, used, unlimited, currency := "", "", "", ""
	if r.Balance != nil {
		unlimited = strconv.FormatBool(r.Balance.Unlimited)
		currency = balanceCurrency(r.Balance)
		if r.Balance.RemainingUSD != nil {
			remaining = cost(*r.Balance.RemainingUSD)
		}
		if r.Balance.UsedUSD != nil {
			used = cost(*r.Balance.UsedUSD)
		}
	}
	return append(row, remaining, used, unlimited, currency)
}

// balanceCurrency is the ISO code a balance is quoted in; MyToken does not
// convert (DeepSeek quotes CNY).
func balanceCurrency(b *relay.Balance) string {
	if b.Currency == "" {
		return "USD"
	}
	return b.Currency
}

// writeReconcile renders the reports in the requested format. by selects the
// detail table for --format table and the CSV row shape for --format csv.
func writeReconcile(out io.Writer, format outputFormat, by string, reports []reconcileReport) error {
	switch format {
	case formatJSON:
		if reports == nil {
			reports = []reconcileReport{}
		}
		return json.NewEncoder(out).Encode(reconcileJSON{Reports: reports})
	case formatCSV:
		return writeReconcileCSV(out, by, reports)
	}
	return writeReconcileTable(out, by, reports)
}

func writeReconcileTable(out io.Writer, by string, reports []reconcileReport) error {
	for i, r := range reports {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "site %s  key %s\n", r.Origin, dash(r.KeyID))
		fmt.Fprintf(out, "range %s .. %s  coverage %s\n",
			r.From.Format(time.RFC3339), r.To.Format(time.RFC3339), syncText(r.Coverage))
		multiplier := ""
		if r.ImpliedMultiplier != nil {
			multiplier = fmt.Sprintf("  implied x%.4f", *r.ImpliedMultiplier)
		}
		// Only charges have a formula; local requests without one do not.
		var count int64
		for _, l := range r.Lines {
			if l.Category != string(reconcile.EventOnly) {
				count += l.Count
			}
		}
		fmt.Fprintf(out, "local $%.6f  formula %s  charged $%.6f%s\n",
			r.LocalUSD, formulaText(r.FormulaUSD, r.FormulaMissing, count, "$"), r.ChargedUSD, multiplier)
		if r.Balance != nil {
			fmt.Fprintf(out, "balance remaining %s  used %s\n", balanceMoney(r.Balance, r.Balance.RemainingUSD), balanceMoney(r.Balance, r.Balance.UsedUSD))
		}
		w := newTable(out)
		switch by {
		case "model":
			fmt.Fprintln(w, "MODEL\tCOUNT\tLOCAL USD\tFORMULA USD\tCHARGED USD")
			for _, l := range r.ByModel {
				fmt.Fprintf(w, "%s\t%d\t%.6f\t%.6f\t%.6f\n", l.Model, l.Count, l.LocalUSD, l.FormulaUSD, l.ChargedUSD)
			}
		case "day":
			fmt.Fprintln(w, "DAY\tCOUNT\tLOCAL USD\tFORMULA USD\tCHARGED USD\tUNPRICED")
			for _, l := range r.ByDay {
				fmt.Fprintf(w, "%s\t%d\t%.6f\t%.6f\t%.6f\t%t\n",
					l.Day.Format("2006-01-02"), l.Count, l.LocalUSD, l.FormulaUSD, l.ChargedUSD, l.Unpriced)
			}
		default:
			if len(r.Lines) == 0 {
				fmt.Fprintln(out, "no charges and no local requests in range")
				continue
			}
			fmt.Fprintln(w, "CATEGORY\tCOUNT\tLOCAL USD\tFORMULA USD\tCHARGED USD\tNOTE")
			for _, l := range r.Lines {
				formula := formulaText(l.FormulaUSD, l.FormulaMissing, l.Count, "")
				if l.Category == string(reconcile.EventOnly) {
					formula = ""
				}
				fmt.Fprintf(w, "%s\t%d\t%.6f\t%s\t%.6f\t%s\n", l.Category, l.Count, l.LocalUSD, formula, l.ChargedUSD, l.Note)
			}

		}
		if e := w.Flush(); e != nil {
			return e
		}
		if r.FormulaMissing > 0 && by != "model" && by != "day" {
			fmt.Fprintf(out, "formula: %d charges have no computable list cost (- none, * some)\n", r.FormulaMissing)
		}
	}
	return nil
}

func writeReconcileCSV(out io.Writer, by string, reports []reconcileReport) error {
	var header []string
	switch by {
	case "model":
		header = reconcileModelCSVHeader
	case "day":
		header = reconcileDayCSVHeader
	default:
		header = reconcileCategoryCSVHeader
	}
	var recs [][]string
	for _, r := range reports {
		switch by {
		case "model":
			for _, l := range r.ByModel {
				recs = append(recs, []string{r.Origin, r.KeyID, l.Model, num(l.Count), cost(l.LocalUSD), cost(l.FormulaUSD), cost(l.ChargedUSD)})
			}
		case "day":
			for _, l := range r.ByDay {
				recs = append(recs, []string{r.Origin, r.KeyID, l.Day.Format(time.RFC3339), num(l.Count),
					cost(l.LocalUSD), cost(l.FormulaUSD), cost(l.ChargedUSD),
					optCost(l.MultiplierDiffUSD), optCost(l.UsageDiffUSD), strconv.FormatBool(l.Unpriced)})
			}
		default:
			for _, l := range r.Lines {
				recs = append(recs, []string{r.Origin, r.KeyID, l.Category, num(l.Count),
					cost(l.LocalUSD), cost(l.FormulaUSD), cost(l.ChargedUSD), l.Note})
			}
		}
	}
	return writeCSV(out, header, recs)
}

// dash renders an empty string as "-", so a table column is never blank.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func layersText(layers []string) string {
	if len(layers) == 0 {
		return "off"
	}
	return strings.Join(layers, ",")
}

func syncText(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339)
}

func balanceText(b *relay.Balance) string {
	if b == nil {
		return "-"
	}
	if b.Unlimited {
		return "unlimited"
	}
	if b.RemainingUSD == nil {
		return "-"
	}
	if c := balanceCurrency(b); c != "USD" {
		return fmt.Sprintf("%.4f %s", *b.RemainingUSD, c)
	}
	return fmt.Sprintf("%.4f", *b.RemainingUSD)
}

// balanceMoney writes a balance amount with its own currency.
func balanceMoney(b *relay.Balance, v *float64) string {
	if c := balanceCurrency(b); c != "USD" && v != nil {
		return fmt.Sprintf("%.4f %s", *v, c)
	}
	return money(v)
}

func money(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("$%.4f", *v)
}

func optCost(v *float64) string {
	if v == nil {
		return ""
	}
	return cost(*v)
}

// formulaText prints a formula total; "-" when no charge had a computable
// list cost, a trailing "*" when only some did.
func formulaText(v float64, missing, count int64, unit string) string {
	switch {
	case missing > 0 && missing >= count:
		return "-"
	case missing > 0:
		return fmt.Sprintf("%s%.6f*", unit, v)
	}
	return fmt.Sprintf("%s%.6f", unit, v)
}
