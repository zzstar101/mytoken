package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
	"sort"
	"strings"
	"sync"
	"time"
)

type service struct{ st *store.Store }

func NewService(st *store.Store) Service { return &service{st: st} }

var _ Service = (*service)(nil)

type sessionKey struct {
	h  model.Harness
	id string
}
type data struct {
	events []AttributedEvent
	meta   map[sessionKey]model.SessionMeta
}

func (s *service) load(ctx context.Context, f Filter) (data, error) {
	d := data{meta: map[sessionKey]model.SessionMeta{}}
	tx, e := s.st.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return d, e
	}
	defer tx.Rollback()
	rows, e := tx.QueryContext(ctx, "SELECT harness,session_id,parent_id,title,project,started_at,updated_at FROM sessions")
	if e != nil {
		return d, e
	}
	for rows.Next() {
		var m model.SessionMeta
		var start, end int64
		if e = rows.Scan(&m.Harness, &m.SessionID, &m.ParentID, &m.Title, &m.Project, &start, &end); e != nil {
			rows.Close()
			return d, e
		}
		if start != 0 {
			m.StartedAt = time.Unix(0, start).UTC()
		}
		if end != 0 {
			m.UpdatedAt = time.Unix(0, end).UTC()
		}
		d.meta[sessionKey{m.Harness, m.SessionID}] = m
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return d, e
	}
	var raw string
	var aliases []ModelAlias
	e = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='model-aliases'").Scan(&raw)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return d, e
	}
	if raw != "" {
		if e = json.Unmarshal([]byte(raw), &aliases); e != nil {
			return d, e
		}
	}
	aliasLookup := aliasMap(aliases)
	models := f.Models
	f.Models = nil
	where, args := filterSQL(f)
	rows, e = tx.QueryContext(ctx, `SELECT harness,dedup_key,session_id,parent_id,project,timestamp,model,provider,base_url,input,output,cache_read,cache_write,reasoning,log_cost,resolved_provider,attrib,cost,priced FROM events`+where+" ORDER BY timestamp,harness,dedup_key", args...)
	if e != nil {
		return d, e
	}
	for rows.Next() {
		var v AttributedEvent
		var at int64
		var cost sql.NullFloat64
		if e = rows.Scan(&v.Harness, &v.DedupKey, &v.SessionID, &v.ParentID, &v.ProjectPath, &at, &v.Model, &v.Provider, &v.BaseURL, &v.Tokens.Input, &v.Tokens.Output, &v.Tokens.CacheRead, &v.Tokens.CacheWrite, &v.Tokens.Reasoning, &cost, &v.ResolvedProvider, &v.Attrib, &v.Cost, &v.Priced); e != nil {
			rows.Close()
			return d, e
		}
		v.Timestamp = time.Unix(0, at).UTC()
		if cost.Valid {
			x := cost.Float64
			v.CostUSD = &x
		}
		if target, ok := aliasLookup[[2]string{v.ResolvedProvider, v.Model}]; ok {
			v.Model = target
		} else if target, ok := aliasLookup[[2]string{"", v.Model}]; ok {
			v.Model = target
		}
		if len(models) > 0 {
			matches := false
			for _, name := range models {
				if name == v.Model {
					matches = true
					break
				}
			}
			if !matches {
				continue
			}
		}
		d.events = append(d.events, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return d, e
	}
	return d, tx.Commit()
}
func filterSQL(f Filter) (string, []any) {
	var clauses []string
	var args []any
	if !f.Range.From.IsZero() {
		clauses = append(clauses, "timestamp>=?")
		args = append(args, f.Range.From.UnixNano())
	}
	if !f.Range.To.IsZero() {
		clauses = append(clauses, "timestamp<?")
		args = append(args, f.Range.To.UnixNano())
	}
	add := func(col string, values []string) {
		if len(values) == 0 {
			return
		}
		clauses = append(clauses, col+" IN ("+strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")+")")
		for _, v := range values {
			args = append(args, v)
		}
	}
	hs := make([]string, len(f.Harnesses))
	for i, h := range f.Harnesses {
		hs[i] = string(h)
	}
	add("harness", hs)
	add("resolved_provider", f.Providers)
	add("model", f.Models)
	if f.Project != "" {
		clauses = append(clauses, "project=?")
		args = append(args, f.Project)
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}
func (d data) root(k sessionKey) sessionKey {
	seen := map[sessionKey]bool{}
	for !seen[k] {
		seen[k] = true
		m, ok := d.meta[k]
		if !ok || m.ParentID == "" {
			return k
		}
		k = sessionKey{k.h, m.ParentID}
	}
	return k
}
func (s *service) Totals(ctx context.Context, f Filter) (Totals, error) {
	d, e := s.load(ctx, f)
	var out Totals
	if e != nil {
		return out, e
	}
	sessions := map[sessionKey]bool{}
	for _, v := range d.events {
		out.Tokens = out.Tokens.Add(v.Tokens)
		if v.Priced {
			out.CostUSD += v.Cost
		} else {
			out.Unpriced++
		}
		out.Requests++
		sessions[d.root(sessionKey{v.Harness, v.SessionID})] = true
	}
	out.Sessions = int64(len(sessions))
	denom := out.Tokens.Input + out.Tokens.CacheRead + out.Tokens.CacheWrite
	if denom > 0 {
		out.CacheHit = float64(out.Tokens.CacheRead) / float64(denom)
	}
	return out, nil
}
func (s *service) Daily(ctx context.Context, f Filter) ([]Point, error) {
	return s.series(ctx, f, false)
}
func (s *service) Hourly(ctx context.Context, f Filter) ([]Point, error) {
	return s.series(ctx, f, true)
}
func floor(t time.Time, hourly bool) time.Time {
	t = t.In(time.Local)
	if hourly {
		return t.Add(-time.Duration(t.Minute())*time.Minute - time.Duration(t.Second())*time.Second - time.Duration(t.Nanosecond()))
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}
func step(t time.Time, hourly bool) time.Time {
	if hourly {
		return t.Add(time.Hour)
	}
	return t.AddDate(0, 0, 1)
}
func (s *service) series(ctx context.Context, f Filter, hourly bool) ([]Point, error) {
	d, e := s.load(ctx, f)
	if e != nil {
		return nil, e
	}
	from, to := f.Range.From, f.Range.To
	if from.IsZero() {
		if len(d.events) == 0 {
			return []Point{}, nil
		}
		from = d.events[0].Timestamp
	}
	if to.IsZero() {
		if len(d.events) == 0 {
			return []Point{}, nil
		}
		to = step(floor(d.events[len(d.events)-1].Timestamp, hourly), hourly)
	}
	out := []Point{}
	indices := map[int64]int{}
	for at := floor(from, hourly); at.Before(to); at = step(at, hourly) {
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		indices[at.UnixNano()] = len(out)
		out = append(out, Point{Day: at})
	}
	for _, v := range d.events {
		if i, ok := indices[floor(v.Timestamp, hourly).UnixNano()]; ok {
			out[i].Tokens = out[i].Tokens.Add(v.Tokens)
			if v.Priced {
				out[i].CostUSD += v.Cost
			}
		}
	}
	return out, nil
}
func (s *service) ByProvider(ctx context.Context, f Filter) ([]Bucket, error) {
	return s.buckets(ctx, f, "provider")
}
func (s *service) ByModel(ctx context.Context, f Filter) ([]Bucket, error) {
	return s.buckets(ctx, f, "model")
}
func (s *service) ByProject(ctx context.Context, f Filter) ([]Bucket, error) {
	return s.buckets(ctx, f, "project")
}
func (s *service) ByHarness(ctx context.Context, f Filter) ([]Bucket, error) {
	return s.buckets(ctx, f, "harness")
}
func (s *service) buckets(ctx context.Context, f Filter, kind string) ([]Bucket, error) {
	d, e := s.load(ctx, f)
	if e != nil {
		return nil, e
	}
	groups := map[string]*Bucket{}
	seen := map[string]map[sessionKey]bool{}
	for _, v := range d.events {
		var key, label string
		switch kind {
		case "provider":
			key = v.ResolvedProvider
		case "model":
			key = v.Model
		case "project":
			key = v.ProjectPath
		case "harness":
			key = string(v.Harness)
			label = v.Harness.DisplayName()
		}
		if label == "" {
			label = key
		}
		b := groups[key]
		if b == nil {
			b = &Bucket{Key: key, Label: label}
			groups[key] = b
			seen[key] = map[sessionKey]bool{}
		}
		b.Tokens = b.Tokens.Add(v.Tokens)
		if v.Priced {
			b.CostUSD += v.Cost
		} else {
			b.Unpriced++
		}
		b.Requests++
		seen[key][d.root(sessionKey{v.Harness, v.SessionID})] = true
	}
	out := make([]Bucket, 0, len(groups))
	for k, b := range groups {
		b.Sessions = int64(len(seen[k]))
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tokens.Total() == out[j].Tokens.Total() {
			return out[i].Key < out[j].Key
		}
		return out[i].Tokens.Total() > out[j].Tokens.Total()
	})
	return out, nil
}
func (d data) rows() map[sessionKey]*SessionRow {
	rows := map[sessionKey]*SessionRow{}
	breakdowns := map[sessionKey]map[[2]string]*ProviderModel{}
	for k, m := range d.meta {
		rows[k] = &SessionRow{SessionMeta: m, Breakdown: []ProviderModel{}}
		breakdowns[k] = map[[2]string]*ProviderModel{}
	}
	for k, m := range d.meta {
		if m.ParentID != "" {
			if p := rows[sessionKey{k.h, m.ParentID}]; p != nil {
				p.Children++
			}
		}
	}
	for _, e := range d.events {
		k := sessionKey{e.Harness, e.SessionID}
		seen := map[sessionKey]bool{}
		for !seen[k] {
			seen[k] = true
			r := rows[k]
			if r == nil {
				break
			}
			r.Tokens = r.Tokens.Add(e.Tokens)
			if e.Priced {
				r.CostUSD += e.Cost
			} else {
				r.Unpriced++
			}
			r.Requests++
			if e.Timestamp.After(r.UpdatedAt) {
				r.UpdatedAt = e.Timestamp
			}
			if r.StartedAt.IsZero() || e.Timestamp.Before(r.StartedAt) {
				r.StartedAt = e.Timestamp
			}
			pk := [2]string{e.ResolvedProvider, e.Model}
			b := breakdowns[k][pk]
			if b == nil {
				b = &ProviderModel{Provider: pk[0], Model: pk[1], Attrib: e.Attrib}
				breakdowns[k][pk] = b
			}
			b.Tokens = b.Tokens.Add(e.Tokens)
			if e.Priced {
				b.CostUSD += e.Cost
			} else {
				b.Unpriced++
			}
			b.Requests++
			if r.ParentID == "" {
				break
			}
			k = sessionKey{k.h, r.ParentID}
		}
	}
	for k, r := range rows {
		for _, b := range breakdowns[k] {
			r.Breakdown = append(r.Breakdown, *b)
		}
		sort.Slice(r.Breakdown, func(i, j int) bool {
			a, b := r.Breakdown[i], r.Breakdown[j]
			if a.Tokens.Total() != b.Tokens.Total() {
				return a.Tokens.Total() > b.Tokens.Total()
			}
			if a.Provider != b.Provider {
				return a.Provider < b.Provider
			}
			return a.Model < b.Model
		})
	}
	return rows
}
func (s *service) Sessions(ctx context.Context, f Filter, order string, limit, offset int) ([]SessionRow, int, error) {
	d, e := s.load(ctx, f)
	if e != nil {
		return nil, 0, e
	}
	out := []SessionRow{}
	for _, r := range d.rows() {
		if r.ParentID == "" && r.Requests > 0 {
			out = append(out, *r)
		}
	}
	sortRows(out, order)
	total := len(out)
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return []SessionRow{}, total, nil
	}
	out = out[offset:]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, total, nil
}
func sortRows(rows []SessionRow, order string) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch order {
		case SortTokens:
			if a.Tokens.Total() != b.Tokens.Total() {
				return a.Tokens.Total() > b.Tokens.Total()
			}
		case SortCost:
			if a.CostUSD != b.CostUSD {
				return a.CostUSD > b.CostUSD
			}
		default:
			if !a.UpdatedAt.Equal(b.UpdatedAt) {
				return a.UpdatedAt.After(b.UpdatedAt)
			}
		}
		if a.Harness != b.Harness {
			return a.Harness < b.Harness
		}
		return a.SessionID < b.SessionID
	})
}
func (s *service) Session(ctx context.Context, h model.Harness, id string) (SessionRow, []SessionRow, []AttributedEvent, error) {
	d, e := s.load(ctx, Filter{Harnesses: []model.Harness{h}})
	if e != nil {
		return SessionRow{}, nil, nil, e
	}
	rows := d.rows()
	r := rows[sessionKey{h, id}]
	if r == nil {
		return SessionRow{}, nil, nil, sql.ErrNoRows
	}
	children := []SessionRow{}
	for _, v := range rows {
		if v.Harness == h && v.ParentID == id {
			children = append(children, *v)
		}
	}
	sortRows(children, SortRecent)
	events := []AttributedEvent{}
	for _, v := range d.events {
		if v.SessionID == id {
			events = append(events, v)
		}
	}
	return *r, children, events, nil
}
func (s *service) Subscribe() (<-chan struct{}, func()) {
	src, unsubscribe := s.st.Subscribe()
	out := make(chan struct{}, 1)
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(out)
		var last time.Time
		var timer *time.Timer
		var tick <-chan time.Time
		defer func() {
			if timer != nil {
				timer.Stop()
			}
		}()
		send := func() {
			select {
			case out <- struct{}{}:
			default:
			}
			last = time.Now()
		}
		for {
			select {
			case <-done:
				return
			case _, ok := <-src:
				if !ok {
					return
				}
				remaining := time.Second - time.Since(last)
				if remaining <= 0 {
					send()
				} else if tick == nil {
					timer = time.NewTimer(remaining)
					tick = timer.C
				}
			case <-tick:
				tick = nil
				send()
			}
		}
	}()
	return out, func() { once.Do(func() { close(done); unsubscribe() }) }
}

var _ = errors.Is
