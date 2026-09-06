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
import time

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

# Without these the harness produces nothing and every assertion silently reads
# an empty result, which looks like a hung suite rather than a missing build.
for _path, _hint in ((SO, "make build"), (HARNESS, "make build/harness")):
    if not os.path.exists(_path):
        sys.exit("missing %s - run `%s` first" % (_path, _hint))


def config(price_url=""):
    return ("enabled: true\npriority: 100\ndata_dir: %s\n"
            "price_source_url: \"%s\"\nevent_log: true\nflush_seconds: 1\n"
            % (DATA_DIR, price_url))


def usage(windows, model="gpt-5.6-sol", cache_read=0, failed=False, extra_headers=None,
          auth="codex-demo-team.json"):
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
        "AuthID": auth, "AuthIndex": "0", "AuthType": "codex",
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


def run(steps, path=None, price_url="", route="data", auth_list=None):
    path = path or os.path.join(BUILD, "script.txt")
    script(steps, path, price_url=price_url, route=route)
    env = dict(os.environ)
    # The harness serves this back from host.auth.list, which is the only way
    # the plugin learns that a credential is disabled.
    env["HARNESS_AUTH_LIST"] = json.dumps(auth_list or [])
    out = subprocess.run([HARNESS, SO, path], capture_output=True, text=True, env=env).stdout
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
# Warnings are structured, not prose, so the panel can render them in either
# language rather than whichever one the plugin was compiled with.
ext = [x for x in rep["warnings"] if x.get("code") == "external_usage"]
check("warning is structured", len(ext), 1)
check("warning carries the percent", round(ext[0]["percent"], 2), 6.00)
check("warning carries the window", ext[0]["window"], "7d")

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
check("ships a language switcher", 'id="lang"' in html, True)
check("ships the availability board", 'id="modeltable"' in html, True)
check("availability strings in both languages",
      ("模型可用性" in html) and ("Model availability" in html), True)
check("carries both dictionaries", ("zh: {" in html) and ("en: {" in html), True)
check("both titles present", ("Codex 额度美元估算" in html) and ("Codex Quota USD" in html), True)
# Dictionary parity is asserted in test_charts.js, which can evaluate the real
# object instead of guessing at it with a regex.
with open(os.path.join(BUILD, "panel.html"), "w", encoding="utf-8") as fh:
    fh.write(html)

print()
print("=" * 74)
print("M. per-model availability: a lockout hits one model, not the credential")
print("=" * 74)
# Deadlines are compared against wall-clock time, so these have to be real
# future instants rather than the fixed cycle keys the accounting tests use.
SOON = int(time.time()) + 3600
LATER = int(time.time()) + 4 * 86400
LIMIT = {"X-Codex-Rate-Limit-Reached-Type": ["usage_limit_reached"]}


def health(report, model):
    for m in report.get("models", []):
        if m["model"] == model:
            return m
    return None


def cred(model_row, index=0):
    return model_row["by_credential"][index]


def codes(report, code):
    return [w for w in report["warnings"] if w.get("code") == code]


def authfile(name, disabled=False):
    return {"id": name, "name": name, "label": name, "type": "codex", "disabled": disabled}


shutil.rmtree(DATA_DIR, ignore_errors=True)
# gpt-5.6-sol keeps working while gpt-6-astra exhausts the 5-hour window: one
# credential, one shared set of quota headers, exactly one model refused.
rep = run([
    ("usage.handle", usage([(FIVEH, 10, SOON), (WEEK, 5, LATER)], model="gpt-5.6-sol")),
    ("usage.handle", usage([(FIVEH, 40, SOON), (WEEK, 8, LATER)], model="gpt-6-astra")),
    ("usage.handle", usage([(FIVEH, 100, SOON), (WEEK, 12, LATER)], model="gpt-6-astra",
                           failed=True, extra_headers=LIMIT)),
    ("usage.handle", usage([(FIVEH, 100, SOON), (WEEK, 13, LATER)], model="gpt-5.6-sol")),
])
astra, sol = health(rep, "gpt-6-astra"), health(rep, "gpt-5.6-sol")
check("astra is down", astra["state"], "down")
check("astra has no credential left", astra["available"], 0)
check("astra is cooling on one", astra["cooling"], 1)
check("sol is unaffected", sol["state"], "ok")
check("sol still has its credential", sol["available"], 1)
check("blocked window identified", cred(astra)["blocked_window"], "5h")
check("deadline is the window reset", cred(astra)["cooldown_until"][:19],
      time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(SOON)))
check("deadline is not a guess", cred(astra)["cooldown_estimated"], False)
check("reason carried through", cred(astra)["reason"], "usage_limit_reached")
check("down model warns", len(codes(rep, "model_unavailable")), 1)
check("warning names the model", codes(rep, "model_unavailable")[0]["model"], "gpt-6-astra")
check("broken model sorts first", rep["models"][0]["model"], "gpt-6-astra")

