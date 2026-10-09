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

## stats

```
mytoken stats [--json] [--since Nd|YYYY-MM-DD] [--by GROUP]
```

| Flag      | Default   | Meaning                                                              |
|-----------|-----------|----------------------------------------------------------------------|
| `--since` | `7d`      | Window start: a day count (`30d`) or a date (`2026-10-01`).          |
| `--by`    | `session` | Grouping: `session`, `provider`, `model`, `project`, `day`, `harness`. |
| `--json`  | off       | Machine-readable output.                                             |

Tokens are split into input, output, cache read, cache write and reasoning;
cost is in USD using the harness-reported cost when present, otherwise the
pricing catalog plus your price rules.

```sh
mytoken stats --since 30d --by model
mytoken stats --by harness --json | jq '.rows[] | {label, costUsd}'
```

## sessions

```
mytoken sessions [--limit N] [--json]
```

Lists sessions, most recently updated first. `--limit` defaults to 50; `0`
lists all. The table shows harness, session id, last update, request count,
total tokens, cost and title.

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
