package gui

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"

	"github.com/zzstar101/mytoken/internal/query"
)

// pricingState backs the settings page's pricing cards: models without a
// price (with one-click mappings), the mappings, and provider multipliers.
type pricingState struct {
	loaded    bool
	rules     []query.PriceRule
	aliases   []query.ModelAlias
	unpriced  []query.UnpricedModel
	providers []string
	err       error

	open    string   // the unpriced model whose catalog search is open
	needle  string   // its search text
	lastQ   string   // the last search sent
	hits    []string // its results
	mult    map[string]string
	busy    bool
	message string
}

func (s *State) settings() query.Settings { return s.Hooks.Settings }

func (s *State) loadPricing() {
	st := s.settings()
	if st == nil {
		return
	}
	s.run("pricing", func(ctx context.Context) func() {
		rules, err := st.PriceRules(ctx)
		aliases, err2 := st.ModelAliases(ctx)
		unpriced, err3 := st.UnpricedModels(ctx)
		providers, err4 := st.Providers(ctx)
		for _, e := range []error{err2, err3, err4} {
			if err == nil {
				err = e
			}
		}
		return func() {
			p := &s.pr
			p.rules, p.aliases, p.unpriced, p.providers, p.err, p.loaded = rules, aliases, unpriced, providers, err, true
			p.mult = map[string]string{}
			for _, r := range rules {
				if r.Provider != "" && r.Model == "" && r.Multiplier != 0 && r.Multiplier != 1 {
					p.mult[r.Provider] = trimNum(r.Multiplier, 3)
				}
			}
		}
	})
}

// mutate runs a settings change off the UI thread, then reloads everything
// (costs and model names may all have moved).
func (s *State) mutate(fn func(ctx context.Context, st query.Settings) (string, error)) {
	st := s.settings()
	if st == nil || s.pr.busy {
		return
	}
	s.pr.busy = true
	s.run("pricing-write", func(ctx context.Context) func() {
		msg, err := fn(ctx, st)
		return func() {
			s.pr.busy = false
			s.pr.message = msg
			if err != nil {
				s.pr.message = tr("failed") + ": " + err.Error()
			}
			s.loadPricing()
			s.Reload()
		}
	})
}

func (s *State) addAlias(from, provider, to string) {
	aliases := append([]query.ModelAlias(nil), s.pr.aliases...)
	aliases = append(aliases, query.ModelAlias{From: from, To: to, Provider: provider})
	s.pr.open = ""
	s.mutate(func(ctx context.Context, st query.Settings) (string, error) {
		return trf("mapped", from, to), st.SetModelAliases(ctx, aliases)
	})
}

func (s *State) removeAlias(i int) {
	aliases := append([]query.ModelAlias(nil), s.pr.aliases[:i]...)
	aliases = append(aliases, s.pr.aliases[i+1:]...)
	s.mutate(func(ctx context.Context, st query.Settings) (string, error) {
		return "", st.SetModelAliases(ctx, aliases)
	})
}

func (s *State) setMultiplier(provider, text string) {
	m, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "x")), 64)
	if err != nil || m <= 0 {
		s.pr.message = tr("badMultiplier")
		return
	}
	var rules []query.PriceRule
	found := false
	for _, r := range s.pr.rules {
		if r.Provider == provider && r.Model == "" && r.Input == nil && r.Output == nil {
			found = true
			if m == 1 {
				continue // 1× is no rule at all
			}
			r.Multiplier, r.Source = m, "user"
		}
		rules = append(rules, r)
	}
	if !found && m != 1 {
		rules = append(rules, query.PriceRule{Provider: provider, Multiplier: m, Source: "user"})
	}
	s.mutate(func(ctx context.Context, st query.Settings) (string, error) {
		return trf("multiplierSet", provider, trimNum(m, 3)), st.SetPriceRules(ctx, rules)
	})
}

