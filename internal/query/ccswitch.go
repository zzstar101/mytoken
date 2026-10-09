package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"
)

func ccColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id, notnull, pk int
		var name, kind string
		var def any
		if err = rows.Scan(&id, &name, &kind, &notnull, &def, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}
func importCCSwitch(ctx context.Context, path string) ([]PriceRule, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	q := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(1500)"}}
	db, err := sql.Open("sqlite", sqlitedsn.URI(path, q.Encode()))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT model_id,input_cost_per_million,output_cost_per_million,cache_read_cost_per_million,cache_creation_cost_per_million FROM model_pricing ORDER BY model_id`)
	if err != nil {
		return nil, fmt.Errorf("cc-switch model pricing: %w", err)
	}
	models := []PriceRule{}
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		var raw [4]string
		if err = rows.Scan(&name, &raw[0], &raw[1], &raw[2], &raw[3]); err != nil {
			rows.Close()
			return nil, err
		}
		rates := [4]float64{}
		for i, v := range raw {
			rates[i], err = strconv.ParseFloat(v, 64)
			if err != nil || !validRate(rates[i]) {
				rows.Close()
				return nil, fmt.Errorf("invalid cc-switch rate for %q: %q", name, v)
			}
		}
		// A model rule supplies unit prices only: its multiplier stays 0
		// ("unset") so the provider's own multiplier still applies.
		r := PriceRule{Model: name, Input: &rates[0], Output: &rates[1], CacheRead: &rates[2], CacheWrite: &rates[3], Source: "cc-switch"}
		key := pricing.Normalize(name)
		if !seen[key] {
			models = append(models, r)
			seen[key] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	cols, err := ccColumns(ctx, tx, "providers")
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("cc-switch providers table missing")
	}
	globals := map[string]string{}
	pc, err := ccColumns(ctx, tx, "proxy_config")
	if err != nil {
		return nil, err
	}
	if pc["app_type"] && pc["default_cost_multiplier"] {
		rows, err = tx.QueryContext(ctx, "SELECT app_type,default_cost_multiplier FROM proxy_config")
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var app, value string
			if err = rows.Scan(&app, &value); err != nil {
				rows.Close()
				return nil, err
			}
			globals[app] = value
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	multiplier, meta, app := "NULL", "NULL", "''"
	if cols["cost_multiplier"] {
		multiplier = "cost_multiplier"
	}
	if cols["meta"] {
		meta = "meta"
	}
	if cols["app_type"] {
		app = "app_type"
	}
	rows, err = tx.QueryContext(ctx, "SELECT name,"+app+","+multiplier+","+meta+" FROM providers ORDER BY name,"+app)
	if err != nil {
		return nil, err
	}
	providers := map[string]float64{}
	names := []string{}
	for rows.Next() {
		var name, app string
		var value, metadata sql.NullString
		if err = rows.Scan(&name, &app, &value, &metadata); err != nil {
			rows.Close()
			return nil, err
		}
		raw := globals[app]
		if raw == "" {
			raw = "1"
		}
		if !cols["cost_multiplier"] && globals[app] == "" && metadata.Valid {
			var m map[string]json.RawMessage
			if err = json.Unmarshal([]byte(metadata.String), &m); err != nil {
				rows.Close()
				return nil, err
			}
			if v, ok := m["costMultiplier"]; ok && string(v) != "null" {
				var s string
				if json.Unmarshal(v, &s) == nil {
					raw = s
				} else {
					raw = string(v)
				}
			}
		}
		if value.Valid && value.String != "" {
			raw = value.String
		}
		n, e := strconv.ParseFloat(raw, 64)
		if e != nil || !validRate(n) {
			rows.Close()
			return nil, fmt.Errorf("invalid cc-switch multiplier for %q: %q", name, raw)
		}
		if n == 0 {
			n = 1
		}
		if old, ok := providers[name]; ok {
			if old != n {
				rows.Close()
				return nil, fmt.Errorf("cc-switch provider %q has conflicting app multipliers", name)
			}
			continue
		}
		if name != "" {
			providers[name] = n
			names = append(names, name)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := append([]PriceRule{}, models...)
	// Materialize provider/model selectors: the provider rule carries the
	// multiplier, the model-scoped copy only narrows which prices apply.
	for _, name := range names {
		n := providers[name]
		out = append(out, PriceRule{Provider: name, Multiplier: n, Source: "cc-switch"})
		for _, m := range models {
			m.Provider = name
			out = append(out, m)
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
