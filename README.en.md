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

## Credential rotation (optional, off by default)

The rotator holds a fixed number of credentials enabled and replaces a member **before** it runs
out.

Why a pool rather than a single credential: the proxy's fill-first selector retries a refused
request on the next enabled credential **within the same request**, so a handover is invisible to
callers as long as the replacement is already in the pool. A pool of one has nothing to hand over
to. The default is `keep_enabled: 2`.

**This is the only part of the plugin that writes anything outside its own data directory**, so it
is off by default and ships with a `dry_run` mode that decides and records exactly as usual while
writing nothing. Running it that way for a while first is time well spent.

### When it switches

Any of:

- **Headroom floor** - the tightest window has `switch_at_percent` left (default 10%).
- **Lead time** - the measured burn would exhaust it within `lead_time_minutes` (default 15). This
  is not a refinement: a credential in production went from 84% to 99% in forty-five minutes, and at
  that rate 10% of headroom is half an hour away.

  **Bounded by `projectionCeiling` (twice the floor).** Lead time is denominated
  in minutes, but a minute is worth whatever the current burn says it is: at a
  measured 270 points an hour, a fifteen-minute lead means "replace anything
  under 67% headroom" - the floor never applies and every credential is retired
  with most of its window unspent. However fast it is burning, the projection
  may not retire a credential with more than `2 x switch_at_percent` headroom.
  The projection only exists to cover the blind spot between checks, and the
  pool's second member already makes a handover invisible: crossing the floor
  costs one 429 the proxy retries elsewhere, not an outage.

- **Rejection** - upstream has stopped accepting the credential (401).

With one rule pointing the other way: if the binding window resets sooner than the projection says
it will empty, **do not switch**. It recovers on its own, and switching would throw away the tail of
a credential that was still working.

### How it picks a replacement

1. **Exclusions first**: a rejected token, an exhausted window that will not reset soon, headroom
   already below the floor (promoting that buys one check of relief), a credential just rotated out,
   anything named in `never_enable`. Every exclusion is shown on the panel, so the decision can be
   argued with rather than trusted.
2. **Rank by how long it will carry the load**, not by remaining percentage. **A percentage point is
   worth different money on different plans** - a Plus account at 100% can be worth less than a Team
   account at 40% - so the arithmetic goes through dollars: remaining USD divided by the current
   spend rate. This plugin is the one that can do that, because valuing the quota is what it does.
3. **Between candidates that both qualify, spend the allowance that expires soonest.** Sixty percent
   of a weekly window that resets in nine hours is use-it-or-lose-it; the same headroom on a window
   that resets in a week is not going anywhere.

### Decisions are derived; probes are spent only on acting

**An idle credential's quota can only do two things**: stay exactly where the last reading left it,
or drop to zero the moment its window closes. Both are arithmetic, so upstream almost never needs
asking:

- **A credential that is serving** reports its quota in the headers of every response, which this
  plugin already records. Its reading is fresh by construction.
- **A credential that is idle** is derived from its last reading plus its reset time. Past the reset,
  the allowance is back; before it, nothing can have been spent.

**A whole rotation costs at most two probes**, and both are spent at the moment of acting:

1. **When the estimate says the incumbent is nearly spent**, one probe checks that claim before
   anything is switched on the strength of it. If the estimate had drifted, that probe just saved a
   pointless rotation.
2. **Before a replacement takes real traffic**, one probe confirms it still works. This is the one
   that pays for itself: a credential revoked while it sat idle looks perfect in the derived picture
   - precisely because nothing has asked it anything - and promoting it would hand callers a failure.

**The budget is per quota cycle**, kept in state.json so a restart does not respend it, with a hard
per-credential daily cap (`max_probes_per_day_per_credential`, default 6) behind it. The panel shows
"N probes today"; on a working system it reads zero for days at a time.

Three measured facts about probing:

- **A minimal request is effectively free** - two back to back report the same percentage.
- **A rejected request carries no quota headers at all**, so there is no free-probe shortcut: a bad
  model name returns 400 with nothing useful on it.
- **A 429 still carries them**, so an exhausted credential is readable rather than a blind spot.

### Probes leave by the credential's own egress

**This is a real defect fixed in 2.4.0.** CLIProxyAPI gives each credential its own egress through
`proxy_url` in its auth document, and both the request path and the token refresh honour it. The
plugin host callback does not: `host.http.do` is built with `h.newHTTPClient(nil)` - the auth is
hardcoded nil - so it falls back to the host's own address.

