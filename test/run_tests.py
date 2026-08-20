#!/usr/bin/env python3
"""Known-answer tests for the codex-weekly-usd plugin, driven through the real
C ABI via harness.c.

The scenario is built so the correct output is arithmetic, not opinion: every
request costs exactly $2.00 at gpt-5.6-sol public pricing and moves the upstream
percentage by exactly one point, so the weekly quota must come out as $200.
"""
import base64
import json
import os
import shutil
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BUILD = os.path.join(ROOT, "build")
SO = os.path.join(ROOT, "codex-weekly-usd.so")
HARNESS = os.path.join(BUILD, "harness")
DATA_DIR = os.path.join(BUILD, "cwu-test")
os.makedirs(BUILD, exist_ok=True)

IN_TOKENS = 200_000
OUT_TOKENS = 33_333
COST = IN_TOKENS / 1e6 * 5 + OUT_TOKENS / 1e6 * 30  # $2.00 (to 5 decimals)
RESET_AT = 1787424611
WINDOW_MIN = 10080

def config(price_url=""):
    """Plugin config block. An empty price_source_url freezes pricing at the
    built-in table, which is what most scenarios want; scenario G sets it so the
    catalog fetch runs."""
    return ("enabled: true\npriority: 100\ndata_dir: %s\n"
            "price_source_url: \"%s\"\nevent_log: true\nflush_seconds: 1\n"
            % (DATA_DIR, price_url))

failures = []


def usage(percent, model="gpt-5.6-sol", reset_at=RESET_AT, cache_read=0, failed=False):
    return {
        "Provider": "codex", "ExecutorType": "codex", "Model": model, "Alias": "",
        "AuthID": "codex-demo-team.json", "AuthIndex": "0", "AuthType": "codex",
        "RequestedAt": "2026-08-20T13:00:00Z", "Failed": failed,
        "Detail": {
            "InputTokens": IN_TOKENS, "OutputTokens": OUT_TOKENS,
            "ReasoningTokens": 12_000, "CachedTokens": cache_read,
            "CacheReadTokens": cache_read, "CacheCreationTokens": 0,
            "TotalTokens": IN_TOKENS + OUT_TOKENS,
        },
        "ResponseHeaders": {
            "X-Codex-Primary-Used-Percent": [str(percent)],
            "X-Codex-Primary-Window-Minutes": [str(WINDOW_MIN)],
            "X-Codex-Primary-Reset-At": [str(reset_at)],
            "X-Codex-Primary-Reset-After-Seconds": ["192290"],
            "X-Codex-Secondary-Used-Percent": ["0"],
            "X-Codex-Secondary-Window-Minutes": ["0"],
            "X-Codex-Plan-Type": ["team"], "X-Codex-Active-Limit": ["premium"],
        },
    }


def script(steps, path, tail=True, price_url="", route="data"):
    lines = ["plugin.register\t" + json.dumps(
        {"config_yaml": base64.b64encode(config(price_url).encode()).decode(), "schema_version": 1},
        separators=(",", ":"))]
    for method, payload in steps:
        lines.append(method + "\t" + json.dumps(payload, separators=(",", ":")))
    if tail:
        lines.append("management.handle\t" + json.dumps(
            {"Method": "GET", "Path": "/v0/management/codex-weekly-usd/" + route},
            separators=(",", ":")))
        lines.append("plugin.shutdown\t{}")
    with open(path, "w") as fh:
        fh.write("\n".join(lines) + "\n")


def run(steps, path=None, price_url="", route="data"):
    path = path or os.path.join(BUILD, "script.txt")
    script(steps, path, price_url=price_url, route=route)
    out = subprocess.run([HARNESS, SO, path], capture_output=True, text=True).stdout
    bodies = []
    for blk in out.split("--- "):
        if blk.startswith("management.handle"):
            env = json.loads(blk.split("\n", 1)[1].strip())
            bodies.append(base64.b64decode(env["result"]["Body"]))
    return json.loads(bodies[-1])


def check(name, got, want, tol=0.02):
    ok = abs(got - want) <= tol if isinstance(want, float) else got == want
    print("  %-46s %-22s %s" % (name, got, "OK" if ok else "FAIL (want %s)" % (want,)))
    if not ok:
        failures.append(name)


print("=" * 74)
print("A. fresh install, 6 requests at $2.00 each, 1% per request")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = run([("usage.handle", usage(i)) for i in range(6)])
acct = rep["accounts"][0]
est = acct["estimate"]
check("quota_usd", round(est["quota_usd"], 2), 200.00)
check("quota_usd_by_window", round(est["quota_usd_by_window"], 2), 200.00)
check("quota_usd_by_delta", round(est["quota_usd_by_delta"], 2), 200.00)
# Only 5 of the 6 requests are covered by the last reading (5%): the sixth is
# still pending, exactly as the estimator intends.
check("attributed_usd (5 x $2)", round(est["attributed_usd"], 2), 10.00)
check("spent_usd (= quota x 5%)", round(est["spent_usd"], 2), 10.00)
check("remaining_usd", round(est["remaining_usd"], 2), 190.00)
check("method", est["method"], "window")
check("window_full_coverage", acct["window_full_coverage"], True)
check("window_label", acct["window_label"], "7d")
check("calibration samples", acct["calibration"]["samples"], 5)

