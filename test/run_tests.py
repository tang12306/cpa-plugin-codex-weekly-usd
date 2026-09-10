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

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from probe_server import start_probe_server, start_socks5  # noqa: E402

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BUILD = os.path.join(ROOT, "build")
SO = os.path.join(ROOT, "codex-weekly-usd.so")
HARNESS = os.path.join(BUILD, "harness")
DATA_DIR = os.path.join(BUILD, "cwu-test")
os.makedirs(BUILD, exist_ok=True)

# Chosen so one request costs exactly $2.00 at the sol rate card, which is what
# every round number in this suite is built on.
IN_TOKENS = 250_000
OUT_TOKENS = 50_000
SOL_IN, SOL_OUT, SOL_CACHE = 4, 20, 0.4
COST = IN_TOKENS / 1e6 * SOL_IN + OUT_TOKENS / 1e6 * SOL_OUT  # $2.00 exactly
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


def config(price_url="", rotator=None):
    text = ("enabled: true\npriority: 100\ndata_dir: %s\n"
            "price_source_url: \"%s\"\nevent_log: true\nflush_seconds: 1\n"
            % (DATA_DIR, price_url))
    if rotator:
        text += "rotator:\n"
        for key in sorted(rotator):
            value = rotator[key]
            if isinstance(value, bool):
                value = "true" if value else "false"
            text += "  %s: %s\n" % (key, value)
    return text


def usage(windows, model="gpt-5.6-sol", cache_read=0, failed=False, extra_headers=None,
          auth="codex-demo-team.json", index=None, status=0, body=""):
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
        "AuthID": auth, "AuthIndex": index or "0", "AuthType": "codex",
        "RequestedAt": "2026-08-27T04:00:00Z", "Failed": failed,
        # The host has always sent this on a failed request; the plugin simply
        # did not declare the field, so 401 looked like any other error.
        "Failure": {"StatusCode": status, "Body": body},
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


def script(steps, path, tail=True, price_url="", route="data", rotator=None, method="GET"):
    lines = ["plugin.register\t" + json.dumps(
        {"config_yaml": base64.b64encode(config(price_url, rotator).encode()).decode(),
         "schema_version": 1},
        separators=(",", ":"))]
    for step_method, payload in steps:
        lines.append(step_method + "\t" + json.dumps(payload, separators=(",", ":")))
    if tail:
        lines.append("management.handle\t" + json.dumps(
            {"Method": method, "Path": "/v0/management/codex-weekly-usd/" + route},
            separators=(",", ":")))
        lines.append("plugin.shutdown\t{}")
    with open(path, "w") as fh:
        fh.write("\n".join(lines) + "\n")


def run(steps, path=None, price_url="", route="data", auth_list=None, rotator=None,
        method="GET", fixtures=None):
    path = path or os.path.join(BUILD, "script.txt")
    script(steps, path, price_url=price_url, route=route, rotator=rotator, method=method)
    env = dict(os.environ)
    # The harness serves this back from host.auth.list, which is the only way
    # the plugin learns that a credential is disabled.
    env["HARNESS_AUTH_LIST"] = json.dumps(auth_list or [])
    if fixtures:
        env.update(fixtures)
    out = subprocess.run([HARNESS, SO, path], capture_output=True, text=True, env=env).stdout
    bodies = []
    for blk in out.split("--- "):
        if blk.startswith("management.handle"):
            env = json.loads(blk.split("\n", 1)[1].strip())
            bodies.append(base64.b64decode(env["result"]["Body"]))
    return json.loads(bodies[-1])


def win(report, minutes, account=0):
    """account is an index, or a credential name - the row order is the map's,
    not the order the steps were fed in, so anything with more than one
    credential must say which one it means."""
    rows = report["accounts"]
    if isinstance(account, str):
        rows = [r for r in rows if account in (r.get("label"), r.get("auth_id"))]
        if not rows:
            raise AssertionError("no account row for %s (have %s)"
                                 % (account, [r.get("label") for r in report["accounts"]]))
        row = rows[0]
    else:
        row = rows[account]
    for w in row["windows"]:
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
want = ((IN_TOKENS - 100_000) / 1e6 * SOL_IN + 100_000 / 1e6 * SOL_CACHE
        + OUT_TOKENS / 1e6 * SOL_OUT)
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
# The new cycle has produced 2 points of movement, which is too little to
# divide by, so the estimate reaches back into the previous cycle - newest
# first, and only far enough to clear the bar. Not all 7: evidence from a cycle
# that has ended prices a quota that may no longer be the one being spent.
check("calibration reaches back only as far as it needs",
      w["estimate"]["samples"], 5)
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
check("ships constant-rate reference", "stroke-dasharray" in html, True)
check("charts are inline svg only", "<script src" not in html and "http://" not in html, True)
check("ships a language switcher", 'id="lang"' in html, True)
# Every Codex account serves every model off one meter, so a per-model board
# repeated the same credential list once per model. The data stays in the
# report - rotation reads the refusals out of it - but the page no longer
# draws it.
check("no per-model availability board", 'id="modeltable"' not in html, True)
check("ships the rotation board", 'id="rottable"' in html, True)
check("rotation strings in both languages",
      ("凭据轮换" in html) and ("Credential rotation" in html), True)
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


def authfile(name, disabled=False, index=None):
    return {"id": name, "name": name, "label": name, "type": "codex",
            "auth_index": index or ("idx-" + name), "disabled": disabled}


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
print("T. the rotator keeps a pool topped up without emptying it")
print("=" * 74)
# The rotator is the only part of this plugin that writes anything outside its
# own data directory, so these scenarios are about what it refuses to do at
# least as much as what it does. The harness answers host.auth.get from a
# fixture and records every host.auth.save instead of applying it, so nothing
# here can touch a real credential.
# What upstream actually returns for a revoked credential: an explicit code,
# and no quota headers of any kind.
REVOKED_BODY = ('{"error":{"message":"Encountered invalidated oauth token for user, '
                'failing request","code":"token_revoked"},"status":401}')

ROT_DIR = os.path.join(BUILD, "rot")
# One clock for every rotator fixture. Real reset times are fixed instants, so
# two decisions inside one quota cycle must see the same ones; deriving them
# from time.time() at each call would move the cycle boundary under the test.
ROT_NOW = int(time.time())
SAVE_LOG = os.path.join(ROT_DIR, "saves.jsonl")


