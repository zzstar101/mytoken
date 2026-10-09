# MyToken!!!!! CLI

The same `mytoken` binary is both the tray app and a command-line tool. Run it
with no arguments (or with `--hidden`, used by "open at login") to start the
app; any other first argument runs a command and exits without opening a
window.

```
mytoken <command> [options]
```

Every command reads the same local index the app uses
(`~/Library/Application Support/MyToken` on macOS, `%APPDATA%\MyToken` on
Windows, `$XDG_DATA_HOME/mytoken` or `~/.local/share/mytoken` on Linux).
Set `MYTOKEN_HOME` to use another directory.

Exit status: `0` success, `1` operational error, `2` invalid arguments.

`stats` and `sessions` share the range, timezone, offline and output flags —
including CSV export — described in [Range, time and output
options](#range-time-and-output-options). `relay list` and `reconcile` accept the
output flags too, and `reconcile` accepts the range flags as well.

## scan

```
mytoken scan [--cpuprofile FILE] [--memprofile FILE]
```

Indexes every supported harness log once (incrementally — only new or changed
files are parsed) and prints a one-line summary:

```
Scanned 2075/2075 sources: 206020 events, 626 sessions in 9.8s
```

The profiling flags write Go `pprof` profiles and are meant for development.

## Range, time and output options

`stats` and `sessions` share these flags.

| Flag | Default | Meaning |
|------|---------|---------|
| `--since DATE\|DUR` | `7d` for `stats`, all time for `sessions` | Range start, included. |
| `--until DATE\|DUR` | none | Range end, **not** included. |
| `--last WINDOW` | none | Recent window ending now: `today`, `week`, `month`, or `7d`, `2w`, `3m`, `12h`, `30s`. |
| `--timezone TZ` | local | IANA zone used to read dates and to bucket `--by day`. |
| `--offline` | off | Never use the network. |
| `--no-cost` | off | Leave out cost columns and `costUsd` fields. |
| `--format FMT` | `table` | `table`, `json` or `csv`. |
| `--json` | off | Alias for `--format json`. |

### Dates and durations

`--since` and `--until` accept:

- `YYYY-MM-DD` and `YYYYMMDD` — midnight in `--timezone`;
- RFC3339 (`2026-10-01T08:30:00+08:00`) — that exact instant;
- a duration counted back from now: `30s`, `12h`, `7d`, `2w`, `3m`. `m` is a
  **calendar month** (`3m` = three months ago), not a minute; write minutes as
  seconds (`90s`) or hours (`0.5h` is not accepted, use `30s`).

The range is half-open: `--since` includes the timestamp, `--until` excludes it,
so `--until 2026-10-01` stops just before October 1st. A duration on `--until`
also counts back from now, so `--until 12h` means "everything up to 12 hours
ago". A range whose start is not before its end is an error.

### --last

`--last` is a shortcut for a window that ends now. It cannot be combined with
`--since` or `--until`; doing so is an error.

| Value | Window starts at |
|-------|------------------|
| `today` | local midnight today |
| `week` | local Monday 00:00 |
| `month` | local 1st 00:00 |
| `30d`, `2w`, `3m`, `12h` | now minus that much (`m` = calendar month) |

```sh
mytoken stats --last today
mytoken stats --last week --by day --timezone Asia/Shanghai
mytoken stats --last month --by model --no-cost
mytoken stats --since 2026-09-01 --until 2026-10-01 --by provider
```

### --timezone

Dates are read in the given zone, and `--by day` buckets are the local days of
that zone — which is why `--by day` can start on a different calendar day than
UTC shows. The IANA time zone database is embedded in the binary, so any zone
name works without host data files.

```sh
mytoken stats --by day --last week --timezone America/New_York
mytoken stats --since 2026-10-01 --timezone UTC --by harness
```

### --offline

By default `stats` and `sessions` open the index with the refresh worker on: it
contacts [models.dev](https://models.dev/api.json) (falling back to the
[LiteLLM price list](https://github.com/BerriAI/litellm)) at most once every 24
hours and caches the result in `prices.json` next to the database. That request
is the only network access these commands make; if the cache is younger than 24
hours they stay offline anyway. `--offline` skips the worker entirely, so no
connection is ever opened — useful in CI, in a sandbox, or when you want a
reproducible run.

```sh
mytoken stats --since 30d --offline
mytoken sessions --last 7d --offline --format csv
```

### --no-cost

Drops the cost column from tables and every `costUsd` field from JSON (totals,
rows and a session's provider breakdown). Counts such as `unpriced` stay.

```sh
mytoken stats --by model --no-cost
mytoken sessions --limit 10 --no-cost
```

### CSV

`--format csv` writes one table per view with a stable header, using Go's
`encoding/csv`: numbers have no thousands separators, times are RFC3339, and
costs have six decimals. There is no `TOTAL` row — the CSV holds exactly the
rows the table shows.

| View | Header |
|------|--------|
| `stats --by provider\|model\|project\|harness` | `key,label,requests,sessions,tokens,input,output,cache_read,cache_write,reasoning,cost_usd,unpriced` |
| `stats --by session`, `sessions` | `harness,session_id,updated_at,requests,tokens,input,output,cache_read,cache_write,reasoning,cost_usd,unpriced,title` |
| `stats --by day` | `day,tokens,input,output,cache_read,cache_write,reasoning,cost_usd` |

`--no-cost` removes the `cost_usd` column instead of blanking it. The sessions
CSV lists sessions only; the per-provider breakdown is in `--json`.

```sh
mytoken stats --last 30d --format csv > usage.csv
mytoken stats --by day --timezone Asia/Shanghai --format csv
mytoken sessions --last 90d --no-cost --format csv
```

## stats

```
mytoken stats [options] [--by GROUP]
```

Prints the totals line, one row per group, and finally the models that had no
price.

| Flag | Default | Meaning |
|------|---------|---------|
| `--by`    | `session` | Grouping: `session`, `provider`, `model`, `project`, `day`, `harness`. |
| `--since` | `7d`      | Range start. |
| other range/output flags | see [Range, time and output options](#range-time-and-output-options) | |

Tokens are split into input, output, cache read, cache write and reasoning;
cost is in USD using the harness-reported cost when present, otherwise the
pricing catalog plus your price rules.

```sh
mytoken stats --since 30d --by model
mytoken stats --by harness --json | jq '.rows[] | {label, costUsd}'
mytoken stats --by day --last week --timezone Europe/Berlin
mytoken stats --by provider --until 2026-10-01 --format csv
```

## sessions

```
mytoken sessions [--limit N] [options]
```

Lists sessions, most recently updated first, filtered by any range flags (all
time by default). `--limit` defaults to 50; `0` lists all. The table shows
harness, session id, last update, request count, total tokens, cost and title;
`--format csv` and `--format json` emit the same rows.

```sh
mytoken sessions --limit 20 --last 7d
mytoken sessions --last 30d --no-cost --format csv
mytoken sessions --since 2026-10-01 --timezone UTC --json
```

## doctor

```
mytoken doctor [--json]
```

Diagnoses the installation: data directory, database path and size, pricing
catalog source and age, and for every harness the log roots that were probed
(and whether they exist) with the number of indexed sources and events. Use it
first when a harness shows no data.

## prices

```
mytoken prices [list]
mytoken prices import-ccswitch
mytoken prices set --provider NAME [--model NAME] [--multiplier N]
                   [--input N] [--output N] [--cache-read N] [--cache-write N]
```

- `list` (default) prints the effective price rules.
- `import-ccswitch` imports provider price multipliers from a local
  [cc-switch](https://github.com/farion1231/cc-switch) configuration.
- `set` adds or replaces a user rule for a provider (optionally one model):
  either a `--multiplier` applied to catalog prices, or explicit prices in USD
  per million tokens. Existing events are repriced with the new rules.

## relay

```
mytoken relay list    [--format table|json|csv] [--json] [--offline]
mytoken relay enable  <origin|provider> [--layers ratio,balance,bills] [--key-id ID]
mytoken relay disable <origin|provider> [--key-id ID]
mytoken relay sync    [<origin|provider>] [--key-id ID]
```

Relay sites (中转站) are **off by default** — nothing is contacted until you
enable one site, one key, one layer at a time. A layer maps to what the gateway
publishes:

| Layer | Flag name | What it reads |
|-------|-----------|---------------|
| L1 ratios | `ratio` | per-model price ratios, stored as dated price rules |
| L2 balance | `balance` | the remaining/used quota of one key, snapshotted over time |
| L3 bills | `bills` | per-request logs (new-api) or daily usage (sub2api) |

- `list` shows every site this build knows about: sites you enabled and sites a
  credential source (cc-switch, harness config) knows about but you never turned
  on. It never uses the network; `--offline` is accepted and redundant.
- `enable` identifies the site (one request to that origin) and starts syncing
  the selected layers. `--layers` defaults to all three; `--key-id` picks one key
  when several credentials match the origin.
- `disable` stops syncing a site and keeps its row and history.
- `sync` refreshes the enabled sites now. With no argument it syncs all of them.
- A target may be an origin or a provider name (`claude-code`, `openai`, …).
  Matching ignores case and a trailing slash on the origin. When a target names
  several sites or several keys, `enable` and `disable` never guess: they list the
  candidates and exit `1`, and you pick one with `--key-id`.

**Privacy.** A key is read in memory, used for requests to its own origin only,
and never stored, logged or printed: `relay list` shows the key *ID* (a truncated
SHA-256, `keyId` in JSON) and nothing else. Every command except
`enable`/`sync` stays entirely offline, and those two refuse `--offline` with
exit status `2` rather than silently ignoring it.

```sh
mytoken relay list
mytoken relay list --json | jq '.sites[] | select(.enabled) | {origin, layers}'
mytoken relay enable https://api.example.com --layers ratio,balance
mytoken relay sync
mytoken relay disable https://api.example.com
```

`--format csv` writes the stable header
`origin,key_id,has_key,kind,version,enabled,layers,providers,last_sync,last_error,remaining_usd,used_usd,unlimited`.

## reconcile

```
mytoken reconcile [<origin|provider>] [--since DATE|DUR] [--until DATE|DUR] [--last WINDOW]
                  [--timezone TZ] [--by category|model|day]
                  [--format table|json|csv] [--json] [--key-id ID]
```

Compares what the local logs say you should have paid with what the relay site
actually charged, and splits the difference into categories (`matched`,
`price-diff`, `token-semantics`, `bill-only`, `event-only`, `refund`). It reads
only the local index and the bills already synced, so it never uses the network.
The default range is `--since 7d`; the range flags are the ones from
[Range, time and output options](#range-time-and-output-options). `--no-cost` is
refused (exit `2`) — the whole view is about money.

The optional target is an origin or a provider name, matched the same way as in
[`relay`](#relay). With no target every enabled site is reconciled, and JSON/CSV
carry one report per site in a `reports` list. A target that matches no enabled
site exits `1`; no enabled site at all is an empty view — the table prints a hint
and exits `0`, and JSON/CSV write an empty list.

`--by` selects the detail table:

| `--by` | Detail rows |
|--------|-------------|
| `category` (default) | one row per difference category, with a note naming the model |
| `model` | one row per model: local, formula and charged USD |
| `day` | one row per local day, plus the multiplier and usage parts of the difference |

The `day` CSV view also carries `multiplier_diff_usd`, `usage_diff_usd` and
`unpriced`; the other views use
`origin,key_id,category,count,local_usd,formula_usd,charged_usd,note` and
`origin,key_id,model,count,local_usd,formula_usd,charged_usd`.

```sh
mytoken reconcile
mytoken reconcile https://api.example.com --last month --by day
mytoken reconcile --by category --format csv > differences.csv
mytoken reconcile --last week --json | jq '.reports[0] | {localUsd, chargedUsd, impliedMultiplier}'
```

## version / help

```
mytoken version
mytoken help
```

`version` prints the build version (the release tag, or the `mygo.json`
version for packaged builds). `help` prints the usage summary.
