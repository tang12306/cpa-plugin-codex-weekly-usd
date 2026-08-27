# codex-weekly-usd

A CLIProxyAPI plugin that answers one question per credential:

> **This account's quota — what is it worth in US dollars at public OpenAI API pricing?**

A credential sits under several limits at once (currently a 5-hour window and a
weekly one) and the same spend counts against all of them, so every window keeps
its own ledger and its own estimate.

## How it works

Every Codex response carries the account's quota state in its headers, and every
request produces token counters. The plugin pairs them:

```
numerator     tokens from UsageRecord.Detail, priced at the public rate card
denominator   that window's used-percentage

quota_usd = spend_usd / (used_percent / 100)
```

Real headers from `chatgpt.com/backend-api/codex`:

```
x-codex-primary-window-minutes:   300      <- 5 hours
x-codex-primary-used-percent:     100
x-codex-secondary-window-minutes: 10080    <- 7 days
x-codex-secondary-used-percent:   63
x-codex-plan-type: team
x-codex-active-limit: premium
```

These are present on **200 responses**, not only on 429s.

### Windows are keyed by length, not by the primary/secondary label

Those labels are not stable. Before the 5-hour limit was reinstated, `primary`
was the weekly window and `secondary` was absent; afterwards `primary` became
the 5-hour window and the weekly one moved to `secondary`.

Treating `primary` as "the window" loses the weekly history outright, and worse,
one percentage point of a weekly window is worth an order of magnitude more than
one point of a 5-hour window — averaging both into one calibration produces a
number that means nothing. Windows are therefore filed under `window_minutes`.

### Two independent estimators

| basis | how | when it applies |
|---|---|---|
| `window` | total spend in the window ÷ percentage used | only when the plugin saw the window open at 0% |
| `delta` | Σ spend between readings ÷ Σ percentage steps | always, after the first percentage tick |

`window` is preferred once at least 3% has been burned, because it prices a whole
window against a whole percentage with no attribution guesswork. `delta` is the
fallback and is immune to the plugin having been installed mid-window — which is
why a fresh install still converges instead of reporting nonsense.

The reported `confidence` tracks how much of the quota the plugin has actually
watched being spent: `high` ≥ 25 points, `medium` ≥ 8, `low` below that, `none`
before the first tick.

**Calibration is bounded** — 160 samples, 21 days. An unbounded sum cannot follow
a plan change, a window-semantics change or a re-priced model: old evidence would
outvote new evidence forever while the evidence count ratcheted into the hundreds,
so confidence would climb as accuracy fell.

### Attribution

The quota headers are emitted when the upstream stream opens, so they describe
the state *before* the current request is counted. Spend is therefore booked into
a `pending` bucket and only paired with a percentage step on the *next* reading.
`attributed_usd` is the spend the latest percentage has had a chance to include;
the raw window total is reported separately as `window_usd_observed`.

### Token accounting

CLIProxyAPI documents the OpenAI counters as subsets:

- `CacheReadTokens` and `CacheCreationTokens` are **inside** `InputTokens`
- `ReasoningTokens` is **inside** `OutputTokens`

so cached tokens are carved out of the input bucket at the cache rate and
reasoning tokens are never added on top of output. Billing either one twice
would inflate every estimate.

A model with no published cache-write rate has those tokens folded back into
fresh input rather than charged nothing.

### Upstream-granted mid-cycle resets

OpenAI frequently hands quota back before a cycle ends: **the percentage drops
while `reset_at` stays put**. Detecting rollovers by `reset_at` alone misses this,
leaving pre-reset spend on the books to be divided by a small post-reset
percentage, which blows the estimate up. A falling percentage therefore also
starts a new cycle, and is counted separately and shown in the panel.

### Quota spent by someone else

If the percentage moves while this plugin billed nothing for it, another client
is sharing the credential. That sample is skipped so it cannot pollute the
calibration, and the amount accumulates into `unexplained_percent` and raises a
warning — because it also means this plugin's ledger is undercounting.

### Restart safety

State is persisted, so a restart does not lose a window. But traffic served while
the plugin was down is inside the percentage and not inside the ledger, so on the
first reading after a restart, if the percentage advanced, `window` coverage is
invalidated and stale pending spend is discarded rather than being paired with a
step it did not cause.

## Pricing

Refreshed daily from `https://models.dev/api.json` (openai provider), cached to
`prices.json`, with a built-in table as the offline fallback. Models with no
public price are counted separately as `unpriced_requests` and raise a warning —
their spend is missing from the estimate rather than silently treated as free.

Manual overrides win over everything:

```yaml
price_overrides:
  gpt-5.6-sol:
    input: 5
    output: 30
    cache_read: 0.5
    cache_write: 6.25
```