def rot_fixtures(creds):
    """creds: name -> dict(disabled, status, windows=[(minutes, percent, reset_in_s)]).

    Writes the host.auth.get documents and the quota fixtures the local probe
    server answers from, and returns the auth list, the harness environment, the
    fixture directory and the absolute reset times so a test can seed the same
    windows through live traffic instead of through a probe.
    """
    auth_dir = os.path.join(ROT_DIR, "auth")
    probe_dir = os.path.join(ROT_DIR, "probe")
    live_dir = os.path.join(ROT_DIR, "live")
    shutil.rmtree(ROT_DIR, ignore_errors=True)
    os.makedirs(auth_dir)
    os.makedirs(probe_dir)
    os.makedirs(live_dir)

    now = ROT_NOW
    auth_list = []
    resets = {}
    for name, spec in creds.items():
        index = "idx-" + name
        # A spec can name its own token, which is how a test says "the operator
        # logged this account back in": re-authorising changes nothing visible
        # about a credential except the token inside it.
        token = spec.get("token") or ("tok-" + name)
        auth_list.append(authfile(name, disabled=spec.get("disabled", False), index=index))
        doc = {"access_token": token, "account_id": "acct-" + name,
               "refresh_token": "refresh-" + name,
               "disabled": spec.get("disabled", False)}
        if spec.get("proxy"):
            doc["proxy_url"] = spec["proxy"]
        if "priority" in spec:
            doc["priority"] = spec["priority"]
        entry = {"auth_index": index, "name": name, "json": doc}
        if spec.get("with_path"):
            # host.auth.get reports where the credential lives, and the plugin
            # writes there directly: host.auth.save cannot turn a credential
            # off, because the host rebuilds its record from the document
            # without reading `disabled` and persists that back over the file.
            live = os.path.join(live_dir, name)
            with open(live, "w") as fh:
                json.dump(doc, fh)
            entry["path"] = live
        with open(os.path.join(auth_dir, index + ".json"), "w") as fh:
            json.dump(entry, fh)
        headers = {}
        absolute = []
        for slot, (minutes, percent, reset_in) in zip(("Primary", "Secondary"),
                                                      spec.get("windows", [])):
            absolute.append((minutes, percent, now + reset_in))
        # probe_windows lets the probe answer with a different cycle from the
        # one live traffic last saw - which is the real case a resync exists
        # for: the traffic reading describes a window that has since rolled.
        for slot, (minutes, percent, reset_in) in zip(("Primary", "Secondary"),
                                                      spec.get("probe_windows",
                                                               spec.get("windows", []))):
            headers["X-Codex-%s-Used-Percent" % slot] = [str(percent)]
            headers["X-Codex-%s-Window-Minutes" % slot] = [str(minutes)]
            headers["X-Codex-%s-Reset-At" % slot] = [str(now + reset_in)]
        headers["X-Codex-Plan-Type"] = [spec.get("plan", "team")]
        resets[name] = absolute
        with open(os.path.join(probe_dir, token + ".json"), "w") as fh:
            json.dump({"StatusCode": spec.get("status", 200), "Headers": headers, "Body": ""}, fh)
    return (auth_list, {"HARNESS_AUTH_DIR": auth_dir, "HARNESS_PROBE_DIR": probe_dir,
                        "HARNESS_SAVE_LOG": SAVE_LOG}, probe_dir, resets)


def saves():
    """Every host.auth.save the plugin attempted, in order."""
    if not os.path.exists(SAVE_LOG):
        return []
    out = []
    for line in open(SAVE_LOG):
        line = line.strip()
        if not line:
            continue
        req = json.loads(line)
        out.append((req["name"], req["json"]["disabled"]))
    return out


# PROBES records every request the local probe server received during the last
# rotate(), which is how the "this design barely probes" claims are measured
# rather than asserted.
PROBES = []
SOCKS = [0]


def rotate(creds, rotator=None, steps=None, seed=True, route="rotate"):
    """Drive one rotator decision.

    seed=True replays each credential's window state through live traffic
    first, which is the state production is actually in: the plugin sees every
    response and records the quota headers off it. A test that skips seeding is
    describing a credential nothing has ever reported on.
    """
    settings = {"enabled": True, "keep_enabled": 2, "switch_at_percent": 10,
                "check_interval_seconds": 3600, "min_switch_gap_minutes": 0,
                "probe_model": "gpt-5.6-sol"}
    auth_list, fixtures, probe_dir, resets = rot_fixtures(creds)
    url, hits, socks_hits, shutdown = start_probe_server(probe_dir)
    settings["probe_url"] = url
    settings.update(rotator or {})

    pre = []
    if seed:
        for name, spec in creds.items():
            if not spec.get("seed", True) or not resets.get(name):
                continue
            pre.append(("usage.handle", usage(resets[name], auth=name,
                                              index="idx-" + name)))
            if spec.get("fail_401"):
                # What the proxy actually saw: a refused request, carrying a
                # status code and no quota headers at all.
                pre.append(("usage.handle", usage([], auth=name, index="idx-" + name,
                                                  failed=True, status=401,
                                                  body=REVOKED_BODY)))
    try:
        rep = run(pre + (steps or []), auth_list=auth_list, rotator=settings,
                  fixtures=fixtures, route=route, method="POST")
    finally:
        shutdown()
    PROBES[:] = hits
    SOCKS[0] = socks_hits[0]
    return rep


WEEKF = 10080
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 5, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 0, 400000)]},
})
check("a healthy pool is left alone", len(saves()), 0)
check("and says so", rep["last_reason"], "pool_healthy")
# The board has to describe the present, not the last rotation: a tick that
# does nothing still refreshes it, which is affordable because ranking no
# longer touches the network.
board = {c["file"]: c["enabled"] for c in rep.get("candidates", [])}
check("the board is refreshed even when nothing happens", len(board), 3)
check("and reflects who is actually enabled", board.get("cred-c.json"), False)

print()
print("=" * 74)
print("U. a draining member is replaced, and the replacement goes in first")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 95, 400000)]},   # 5% left
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},    # healthy
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 10, 400000)]},    # fresh standby
    "cred-d.json": {"disabled": True, "windows": [(WEEKF, 100, 400000)]},   # spent
})
log = saves()
check("two credentials were rewritten", len(log), 2)
# Enabling first is the whole safety property: a moment with an empty pool is a
# total outage, and no ordering makes disabling first safer.
check("the replacement is enabled first", log[0], ("cred-c.json", False))
check("the drained one is disabled after", log[1], ("cred-a.json", True))
check("the spent standby was not chosen",
      any(name == "cred-d.json" for name, _ in log), False)
skips = {c["file"]: c.get("skipped") for c in rep["candidates"]}
check("and the panel says why", skips["cred-d.json"], "exhausted")

print()
print("=" * 74)
print("V. it never empties the pool just because nothing qualifies")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 98, 400000)]},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 100, 400000)]},
    "cred-d.json": {"disabled": True, "windows": [(WEEKF, 97, 400000)]},
})
check("nothing was written", len(saves()), 0)
check("the incumbent stays enabled", rep["last_reason"], "pool_has_nothing_serving")
check("a standby at the floor is not a replacement",
      [c.get("skipped") for c in rep["candidates"] if c["file"] == "cred-d.json"][0],
      "below_floor")
# Nothing here can serve at all, so this one really is an alarm.
check("and it warns", any(w["code"] == "rotator_stuck" for w in rep.get("warnings", [])), True)
check("not downgraded to degraded",
      any(w["code"] == "rotator_degraded" for w in rep.get("warnings", [])), False)

print()
print("=" * 74)
print("W. dry run decides everything and writes nothing")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 95, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 10, 400000)]},
}, rotator={"dry_run": True})
check("no credential was touched", len(saves()), 0)
check("but the decision is recorded", len(rep["log"]) >= 2, True)
check("and marked as a rehearsal", rep["log"][0]["dry_run"], True)
check("the panel flags dry run",
      any(w["code"] == "rotator_dry_run" for w in rep.get("warnings", [])), True)

print()
print("=" * 74)
print("X. a credential upstream stopped accepting is retired, not promoted")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
# A 401 means the refresh token is gone and the account needs a new login. A 429
# is a working credential with no quota left. Confusing the two would either
# throw away a good account or keep a broken one in the pool.
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 5, 400000)]},
    "cred-b.json": {"disabled": False, "status": 401, "windows": []},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 12, 400000)]},
    "cred-e.json": {"disabled": True, "status": 401, "windows": []},
})
log = saves()
# The refused member goes first here, and that is deliberate: it has no capacity
# to lose, so waiting for a replacement only prolongs the wasted attempt it
# costs on every request. The enable-before-disable rule protects a *drained*
# member, which still has quota worth keeping until a replacement is in - that
# ordering is asserted in U.
check("the refused one is retired", ("cred-b.json", True) in log, True)
check("the replacement is promoted", ("cred-c.json", False) in log, True)
check("both happened, nothing else", len(log), 2)
check("a dead standby is never promoted",
      any(name == "cred-e.json" and not disabled for name, disabled in log), False)
# cred-e is refused upstream, and the panel says "never_observed" rather than
# "dead_token" - which is the honest answer. cred-c filled the gap, so nothing
# ever asked cred-e anything, and claiming to know it is dead would be claiming
# knowledge this design deliberately does not buy. It finds out if it ever needs
# it, and until then the account is not sent a single request.
skips = {c["file"]: c.get("skipped") for c in rep["candidates"]}
check("an untried standby is not claimed to be dead", skips["cred-e.json"], "never_observed")