print()
print("=" * 74)
print("B. cache-read tokens are carved out of input, not billed on top")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = run([("usage.handle", usage(0, cache_read=100_000))])
acct = rep["accounts"][0]
# 100k fresh input @ $5 + 100k cache-read @ $0.50 + 33,333 output @ $30
want = 100_000 / 1e6 * 5 + 100_000 / 1e6 * 0.5 + OUT_TOKENS / 1e6 * 30
check("cached request cost", round(acct["window_usd_observed"], 4), round(want, 4))
check("reasoning tokens recorded", acct["window_tokens"]["ReasoningTokens"], 12000)

print()
print("=" * 74)
print("C. restart with a gap: percentage moved while the plugin was down")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
run([("usage.handle", usage(i)) for i in range(6)])          # first process
rep = run([("usage.handle", usage(20))])                      # restart; 6% -> 20%
acct = rep["accounts"][0]
est = acct["estimate"]
check("window_full_coverage invalidated", acct["window_full_coverage"], False)
check("by_window suppressed", est["quota_usd_by_window"], 0.0)
check("falls back to calibration", est["method"], "delta")
check("quota still ~$200", round(est["quota_usd"], 2), 200.00)

print()
print("=" * 74)
print("D. window rollover: per-window counters reset, calibration survives")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
steps = [("usage.handle", usage(i)) for i in range(6)]
steps += [("usage.handle", usage(i, reset_at=RESET_AT + 7 * 86400)) for i in range(3)]
rep = run(steps)
acct = rep["accounts"][0]
est = acct["estimate"]
check("window_requests reset", acct["window_requests"], 3)
check("calibration survived rollover", acct["calibration"]["samples"], 7)
check("quota still ~$200", round(est["quota_usd"], 2), 200.00)
check("new window full coverage", acct["window_full_coverage"], True)

print()
print("=" * 74)
print("E. derived metrics: cache savings, failures, pace, hourly series")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = run([("usage.handle", usage(0)),
           ("usage.handle", usage(1, cache_read=100_000)),
           ("usage.handle", usage(2, failed=True))])
acct = rep["accounts"][0]
# 100k cache-read tokens cost $0.50/M instead of $5/M -> $0.45 saved
check("cache saved usd", round(acct["window_saved_usd"], 4), 0.45)
check("cache hit rate", round(acct["cache_hit_rate"], 1), round(100_000 / 600_000 * 100, 1))
check("failed request counted", acct["window_failed"], 1)
check("failed marked on model row", acct["by_model"][0]["failed"], 1)
check("failure rate", round(acct["failure_rate"], 1), round(1 / 3 * 100, 1))
check("hourly series present", len(acct["series"]) >= 1, True)
check("series carries spend", acct["series"][-1]["usd"] > 0, True)
check("series carries percent", "percent" in acct["series"][-1], True)
check("fleet series present", len(rep["fleet_series"]) >= 1, True)
check("time progress reported", "time_progress_percent" in acct, True)
check("pace ratio reported", "pace_ratio" in acct, True)
check("totals carry savings", round(rep["totals"]["cache_saved_usd"], 4), 0.45)
check("totals carry failures", rep["totals"]["window_failed"], 1)

print()
print("=" * 74)
print("F. panel is unauthenticated, carries no data, and ships the charts")
print("=" * 74)
panel_script = os.path.join(BUILD, "panel.txt")
script([], panel_script, tail=False)
with open(panel_script, "a") as fh:
    fh.write("management.handle\t" + json.dumps(
        {"Method": "GET", "Path": "/v0/resource/plugins/codex-weekly-usd/panel"}) + "\n")
out = subprocess.run([HARNESS, SO, panel_script], capture_output=True, text=True).stdout
env = json.loads(out.split("--- management.handle (rc=0) ---")[1].strip())
html = base64.b64decode(env["result"]["Body"]).decode()
check("content-type", env["result"]["Headers"]["content-type"][0], "text/html; charset=utf-8")
check("is html", html.startswith("<!doctype html>"), True)
check("leaks no credential id", "codex-demo-team.json" not in html, True)
check("leaks no plan or percent data", ("premium" not in html) and ("10080" not in html), True)
check("asks for management key", "X-Management-Key" in html, True)
check("ships cumulative line chart", "polyline" in html, True)
check("ships quota curve", "drawQuotaCurve" in html, True)
check("ships constant-rate reference", "stroke-dasharray" in html, True)
check("charts are inline svg only", "<script src" not in html and "http://" not in html, True)
with open(os.path.join(BUILD, "panel.html"), "w", encoding="utf-8") as fh:
    fh.write(html)

print()
print("=" * 74)
print("G. price catalog is fetched through the host, not by dialling out")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
# The harness answers host.http.do with a one-model catalog priced at
# input $3 / output $9, so 200k in + 33,333 out must come to $0.90.
rep = run([("sleep", 400), ("usage.handle", usage(0, model="harness-model"))],
          price_url="https://harness.invalid/catalog.json")
acct = rep["accounts"][0]
want = IN_TOKENS / 1e6 * 3 + OUT_TOKENS / 1e6 * 9
check("priced from host-fetched catalog", round(acct["window_usd_observed"], 4), round(want, 4))
check("model was not left unpriced", acct["unpriced_requests"], 0)

prices = run([("sleep", 400)], price_url="https://harness.invalid/catalog.json", route="prices")
check("transport recorded as host", prices["transport"], "host")
check("catalog model present", any(m["model"] == "harness-model" for m in prices["models"]), True)
check("builtin table still merged", any(m["model"] == "gpt-5.6-sol" for m in prices["models"]), True)

print()
print("=" * 74)
if failures:
    print("FAILED: " + ", ".join(failures))
    sys.exit(1)
print("all checks passed")
