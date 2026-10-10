package cli

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// statsReport is the --format json payload of stats.
type statsReport struct {
	Totals         query.Totals `json:"totals"`
	By             string       `json:"by"`
	Rows           any          `json:"rows"`
	UnpricedModels []string     `json:"unpricedModels"`
}

// sessionsReport is the --format json payload of sessions.
type sessionsReport struct {
	Total    int                `json:"total"`
	Sessions []query.SessionRow `json:"sessions"`
}

// CSV headers. They are stable per view: adding a column appends to the end,
// and cost_usd is dropped (not blanked) under --no-cost.
var (
	bucketCSVHeader  = []string{"key", "label", "requests", "sessions", "tokens", "input", "output", "cache_read", "cache_write", "reasoning", "cost_usd", "unpriced"}
	sessionCSVHeader = []string{"harness", "session_id", "updated_at", "requests", "tokens", "input", "output", "cache_read", "cache_write", "reasoning", "cost_usd", "unpriced", "title"}
	dayCSVHeader     = []string{"day", "tokens", "input", "output", "cache_read", "cache_write", "reasoning", "cost_usd"}
)

// stripCost removes every costUsd field from a JSON payload, at any depth
// (totals, rows and a session's provider breakdown). --no-cost only drops cost;
// counts such as unpriced are kept.
func stripCost(v any) (any, error) {
	raw, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	var out any
	if e = json.Unmarshal(raw, &out); e != nil {
		return nil, e
	}
	removeCost(out)
	return out, nil
}

func removeCost(v any) {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "costUsd")
		for _, x := range t {
			removeCost(x)
		}
	case []any:
		for _, x := range t {
			removeCost(x)
		}
	}
}

// csvHeaderFor drops the cost column under --no-cost.
func csvHeaderFor(header []string, noCost bool) []string {
	if !noCost {
		return header
	}
	out := make([]string, 0, len(header))
	for _, h := range header {
		if h != "cost_usd" {
			out = append(out, h)
		}
	}
	return out
}

func writeCSV(out io.Writer, header []string, rows [][]string) error {
	w := csv.NewWriter(out)
	if e := w.Write(header); e != nil {
		return e
	}
	for _, r := range rows {
		if e := w.Write(r); e != nil {
			return e
		}
	}
	w.Flush()
	return w.Error()
}

// num renders an integer without thousands separators.
func num(v int64) string { return strconv.FormatInt(v, 10) }

// cost renders a cost with six decimals and no thousands separators, so
// spreadsheets do not show float64 noise.
func cost(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }

func tokensCSV(t model.Tokens) []string {
	return []string{num(t.Total()), num(t.Input), num(t.Output), num(t.CacheRead), num(t.CacheWrite), num(t.Reasoning)}
}

func bucketCSVRow(b query.Bucket, noCost bool) []string {
	row := []string{b.Key, b.Label, num(b.Requests), num(b.Sessions)}
	row = append(row, tokensCSV(b.Tokens)...)
	if !noCost {
		row = append(row, cost(b.CostUSD))
	}
	return append(row, num(b.Unpriced))
}

func sessionCSVRow(s query.SessionRow, noCost bool) []string {
	row := []string{string(s.Harness), s.SessionID, s.UpdatedAt.Format(time.RFC3339), num(s.Requests)}
	row = append(row, tokensCSV(s.Tokens)...)
	if !noCost {
		row = append(row, cost(s.CostUSD))
	}
	return append(row, num(s.Unpriced), s.Title)
}

func dayCSVRow(p query.Point, noCost bool) []string {
	row := []string{p.Day.Format(time.RFC3339)}
	row = append(row, tokensCSV(p.Tokens)...)
	if !noCost {
		row = append(row, cost(p.CostUSD))
	}
	return row
}