print()
print("=" * 74)
print("Y. the circuit breaker holds, even when a human pressed the button")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
# One rotation spends two changes: a promotion and a retirement. With a budget
# of two, a second rotation in the same day must do nothing at all.
creds = {
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 95, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 10, 400000)]},
    "cred-d.json": {"disabled": True, "windows": [(WEEKF, 12, 400000)]},
}
rep = rotate(creds, rotator={"max_changes_per_day": 2})
first = len(saves())
check("the first rotation is allowed", first, 2)
# The plugin keeps its budget in state.json, so a second process picks up where
# the first left off - a proxy that bounces cannot spend the budget twice.
auth_list, fixtures, probe_dir, _ = rot_fixtures(creds)
probe_url, _, _, stop_probe = start_probe_server(probe_dir)
rep = run([], auth_list=auth_list, route="rotate", method="POST",
          rotator={"enabled": True, "keep_enabled": 2, "switch_at_percent": 10,
                   "check_interval_seconds": 3600, "min_switch_gap_minutes": 0,
                   "max_changes_per_day": 2, "probe_model": "gpt-5.6-sol",
                   "probe_url": probe_url},
          fixtures=fixtures)
stop_probe()
check("the budget survives a restart", rep["changes_today"], 2)
check("and the second rotation writes nothing", len(saves()), 0)

print()
print("=" * 74)
print("Y2. a refused credential is retired even when nothing can replace it")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
# The bug this covers: retirement used to run only after a successful
# promotion, and the "no standby qualifies" path returns before that. A
# credential upstream had refused therefore stayed in the pool for ever,
# costing a wasted attempt on every request. It contributes nothing, so
# switching it off cannot reduce capacity and must not wait for a replacement.
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},   # healthy
    "cred-b.json": {"disabled": False, "status": 401, "windows": []},      # refused
    "cred-d.json": {"disabled": True, "windows": [(WEEKF, 100, 400000)]},  # spent
})
log = saves()
check("the refused member is retired anyway", ("cred-b.json", True) in log, True)
check("and nothing else was touched", len(log), 1)
# One member is still serving, so this is degraded, not down - and saying so is
# the difference between an alarm worth reading and one worth ignoring.
check("reported as degraded, not down", rep["last_reason"], "no_standby_available")
check("warned as degraded", any(w["code"] == "rotator_degraded" for w in rep.get("warnings", [])), True)
check("not raised as stuck", any(w["code"] == "rotator_stuck" for w in rep.get("warnings", [])), False)
check("says how many are still serving",
      [w for w in rep["warnings"] if w["code"] == "rotator_degraded"][0]["serving"], 1)

shutil.rmtree(DATA_DIR, ignore_errors=True)
# A probe that merely failed to complete is not a refusal. Retiring on that
# would let one network blip cost real capacity.
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-b.json": {"disabled": False, "status": 500, "windows": []},
    "cred-d.json": {"disabled": True, "windows": [(WEEKF, 100, 400000)]},
})
check("a failed probe is not a refusal", len(saves()), 0)

print()
print("=" * 74)
print("Y3. an instance that has been superseded stands down")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
# A hot reload does not stop the instance it replaces: the host retires a plugin
# by moving it to a list, never calls Shutdown, and a Go c-shared library is not
# unloaded - so the old library keeps ticking with its own config and its own
# circuit breaker. Two rotators writing credentials is the one failure this
# component must not have, so the newest instance takes a lease.
os.makedirs(DATA_DIR, exist_ok=True)
with open(os.path.join(DATA_DIR, "rotator.lease"), "w") as fh:
    # Nanoseconds: a lease is stamped at nanosecond resolution so that two
    # instances starting inside the same second cannot compare equal, which a
    # newcomer reads as "someone else holds it" and stands down for good.
    json.dump({"instance": "someone-else", "version": "9.9.9",
               "at": int(time.time() * 1e9) + 3600 * 10 ** 9}, fh)
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 95, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 10, 400000)]},
})
check("a superseded instance writes nothing", len(saves()), 0)
check("and says why", rep["last_reason"], "superseded")

shutil.rmtree(DATA_DIR, ignore_errors=True)
os.makedirs(DATA_DIR, exist_ok=True)
# An older lease does not outrank a running instance, and a corrupt one must not
# wedge the rotator shut.
with open(os.path.join(DATA_DIR, "rotator.lease"), "w") as fh:
    json.dump({"instance": "an-older-one", "version": "0.0.1", "at": 1}, fh)
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 95, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 10, 400000)]},
})
check("an older lease is overruled", len(saves()) > 0, True)

shutil.rmtree(DATA_DIR, ignore_errors=True)
os.makedirs(DATA_DIR, exist_ok=True)
with open(os.path.join(DATA_DIR, "rotator.lease"), "w") as fh:
    fh.write("{not json at all")
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 95, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 10, 400000)]},
})
check("a corrupt lease fails open", len(saves()) > 0, True)

print()
print("=" * 74)
print("Z. the rules that decide which standby wins")
print("=" * 74)
shutil.rmtree(DATA_DIR, ignore_errors=True)
# A window that has already rolled over is full again whatever the stored
# percentage says, so a credential refused an hour ago can be the right answer
# now - which is exactly the credential that was switched off by hand today.
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 96, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "rolled.json": {"disabled": True, "windows": [(300, 100, -60), (WEEKF, 16, 500000)]},
}, rotator={"keep_enabled": 2})
log = saves()
check("a window past its reset counts as full", log[0], ("rolled.json", False))

shutil.rmtree(DATA_DIR, ignore_errors=True)
# Use it or lose it: with two candidates that both carry the horizon, the one
# whose allowance expires first is the one to spend.
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 96, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "soon.json": {"disabled": True, "windows": [(WEEKF, 40, 9 * 3600)]},
    "later.json": {"disabled": True, "windows": [(WEEKF, 40, 160 * 3600)]},
})
order = [c["file"] for c in rep["candidates"] if not c.get("skipped") and not c["enabled"]]
check("the sooner-expiring allowance ranks first", order[0], "soon.json")

shutil.rmtree(DATA_DIR, ignore_errors=True)
# An empty window minutes from its reset is not a reason to reject a candidate,
# and not a reason to replace a member either.
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(300, 100, 120), (WEEKF, 20, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 10, 400000)]},
})
check("a window about to reset is not an outage", len(saves()), 0)

print()
print("=" * 74)
print("AA. what probing actually costs")
print("=" * 74)
# The design claim is that upstream is asked almost nothing. These assertions
# measure it at the socket: PROBES is one entry per request the probe server
# actually received. The previous design sent roughly 1,100 requests in eight
# hours, about 123 per credential, by re-asking questions it already had
# answers to.
shutil.rmtree(DATA_DIR, ignore_errors=True)
rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 5, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 0, 400000)]},
})
check("a healthy pool asks upstream nothing at all", len(PROBES), 0)

shutil.rmtree(DATA_DIR, ignore_errors=True)
# An idle standby cannot have spent quota since it was last seen, and when its
# window closes the allowance is back. Both are arithmetic, so a standby whose
# last reading was "spent" is picked without anyone being asked anything.
rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 96, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    # Last seen fully spent, but that window closed a minute ago.
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 100, -60)]},
}, rotator={"confirm_before_switch": False, "resync_after_reset": False})
log = saves()
check("a rolled-over standby is promoted", ("cred-c.json", False) in log, True)
# With both checks switched off the decision runs on arithmetic alone, which is
# the claim being tested here. Left on, the rollover is verified once against
# upstream - that is AI's subject, not this one's.
check("and the decision itself asked upstream nothing", len(PROBES), 0)

shutil.rmtree(DATA_DIR, ignore_errors=True)
# A whole rotation costs two probes at the very most: one to check the estimate
# that condemned the incumbent, one to check the replacement before it takes
# traffic. Never more, and never per-tick.
rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 96, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 10, 400000)]},
})
check("a whole rotation costs at most two probes", len(PROBES) <= 2, True)
check("and never asks one credential twice",
      len(PROBES) == len({h["token"] for h in PROBES}), True)

