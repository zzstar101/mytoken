package gui

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// sourceState holds the harnesses with indexed requests, all time, most
// requests first.
type sourceState struct {
	loaded bool
	rows   []query.Bucket
	err    error
}

func (s *State) loadSources() {
	s.run("sources", func(ctx context.Context) func() {
		f := query.Filter{Range: query.Range{From: time.Unix(0, 0), To: s.Hooks.Now().AddDate(0, 0, 1)}}
		buckets, err := s.Q.ByHarness(ctx, f)
		rows := buckets[:0:0]
		for _, b := range buckets {
			if b.Requests > 0 {
				rows = append(rows, b)
			}
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Requests > rows[j].Requests })
		return func() { s.src = sourceState{loaded: true, rows: rows, err: err} }
	})
}

// sourcesCard lists the harnesses with indexed requests, and the roots
// that hold their logs; the supported ones without data are only counted.
func (s *State) sourcesCard(c *ui.Context, pal palette) {
	if !s.src.loaded {
		s.src.loaded = true
		s.loadSources()
	}
	card(c, pal, tr("sources"), nil, func() {
		ui.Text(c, tr("sourcesSub")).FontSize(12).TextColor(pal.muted)
		if s.src.err != nil {
			ui.Text(c, s.src.err.Error()).FontSize(12).TextColor(Soyo)
		}
		for _, b := range s.src.rows {
			h := model.Harness(b.Key)
			ui.Row(c.Key(b.Key)).Gap(10).Padding(6, 0).AlignItems(ui.Start).Children(func() {
				harnessMark(c, h, 16)
				ui.Column(c).Grow(1).Basis(0).Gap(3).Children(func() {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Text(c, h.DisplayName()).FontSize(13).FontWeight(650).TextColor(pal.ink).Grow(1)
						ui.Text(c, fmt.Sprintf(tr("sourceRequests"), fmtInt(b.Requests))).FontSize(11.5).TextColor(pal.muted).FontFeatures("tnum")
					})
					if p, ok := harness.Get(h); ok {
						for _, r := range p.Roots() {
							if exists(r) {
								ui.Text(c.Key(r), shortPath(r)).FontSize(11.5).TextColor(pal.muted).SingleLine().Ellipsis("…")
							}
						}
					}
				})
			})
		}
		if idle := len(harness.All()) - len(s.src.rows); s.src.err == nil && idle > 0 {
			ui.Text(c, fmt.Sprintf(tr("sourcesIdle"), idle)).FontSize(11.5).TextColor(pal.muted.Alpha(0.7))
		}
	})
}

// aboutVersion is the version shown in the about card.
func aboutVersion(v string) string {
	if v == "" {
		return "dev"
	}
	return "v" + v
}
