# MyToken!!!!!

**English** | [简体中文](README.zh-CN.md)

**Where did all your tokens go?** MyToken!!!!! reads the session logs your AI coding harnesses
already write to disk, attributes every request to a session, provider and model, prices it, and
shows you the result in a native desktop app — or as JSON from the command line.

Everything happens on your machine. No account, no telemetry, no uploads: the only network request
the app ever makes is the price table download.

- **Ten harnesses in one window** — Claude Code, Codex CLI, DSH, Gemini CLI, opencode, Crush,
  Cline, Roo Code, Kilo Code and pi.
- **Session × provider × model** — token classes kept apart (input, output, cache read, cache write,
  reasoning), with cache-hit rate, per-project and per-day breakdowns, and sub-agent trees.
- **Attribution you can audit** — every provider is labelled with how it was determined: `log`,
  `cc-switch`, `config-timeline`, `inferred` or `user-rule` (see
  [Provider attribution](#provider-attribution)).
- **Costs that match your bill** — models.dev prices with a LiteLLM fallback, plus per-provider
  multipliers and per-model overrides for plans and credit packs.
- **Tray-resident** — closing the window keeps scanning; the tray panel shows today's tokens and
  cost, the last 24 hours, and the top models.
- **CLI for scripts** — `mytoken stats --json` prints the same numbers the app shows.
- **Native, small, fast** — built with [MyGo](https://github.com/egoist/mygo)'s native UI package:
  no Electron, no WebView, no cgo. A full first scan of this machine's logs takes seconds.

## Screenshots

| Overview | Sessions |
| --- | --- |
| ![Overview: trends, heatmap, cache hit rate, harness split](docs/screenshots/overview.png) | ![Sessions: virtual list and the provider × model breakdown of one session](docs/screenshots/sessions.png) |

| Rankings | Tray panel |
| --- | --- |
| ![Rankings: providers and models](docs/screenshots/models.png) | ![Tray panel: today's tokens and cost, 24h bars, top models](docs/screenshots/tray.png) |

| CLI | Settings |
| --- | --- |
| ![mytoken stats in a terminal](docs/screenshots/cli.png) | ![Settings: sources, attribution rules, prices and multipliers](docs/screenshots/settings.png) |

## Supported harnesses

MyToken!!!!! reads the logs each harness already keeps. Nothing is written back to them: the
readers are read-only and store only numbers plus a session title (the first 60 characters of the
first user message).

| Harness | ID | Where the logs live |
| --- | --- | --- |
| Claude Code | `claude-code` | `~/.claude/projects/**/*.jsonl` (`$CLAUDE_CONFIG_DIR/projects`), plus `~/.config/claude/projects` when it exists |
| Codex CLI | `codex` | `$CODEX_HOME/sessions` and `$CODEX_HOME/archived_sessions` (`~/.codex` by default) |
| DSH — DeepSeek Harness | `dsh` | `$DSH_HOME/sessions` (`~/.dsh/sessions` by default) |
| pi | `pi` | `$PI_HOME/agent/sessions` (`~/.pi/agent/sessions` by default) |
| Gemini CLI | `gemini` | `$GEMINI_CLI_HOME/tmp` (`~/.gemini/tmp` by default) |
| opencode | `opencode` | `$OPENCODE_DATA_HOME`, else `$XDG_DATA_HOME/opencode`, else `~/.local/share/opencode` |
| Crush | `crush` | `$CRUSH_GLOBAL_DATA`, else `$XDG_DATA_HOME/crush`, else `~/.local/share/crush` |
| Cline | `cline` | `…/globalStorage/saoudrizwan.claude-dev/tasks`, plus `~/.cline/data/tasks` and `~/.cline/data/sessions` |
| Roo Code | `roo` | `…/globalStorage/rooveterinaryinc.roo-cline/tasks` |
| Kilo Code | `kilo` | `…/globalStorage/kilocode.kilo-code/tasks` |

For the VS Code family extensions, `…/globalStorage` is resolved per platform and per editor —
editors: VS Code, VS Code Insiders, Cursor, Windsurf, VSCodium, Trae; platforms:
`~/Library/Application Support/<editor>/User/globalStorage` on macOS,
`~/.config/<editor>/User/globalStorage` (or `$XDG_CONFIG_HOME/…`) on Linux, and
`%APPDATA%\<editor>\User\globalStorage` on Windows.

Every reader can be pointed somewhere else, which is handy for non-default installs, containers and
archived logs:

| Variable | Effect |
| --- | --- |
| `MYTOKEN_HOME` | Where the index lives (all platforms) |
| `CLAUDE_CONFIG_DIR` | Claude Code config dir instead of `~/.claude` |
| `CODEX_HOME` | Codex data dir instead of `~/.codex` |
| `DSH_HOME` | DSH data dir instead of `~/.dsh` |
| `PI_HOME` | pi data dir instead of `~/.pi` |
| `GEMINI_CLI_HOME` | Gemini CLI data dir instead of `~/.gemini` |
| `MYTOKEN_GEMINI_DIRS` | Explicit Gemini CLI log dirs (path list) |
| `OPENCODE_DATA_HOME` | opencode data dir |
| `MYTOKEN_OPENCODE_DIRS` | Explicit opencode dirs (path list) |
| `CRUSH_GLOBAL_DATA` | Crush data dir |
| `MYTOKEN_CRUSH_DIRS` | Explicit Crush dirs (path list) |
| `CLINE_DIR`, `CLINE_DATA_DIR`, `CLINE_SESSION_DATA_DIR` | Cline data dirs |
| `MYTOKEN_CLINE_DIRS`, `MYTOKEN_ROO_DIRS`, `MYTOKEN_KILO_DIRS` | Explicit Cline / Roo / Kilo dirs (path list) |

## Provider attribution

A log rarely says which account or gateway served a request, so the provider is resolved by a
priority chain, and the app tells you which step produced the answer:

1. **`log`** — the harness recorded the provider itself.
2. **`cc-switch`** — matched against the read-only `~/.cc-switch/cc-switch.db` request log
   (same app type, model and tokens, within ±120 s).
3. **`config-timeline`** — the base URL in `~/.claude/settings.json` or `~/.codex/config.toml` at
   the time of the request, tracked as a timeline.
4. **`inferred`** — deduced from the model name (`claude-*` → Anthropic, `gpt-*`/`o*` → OpenAI,
   `gemini-*` → Google, `deepseek-*` → DeepSeek, …). Shown as inferred, never as fact.
5. **`user-rule`** — your own rules win over everything above.

## Pricing

- Prices come from [models.dev](https://models.dev), with LiteLLM's
  [`model_prices_and_context_window.json`](https://github.com/BerriAI/litellm) as the fallback and a
  bundled offline snapshot for the first run. The table is cached on disk for 24 hours and the app
  works entirely offline with the cached copy.
- Each of the five token classes is priced separately; reasoning tokens fall back to the output
  rate when the table has no reasoning price.
- Per-provider multipliers and per-model overrides cover subscriptions, credit packs and negotiated
  rates, and pricing can be imported from cc-switch (`model_pricing`, `providers.cost_multiplier`).
- **Model mappings.** Model names a relay or your own config invented have no price in any table, so
  they are listed as *unpriced* instead of guessed at. Map each one to the real model — and
  optionally to a provider — under *Settings → Model mappings*, and those requests are priced and
  merged into the rankings from then on.
- A cost recorded in the log itself always wins over the computed one, and requests that no price
  can be found for are reported as *unpriced* instead of silently costing nothing.

## Privacy

- **Local only.** The index, the price cache and the settings never leave your machine. There is no
  telemetry, no analytics, no crash reporting, no account and no sync.
- **One network request.** The price table download (models.dev, then LiteLLM) — nothing else. With
  a cached or bundled table the app makes no requests at all.
- **Read-only readers.** Harness logs are opened for reading; the index is a separate SQLite file
  that you can delete at any time to start over.
- **No conversation content.** Only numbers and metadata are stored. The single exception is the
  session title: the first 60 characters of the first user message, so the session list is readable.

| Platform | Index directory |
| --- | --- |
| macOS | `~/Library/Application Support/MyToken` |
| Windows | `%APPDATA%\MyToken` |
| Linux | `$XDG_DATA_HOME/mytoken` (usually `~/.local/share/mytoken`) |

The database is `mytoken.db` inside it. Set `MYTOKEN_HOME` to keep it elsewhere (handy for trying
the app against a scratch index); delete the directory to reset.

## Install

### macOS (disk image)

Download `mytoken <version>.dmg` from [Releases](../../releases), open it and drag **MyToken!!!!!**
into *Applications*. The bundle is ad-hoc signed rather than notarized, so the first launch needs
right-click → *Open* (or `xattr -dr com.apple.quarantine /Applications/mytoken.app`). The app's file
name is `mytoken.app`; Finder and the menu bar show `MyToken!!!!!`.

### Windows (installer)

Download `mytoken Setup <version>.exe` and run it. The installer is unsigned, so SmartScreen asks
for *More info* → *Run anyway* the first time. It installs `mytoken.exe`, a Start Menu entry and the
uninstaller.

### Linux (install script)

```sh
curl -fsSL https://github.com/zzstar/mytoken/releases/latest/download/install.sh | sh
```

Installs per user, without root: the app in `~/.local/mytoken.app`, the `mytoken` command in
`~/.local/bin` and an entry in the applications menu. `sh install.sh --uninstall` removes it and
keeps your data.

### Linux (Debian package)

```sh
sudo apt install ./mytoken_<version>_amd64.deb
```

Installs `/opt/mytoken`, the `/usr/bin/mytoken` command and a desktop entry. It depends on GTK 3,
WebKitGTK 4.1 and libayatana-appindicator3 (the tray icon).

### With the Go toolchain

```sh
go install github.com/zzstar/mytoken@latest
```

Builds the same binary the bundles carry, including the CLI, into `$(go env GOPATH)/bin/mytoken`.

## Command line

The app binary is also the CLI: run it with no arguments for the GUI, or with a subcommand for the
terminal. The full reference is in [docs/CLI.md](docs/CLI.md).

```sh
mytoken stats                                  # last 7 days, per session
mytoken stats --since 30d --by model           # 30 days, grouped by model
mytoken stats --since 2026-10-01 --by provider # since a date, grouped by provider
mytoken stats --json                           # machine-readable output
mytoken sessions --limit 20                    # session list, most recent first
mytoken scan                                   # re-index now
mytoken doctor                                 # check sources, paths and versions
mytoken prices list                            # the active price table
mytoken prices set --provider zai --multiplier 0.5
mytoken prices import-ccswitch                 # import cc-switch pricing
mytoken version
```

`--by` accepts `session`, `provider`, `model`, `project`, `day` or `harness`, and `--since` accepts a
duration (`7d`, `30d`) or a date (`YYYY-MM-DD`). `mytoken help` lists everything.

## Building from source

Requirements: Go 1.27 or newer, and no cgo (`CGO_ENABLED=0` — the SQLite driver is pure Go).
Running the app needs macOS 12+, Windows 10+ or a Linux desktop with GTK 3 and WebKitGTK 4.1.

```sh
git clone https://github.com/zzstar/mytoken
cd mytoken

make build     # ./mytoken, version taken from the latest v* tag
make run       # run the app from source
make check     # gofmt check, go vet, go test
```

`make help` lists the packaging targets:

```sh
make app       # this platform
make dmg       # universal macOS .app and .dmg
make linux     # linux/amd64 + linux/arm64: binaries, .deb, tarballs, install.sh
make windows   # windows/amd64 + windows/arm64 (.exe and installer; needs makensis)
```

These wrap `go tool mygo build -platform …`. [MyGo](https://github.com/egoist/mygo) is pinned in
`go.mod`'s `tool` directive, so there is nothing to install globally, and its bundle metadata lives
in [mygo.json](mygo.json) — identifier, icon, macOS display name, Linux desktop entry. Note that
MyGo names the bundle and executable after `name` (`mytoken`), which keeps the CLI command and the
artifact file names shell-friendly; the macOS display name is set back to `MyToken!!!!!` through
`CFBundleDisplayName`.

## Releasing

Releases are cut from tags:

```sh
make bump TAG=v0.2.0   # writes the version into mygo.json (or edit it by hand)
git commit -am "release v0.2.0"
git tag v0.2.0
git push origin main --tags
```

[.github/workflows/release.yml](.github/workflows/release.yml) then creates a draft release and
builds the macOS universal bundle, the Windows installers and the Linux packages, attaching them to
that draft; review it and publish. The version always comes from the tag, and no secrets are
required: macOS bundles are ad-hoc signed and Windows installers unsigned. Signing and notarization
secrets are documented at the top of the workflow.

[.github/workflows/ci.yml](.github/workflows/ci.yml) runs `go vet` and `go test` on macOS, Linux and
Windows, plus a `gofmt` check, for every push and pull request.

## Credits

- **[tokscale](https://github.com/search?q=tokscale)** — several harness readers are ports of
  tokscale's parsing rules, including Gemini CLI's token folding and accepted field names.
- **[MyGo](https://github.com/egoist/mygo)** — the native UI toolkit the app is built with.
- **[models.dev](https://models.dev)** and **[LiteLLM](https://github.com/BerriAI/litellm)** — the
  price tables.
- **cc-switch** — the pricing import format and the provider request log used by attribution.

## Disclaimer

MyToken!!!!! is an unofficial fan tribute to the band **MyGO!!!!!** — the name, the colour palette
and the little decorations in the UI are affectionate nods, nothing more. This project is not
affiliated with, endorsed by or connected to the band, its members or their labels, and it ships no
official artwork. All trademarks belong to their respective owners.

## License

[MIT](LICENSE) © 2026 zzstar101