shutil.rmtree(DATA_DIR, ignore_errors=True)
# The budget is per quota cycle and lives in state.json, so a second decision in
# the same cycle - or after a restart - asks nothing further.
creds = {
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 96, 400000)]},
    "cred-b.json": {"disabled": False, "status": 401, "windows": []},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 10, 400000)]},
}
rotate(creds)
first = len(PROBES)
check("the first decision spends probes", first > 0, True)
rotate(creds, steps=[])
check("a repeat decision in the same cycle spends none", len(PROBES), 0)

shutil.rmtree(DATA_DIR, ignore_errors=True)
# The rule that matters most: a credential upstream has refused is never asked
# again. Only a fresh login can change the answer, and re-asking every couple of
# minutes for hours is the least defensible traffic this plugin can produce.
creds = {
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 96, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-c.json": {"disabled": True, "status": 401, "windows": []},
    "cred-d.json": {"disabled": True, "status": 401, "windows": []},
}
rotate(creds)
refused = {"tok-cred-c.json", "tok-cred-d.json"}
check("a refused credential is asked once",
      refused.issubset({h["token"] for h in PROBES}), True)
rotate(creds)
check("and never asked again", [h["token"] for h in PROBES if h["token"] in refused], [])

print()
print("=" * 74)
print("AB. a probe leaves by the credential's own egress")
print("=" * 74)
# The defect this replaces: host.http.do builds its client with a nil auth, so
# probes ignored proxy_url and left by the host's address - the account was seen
# authenticating from somewhere its real traffic never appears.
shutil.rmtree(DATA_DIR, ignore_errors=True)
socks_count = [0]
socks_url, stop_socks = start_socks5(socks_count)
rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 96, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 10, 400000)], "proxy": socks_url},
})
stop_socks()
check("the probe went through the credential's proxy", socks_count[0] > 0, True)
check("and still arrived", ("cred-c.json", False) in saves(), True)

shutil.rmtree(DATA_DIR, ignore_errors=True)
# The same, with a proxy that demands a password: the dialer speaks the
# username/password sub-negotiation, not just the no-auth path.
auth_count = [0]
auth_url, stop_auth = start_socks5(auth_count, require_auth=("u", "p"))
scheme, hostport = auth_url.split("://")
rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 96, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 10, 400000)],
                    "proxy": "%s://u:p@%s" % (scheme, hostport)},
})
stop_auth()
check("an authenticated proxy is handled too", auth_count[0] > 0, True)
check("and the credential was promoted", ("cred-c.json", False) in saves(), True)

shutil.rmtree(DATA_DIR, ignore_errors=True)
# A proxy that cannot be reached must fail the probe, never fall back to a
# direct dial: falling back is exactly the leak this design removes.
rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 96, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 10, 400000)],
                    "proxy": "socks5://127.0.0.1:1"},
})
check("an unreachable proxy does not leak a direct probe",
      [h for h in PROBES if h["token"] == "tok-cred-c.json"], [])
check("and a credential it could not check is not promoted",
      ("cred-c.json", False) in saves(), False)

print()
print("=" * 74)
print("AC. a retirement actually lands on disk")
print("=" * 74)
# The defect this covers was measured in production: the rotator logged a
# retirement, the file was written, and the host reverted it inside the same
# second. host.auth.save rebuilds the host's own record from the document,
# never reads `disabled`, and persists that record back. So when the host says
# where the credential lives, the plugin writes there and lets the file watcher
# - whose loader does read the field - apply it.
shutil.rmtree(DATA_DIR, ignore_errors=True)
rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 96, 400000)], "with_path": True},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)], "with_path": True},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 10, 400000)], "with_path": True},
})
live = os.path.join(ROT_DIR, "live")
on_disk = {n: json.load(open(os.path.join(live, n)))["disabled"]
           for n in sorted(os.listdir(live))}
check("the drained member is disabled on disk", on_disk["cred-a.json"], True)
check("the replacement is enabled on disk", on_disk["cred-c.json"], False)
check("the healthy member is left alone", on_disk["cred-b.json"], False)
# And nothing went through the callback that cannot express a disable.
check("the failing callback was not used", len(saves()), 0)

shutil.rmtree(DATA_DIR, ignore_errors=True)
# Every other field survives byte for byte: these documents hold refresh tokens
# and a lossy round trip would quietly drop whatever the plugin does not model.
rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 96, 400000)], "with_path": True},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)], "with_path": True},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 10, 400000)],
                    "with_path": True, "proxy": "socks5://127.0.0.1:9"},
})
kept = json.load(open(os.path.join(live, "cred-a.json")))
check("the refresh token survived", kept.get("refresh_token"), "refresh-cred-a.json")
check("the access token survived", kept.get("access_token"), "tok-cred-a.json")
check("the account id survived", kept.get("account_id"), "acct-cred-a.json")

print()
print("=" * 74)
print("AD. every route the panel uses is actually registered")
print("=" * 74)
# The handler for /rotate existed from 2.3.0 and the route was never declared,
# so the host answered 404 before the plugin was consulted. The suite missed it
# because it drives management.handle directly, which skips registration
# entirely - so the registration is now asserted on its own terms.
script([], os.path.join(BUILD, "reg.txt"), tail=False)
with open(os.path.join(BUILD, "reg.txt"), "a") as fh:
    fh.write("management.register\t{}\n")
raw = subprocess.run([HARNESS, SO, os.path.join(BUILD, "reg.txt")], capture_output=True,
                     text=True, env=dict(os.environ, HARNESS_AUTH_LIST="[]")).stdout
declared = set()
for blk in raw.split("--- "):
    if blk.startswith("management.register"):
        body = json.loads(blk.split("\n", 1)[1].strip())
        for route in (body.get("result") or {}).get("routes", []):
            declared.add((route["Method"], route["Path"]))
check("the data route is declared", ("GET", "/codex-weekly-usd/data") in declared, True)
check("the prices route is declared", ("GET", "/codex-weekly-usd/prices") in declared, True)
check("the rotate route is declared", ("POST", "/codex-weekly-usd/rotate") in declared, True)
check("the refresh route is declared", ("POST", "/codex-weekly-usd/refresh") in declared, True)
check("and rotate is POST-only",
      ("GET", "/codex-weekly-usd/rotate") in declared, False)

print()
print("=" * 74)
print("AE. a 401 from live traffic is remembered, and costs no probe")
print("=" * 74)
# The gap this closes: a failure carrying no quota signal was treated as an
# ordinary error and dropped, so `last_fail` was never set. But 401 is exactly
# the failure no quota reading can describe - a refused request carries no
# quota headers at all - so the one error that means "this credential is dead"
# was the one deliberately forgotten. A credential refused at 07:19 was still
# being reported at its last healthy percentage a day later.
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = run([
    ("usage.handle", weekly(20, auth="cred-a.json")),
    ("usage.handle", weekly(0, auth="cred-a.json", failed=True, status=401, body=REVOKED_BODY)),
], auth_list=[authfile("cred-a.json")])
row = cred(health(rep, "gpt-5.6-sol"))
check("the credential reads as rejected", row["state"], "rejected")
check("and upstream's own word for it is kept", row.get("reason"), "token_revoked")
check("no cooldown is invented for it", "cooldown_in_seconds" in row, False)

shutil.rmtree(DATA_DIR, ignore_errors=True)
# 403 counts too, and a body that is not the shape we expect still yields
# something better than silence.
rep = run([
    ("usage.handle", weekly(20, auth="cred-a.json")),
    ("usage.handle", weekly(0, auth="cred-a.json", failed=True, status=403, body="nope")),
], auth_list=[authfile("cred-a.json")])
check("403 is a rejection too", cred(health(rep, "gpt-5.6-sol"))["state"], "rejected")
check("an unparseable body falls back to the status",
      cred(health(rep, "gpt-5.6-sol")).get("reason"), "http_403")