## Dashboard

The panel is in Simplified Chinese and shows, per credential:

- **额度 / 时间双进度条** — quota consumed against clock elapsed, side by side.
  The comparison is the fastest read on who is over-consuming.
- **节奏** — `used_percent ÷ time_progress_percent`. Above 1.15 is flagged 偏快,
  below 0.85 is 宽裕. Above 1 means the window runs out before it resets.
- **周额度 / 已用 / 剩余 / 日均** in USD, plus a reset countdown.
- **依据** — which estimator produced the number and how much evidence backs it.

Expanding a row adds a per-model table (requests, failures, input/output tokens
with the reasoning subset called out, cache hit rate, the rate card in use,
average cost per request, total), an hourly spend sparkline for that credential,
and the full derivation: attributed vs pending spend, both estimators side by
side, calibration sample count, window coverage, runway and exhaustion warning.

Fleet level: summary cards (weekly value, spent, remaining, observed spend,
cache savings, request and failure counts), the usage chart, the active rate
card, sorting, and JSON export.

### Charts

All inline SVG, no external library, so the page stays self-contained.

- **Fleet usage** — hourly spend as bars on the left axis with a **cumulative
  spend line** on its own right axis. Flat stretches are idle hours; a
  steepening slope is spend accelerating.
- **Quota curve** (per credential) — the used-percentage over time against a
  **dashed constant-rate reference**, which is 100% spread evenly across the
  window. A curve above the dashed line is the visual form of a pace ratio
  greater than one: the window will be exhausted before it resets.

The quota curve draws only the current window. A falling percentage means a
rollover, so the curve restarts there rather than drawing a sawtooth with the
reference line anchored in a window that has already closed. A single reading
renders nothing at all rather than an empty box with a caption promising a trend.

### Rolling series

Spend is bucketed by hour into `Account.Hours` and persisted with the rest of the
state, so the chart survives restarts and no event-log reparsing is needed on
page load. Retention is 240 hours; the series is not reset by a window rollover
because it is a time series, not window state.

### Cache savings

`CacheReadTokens × (input_rate − cache_read_rate)` — what prompt caching is worth
against paying full input price for the same tokens.

## Routes

| route | auth | contents |
|---|---|---|
| `/v0/resource/plugins/codex-weekly-usd/panel` | **none** | inert HTML shell, zero data |
| `/v0/management/codex-weekly-usd/data` | management key | the full report |
| `/v0/management/codex-weekly-usd/prices` | management key | active rate card |

Plugin resource routes are not management-authenticated, so the panel deliberately
contains no data at all: it prompts for the management key and calls the
authenticated route itself. Verified: the panel returns 200 with no credential
identifiers, and `/data` returns 401 without a valid key.

## Configuration

```yaml
plugins:
  configs:
    codex-weekly-usd:
      enabled: true
      priority: 130
      data_dir: /var/lib/cliproxyapi/data/codex-weekly-usd
      price_source_url: https://models.dev/api.json
      price_refresh_hours: 24
      event_log: true
      event_log_keep_days: 60
      flush_seconds: 15
      # long_context_threshold: 272000
      # long_context_multiplier: 2
```

## Files

```
data_dir/
  state.json              per-credential accumulators (atomic writes)
  prices.json             cached rate card
  events/YYYY-MM-DD.jsonl one line per request, for auditing the estimate
```

## Build

The CLIProxyAPI plugin ABI is a plain C ABI over JSON, not Go's `plugin`
package, so the plugin does **not** have to match the host's Go version and does
not import the CLIProxyAPI module at all.

```sh
make build
```

Install as `<work-dir>/plugins/linux/amd64/codex-weekly-usd.so`. The filename
base must equal the plugin id. Note that the host resolves that path relative to
the service `WorkingDirectory`.

## Tests

`harness.c` loads the `.so` through the real ABI and drives it with scripted
JSON, so the accounting is checked without touching a live proxy.

```sh
make test
```

`test_charts.js` lifts the SVG builders straight out of the **served** panel and
replays them over a synthetic multi-hour series, so what is asserted is
byte-for-byte what a browser receives: bar count, cumulative monotonicity,
points per known hour, rollover restart, and the degenerate cases.

Currently 77 checks. The scenario is arithmetic rather than opinion: every request costs exactly
$2.00 at gpt-5.6-sol public pricing and moves the percentage by exactly one
point, so the answer must be $200. Covered: both estimators agreeing, cache-read
carve-out, reasoning tokens not double-billed, unknown models flagged, restart
with a downtime gap, window rollover, the panel leaking nothing, plus cache
savings, failure counting, pace ratio and the hourly series.
