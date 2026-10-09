package gui

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/zzstar/mytoken/internal/model"
	"github.com/zzstar/mytoken/internal/query"
)

// demo is a deterministic query.Service of made-up usage, for screenshots,
// tests and `mytoken --demo`.
type demo struct {
	sessions []query.SessionRow
	events   map[string][]query.AttributedEvent // by harness/session
}

type demoModel struct {
	provider, model string
	attrib          model.AttribSource
	in, out         float64 // $ per million
}

var demoModels = map[model.Harness][]demoModel{
	model.ClaudeCode: {{"Anthropic", "claude-sonnet-4-5", model.AttribConfig, 3, 15}, {"OpenRouter", "claude-opus-4-1", model.AttribCCSwitch, 15, 75}, {"Anthropic", "claude-haiku-4-5", model.AttribInferred, 1, 5}},
	model.Codex:      {{"OpenAI", "gpt-5-codex", model.AttribLog, 1.25, 10}, {"kami", "gpt-6-astra", model.AttribLog, 2, 12}},
	model.DSH:        {{"nerv-base", "deepseek-flash", model.AttribLog, 0.28, 0.42}, {"nerv-base", "step-5-preview", model.AttribLog, 0.5, 2}, {"kami-cn", "gpt-6-astra", model.AttribLog, 2, 12}},
	model.Pi:         {{"deepseek", "deepseek-chat", model.AttribLog, 0.28, 0.42}, {"moonshot", "kimi-k2", model.AttribLog, 0.6, 2.5}},
}

var demoTitles = []string{
	"给 MyToken 写一个纯原生的托盘面板", "修复 claude 解析器的流式去重", "把热力图改成 26 周", "为什么 cache hit 这么低",
	"refactor the store into a single writer", "add zstd support for DSH sessions", "排查 fsnotify 在 macOS 上丢事件",
	"做一个 Liquid Glass 风格的侧边栏", "write release workflow for goreleaser", "春日影 BPM 检测脚本", "迷星叫的和弦分析",
	"migrate cc-switch pricing import", "解释一下 Codex 的 token_count 事件", "port tokscale opencode parser", "整理 SPEC 里的归因链",
	"optimize SQLite indexes for time range", "给会话详情加时间线", "draw donut chart with Painter paths", "i18n: 中英双语",
	"benchmark first scan on 1300 files",
}

var demoProjects = []string{"~/Developer/Programs/MyToken!!!!!", "~/Developer/Programs/AniBand-Radar", "~/Developer/Programs/GBC-Club-CN", "~/Desktop/Programs/FISH", "~/Desktop/Programs/CPA"}