shutil.rmtree(DATA_DIR, ignore_errors=True)
# A success afterwards means someone logged the account back in.
rep = run([
    ("usage.handle", weekly(20, auth="cred-a.json")),
    ("usage.handle", weekly(0, auth="cred-a.json", failed=True, status=401, body=REVOKED_BODY)),
    ("usage.handle", weekly(22, auth="cred-a.json")),
], auth_list=[authfile("cred-a.json")])
check("a later success clears the rejection",
      cred(health(rep, "gpt-5.6-sol"))["state"], "ok")

shutil.rmtree(DATA_DIR, ignore_errors=True)
# An ordinary failure is still not a rejection: a 500 or a dropped connection
# says nothing about whether the credential is accepted.
rep = run([
    ("usage.handle", weekly(20, auth="cred-a.json")),
    ("usage.handle", weekly(0, auth="cred-a.json", failed=True, status=500, body="boom")),
], auth_list=[authfile("cred-a.json")])
check("a 500 is not a rejection", cred(health(rep, "gpt-5.6-sol"))["state"], "ok")

shutil.rmtree(DATA_DIR, ignore_errors=True)
# And the rotator acts on it without asking upstream anything: live traffic
# already answered the question a probe would have asked. The pool is healthy
# here, which is the point - a refusal is worth re-testing only when the
# capacity it removed is actually needed (AN), never on a tick with nothing to
# decide.
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 9, 400000)]},
    # Plenty of headroom on paper, and refused in practice.
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 9, 400000)], "fail_401": True},
})
skips = {c["file"]: c.get("skipped") for c in rep["candidates"]}
check("a credential traffic saw refused is written off", skips["cred-c.json"], "dead_token")
check("and no probe was spent finding that out", len(PROBES), 0)

print()
print("=" * 74)
print("AF. a deleted credential stops being listed")
print("=" * 74)
# Accounting for a credential whose auth file is gone is real history, but
# showing it beside live credentials invites reading a deleted account as a
# working one - and there is nothing left to act on.
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = run([
    ("usage.handle", weekly(20, auth="cred-a.json")),
    ("usage.handle", weekly(30, auth="cred-gone.json")),
], auth_list=[authfile("cred-a.json")])
listed = [a["auth_id"] for a in rep["accounts"]]
check("the live credential is listed", "cred-a.json" in listed, True)
check("the deleted one is not", "cred-gone.json" in listed, False)
check("but it is accounted for", [r["auth_id"] for r in rep.get("removed", [])],
      ["cred-gone.json"])
check("and does not inflate the credential count", rep["totals"]["credentials"], 1)

shutil.rmtree(DATA_DIR, ignore_errors=True)
# The guard that matters: an auth list that came back empty is a failure to
# ask, not evidence that every credential was deleted. Hiding everything then
# would turn one bad reply into a blank panel.
rep = run([
    ("usage.handle", weekly(20, auth="cred-a.json")),
    ("usage.handle", weekly(30, auth="cred-gone.json")),
], auth_list=[])
check("an empty auth list hides nothing", len(rep["accounts"]), 2)
check("and reports nothing as removed", len(rep.get("removed", [])), 0)

print()
print("=" * 74)
print("AG. the burn projection cannot retire a credential that still has quota")
print("=" * 74)
# Measured on the live fleet: 270 percentage points an hour. At that rate a
# fifteen-minute lead time means "replace anything under 67% headroom" - the
# floor never applies, and every credential is retired with a third of its
# window spent and the rest parked until it resets.
shutil.rmtree(DATA_DIR, ignore_errors=True)
# The percentage has to move against priced traffic, or no calibration sample
# is recorded and the burn rate stays zero - which would make this test pass
# for the wrong reason.
FAST = [("usage.handle", usage([(FIVEH, p, ROT_NOW + 9000)], auth="cred-a.json",
                               index="idx-cred-a.json"))
        for p in (0, 15, 30, 45)]
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(FIVEH, 45, 9000)], "seed": False},
    "cred-b.json": {"disabled": False, "windows": [(FIVEH, 8, 9000)]},
    "cred-c.json": {"disabled": True, "windows": [(FIVEH, 5, 9000)]},
}, rotator={"lead_time_minutes": 15, "switch_at_percent": 10}, steps=FAST)
check("a credential at 55% headroom is left alone", len(saves()), 0)
check("however fast it is burning", rep["last_reason"], "pool_healthy")

shutil.rmtree(DATA_DIR, ignore_errors=True)
# Near the floor the projection still does its job: it brings the replacement
# forward by a check rather than waiting for the credential to hit zero.
SPENT = [("usage.handle", usage([(FIVEH, p, ROT_NOW + 9000)], auth="cred-a.json",
                                index="idx-cred-a.json"))
         for p in (70, 78, 84, 88)]
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(FIVEH, 88, 9000)], "seed": False},
    "cred-b.json": {"disabled": False, "windows": [(FIVEH, 8, 9000)]},
    "cred-c.json": {"disabled": True, "windows": [(FIVEH, 5, 9000)]},
}, rotator={"lead_time_minutes": 15, "switch_at_percent": 10}, steps=SPENT)
log = saves()
check("but 12% headroom and burning is replaced", ("cred-c.json", False) in log, True)
check("and the spent one steps down", ("cred-a.json", True) in log, True)

print()
print("=" * 74)
print("AH. the panel describes now, not the last decision")
print("=" * 74)
# A boundary that has gone past used to clamp the countdown to zero and leave
# it there for as long as the credential stayed idle - so the panel showed a
# deadline that expired hours ago beside a percentage that stopped being true
# at the same moment.
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = run([("usage.handle", usage([(FIVEH, 80, int(time.time()) - 600)],
                                  auth="cred-a.json"))],
          auth_list=[authfile("cred-a.json")])
w = win(rep, FIVEH)
check("the countdown is rolled forward, not clamped", w["reset_in_seconds"] > 0, True)
check("and lands inside one period",
      w["reset_in_seconds"] <= FIVEH * 60, True)
check("the rollover is marked as inferred", w.get("reset_inferred"), True)
check("and the percentage is flagged as stale", w.get("used_percent_stale"), True)
check("without inventing a new percentage", w["used_percent"], 80.0)

shutil.rmtree(DATA_DIR, ignore_errors=True)
# A boundary still ahead is left exactly alone.
rep = run([("usage.handle", usage([(FIVEH, 80, int(time.time()) + 3600)],
                                  auth="cred-a.json"))],
          auth_list=[authfile("cred-a.json")])
w = win(rep, FIVEH)
check("a live window is not marked inferred", w.get("reset_inferred"), None)
check("nor its reading stale", w.get("used_percent_stale"), None)

shutil.rmtree(DATA_DIR, ignore_errors=True)
# The rotator board is derived per request. Reading it twice with a credential
# switched off in between must show the change without any tick having run.
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 5, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)]},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 0, 400000)]},
})
board = {c["file"]: c["enabled"] for c in rep["candidates"]}
check("the board reports who is enabled right now", board["cred-c.json"], False)
check("and covers the whole pool", len(board), 3)

print()
print("=" * 74)
print("AI. a window is re-read once after the clock says it reset")
print("=" * 74)
# Inferring the rollover is right in principle and worth checking against
# upstream - once per boundary, which for a five-hour window is under five
# requests a day. Doing it more often would be the old design again.
shutil.rmtree(DATA_DIR, ignore_errors=True)
rotate({
    # Its reading describes a cycle that closed ten minutes ago.
    "cred-a.json": {"disabled": False, "windows": [(FIVEH, 90, -600)]},
    "cred-b.json": {"disabled": False, "windows": [(FIVEH, 8, 9000)]},
    "cred-c.json": {"disabled": True, "windows": [(FIVEH, 5, 9000)]},
})
asked = [h["token"] for h in PROBES]
check("the stale window is re-read", "tok-cred-a.json" in asked, True)
check("and nothing else is disturbed", sorted(set(asked)), ["tok-cred-a.json"])