The consequence is worse than probe volume: an account whose traffic normally leaves from Los
Angeles was seen authenticating from Chicago, which is exactly the shape an abuse detector looks
for. And `pluginapi.HTTPRequest` carries no proxy field, so a plugin cannot ask for a different
route.

So from 2.4.0 the plugin **dials for itself**: it reads the credential's `proxy_url` and speaks
SOCKS5 (username/password included), leaving by the same route that credential's real traffic uses.
**An unreachable proxy fails the probe rather than falling back to a direct dial** - falling back is
the defect itself.

**And one absolute rule**: a credential upstream has refused (401/403) is **never probed again**
until its access token changes, which means someone has logged the account back in. Only a fresh
login can change the answer; asking repeatedly is the least defensible traffic this plugin can
generate.

### Guard rails

- Off by default; `dry_run` decides without writing.
- **The replacement is always enabled before the incumbent is disabled.** A moment with an empty
  pool is a total outage, and a mutation test guards the ordering.
- If no candidate qualifies it **changes nothing and warns**. It will never disable the last
  credential because it could not find a better one.
- `min_switch_gap_minutes` and `max_changes_per_day` are a circuit breaker, and **a manual trigger
  does not bypass it**: the breaker exists to stop the rotator doing damage quickly, and a human
  pressing the button is not evidence that this time is different.
- The budget persists in state.json, so a proxy that bounces cannot spend it twice.
- `never_enable` / `never_disable` lists.
- Writes go through `map[string]json.RawMessage`, so fields this plugin does not know about survive
  untouched, and a document without a `refresh_token` is refused rather than written.

`POST /v0/management/codex-weekly-usd/rotate` evaluates immediately. POST only: the route changes
state and must not be reachable by following a link.

`POST /v0/management/codex-weekly-usd/refresh` **re-reads every credential now.**
Every automatic path waits for a reason - a window past its reset, a short pool, a replacement about
to take traffic. A quota reset granted out of band satisfies none of them: it moves no clock and
empties no pool, so nothing would ever notice, and the panel would keep reporting figures that
stopped being true the moment it happened. This is the operator saying the stored numbers are wrong.

It bypasses the per-cycle probe budget - a person asking a new question is not the rotator repeating
an old one - but not the daily cap, and not the rule that a credential upstream has refused is never
asked again. Only a fresh login can change that answer.

**A known limit**: the proxy does not refresh the access token of a long-disabled credential, so
once it expires the probe reads as a rejection. This version warns rather than running the OAuth
refresh itself.

The knock-on effect inside CLIProxyAPI is worse. `shouldRefresh()` opens with
`if hasUnauthorizedAuthFailure(a) { return false }`, and both places that clear that error state are
guarded by `if !auth.Disabled`. **So a credential that 401s once and is then disabled can never
clear the error, and CLIProxyAPI will never refresh its token again.** That state is not persisted,
so restarting CLIProxyAPI clears it.