// NewDemoService returns a deterministic fake query.Service anchored at now.
func NewDemoService(now time.Time) query.Service {
	d := &demo{events: map[string][]query.AttributedEvent{}}
	rnd := uint32(7)
	next := func() float64 { rnd = rnd*1664525 + 1013904223; return float64(rnd>>8) / float64(1<<24) }
	harnesses := []model.Harness{model.ClaudeCode, model.Codex, model.DSH, model.Pi}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	id := 0
	for back := 300; back >= 0; back-- {
		date := day.AddDate(0, 0, -back)
		wk := date.Weekday()
		busy := 0.5 + 0.5*math.Sin(float64(back)/9) + next()*0.6
		if wk == time.Saturday || wk == time.Sunday {
			busy *= 0.35
		}
		if back > 120 {
			busy *= 0.4
		}
		if back > 0 && next() < 0.12 {
			continue
		}
		n := int(busy*3.2) + 1
		if back == 0 {
			n = 4
		}
		for k := 0; k < n; k++ {
			id++
			h := harnesses[int(next()*float64(len(harnesses)))%len(harnesses)]
			start := date.Add(time.Duration(9+next()*13) * time.Hour).Add(time.Duration(next()*60) * time.Minute)
			if back == 0 {
				start = now.Add(-time.Duration(20+next()*320) * time.Minute)
			}
			sid := fmt.Sprintf("%s-%04d", h, id)
			meta := model.SessionMeta{Harness: h, SessionID: sid, Title: demoTitles[id%len(demoTitles)], Project: demoProjects[int(next()*5)%5], StartedAt: start}
			reqs := 4 + int(next()*40*busy)
			ms := demoModels[h]
			t := start
			var evs []query.AttributedEvent
			for r := 0; r < reqs; r++ {
				m := ms[0]
				if x := next(); x > 0.7 && len(ms) > 1 {
					m = ms[1+int(next()*float64(len(ms)-1))%(len(ms)-1)]
				}
				t = t.Add(time.Duration(15+next()*120) * time.Second)
				if t.After(now) {
					t = now.Add(-time.Duration(next()*120) * time.Second)
				}
				ctx := 8000 + next()*60000 + float64(r)*1500
				tok := model.Tokens{
					Input:      int64(ctx * (0.04 + next()*0.1)),
					CacheRead:  int64(ctx * (0.75 + next()*0.15)),
					CacheWrite: int64(ctx * next() * 0.06),
					Output:     int64(200 + next()*2400),
				}
				if strings.Contains(m.model, "gpt") || strings.Contains(m.model, "deepseek") {
					tok.Reasoning = int64(next() * 1800)
					tok.CacheWrite = 0
				}
				cost := (float64(tok.Input)+float64(tok.CacheWrite)*1.25)*m.in/1e6 + float64(tok.CacheRead)*m.in*0.1/1e6 + float64(tok.Output+tok.Reasoning)*m.out/1e6
				evs = append(evs, query.AttributedEvent{
					UsageEvent:       model.UsageEvent{Harness: h, DedupKey: fmt.Sprintf("%s-%d", sid, r), SessionID: sid, ProjectPath: meta.Project, Timestamp: t.UTC(), Model: m.model, Tokens: tok},
					ResolvedProvider: m.provider, Attrib: m.attrib, Cost: cost, Priced: true,
				})
			}
			meta.UpdatedAt = t
			d.add(meta, evs)
			// Some sessions spawn subagents.
			if h != model.Pi && next() < 0.3 {
				kids := 1 + int(next()*3)
				for c := 0; c < kids; c++ {
					cid := fmt.Sprintf("%s-sub%d", sid, c)
					cm := model.SessionMeta{Harness: h, SessionID: cid, ParentID: sid, Title: []string{"Explore: 查找解析入口", "Review: 去重逻辑", "Task: 写 fixture 测试"}[c%3], Project: meta.Project, StartedAt: start.Add(time.Minute)}
					var cevs []query.AttributedEvent
					for r := 0; r < 3+int(next()*8); r++ {
						e := evs[int(next()*float64(len(evs)))%len(evs)]
						e.SessionID, e.ParentID = cid, sid
						e.DedupKey = fmt.Sprintf("%s-%d", cid, r)
						e.Tokens.Input /= 2
						e.Tokens.CacheRead /= 2
						e.Cost /= 2
						cevs = append(cevs, e)
					}
					cm.UpdatedAt = cevs[len(cevs)-1].Timestamp.Local()
					d.add(cm, cevs)
				}
			}
		}
	}
	// Roll children into parents.
	idx := map[string]int{}
	for i, s := range d.sessions {
		idx[string(s.Harness)+"/"+s.SessionID] = i
	}
	for _, s := range d.sessions {
		if s.ParentID == "" {
			continue
		}
		p := &d.sessions[idx[string(s.Harness)+"/"+s.ParentID]]
		p.Children++
		p.Tokens = p.Tokens.Add(s.Tokens)
		p.CostUSD += s.CostUSD
		p.Requests += s.Requests
		p.Breakdown = mergeBreakdown(p.Breakdown, s.Breakdown)
	}
	return d
}

func (d *demo) add(meta model.SessionMeta, evs []query.AttributedEvent) {
	row := query.SessionRow{SessionMeta: meta}
	var bd []query.ProviderModel
	for _, e := range evs {
		row.Tokens = row.Tokens.Add(e.Tokens)
		row.CostUSD += e.Cost
		row.Requests++
		bd = mergeBreakdown(bd, []query.ProviderModel{{Provider: e.ResolvedProvider, Model: e.Model, Attrib: e.Attrib, Tokens: e.Tokens, CostUSD: e.Cost, Requests: 1}})
	}
	row.Breakdown = bd
	d.sessions = append(d.sessions, row)
	d.events[string(meta.Harness)+"/"+meta.SessionID] = evs
}

func mergeBreakdown(a, b []query.ProviderModel) []query.ProviderModel {
	out := append([]query.ProviderModel(nil), a...)
	for _, x := range b {
		found := false
		for i := range out {
			if out[i].Provider == x.Provider && out[i].Model == x.Model {
				out[i].Tokens = out[i].Tokens.Add(x.Tokens)
				out[i].CostUSD += x.CostUSD
				out[i].Requests += x.Requests
				found = true
				break
			}
		}
		if !found {
			out = append(out, x)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Tokens.Total() > out[j].Tokens.Total() })
	return out
}