shutil.rmtree(DATA_DIR, ignore_errors=True)
# Turning it off means the inference stands on its own, and no request is made.
rotate({
    "cred-a.json": {"disabled": False, "windows": [(FIVEH, 90, -600)]},
    "cred-b.json": {"disabled": False, "windows": [(FIVEH, 8, 9000)]},
    "cred-c.json": {"disabled": True, "windows": [(FIVEH, 5, 9000)]},
}, rotator={"resync_after_reset": False, "confirm_before_switch": False})
check("with resync off nothing is asked", len(PROBES), 0)

print()
print("=" * 74)
print("AJ. a re-read updates the plugin's own data, not just the rotator's view")
print("=" * 74)
# A probe that only reaches the rotator leaves the panel showing whatever
# percentage live traffic last saw. The window stays marked inferred for good,
# because nothing ever confirms it - which is the one thing the re-read was
# for. The reading is the same measurement a served request would produce, so
# it is folded into the accounting state the same way.
shutil.rmtree(DATA_DIR, ignore_errors=True)
rotate({
    # Traffic last saw a cycle that closed ten minutes ago at 90% spent.
    # Upstream now reports the new cycle: 3% spent, resetting in five hours.
    "cred-a.json": {"disabled": False, "windows": [(FIVEH, 90, -600)],
                    "probe_windows": [(FIVEH, 3, 18000)]},
    "cred-b.json": {"disabled": False, "windows": [(FIVEH, 8, 9000)]},
    "cred-c.json": {"disabled": True, "windows": [(FIVEH, 5, 9000)]},
})
check("the stale window was re-read",
      "tok-cred-a.json" in [h["token"] for h in PROBES], True)
# Read the panel back out of the state the run just wrote.
rep = run([], auth_list=[authfile("cred-a.json")])
w = win(rep, FIVEH)
check("the accounting state took the new reading", w["used_percent"], 3.0)
check("the window is no longer inferred", w.get("reset_inferred"), None)
check("nor its reading stale", w.get("used_percent_stale"), None)
check("and the countdown is real", w["reset_in_seconds"] > 3600, True)

print()
print("=" * 74)
print("AK. the fleet series carries a trailing total, not a running one")
print("=" * 74)
# Spend accumulated since recording began only ever rises, so the line said
# nothing beyond "time has passed". A trailing seven-day total is stationary:
# flat under steady load, rising only when load actually grows.
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = run([("usage.handle", weekly(i)) for i in range(4)])
pts = rep["fleet_series"]
check("the series is emitted", len(pts) >= 1, True)
check("every point carries a trailing total",
      all("rolling_usd" in p for p in pts), True)
# All four requests land in the current hour, so the trailing total for that
# hour is simply what was spent in it.
check("which sums the window ending at that hour", pts[-1]["rolling_usd"], COST * 4)
# One hour of history cannot describe seven days, and saying so is the point:
# the panel draws that stretch as provisional rather than as a real climb.
check("and is flagged while the window is not covered",
      pts[-1].get("rolling_partial"), True)

print()
print("=" * 74)
print("AL. the hourly series is bounded by age, not by how many buckets exist")
print("=" * 74)
# Retention was guarded by the size of the map, which bounded nothing: most
# hours carry no traffic, so the map stayed under the limit while the series
# stretched far past the ten days it claimed. Measured in production: an
# eighteen-day chart.
shutil.rmtree(DATA_DIR, ignore_errors=True)
os.makedirs(DATA_DIR, exist_ok=True)
_hour = int(time.time()) // 3600
json.dump({
    "version": 3,
    "accounts": {
        "codex-demo-team.json": {
            "auth_id": "codex-demo-team.json", "provider": "codex",
            "hours": {
                str(_hour - 400): {"r": 1, "u": 1.0},   # 16 days old
                str(_hour - 250): {"r": 1, "u": 1.0},   # just past retention
                str(_hour - 100): {"r": 1, "u": 1.0},   # inside it
            },
        }
    },
}, open(os.path.join(DATA_DIR, "state.json"), "w"))

rep = run([("usage.handle", weekly(5))])
ages = sorted(p["ago"] for p in rep["fleet_series"])
# The chart covers a week; a bucket from sixteen days ago is gone from the
# state entirely, and one from ten days ago is retained but not charted -
# it is there to be summed into the trailing totals of the points that are.
check("the chart covers a week, not everything ever seen", max(ages) < 168, True)
check("the bucket inside the week is charted", 100 in ages, True)
check("the one outside it is not", 250 in ages, False)
check("and the new hour is there", 0 in ages, True)

shutil.rmtree(DATA_DIR, ignore_errors=True)
os.makedirs(DATA_DIR, exist_ok=True)
# Loading is enough on its own: a file written by a build that bounded the
# series by map size must not keep its overlong history until the clock happens
# to tick into a new hour.
json.dump({
    "version": 3,
    "accounts": {
        "codex-demo-team.json": {
            "auth_id": "codex-demo-team.json", "provider": "codex",
            "hours": {str(_hour - 400): {"r": 1, "u": 1.0},
                      str(_hour - 10): {"r": 1, "u": 1.0}},
        }
    },
}, open(os.path.join(DATA_DIR, "state.json"), "w"))
rep = run([], auth_list=[authfile("codex-demo-team.json")])
ages = sorted(p["ago"] for p in rep.get("fleet_series") or [])
check("loading alone drops what has aged out", ages, [10])

print()
print("=" * 74)
print("AM. the operator can force a re-read of every credential")
print("=" * 74)
# Every automatic path waits for a reason: a window past its reset, a short
# pool, a replacement about to take traffic. A quota reset granted out of band
# satisfies none of them - it moves no clock and empties no pool - so nothing
# would ever notice, and the panel would keep reporting figures that stopped
# being true the moment it happened.
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(FIVEH, 90, 9000)],
                    "probe_windows": [(FIVEH, 2, 18000)]},
    "cred-b.json": {"disabled": False, "windows": [(FIVEH, 80, 9000)],
                    "probe_windows": [(FIVEH, 1, 18000)]},
    "cred-c.json": {"disabled": True, "windows": [(FIVEH, 70, 9000)],
                    "probe_windows": [(FIVEH, 3, 18000)]},
}, route="refresh")
check("every credential is re-read", rep["read"], 3)
check("and none skipped", rep["skipped"], 0)
seen = {c["file"]: c.get("5h_used_percent") for c in rep["credentials"]}
check("the fresh figure is reported back", seen["cred-a.json"], 2.0)
check("for the disabled one too", seen["cred-c.json"], 3.0)

# And it reaches the accounting state, not just the rotator's view.
rep = run([], auth_list=[authfile("cred-a.json")])
check("the panel takes the new reading", win(rep, FIVEH)["used_percent"], 2.0)

shutil.rmtree(DATA_DIR, ignore_errors=True)
# The per-cycle budget does not apply - the operator is asking a new question,
# not the rotator repeating an old one - but the rule about refused
# credentials does, because only a fresh login can change that answer.
creds = {
    "cred-a.json": {"disabled": False, "windows": [(FIVEH, 90, 9000)],
                    "probe_windows": [(FIVEH, 2, 18000)]},
    "cred-b.json": {"disabled": False, "status": 401, "windows": []},
}
rotate(creds, route="refresh")
rep = rotate(creds, route="refresh")
check("a second refresh still re-reads", rep["read"] >= 1, True)
skips = {c["file"]: c.get("skipped") for c in rep["credentials"]}
check("but a refused credential is left alone", skips["cred-b.json"], "dead_token")