`disable_dead_tokens` therefore cuts both ways: it saves a wasted attempt on every request, but it
can also freeze a credential that would otherwise have recovered. The error code tells them apart -
`token_revoked` on a request may be transient, while `refresh_token_invalidated` ("Your session has
ended") on a refresh is terminal and needs a fresh login.

## Model availability

CLIProxyAPI cools a credential down per **credential and model**, not as a whole: upstream keeps a
separate allowance for each model, so a credential can be out of one model while every other model
it serves keeps working.

**That produces an outage shape that is genuinely hard to diagnose.** When every credential for a
model is cooling at the same time, what you see from outside is *only this one model is broken*,
while the credential list reports every file as `active` and the upstream channel looks healthy.
Finding it means reading three different places in the proxy log.

The availability board at the top of the panel answers it directly:

| Column | Meaning |
|---|---|
| State | `healthy` / `partly cooling` / `all cooling`, plus a single-credential flag |
| Available / credentials | How many credentials can serve the model now. **Disabled credentials are listed but are not capacity.** |
| First back | Countdown to the first credential leaving cooldown |
| Per credential | One chip each; hover for the reason, which window filled, the deadline, and how often it has been locked out |

Two structured warnings:

- **`model_unavailable`** - nothing is left to serve this model, with the time the first credential
  returns. This is the named form of "only this one model is down".
- **`model_single_point`** - the model runs on one credential. Its next 429 takes the model out
  entirely, and nothing in the proxy's own status says so beforehand.

How it decides:

- **The deadline is the window reset upstream reported**, the same value the proxy cools down to. It
  belongs to whichever window is actually **full** (the later one when both are), not to whichever
  window happens to reset next.
- **Credit exhaustion can refuse a request with every percentage under the limit.** With no full
  window to read, the soonest reset is used instead and is **flagged as an estimate**.
- **A served request clears the lockout immediately**, however much of the recorded deadline is
  left: upstream hands quota back early often enough that the deadline alone cannot be trusted.
- **A failure with no quota signal is not a lockout** (a dropped connection, a rejected request); it
  still counts as a failure.
- Repeated 429s inside one cooldown count as **one** lockout: the number measures how often the
  model went out, not how many requests bounced off it.

### The dashboard describes now, not the last decision

Two things used to mislead:

- **The candidate board was a stored snapshot**, refreshed only when the rotator acted. Refreshing
  the page by hand changed nothing, because nothing recomputed. It is derived per request now -
  ranking is arithmetic and touches nothing upstream, so there is no reason to cache it.
- **A countdown past its reset clamped to zero** and stayed there, so the panel showed a deadline
  that expired hours ago beside a percentage that stopped being true at the same moment. Measured on
  the live fleet: three credentials displayed at 80%, 86% and 95% spent were in fact all at 0%,
  fully reset. The boundary is now rolled forward by whole periods and flagged `reset_inferred`, and
  the reconciliation replaces the percentage with a real reading - three probes corrected all three.

A credential upstream has refused is never probed, so its window stays marked inferred for good.
That is honest rather than a gap: nothing short of a fresh login can make it answer.

## Dashboard

**The panel speaks English and Simplified Chinese**, switchable in the toolbar and
remembered in the browser; the first visit follows the browser's language.
Backend warnings travel as structured codes rather than finished sentences, so
they switch language with everything else instead of leaving the page half
translated.

Per credential:

- **额度 / 时间双进度条** — quota consumed against clock elapsed, side by side.
  The comparison is the fastest read on who is over-consuming.
- **节奏** — `used_percent ÷ time_progress_percent`. Above 1.15 is flagged 偏快,
  below 0.85 is 宽裕. Above 1 means the window runs out before it resets.
- **Quota / spent / remaining / per-day** in USD for every window, plus a reset countdown.
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

- **Fleet usage** — **the chart covers one week.** Hourly spend as bars on the
  left axis, with a **trailing seven-day total** on its own right axis.

  The line used to be spend accumulated since the chart began, which could only
  ever rise: it said nothing beyond "time has passed". A trailing window is
  stationary - flat while load is steady, rising only when load genuinely
  grows, falling when it eases. Seven days because that is the window the quota
  itself is denominated in.

  The left-hand stretch is dashed where fewer than seven days of buckets sit
  behind those points, so the total is short by an unknown amount rather than
  genuinely lower. Retention is deliberately twice the chart span, so that every
  visible point ends up with a complete week behind it.
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

`model_health_days` (default 14) is how long a (credential, model) availability record is kept after
its last request. It also decides how long a credential that has stopped serving a model still
counts towards that model's capacity, so it wants to be comfortably longer than the longest quota
window and comfortably shorter than "that credential is retired".

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
      rotator:
        enabled: false          # off by default: this is the one part that writes outside data_dir
        dry_run: true           # decide and log everything, change nothing
        keep_enabled: 2         # a pool of one cannot hand over
        switch_at_percent: 10
        lead_time_minutes: 15
        horizon_hours: 4
        check_interval_seconds: 60   # costs nothing: the check is arithmetic
        min_switch_gap_minutes: 10
        max_changes_per_day: 20
        probe_model: gpt-5.6-sol     # must be a model these accounts can actually call
        disable_dead_tokens: true
        confirm_before_switch: true  # one probe each side before acting; off means pure arithmetic
        max_probes_per_day_per_credential: 6   # hard per-credential backstop
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
byte-for-byte what a browser receives: bar count, a trailing window that must go flat
under steady load, the dashed/solid split,
points per known hour, rollover restart, and the degenerate cases.

Currently 91 checks. The scenario is arithmetic rather than opinion: every request costs exactly
$2.00 at gpt-5.6-sol public pricing and moves the percentage by exactly one
point, so the answer must be $200. Covered: both estimators agreeing, cache-read
carve-out, reasoning tokens not double-billed, unknown models flagged, restart
with a downtime gap, window rollover, the panel leaking nothing, plus cache
savings, failure counting, pace ratio and the hourly series.