// writeStatsTable prints the totals line and the table for one --by view.
func writeStatsTable(out io.Writer, total query.Totals, rows any, noCost bool) error {
	summary := fmt.Sprintf("Requests: %d  Sessions: %d  Tokens: %d", total.Requests, total.Sessions, total.Tokens.Total())
	if !noCost {
		summary += fmt.Sprintf("  Cost: $%.4f", total.CostUSD)
	}
	summary += fmt.Sprintf("  Cache hit: %.1f%%  Unpriced: %d\n", total.CacheHit*100, total.Unpriced)
	if _, e := io.WriteString(out, summary); e != nil {
		return e
	}
	w := newTable(out)
	switch values := rows.(type) {
	case []query.Bucket:
		if noCost {
			fmt.Fprintln(w, "NAME\tREQUESTS\tTOKENS\tUNPRICED")
		} else {
			fmt.Fprintln(w, "NAME\tREQUESTS\tTOKENS\tCOST USD\tUNPRICED")
		}
		for _, v := range values {
			if noCost {
				fmt.Fprintf(w, "%s\t%d\t%d\t%d\n", v.Label, v.Requests, v.Tokens.Total(), v.Unpriced)
			} else {
				fmt.Fprintf(w, "%s\t%d\t%d\t%.4f\t%d\n", v.Label, v.Requests, v.Tokens.Total(), v.CostUSD, v.Unpriced)
			}
		}
	case []query.SessionRow:
		if noCost {
			fmt.Fprintln(w, "NAME\tREQUESTS\tTOKENS\tUNPRICED")
		} else {
			fmt.Fprintln(w, "NAME\tREQUESTS\tTOKENS\tCOST USD\tUNPRICED")
		}
		for _, v := range values {
			name := fmt.Sprintf("%s/%s", v.Harness, v.SessionID)
			if noCost {
				fmt.Fprintf(w, "%s\t%d\t%d\t%d\n", name, v.Requests, v.Tokens.Total(), v.Unpriced)
			} else {
				fmt.Fprintf(w, "%s\t%d\t%d\t%.4f\t%d\n", name, v.Requests, v.Tokens.Total(), v.CostUSD, v.Unpriced)
			}
		}
	case []query.Point:
		if noCost {
			fmt.Fprintln(w, "NAME\tREQUESTS\tTOKENS\tUNPRICED")
		} else {
			fmt.Fprintln(w, "NAME\tREQUESTS\tTOKENS\tCOST USD\tUNPRICED")
		}
		for _, v := range values {
			if noCost {
				fmt.Fprintf(w, "%s\t-\t%d\t-\n", v.Day.Format("2006-01-02"), v.Tokens.Total())
			} else {
				fmt.Fprintf(w, "%s\t-\t%d\t%.4f\t-\n", v.Day.Format("2006-01-02"), v.Tokens.Total(), v.CostUSD)
			}
		}
	}
	return w.Flush()
}

// writeStatsCSV prints one --by view as CSV rows (no totals row).
func writeStatsCSV(out io.Writer, rows any, noCost bool) error {
	switch values := rows.(type) {
	case []query.Bucket:
		recs := make([][]string, 0, len(values))
		for _, v := range values {
			recs = append(recs, bucketCSVRow(v, noCost))
		}
		return writeCSV(out, csvHeaderFor(bucketCSVHeader, noCost), recs)
	case []query.SessionRow:
		recs := make([][]string, 0, len(values))
		for _, v := range values {
			recs = append(recs, sessionCSVRow(v, noCost))
		}
		return writeCSV(out, csvHeaderFor(sessionCSVHeader, noCost), recs)
	case []query.Point:
		recs := make([][]string, 0, len(values))
		for _, v := range values {
			recs = append(recs, dayCSVRow(v, noCost))
		}
		return writeCSV(out, csvHeaderFor(dayCSVHeader, noCost), recs)
	}
	return fmt.Errorf("no CSV view for %T", rows)
}

// writeSessionsTable prints the sessions table.
func writeSessionsTable(out io.Writer, rows []query.SessionRow, noCost bool) error {
	w := newTable(out)
	if noCost {
		fmt.Fprintln(w, "HARNESS\tSESSION\tUPDATED\tREQUESTS\tTOKENS\tTITLE")
	} else {
		fmt.Fprintln(w, "HARNESS\tSESSION\tUPDATED\tREQUESTS\tTOKENS\tCOST USD\tTITLE")
	}
	for _, v := range rows {
		updated := v.UpdatedAt.Format(time.RFC3339)
		if noCost {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%s\n", v.Harness, v.SessionID, updated, v.Requests, v.Tokens.Total(), v.Title)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%.4f\t%s\n", v.Harness, v.SessionID, updated, v.Requests, v.Tokens.Total(), v.CostUSD, v.Title)
		}
	}
	return w.Flush()
}

// writeSessionsCSV prints the sessions view as CSV rows.
func writeSessionsCSV(out io.Writer, rows []query.SessionRow, noCost bool) error {
	recs := make([][]string, 0, len(rows))
	for _, v := range rows {
		recs = append(recs, sessionCSVRow(v, noCost))
	}
	return writeCSV(out, csvHeaderFor(sessionCSVHeader, noCost), recs)
}