print()
print("==========================================================================")
print("AN. a re-authorised credential comes back; a still-dead one is asked once")
print("==========================================================================")
# The deadlock this closes, seen in production: live traffic gets a 401, the
# refusal is recorded against the account, the credential is disabled on the
# strength of it - and from then on nothing can clear the refusal, because the
# only event that clears it is a successful request and a disabled credential
# never gets one. The operator re-authorises, the token works, and the panel
# goes on reporting a dead credential indefinitely.
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = rotate({
    # Refused by live traffic and already retired, but the probe now answers
    # 200: this is what a re-authorised credential looks like from here.
    "cred-a.json": {"disabled": True, "fail_401": True,
                    "windows": [(WEEKF, 4, 400000)]},
    # Refused and still refused. It must cost exactly one probe to establish.
    "cred-b.json": {"disabled": True, "fail_401": True, "status": 401,
                    "windows": [(WEEKF, 4, 400000)]},
    "cred-c.json": {"disabled": False, "windows": [(WEEKF, 99, 400000)]},
}, rotator={"keep_enabled": 2})
board = {c["file"]: c for c in rep["candidates"]}
check("the re-authorised credential is no longer written off",
      board["cred-a.json"].get("skipped"), None)
check("and is promoted back into the pool",
      ("cred-a.json", False) in saves(), True)
check("the genuinely dead one stays written off",
      board["cred-b.json"].get("skipped"), "dead_token")
check("having cost one probe to find out",
      len([h for h in PROBES if h["token"] == "tok-cred-b.json"]), 1)

# Now the second half: having pinned the dead token's fingerprint, the rotator
# must stop asking. A refusal that is re-probed every tick is the traffic this
# whole design exists to remove.
before = len(PROBES)
rep = rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 4, 400000)]},
    "cred-b.json": {"disabled": True, "fail_401": True, "status": 401,
                    "windows": [(WEEKF, 4, 400000)]},
    "cred-c.json": {"disabled": False, "windows": [(WEEKF, 99, 400000)]},
}, rotator={"keep_enabled": 2})
skips = {c["file"]: c.get("skipped") for c in rep["candidates"]}
check("the dead token is still refused", skips["cred-b.json"], "dead_token")
check("and is not asked a second time",
      len([h for h in PROBES if h["token"] == "tok-cred-b.json"]), 0)

print()
print("==========================================================================")
print("AO. re-authorising revives a credential without any probe at all")
print("==========================================================================")
# AN covers the pool being short enough to spend a probe. This is the case that
# actually bit: the pool is fine, so nothing is ever short, so no probe is ever
# spent - and the refusal has to expire on its own or the panel reports a
# working credential as dead forever. It can, because a refusal is about one
# token, and re-authorising replaces it.
shutil.rmtree(DATA_DIR, ignore_errors=True)
rotate({
    # Refused in traffic and refused again when asked: genuinely dead, which is
    # what gets a credential retired and its token fingerprint written down.
    "cred-a.json": {"disabled": False, "fail_401": True, "status": 401,
                    "windows": [(WEEKF, 4, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 5, 400000)]},
    "cred-c.json": {"disabled": True, "windows": [(WEEKF, 6, 400000)]},
}, rotator={"keep_enabled": 2})
check("the refused credential is retired", ("cred-a.json", True) in saves(), True)

# Same state, no traffic replayed for cred-a - it is disabled, so in production
# it would get none - and a new token in the file. The pool is healthy, so
# nothing here has any reason to probe.
rep = rotate({
    "cred-a.json": {"disabled": True, "seed": False, "token": "tok-cred-a-relogin",
                    "windows": [(WEEKF, 4, 400000)]},
    "cred-b.json": {"disabled": False, "windows": [(WEEKF, 5, 400000)]},
    "cred-c.json": {"disabled": False, "windows": [(WEEKF, 6, 400000)]},
}, rotator={"keep_enabled": 2})
check("the pool had no reason to act", rep["last_reason"], "pool_healthy")
check("so nothing was asked upstream", len(PROBES), 0)
skips = {c["file"]: c.get("skipped") for c in rep["candidates"]}
# Whatever else the board says about it - it was just rotated, so it is held
# back for a cooling-off period - it is no longer written off as dead.
check("and the refusal expired with the token it described",
      skips["cred-a.json"] == "dead_token", False)

# The panel has to agree. Reporting a re-authorised credential as expired is
# the symptom the whole fix exists for.
rep = run([], auth_list=[authfile("cred-a.json"), authfile("cred-b.json"),
                         authfile("cred-c.json")])
row = health(rep, "gpt-5.6-sol")
states = {c["credential"]: c["state"] for c in (row or {}).get("by_credential", [])}
check("the panel no longer calls it expired", states.get("cred-a.json") == "rejected", False)

print()
print("==========================================================================")
print("AP. a quota that changes between cycles is followed, not averaged")
print("==========================================================================")
# The assumption this replaces was written down in startCycle: samples survive
# a rollover because "they measure the size of the quota, which a rollover does
# not change". Measured on the live fleet over nine days, the dollars behind
# one percentage point fell by about a third on every account independently,
# and pooling held the estimate 20-47% above what the current cycle was itself
# measuring. Evidence has to be told apart by which quota it priced.
shutil.rmtree(DATA_DIR, ignore_errors=True)
# First cycle: $2.00 buys one percentage point, so the quota is $200.
steps = [("usage.handle", weekly(i)) for i in range(8)]
# Second cycle: the same $2.00 now buys two points. The quota has halved.
steps += [("usage.handle", usage([(WEEK, 2 * i, RESET_W + 7 * 86400)]))
          for i in range(6)]
rep = run(steps)
est = win(rep, WEEK)["estimate"]
# Pooling all twelve samples would answer $141 - an average of two quotas, one
# of which no longer exists, and a number that was never true of either cycle.
check("calibration follows the new quota",
      round(est["quota_usd_by_delta"], 2), 100.00)
check("and rests only on this cycle's evidence", est["samples"], 5)
check("the two methods now agree", round(est["quota_usd_by_window"], 2), 100.00)

# The reverse case matters just as much: the estimate must not lag a quota that
# grew, or the rotator retires credentials that still have room.
shutil.rmtree(DATA_DIR, ignore_errors=True)
steps = [("usage.handle", usage([(WEEK, 2 * i, RESET_W)])) for i in range(6)]
steps += [("usage.handle", usage([(WEEK, i, RESET_W + 7 * 86400)])) for i in range(8)]
rep = run(steps)
est = win(rep, WEEK)["estimate"]
check("a quota that grew is followed too",
      round(est["quota_usd_by_delta"], 2), 200.00)

print()
print("==========================================================================")
print("AQ. one quota pool, priced separately in each model")
print("==========================================================================")
# A window is one pool of quota, but a dollar does not buy the same share of it
# in every model: measured on the live fleet, a dollar of gpt-6-astra consumes
# about 1.4x the quota a dollar of gpt-5.6-sol does. A single blended figure
# therefore describes the mix that produced it and nothing else, and goes stale
# the moment traffic moves - which is what actually happened, and what looked
# for a while like the quota itself shrinking by a third.
shutil.rmtree(DATA_DIR, ignore_errors=True)
# sol: $2.00 buys one point, so sol prices this window at $200.
steps = [("usage.handle", weekly(i, auth="cred-a.json")) for i in range(6)]
# astra: 2.5x the price per request, and five points each. The same window is
# worth $100 of astra - it burns twice as fast per dollar.
steps += [("usage.handle", weekly(5 + 5 * i, model="gpt-6-astra", auth="cred-a.json"))
          for i in range(6)]
# A second credential that has only ever served sol. It must still be able to
# say what its window is worth in astra, because it is exactly the credential
# the rotator is about to hand astra traffic to.
steps += [("usage.handle", weekly(i, auth="cred-b.json")) for i in range(6)]
rep = run(steps, auth_list=[authfile("cred-a.json"), authfile("cred-b.json")])