func inRange(r query.Range, t time.Time) bool {
	t = t.Local()
	return (r.From.IsZero() || !t.Before(r.From)) && (r.To.IsZero() || t.Before(r.To))
}

// each calls fn for every event (of every session) in f.
func (d *demo) each(f query.Filter, fn func(s *query.SessionRow, e *query.AttributedEvent)) {
	for i := range d.sessions {
		s := &d.sessions[i]
		for j := range d.events[string(s.Harness)+"/"+s.SessionID] {
			e := &d.events[string(s.Harness)+"/"+s.SessionID][j]
			if inRange(f.Range, e.Timestamp) {
				fn(s, e)
			}
		}
	}
}

func (d *demo) Totals(_ context.Context, f query.Filter) (query.Totals, error) {
	var t query.Totals
	seen := map[string]bool{}
	d.each(f, func(s *query.SessionRow, e *query.AttributedEvent) {
		t.Tokens = t.Tokens.Add(e.Tokens)
		t.CostUSD += e.Cost
		t.Requests++
		root := s.SessionID
		if s.ParentID != "" {
			root = s.ParentID
		}
		seen[root] = true
	})
	t.Sessions = int64(len(seen))
	if den := t.Tokens.Input + t.Tokens.CacheRead + t.Tokens.CacheWrite; den > 0 {
		t.CacheHit = float64(t.Tokens.CacheRead) / float64(den)
	}
	return t, nil
}

func (d *demo) series(f query.Filter, step func(time.Time) time.Time, trunc func(time.Time) time.Time) []query.Point {
	from, to := f.Range.From, f.Range.To
	if from.IsZero() {
		from = trunc(time.Now().AddDate(0, 0, -300))
	}
	if to.IsZero() {
		to = step(trunc(time.Now()))
	}
	var pts []query.Point
	idx := map[time.Time]int{}
	for t := trunc(from); t.Before(to); t = step(t) {
		idx[t] = len(pts)
		pts = append(pts, query.Point{Day: t})
	}
	d.each(f, func(_ *query.SessionRow, e *query.AttributedEvent) {
		if i, ok := idx[trunc(e.Timestamp.Local())]; ok {
			pts[i].Tokens = pts[i].Tokens.Add(e.Tokens)
			pts[i].CostUSD += e.Cost
		}
	})
	return pts
}

func (d *demo) Daily(_ context.Context, f query.Filter) ([]query.Point, error) {
	return d.series(f, func(t time.Time) time.Time { return t.AddDate(0, 0, 1) },
		func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local) }), nil
}

func (d *demo) Hourly(_ context.Context, f query.Filter) ([]query.Point, error) {
	return d.series(f, func(t time.Time) time.Time { return t.Add(time.Hour) },
		func(t time.Time) time.Time {
			return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.Local)
		}), nil
}

func (d *demo) by(f query.Filter, key func(s *query.SessionRow, e *query.AttributedEvent) (string, string)) []query.Bucket {
	m := map[string]*query.Bucket{}
	sess := map[string]map[string]bool{}
	d.each(f, func(s *query.SessionRow, e *query.AttributedEvent) {
		k, l := key(s, e)
		b := m[k]
		if b == nil {
			b = &query.Bucket{Key: k, Label: l}
			m[k] = b
			sess[k] = map[string]bool{}
		}
		b.Tokens = b.Tokens.Add(e.Tokens)
		b.CostUSD += e.Cost
		b.Requests++
		sess[k][s.SessionID] = true
	})
	out := make([]query.Bucket, 0, len(m))
	for k, b := range m {
		b.Sessions = int64(len(sess[k]))
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tokens.Total() > out[j].Tokens.Total() })
	return out
}

func (d *demo) ByProvider(_ context.Context, f query.Filter) ([]query.Bucket, error) {
	return d.by(f, func(_ *query.SessionRow, e *query.AttributedEvent) (string, string) {
		return e.ResolvedProvider, e.ResolvedProvider
	}), nil
}

func (d *demo) ByModel(_ context.Context, f query.Filter) ([]query.Bucket, error) {
	return d.by(f, func(_ *query.SessionRow, e *query.AttributedEvent) (string, string) {
		return e.ResolvedProvider + "/" + e.Model, e.Model
	}), nil
}

func (d *demo) ByProject(_ context.Context, f query.Filter) ([]query.Bucket, error) {
	return d.by(f, func(s *query.SessionRow, _ *query.AttributedEvent) (string, string) {
		return s.Project, baseName(s.Project)
	}), nil
}