func (s *State) searchCatalog() {
	st := s.settings()
	q := strings.TrimSpace(s.pr.needle)
	if st == nil || q == s.pr.lastQ {
		return
	}
	s.pr.lastQ = q
	if q == "" {
		s.pr.hits = nil
		return
	}
	s.run("catalog", func(ctx context.Context) func() {
		hits, _ := st.SearchCatalog(ctx, q, 8)
		return func() {
			if strings.TrimSpace(s.pr.needle) == q {
				s.pr.hits = hits
			}
		}
	})
}

// pricingCards are the settings page's pricing section.
func (s *State) pricingCards(c *ui.Context, pal palette) {
	if s.settings() == nil {
		return
	}
	if !s.pr.loaded {
		s.pr.loaded = true // once: the load sets it again with the data
		s.loadPricing()
	}
	p := &s.pr

	// Models without a price, each with suggested mappings.
	card(c, pal, tr("unpricedTitle"), func() {
		if ui.Button(c, tr("importCCSwitch")).Clicked() {
			s.mutate(func(ctx context.Context, st query.Settings) (string, error) {
				n, err := st.ImportCCSwitch(ctx)
				return trf("imported", n), err
			})
		}
	}, func() {
		if p.message != "" {
			ui.Row(c).Padding(8, 10).Radius(10).Background(Taki.Alpha(0.12)).Children(func() {
				ui.Text(c, p.message).FontSize(12).TextColor(pal.ink)
			})
		}
		if len(p.unpriced) == 0 {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				dot(c, Rana, 8)
				ui.Text(c, tr("allPriced")).FontSize(12.5).TextColor(pal.muted)
			})
			return
		}
		ui.Text(c, tr("unpricedSub")).FontSize(12).TextColor(pal.muted)
		for i, m := range p.unpriced {
			if i > 0 {
				ui.Divider(c)
			}
			s.unpricedRow(c.Key("u/"+m.Provider+"/"+m.Model), pal, m)
		}
	})

	if len(p.aliases) > 0 {
		card(c, pal, tr("aliasesTitle"), nil, func() {
			for i, a := range p.aliases {
				ui.Row(c.Key("a/"+a.Provider+"/"+a.From)).Gap(10).Padding(4, 0).AlignItems(ui.Center).Children(func() {
					ui.Text(c, a.From).FontSize(13).FontWeight(600).TextColor(pal.ink).SingleLine().Shrink(1).Ellipsis("…")
					ui.Text(c, "→").FontSize(13).TextColor(pal.muted)
					ui.Text(c, a.To).FontSize(13).FontWeight(650).TextColor(Taki).SingleLine().Shrink(1).Ellipsis("…")
					if a.Provider != "" {
						chipOn(c, pal, "@"+a.Provider, Tomori)
					}
					ui.Spacer(c)
					ui.Text(c, tr("remove")).FontSize(12).TextColor(Anon).Cursor(ui.CursorPointer).Role(ui.RoleButton).OnClick(func() { s.removeAlias(i) })
				})
			}
		})
	}

	// Relays often bill a fraction of list price: one multiplier per provider.
	if len(p.providers) > 0 {
		card(c, pal, tr("multipliersTitle"), nil, func() {
			ui.Text(c, tr("multipliersSub")).FontSize(12).TextColor(pal.muted)
			provs := append([]string(nil), p.providers...)
			sort.Strings(provs)
			for _, prov := range provs {
				ui.Row(c.Key("m/"+prov)).Gap(10).Padding(3, 0).AlignItems(ui.Center).Children(func() {
					dot(c, bandAt(len(prov)), 8)
					ui.Text(c, prov).FontSize(13).FontWeight(600).TextColor(pal.ink).Grow(1).Basis(0).SingleLine().Ellipsis("…")
					v, ok := p.mult[prov]
					if !ok {
						v = "1"
						p.mult[prov] = v
					}
					val := p.mult[prov]
					in := ui.TextInput(c, &val).Width(72).Placeholder("1").OnSubmit(func() { s.setMultiplier(prov, p.mult[prov]) })
					p.mult[prov] = val
					_ = in
					ui.Text(c, "×").FontSize(13).TextColor(pal.muted)
				})
			}
			custom := 0
			for _, r := range p.rules {
				if r.Model != "" {
					custom++
				}
			}
			if custom > 0 {
				ui.Text(c, trf("customPrices", custom)).FontSize(11.5).TextColor(pal.muted)
			}
		})
	}
}