a = win(rep, WEEK, account="cred-a.json")["estimate"]
bym = a.get("quota_usd_by_model") or {}
check("the window is priced in sol", round(bym.get("gpt-5.6-sol", 0), 2), 200.00)
check("and in astra, separately", round(bym.get("gpt-6-astra", 0), 2), 100.00)
check("both from this window's own evidence",
      (a.get("quota_model_source") or {}).get("gpt-6-astra"), "measured")
check("the model taking the traffic is named", a.get("dominant_model"), "gpt-6-astra")
# Remaining follows the same split: 30% of the pool is gone whichever model
# spent it, so what is left is 70% of each model's own figure.
rem = a.get("remaining_usd_by_model") or {}
check("remaining is priced per model too", round(rem.get("gpt-6-astra", 0), 2), 70.00)
check("and differs from the sol figure", round(rem.get("gpt-5.6-sol", 0), 2), 140.00)

b = win(rep, WEEK, account="cred-b.json")["estimate"]
bym = b.get("quota_usd_by_model") or {}
check("a credential that never served astra still prices it",
      round(bym.get("gpt-6-astra", 0), 2), 100.00)
check("and says the figure was carried, not measured",
      (b.get("quota_model_source") or {}).get("gpt-6-astra"), "carried from gpt-5.6-sol")
check("while its own model is measured",
      (b.get("quota_model_source") or {}).get("gpt-5.6-sol"), "measured")

print()
print("==========================================================================")
print("AR. the model ratio is measured inside a window, so pool size cancels")
print("==========================================================================")
# The ratio has to come from one window watching both models. Across windows it
# would be meaningless: two accounts have different quotas and a weekly window
# is not a five-hour one, so only a ratio taken against the same pool says
# anything about the models.
shutil.rmtree(DATA_DIR, ignore_errors=True)
# cred-a's pool is half the size of cred-b's, and only cred-b ever mixes models.
steps = [("usage.handle", weekly(2 * i, auth="cred-a.json")) for i in range(6)]
steps += [("usage.handle", weekly(i, auth="cred-b.json")) for i in range(6)]
steps += [("usage.handle", weekly(5 + 5 * i, model="gpt-6-astra", auth="cred-b.json"))
          for i in range(6)]
rep = run(steps, auth_list=[authfile("cred-a.json"), authfile("cred-b.json")])
a = win(rep, WEEK, account="cred-a.json")["estimate"]
bym = a.get("quota_usd_by_model") or {}
check("the small pool is priced in sol", round(bym.get("gpt-5.6-sol", 0), 2), 100.00)
# Half the pool, same 2x model ratio: $50, not the $100 that carrying cred-b's
# absolute figure across would have given.
check("and carries the ratio, not the other account's dollars",
      round(bym.get("gpt-6-astra", 0), 2), 50.00)

print()
print("==========================================================================")
print("AS. every live credential is capacity for every model")
print("==========================================================================")
# The ledger only has an entry for a model once a credential has served it, and
# the availability board used to be built from it - so an account that had only
# ever run astra was not counted as sol capacity, and the panel warned "only one
# credential can serve sol" while two enabled accounts could. Any Codex account
# serves any model. History says what a credential has done, not what it can do.
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = run([
    ("usage.handle", usage([(WEEK, 5, LATER)], model="gpt-5.6-sol", auth="cred-a.json")),
    ("usage.handle", usage([(WEEK, 6, LATER)], model="gpt-6-astra", auth="cred-a.json")),
    ("usage.handle", usage([(WEEK, 3, LATER)], model="gpt-6-astra", auth="cred-b.json")),
] + [("usage.handle", usage([(WEEK, 5 + i, LATER)], model="gpt-5.6-sol", auth="cred-a.json"))
     for i in range(25)],
    auth_list=[authfile("cred-a.json"), authfile("cred-b.json"),
               # Never served anything at all: no accounting, still an account.
               authfile("cred-new.json"),
               authfile("cred-off.json", disabled=True)])
sol = health(rep, "gpt-5.6-sol")
check("sol counts every enabled credential", sol["credentials"], 3)
check("and all of them are available", sol["available"], 3)
check("disabled ones are listed, not counted", sol["disabled"], 1)
check("so it is not a single point of failure", sol["single_point"], False)
check("and nothing warns that it is", len(codes(rep, "model_single_point")), 0)
rows = {c["credential"]: c for c in sol["by_credential"]}
check("a credential that never ran sol is marked untested",
      rows["cred-b.json"].get("untested"), True)
check("one that did is not", rows["cred-a.json"].get("untested", False), False)
check("a credential with no accounting at all is still listed",
      rows.get("cred-new.json", {}).get("state"), "ok")

# A refusal of the credential is about its token, and every model uses the same
# token - so it does cross models, unlike a quota lockout (M).
shutil.rmtree(DATA_DIR, ignore_errors=True)
rep = run([
    ("usage.handle", usage([(WEEK, 5, LATER)], model="gpt-5.6-sol", auth="cred-a.json")),
    ("usage.handle", usage([(WEEK, 3, LATER)], model="gpt-6-astra", auth="cred-b.json")),
    ("usage.handle", usage([], model="gpt-6-astra", auth="cred-b.json",
                           failed=True, status=401, body=REVOKED_BODY)),
], auth_list=[authfile("cred-a.json"), authfile("cred-b.json")])
sol = health(rep, "gpt-5.6-sol")
rows = {c["credential"]: c for c in sol["by_credential"]}
check("a token refused on astra is refused on sol too",
      rows["cred-b.json"]["state"], "rejected")
check("and the panel says where it was refused",
      rows["cred-b.json"].get("refused_on"), "gpt-6-astra")
check("it is not counted as sol capacity", sol["available"], 1)

print()
print("==========================================================================")
print("AT. a replacement joins the back of the serving queue")
print("==========================================================================")
# The host serves fill-first: highest priority wins, and within one priority the
# lowest credential ID - a hash-like file name, so an arbitrary fixed order. A
# standby whose name sorted late was therefore never served: each replacement
# that sorted ahead of it took the traffic the moment it was enabled. One sat
# enabled for two and a half days that way on 21 requests while its weekly
# window ran down unused.
shutil.rmtree(DATA_DIR, ignore_errors=True)
rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 96, 400000)], "with_path": True},
    # The standby. Its name sorts after the replacement's, which is the case
    # that used to leave it idle forever.
    "cred-z.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)], "with_path": True},
    "cred-b.json": {"disabled": True, "windows": [(WEEKF, 5, 400000)], "with_path": True},
})
disk = {n: json.load(open(os.path.join(live, n))) for n in sorted(os.listdir(live))}
check("the drained incumbent is retired", disk["cred-a.json"]["disabled"], True)
check("the replacement is enabled", disk["cred-b.json"]["disabled"], False)
check("behind everything already enabled", disk["cred-b.json"].get("priority"), -1)
check("the standby is not rewritten to get there", "priority" in disk["cred-z.json"], False)
# What the host will do with that: the highest priority is served first.
enabled = [n for n, d in disk.items() if not d["disabled"]]
serving = max(enabled, key=lambda n: (disk[n].get("priority", 0), [-ord(ch) for ch in n]))
check("so the standby that waited serves next, not the newcomer", serving, "cred-z.json")

# Once priorities exist the newcomer still goes to the back, below the lowest.
shutil.rmtree(DATA_DIR, ignore_errors=True)
rotate({
    "cred-a.json": {"disabled": False, "windows": [(WEEKF, 96, 400000)],
                    "with_path": True, "priority": -3},
    "cred-z.json": {"disabled": False, "windows": [(WEEKF, 8, 400000)],
                    "with_path": True, "priority": -4},
    "cred-b.json": {"disabled": True, "windows": [(WEEKF, 5, 400000)], "with_path": True},
})
disk = {n: json.load(open(os.path.join(live, n))) for n in sorted(os.listdir(live))}
check("a queued newcomer lands below the lowest", disk["cred-b.json"].get("priority"), -5)

print()
print("=" * 74)
if failures:
    print("FAILED: " + ", ".join(failures))
    sys.exit(1)
print("all checks passed")
