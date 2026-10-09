package gui

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// Span is a range choice of the overview.
type Span int

const (
	SpanToday Span = iota
	Span7
	Span30
	Span90
	SpanAll
)

var spanKeys = []string{"today", "7d", "30d", "90d", "all"}

// rangeOf returns the local-time range of a span ending today.
func rangeOf(s Span, now time.Time) query.Range {
	now = now.Local()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	end := day.AddDate(0, 0, 1)
	switch s {
	case SpanToday:
		return query.Range{From: day, To: end}
	case Span7:
		return query.Range{From: day.AddDate(0, 0, -6), To: end}
	case Span30:
		return query.Range{From: day.AddDate(0, 0, -29), To: end}
	case Span90:
		return query.Range{From: day.AddDate(0, 0, -89), To: end}
	}
	return query.Range{}
}

// prevRange is the range of equal length just before r.
func prevRange(r query.Range) query.Range {
	if r.From.IsZero() {
		return query.Range{}
	}
	d := r.To.Sub(r.From)
	return query.Range{From: r.From.Add(-d), To: r.From}
}

// Overview is everything the overview page and the tray panel draw.
type Overview struct {
	Span      Span
	Totals    query.Totals
	Prev      query.Totals
	Daily     []query.Point // the span's days (for Today: the last 14 days)
	Hourly    []query.Point // last 24h
	Heat      []query.Point // last 26 weeks, daily
	Models    []query.Bucket
	Providers []query.Bucket
	Harnesses []query.Bucket
	Projects  []query.Bucket
	Active    []query.SessionRow // most recently updated sessions
	Loaded    time.Time
}

// loadOverview queries everything for span s.
func loadOverview(ctx context.Context, q query.Service, s Span, now time.Time) (Overview, error) {
	r := rangeOf(s, now)
	f := query.Filter{Range: r}
	o := Overview{Span: s, Loaded: now}
	var err error
	if o.Totals, err = q.Totals(ctx, f); err != nil {
		return o, err
	}
	if pr := prevRange(r); !pr.From.IsZero() {
		o.Prev, _ = q.Totals(ctx, query.Filter{Range: pr})
	}
	dr := r
	day := rangeOf(SpanToday, now)
	switch s {
	case SpanToday:
		dr = query.Range{From: day.From.AddDate(0, 0, -13), To: day.To}
	case SpanAll:
		dr = query.Range{From: day.From.AddDate(0, 0, -179), To: day.To}
	}
	if o.Daily, err = q.Daily(ctx, query.Filter{Range: dr}); err != nil {
		return o, err
	}
	hr := now.Truncate(time.Hour).Add(time.Hour)
	o.Hourly, _ = q.Hourly(ctx, query.Filter{Range: query.Range{From: hr.Add(-24 * time.Hour), To: hr}})
	o.Heat, _ = q.Daily(ctx, query.Filter{Range: query.Range{From: heatStart(now), To: day.To}})
	o.Models, _ = q.ByModel(ctx, f)
	o.Providers, _ = q.ByProvider(ctx, f)
	o.Harnesses, _ = q.ByHarness(ctx, f)
	o.Projects, _ = q.ByProject(ctx, f)
	o.Active, _, _ = q.Sessions(ctx, query.Filter{}, query.SortRecent, 6, 0)
	return o, nil
}

// heatStart is the Monday 52 weeks before this week's Monday: up to 53 columns;
// the heatmap shows as many recent ones as fit.
func heatStart(now time.Time) time.Time {
	now = now.Local()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	wd := (int(day.Weekday()) + 6) % 7 // Monday = 0
	return day.AddDate(0, 0, -wd-7*52)
}

// SessionPage is the sessions list.
type SessionPage struct {
	Rows  []query.SessionRow
	Total int
}

// SessionDetail is one session opened.
type SessionDetail struct {
	Row      query.SessionRow
	Children []query.SessionRow
	Events   []query.AttributedEvent
}

func loadSessions(ctx context.Context, q query.Service, sortKey string) (SessionPage, error) {
	rows, total, err := q.Sessions(ctx, query.Filter{}, sortKey, 2000, 0)
	return SessionPage{Rows: rows, Total: total}, err
}

func loadSession(ctx context.Context, q query.Service, h model.Harness, id string) (SessionDetail, error) {
	row, kids, evs, err := q.Session(ctx, h, id)
	return SessionDetail{Row: row, Children: kids, Events: evs}, err
}

// filterRows keeps the rows matching every word of needle.
func filterRows(rows []query.SessionRow, needle string) []query.SessionRow {
	needle = strings.TrimSpace(strings.ToLower(needle))
	if needle == "" {
		return rows
	}
	words := strings.Fields(needle)
	out := rows[:0:0]
	for _, r := range rows {
		hay := strings.ToLower(r.Title + " " + r.Project + " " + string(r.Harness) + " " + r.Harness.DisplayName())
		for _, b := range r.Breakdown {
			hay += " " + strings.ToLower(b.Provider+" "+b.Model)
		}
		ok := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out
}

// streak is the number of consecutive active days ending today (or yesterday).
func streak(days []query.Point) int {
	n := 0
	for i := len(days) - 1; i >= 0; i-- {
		if days[i].Tokens.Total() == 0 {
			if i == len(days)-1 {
				continue
			}
			break
		}
		n++
	}
	return n
}

// busiest returns the day with most tokens.
func busiest(days []query.Point) (query.Point, bool) {
	var best query.Point
	ok := false
	for _, d := range days {
		if d.Tokens.Total() > best.Tokens.Total() {
			best, ok = d, true
		}
	}
	return best, ok
}

// topN sorts buckets by tokens and keeps n, folding the rest into "other".
func topN(bs []query.Bucket, n int, other string) []query.Bucket {
	bs = append([]query.Bucket(nil), bs...)
	sort.SliceStable(bs, func(i, j int) bool { return bs[i].Tokens.Total() > bs[j].Tokens.Total() })
	if len(bs) <= n {
		return bs
	}
	rest := query.Bucket{Key: "_other", Label: other}
	for _, b := range bs[n:] {
		rest.Tokens = rest.Tokens.Add(b.Tokens)
		rest.CostUSD += b.CostUSD
		rest.Requests += b.Requests
		rest.Sessions += b.Sessions
	}
	return append(bs[:n], rest)
}