print()
print("=" * 74)
print("N. a served request ends the lockout, whatever the deadline said")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
# Upstream hands quota back early often enough that a recorded deadline must
# never outrank a request that actually went through.
rep = run([
    ("usage.handle", usage([(FIVEH, 100, SOON)], model="gpt-6-astra", failed=True,
                           extra_headers=LIMIT)),
    ("usage.handle", usage([(FIVEH, 20, SOON)], model="gpt-6-astra")),
])
astra = health(rep, "gpt-6-astra")
check("credential is back", astra["available"], 1)
check("model is healthy again", astra["state"], "ok")
check("no stale countdown", "cooldown_until" in cred(astra), False)
check("the lockout is still counted", cred(astra)["blocks"], 1)
check("no warning once it recovers", len(codes(rep, "model_unavailable")), 0)

print()
print("=" * 74)
print("O. the deadline belongs to the full window, not the next reset")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
# The weekly window is the full one, so the model stays out until the weekly
# reset four days away, even though the 5-hour window resets within the hour.
rep = run([
    ("usage.handle", usage([(FIVEH, 30, SOON), (WEEK, 100, LATER)], model="gpt-6-astra",
                           failed=True, extra_headers=LIMIT)),
])
c = cred(health(rep, "gpt-6-astra"))
check("waits for the weekly reset", c["blocked_window"], "7d")
check("countdown is days, not the hour", c["cooldown_in_seconds"] > 3 * 86400, True)

print()
print("=" * 74)
print("P. credits can run out with every percentage still looking fine")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
# No window is full, so there is no reset to read a deadline off. The soonest
# reset is the earliest the situation can change, and it is flagged as a guess.
rep = run([
    ("usage.handle", usage([(FIVEH, 40, SOON), (WEEK, 50, LATER)], model="gpt-6-astra",
                           failed=True, extra_headers={
                               "X-Codex-Credits-Has-Credits": ["False"],
                               "X-Codex-Rate-Limit-Reached-Type": ["workspace_member_credits_depleted"]})),
])
c = cred(health(rep, "gpt-6-astra"))
check("still recognised as a lockout", c["state"], "cooling")
check("deadline is the soonest reset", c["blocked_window"], "5h")
check("deadline is flagged as a guess", c["cooldown_estimated"], True)
check("reason is the credit one", c["reason"], "workspace_member_credits_depleted")

print()
print("=" * 74)
print("Q. an ordinary failure is not a lockout")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
# A dropped connection or a rejected request fails with no quota signal at all,
# and must not take the model out of service.
rep = run([
    ("usage.handle", usage([(FIVEH, 40, SOON)], model="gpt-6-astra")),
    ("usage.handle", usage([(FIVEH, 40, SOON)], model="gpt-6-astra", failed=True)),
])
astra = health(rep, "gpt-6-astra")
check("model still available", astra["state"], "ok")
check("credential still counted", astra["available"], 1)
check("failure still recorded", astra["failed"], 1)
check("no lockout counted", cred(astra)["blocks"], 0)
check("no false alarm", len(codes(rep, "model_unavailable")), 0)

print()
print("=" * 74)
print("R. capacity is counted across credentials, disabled ones excluded")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
# Three credentials serve the model: one healthy, one cooling, one switched off.
# A disabled credential is listed but is not capacity - eight of nine disabled
# is exactly what turns a single 429 into a dead model.
rep = run([
    ("usage.handle", usage([(FIVEH, 10, SOON)], model="gpt-6-astra", auth="cred-a.json")),
    ("usage.handle", usage([(FIVEH, 100, SOON)], model="gpt-6-astra", auth="cred-b.json",
                           failed=True, extra_headers=LIMIT)),
    ("usage.handle", usage([(FIVEH, 10, SOON)], model="gpt-6-astra", auth="cred-c.json")),
], auth_list=[authfile("cred-a.json"), authfile("cred-b.json"),
              authfile("cred-c.json", disabled=True)])
astra = health(rep, "gpt-6-astra")
check("disabled one is not capacity", astra["credentials"], 2)
check("one credential is serving", astra["available"], 1)
check("one credential is cooling", astra["cooling"], 1)
check("the disabled one is still listed", astra["disabled"], 1)
check("degraded, not down", astra["state"], "degraded")
check("all three appear", len(astra["by_credential"]), 3)
check("the cooling one sorts first", cred(astra)["state"], "cooling")
check("no outage warning while one serves", len(codes(rep, "model_unavailable")), 0)

print()
print("=" * 74)
print("S. one credential for a model is a standing outage warning")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
steps = [("usage.handle", usage([(FIVEH, 10, SOON)], model="gpt-6-astra", auth="cred-a.json"))] * 25
steps += [("usage.handle", usage([(FIVEH, 100, SOON)], model="gpt-6-astra", auth="cred-b.json",
                                 failed=True, extra_headers=LIMIT))] * 2
rep = run(steps, auth_list=[authfile("cred-a.json"), authfile("cred-b.json", disabled=True)])
astra = health(rep, "gpt-6-astra")
check("single point flagged", astra["single_point"], True)
single = codes(rep, "model_single_point")
check("warned once", len(single), 1)
check("warning names the credential", single[0]["credential"], "cred-a.json")
check("warning counts the disabled ones", single[0]["disabled"], 1)
# Repeated refusals inside one cooldown are one lockout, not two: the count is
# how often the model went out, not how many requests bounced off it.
b = [c for c in astra["by_credential"] if c["credential"] == "cred-b.json"][0]
check("repeat 429s are one lockout", b["blocks"], 1)

print()
print("=" * 74)
if failures:
    print("FAILED: " + ", ".join(failures))
    sys.exit(1)
print("all checks passed")