func (d *demo) ByHarness(_ context.Context, f query.Filter) ([]query.Bucket, error) {
	return d.by(f, func(s *query.SessionRow, _ *query.AttributedEvent) (string, string) {
		return string(s.Harness), s.Harness.DisplayName()
	}), nil
}

func (d *demo) Sessions(_ context.Context, f query.Filter, sortKey string, limit, offset int) ([]query.SessionRow, int, error) {
	var rows []query.SessionRow
	for _, s := range d.sessions {
		if s.ParentID == "" && (f.Range.From.IsZero() || inRange(f.Range, s.UpdatedAt)) {
			rows = append(rows, s)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		switch sortKey {
		case query.SortTokens:
			return rows[i].Tokens.Total() > rows[j].Tokens.Total()
		case query.SortCost:
			return rows[i].CostUSD > rows[j].CostUSD
		}
		return rows[i].UpdatedAt.After(rows[j].UpdatedAt)
	})
	total := len(rows)
	if offset > len(rows) {
		offset = len(rows)
	}
	rows = rows[offset:]
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, total, nil
}

func (d *demo) Session(_ context.Context, h model.Harness, id string) (query.SessionRow, []query.SessionRow, []query.AttributedEvent, error) {
	var row query.SessionRow
	var kids []query.SessionRow
	for _, s := range d.sessions {
		if s.Harness != h {
			continue
		}
		if s.SessionID == id {
			row = s
		} else if s.ParentID == id {
			kids = append(kids, s)
		}
	}
	return row, kids, d.events[string(h)+"/"+id], nil
}

func (d *demo) Subscribe() (<-chan struct{}, func()) {
	return make(chan struct{}), func() {}
}

// demoSettings is an in-memory query.Settings for previews: two made-up
// relay aliases lack a price until mapped.
type demoSettings struct {
	rules    []query.PriceRule
	aliases  []query.ModelAlias
	unpriced []query.UnpricedModel
}

// NewDemoSettings returns pricing settings to go with NewDemoService.
func NewDemoSettings() query.Settings {
	m := 1.0
	return &demoSettings{
		rules:   []query.PriceRule{{Provider: "kami-cn", Multiplier: 0.3, Source: "user"}, {Provider: "nerv-base", Model: "step-5-preview", Multiplier: m, Source: "cc-switch"}},
		aliases: []query.ModelAlias{{From: "opus-max", To: "claude-opus-4-5", Provider: "kami-cn"}},
		unpriced: []query.UnpricedModel{
			{Model: "claude-opus-5-5-high", Provider: "kami-cn", Requests: 42, Tokens: 2_720_000, Suggestions: []string{"claude-opus-4-5", "claude-opus-4-1", "claude-sonnet-4-5"}},
			{Model: "deepseek-v41", Provider: "nerv-base", Requests: 17, Tokens: 860_000, Suggestions: []string{"deepseek-v4.1-flash", "deepseek-chat"}},
		},
	}
}

func (d *demoSettings) PriceRules(context.Context) ([]query.PriceRule, error) { return d.rules, nil }
func (d *demoSettings) SetPriceRules(_ context.Context, r []query.PriceRule) error {
	d.rules = r
	return nil
}
func (d *demoSettings) ImportCCSwitch(context.Context) (int, error) { return 0, nil }
func (d *demoSettings) Providers(context.Context) ([]string, error) {
	return []string{"anthropic", "kami-cn", "nerv-base", "openai", "deepseek", "moonshot"}, nil
}
func (d *demoSettings) ModelAliases(context.Context) ([]query.ModelAlias, error) {
	return d.aliases, nil
}
func (d *demoSettings) SetModelAliases(_ context.Context, a []query.ModelAlias) error {
	d.aliases = a
	keep := d.unpriced[:0]
	for _, u := range d.unpriced {
		mapped := false
		for _, x := range a {
			mapped = mapped || x.From == u.Model
		}
		if !mapped {
			keep = append(keep, u)
		}
	}
	d.unpriced = keep
	return nil
}
func (d *demoSettings) UnpricedModels(context.Context) ([]query.UnpricedModel, error) {
	return d.unpriced, nil
}
func (d *demoSettings) SearchCatalog(_ context.Context, q string, limit int) ([]string, error) {
	var out []string
	for _, id := range []string{"claude-opus-4-5", "claude-opus-4-1", "claude-sonnet-4-5", "gpt-5-codex", "deepseek-chat", "deepseek-v4.1-flash", "kimi-k2"} {
		if strings.Contains(id, strings.ToLower(q)) && len(out) < limit {
			out = append(out, id)
		}
	}
	return out, nil
}
