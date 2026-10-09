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
options](#range-time-and-output-options).

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

## version / help

```
mytoken version
mytoken help
```

`version` prints the build version (the release tag, or the `mygo.json`
version for packaged builds). `help` prints the usage summary.
