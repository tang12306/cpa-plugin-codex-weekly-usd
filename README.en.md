# codex-weekly-usd

English · [简体中文](README.md)

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin. Values each Codex credential's quota windows (5-hour / weekly) in US dollars at public OpenAI API pricing.

- **What it's worth**: quota, spent and remaining per window, priced separately per model
- **Whether it works now**: how many credentials can serve each model, which are cooling down, when they recover
- **Rotation** (optional): switch credentials before they run out

## How it works

Every Codex response carries quota headers (`x-codex-*-used-percent`, `window-minutes`, `reset-at`), and every request carries token counts. Price the tokens at the public rate card and divide by the percentage:

```
window quota (USD) = spend / (used percent / 100)
```

- Windows are identified by length, not by the primary / secondary slot (upstream has swapped them once)
- A window is one quota pool, metered on one gauge for every model. But **a dollar buys a different share of it in each model** (measured: gpt-6-astra consumes about 1.4x what gpt-5.6-sol does), so quota is estimated per model. The ratio between models is measured inside a single window, then carried to credentials that have never served a given model
- Calibration uses the current cycle's evidence, reaching back to earlier cycles only when there is too little

## Install

Search `codex-weekly-usd` in the plugin store, or download from [Releases](https://github.com/tang12306/cpa-plugin-codex-weekly-usd/releases) and place it at:

```
<CLIProxyAPI working dir>/plugins/<goos>/<goarch>/codex-weekly-usd.so
```

`codex-weekly-usd-v<version>.so` also works. The path is relative to systemd's `WorkingDirectory`; without that setting it defaults to `/` and the plugin is not found.

## Configure

```yaml
plugins:
  enabled: true
  configs:
    codex-weekly-usd:
      enabled: true
      data_dir: /var/lib/cliproxyapi/data/codex-weekly-usd
      rotator:
        enabled: false        # the only feature that writes credential files; off by default
        dry_run: false        # true = decide and log, write nothing
        keep_enabled: 2       # credentials kept enabled at once
        switch_at_percent: 10 # switch when the tightest window has less headroom than this
        probe_model: gpt-5.6-sol
```

Everything else has a default; see [config.go](config.go). Prices are refreshed daily from `models.dev` (through CPA's configured proxy), with a built-in table as the offline fallback. Models without a price are flagged, never counted as free.

## Rotation

- Keeps `keep_enabled` credentials enabled. When one is refused, CPA retries the next one within the same request, so a switch is invisible to callers
- A replacement is written with a `priority` below every enabled credential, joining the back of the queue. CPA's fill-first picks by file name within one priority; without this, a standby whose name sorts late is never served
- Runs on arithmetic, not upstream calls; it probes once before a switch and once after a window resets. Probes use the credential's own `proxy_url`
- Credentials refused upstream (401/403) are disabled, and become candidates again once a fresh login replaces the token
- Writes the credential file directly (`disabled`, `priority`) and lets CPA's file watcher apply it

## Routes

| Route | Auth | Purpose |
|---|---|---|
| `GET /v0/resource/plugins/codex-weekly-usd/panel` | none | Dashboard (a shell with no data in it) |
| `GET /v0/management/codex-weekly-usd/data` | management key | Full report |
| `GET /v0/management/codex-weekly-usd/prices` | management key | Effective prices |
| `POST /v0/management/codex-weekly-usd/rotate` | management key | Run one rotation decision now |
| `POST /v0/management/codex-weekly-usd/refresh` | management key | Re-read every credential's quota (after an out-of-band reset) |

Plugin resource routes bypass auth, so the dashboard is only a shell: it asks for the management key and calls the protected routes itself. The management key is accepted as a header only (`Authorization: Bearer` or `X-Management-Key`).

## Data

```
data_dir/
  state.json               accumulators
  prices.json              price cache
  events/YYYY-MM-DD.jsonl  one line per request
```

Contains per-credential usage; keep it out of version control.

## Build and test

The plugin ABI is C ABI + JSON: no dependency on CLIProxyAPI modules, no Go version lock-step with the host.

```sh
make build   # codex-weekly-usd.so
make test    # needs Go (cgo), Python 3, Node
```

Tests load the `.so` through the real C ABI, so no running CLIProxyAPI is needed.

## License

MIT