func (s *State) unpricedRow(c *ui.Context, pal palette, m query.UnpricedModel) {
	p := &s.pr
	key := m.Provider + "/" + m.Model
	ui.Column(c).Gap(8).Padding(4, 0).Children(func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			dot(c, Soyo, 8)
			if m.Model == "" {
				ui.Text(c, tr("noModelName")).FontSize(13.5).FontWeight(650).TextColor(pal.muted).SingleLine()
			} else {
				ui.Text(c, m.Model).FontSize(13.5).FontWeight(650).TextColor(pal.ink).SingleLine().Shrink(1).Ellipsis("…")
			}
			if m.Provider != "" {
				ui.Text(c, "@"+m.Provider).FontSize(12).TextColor(pal.muted).SingleLine()
			}
			ui.Spacer(c)
			ui.Text(c, fmtTokens(m.Tokens)+" · "+trf("nReq", fmtInt(m.Requests))).FontSize(12).TextColor(pal.muted).FontFeatures("tnum").SingleLine()
		})
		if m.Model == "" {
			// Nothing to map: the log never named a model.
			ui.Text(c, tr("noModelNameSub")).FontSize(12).TextColor(pal.muted)
			return
		}
		ui.Row(c).Gap(6).AlignItems(ui.Center).Wrap().Children(func() {
			if len(m.Suggestions) > 0 {
				ui.Text(c, tr("mapTo")).FontSize(12).TextColor(pal.muted)
			}
			for _, sug := range m.Suggestions {
				suggestion(c.Key(sug), pal, sug, func() { s.addAlias(m.Model, m.Provider, sug) })
			}
			label := tr("other")
			if p.open == key {
				label = tr("cancel")
			}
			ui.Text(c, label).FontSize(12).FontWeight(600).TextColor(Tomori).Cursor(ui.CursorPointer).Role(ui.RoleButton).Padding(3, 6).OnClick(func() {
				if p.open == key {
					p.open = ""
					return
				}
				p.open, p.needle, p.lastQ, p.hits = key, "", "", nil
			})
		})
		if p.open == key {
			ui.SearchField(c, &p.needle).FillWidth().Label(tr("searchCatalog"))
			s.searchCatalog()
			if len(p.hits) > 0 {
				ui.Row(c).Gap(6).Wrap().Children(func() {
					for _, h := range p.hits {
						suggestion(c.Key("h/"+h), pal, h, func() { s.addAlias(m.Model, m.Provider, h) })
					}
				})
			} else if strings.TrimSpace(p.needle) != "" {
				ui.Text(c, tr("noMatch")).FontSize(12).TextColor(pal.muted)
			}
		}
	})
}

// suggestion is a clickable catalog model pill.
func suggestion(c *ui.Context, pal palette, name string, onClick func()) {
	e := ui.Row(c).Padding(3, 10).Radius(99).Shrink(0).Cursor(ui.CursorPointer).Role(ui.RoleButton).Label(name)
	bg := Taki.Alpha(0.12)
	if e.Hovered() {
		bg = Taki.Alpha(0.22)
	}
	e.Background(bg).OnClick(onClick).Children(func() {
		ink := Taki.Mix(ui.RGB(0, 0, 0), 0.25)
		if pal.dark {
			ink = Taki.Mix(ui.RGB(255, 255, 255), 0.4)
		}
		ui.Text(c, name).FontSize(12).FontWeight(600).TextColor(ink).SingleLine()
	})
}
