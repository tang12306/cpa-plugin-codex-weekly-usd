#!/usr/bin/env python3
"""Known-answer tests for the codex-weekly-usd plugin, driven through the real
C ABI via harness.c.

The scenarios are built so the correct output is arithmetic, not opinion: every
request costs exactly $2.00 at gpt-5.6-sol public pricing, and the percentage
steps are chosen so the quota must come out at a round number.
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
WEEK = 10080
FIVEH = 300
RESET_W = 1787424611
RESET_5 = 1787400000

failures = []


def config(price_url=""):
    return ("enabled: true\npriority: 100\ndata_dir: %s\n"
            "price_source_url: \"%s\"\nevent_log: true\nflush_seconds: 1\n"
            % (DATA_DIR, price_url))


def usage(windows, model="gpt-5.6-sol", cache_read=0, failed=False, extra_headers=None):
    """windows: list of (minutes, percent, reset_at) in slot order.

    The first entry is emitted as "primary" and the second as "secondary".
    Which window occupies which slot is deliberately a test parameter: upstream
    has already swapped them once, and the plugin must not care.
    """
    headers = {
        "X-Codex-Plan-Type": ["team"],
        "X-Codex-Active-Limit": ["premium"],
    }
    for slot, (minutes, percent, reset_at) in zip(("Primary", "Secondary"), windows):
        headers["X-Codex-%s-Used-Percent" % slot] = [str(percent)]
        headers["X-Codex-%s-Window-Minutes" % slot] = [str(minutes)]
        headers["X-Codex-%s-Reset-At" % slot] = [str(reset_at)]
    if extra_headers:
        headers.update(extra_headers)
    return {
        "Provider": "codex", "ExecutorType": "codex", "Model": model, "Alias": "",
        "AuthID": "codex-demo-team.json", "AuthIndex": "0", "AuthType": "codex",
        "RequestedAt": "2026-08-27T04:00:00Z", "Failed": failed,
        "Detail": {
            "InputTokens": IN_TOKENS, "OutputTokens": OUT_TOKENS,
            "ReasoningTokens": 12_000, "CachedTokens": cache_read,
            "CacheReadTokens": cache_read, "CacheCreationTokens": 0,
            "TotalTokens": IN_TOKENS + OUT_TOKENS,
        },
        "ResponseHeaders": headers,
    }


def weekly(percent, **kw):
    """One window only, in the primary slot: the pre-5h-limit header shape."""
    return usage([(WEEK, percent, RESET_W)], **kw)


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


def win(report, minutes, account=0):
    for w in report["accounts"][account]["windows"]:
        if w["minutes"] == minutes:
            return w
    return None


def check(name, got, want, tol=0.02):
    ok = abs(got - want) <= tol if isinstance(want, float) else got == want
    print("  %-46s %-22s %s" % (name, got, "OK" if ok else "FAIL (want %s)" % (want,)))
    if not ok:
        failures.append(name)


print("=" * 74)
print("A. fresh install, 6 requests at $2.00 each, 1% per request")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = run([("usage.handle", weekly(i)) for i in range(6)])
w = win(rep, WEEK)
est = w["estimate"]
check("quota_usd", round(est["quota_usd"], 2), 200.00)
check("quota_usd_by_window", round(est["quota_usd_by_window"], 2), 200.00)
check("quota_usd_by_delta", round(est["quota_usd_by_delta"], 2), 200.00)
# Only 5 of the 6 requests are covered by the last reading (5%): the sixth is
# still pending, exactly as the estimator intends.
check("attributed_usd (5 x $2)", round(est["attributed_usd"], 2), 10.00)
check("spent_usd (= quota x 5%)", round(est["spent_usd"], 2), 10.00)
check("remaining_usd", round(est["remaining_usd"], 2), 190.00)
check("method", est["method"], "window")
check("full_coverage", w["full_coverage"], True)
check("window label", w["label"], "7d")
check("calibration samples", est["samples"], 5)

print()
print("=" * 74)
print("B. cache-read tokens are carved out of input, not billed on top")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = run([("usage.handle", weekly(0, cache_read=100_000))])
w = win(rep, WEEK)
want = 100_000 / 1e6 * 5 + 100_000 / 1e6 * 0.5 + OUT_TOKENS / 1e6 * 30
check("cached request cost", round(w["usd_observed"], 4), round(want, 4))
check("reasoning tokens recorded", w["tokens"]["ReasoningTokens"], 12000)

print()
print("=" * 74)
print("C. restart with a gap: percentage moved while the plugin was down")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
run([("usage.handle", weekly(i)) for i in range(6)])
rep = run([("usage.handle", weekly(20))])
w = win(rep, WEEK)
est = w["estimate"]
check("full_coverage invalidated", w["full_coverage"], False)
check("by_window suppressed", est["quota_usd_by_window"], 0.0)
check("falls back to calibration", est["method"], "delta")
check("quota still ~$200", round(est["quota_usd"], 2), 200.00)

print()
print("=" * 74)
print("D. cycle rollover: per-cycle counters reset, calibration survives")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
steps = [("usage.handle", weekly(i)) for i in range(6)]
steps += [("usage.handle", usage([(WEEK, i, RESET_W + 7 * 86400)])) for i in range(3)]
rep = run(steps)
w = win(rep, WEEK)
check("requests reset", w["requests"], 3)
check("cycles counted", w["cycles"], 2)
check("calibration survived rollover", w["estimate"]["samples"], 7)
check("quota still ~$200", round(w["estimate"]["quota_usd"], 2), 200.00)
check("new cycle full coverage", w["full_coverage"], True)

print()
print("=" * 74)
print("E. the 5-hour and weekly limits are priced independently")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
# Weekly moves 1 point per request, the 5-hour window 5 points, for the same
# $2.00. The quotas must therefore differ by exactly 5x.
steps = [("usage.handle", usage([(FIVEH, i * 5, RESET_5), (WEEK, i, RESET_W)])) for i in range(6)]
rep = run(steps)
w5, w7 = win(rep, FIVEH), win(rep, WEEK)
check("5h quota  = $10 / 25%", round(w5["estimate"]["quota_usd"], 2), 40.00)
check("7d quota  = $10 / 5%", round(w7["estimate"]["quota_usd"], 2), 200.00)
check("5h label", w5["label"], "5h")
check("both windows billed the same spend", round(w5["usd_observed"], 4), round(w7["usd_observed"], 4))
check("binding window is the fuller one", rep["accounts"][0]["binding"]["minutes"], FIVEH)
check("headline window is the longest", rep["accounts"][0]["longest"]["minutes"], WEEK)
check("window totals cover both", len(rep["window_totals"]), 2)

print()
print("=" * 74)
print("F. primary/secondary swapping slots must not corrupt a window")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
# Phase 1 is the old header shape (weekly in the primary slot, no 5h window).
steps = [("usage.handle", usage([(WEEK, i, RESET_W)])) for i in range(6)]
# Phase 2 is the shape upstream switched to: 5h primary, weekly secondary.
steps += [("usage.handle", usage([(FIVEH, i * 5, RESET_5), (WEEK, 6 + i, RESET_W)])) for i in range(6)]
rep = run(steps)
w7 = win(rep, WEEK)
check("weekly window kept its identity", w7["minutes"], WEEK)
check("weekly cycles not restarted", w7["cycles"], 1)
check("weekly quota still ~$200", round(w7["estimate"]["quota_usd"], 2), 200.00)
check("weekly gained more samples", w7["estimate"]["samples"] > 5, True)
check("5h window created separately", win(rep, FIVEH)["minutes"], FIVEH)

print()
print("=" * 74)
print("G. an upstream-granted mid-cycle reset must not inflate the quota")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
steps = [("usage.handle", weekly(i)) for i in range(6)]
# Percentage falls back to zero while the reset timestamp stays put: OpenAI
# handed back the quota mid-cycle. Without detection the pre-reset spend would
# be divided by the new small percentage.
steps += [("usage.handle", weekly(0))]
steps += [("usage.handle", weekly(i)) for i in range(1, 4)]
rep = run(steps)
w = win(rep, WEEK)
check("granted reset detected", w["granted_resets"], 1)
check("cycle spend restarted", w["requests"], 4)
check("quota not inflated", round(w["estimate"]["quota_usd"], 2), 200.00)

print()
print("=" * 74)
print("H. calibration is bounded, so evidence cannot ratchet up forever")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
# 400 percentage steps across four cycles: far more samples than the cap.
steps = []
for cycle in range(4):
    reset = RESET_W + cycle * 7 * 86400
    for i in range(101):
        steps.append(("usage.handle", usage([(WEEK, i, reset)])))
rep = run(steps)
w = win(rep, WEEK)
est = w["estimate"]
check("samples capped", est["samples"] <= 160, True)
check("evidence stays bounded", est["evidence_percent"] <= 160.0, True)
check("quota still ~$200", round(est["quota_usd"], 2), 200.00)
check("all four cycles seen", w["cycles"], 4)

print()
print("=" * 74)
print("I. quota consumed with no spend of ours is reported, not swallowed")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = run([
    ("usage.handle", weekly(0)),
    ("usage.handle", weekly(1)),
    ("usage.handle", weekly(2, failed=True)),   # pairs, leaves pending at 0
    ("usage.handle", weekly(8, failed=True)),   # +6 points with nothing billed
])
w = win(rep, WEEK)
check("unexplained percent recorded", round(w["unexplained_percent"], 2), 6.00)
check("failed requests counted", w["failed"], 2)
check("warning raised", any("其它客户端" in x for x in rep["warnings"]), True)

print()
print("=" * 74)
print("J. credit exhaustion is captured even when percentages look fine")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = run([("usage.handle", weekly(1, extra_headers={
    "X-Codex-Credits-Has-Credits": ["False"],
    "X-Codex-Credits-Unlimited": ["False"],
    "X-Codex-Rate-Limit-Reached-Type": ["workspace_owner_credits_depleted"],
}))])
acct = rep["accounts"][0]
check("limit reason surfaced", acct["limit_reached_type"], "workspace_owner_credits_depleted")
check("credits recorded", acct["credits"]["has_credits"], False)

print()
print("=" * 74)
print("K. price catalog is fetched through the host, not by dialling out")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = run([("sleep", 400), ("usage.handle", weekly(0, model="harness-model"))],
          price_url="https://harness.invalid/catalog.json")
w = win(rep, WEEK)
want = IN_TOKENS / 1e6 * 3 + OUT_TOKENS / 1e6 * 9
check("priced from host-fetched catalog", round(w["usd_observed"], 4), round(want, 4))
check("model was not left unpriced", rep["accounts"][0]["unpriced_requests"], 0)
prices = run([("sleep", 400)], price_url="https://harness.invalid/catalog.json", route="prices")
check("transport recorded as host", prices["transport"], "host")
check("builtin table still merged", any(m["model"] == "gpt-5.6-sol" for m in prices["models"]), True)

print()
print("=" * 74)
print("L. panel is unauthenticated, carries no data, and ships the charts")
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
if failures:
    print("FAILED: " + ", ".join(failures))
    sys.exit(1)
print("all checks passed")
