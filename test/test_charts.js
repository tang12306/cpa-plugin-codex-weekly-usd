// Chart tests. The SVG builders are pulled straight out of the served panel so
// what runs here is byte-for-byte what the browser gets, then fed a synthetic
// multi-hour series and checked coordinate by coordinate.
const fs = require("fs");

const panelPath = process.argv[2];
if (!panelPath || !fs.existsSync(panelPath)) {
  // run_tests.py writes this from the panel the plugin actually serves; without
  // it there is nothing meaningful to assert against.
  console.error("missing panel html - run `make test` (or python3 test/run_tests.py) first");
  process.exit(2);
}
const html = fs.readFileSync(panelPath, "utf8");

// This runs before anything else because everything else eval()s functions
// lifted out of the page: a syntax error would kill this file with a stack
// trace rather than a verdict. It is also the only check that covers the parts
// of the script no test lifts - a broken one of those ships a panel whose
// script never runs and whose page renders nothing, while every string the HTML
// is grepped for is still present. new Function compiles without executing,
// which is what is wanted: the body reaches for document on its first line.
const script = html.slice(html.indexOf("<script>") + 8, html.lastIndexOf("</script>"));
if (script.length < 20000) {
  console.error("panel script looks truncated (" + script.length + " bytes)");
  process.exit(1);
}
try {
  new Function(script);
  console.log("  panel script compiles                          none".padEnd(72) + "OK");
} catch (err) {
  console.error("  panel script compiles                          " + err.message + "  FAIL");
  console.error("\nThe panel ships a script that does not parse: the page would render");
  console.error("nothing at all, however healthy the served HTML looks to a grep.");
  process.exit(1);
}

// Lift the chart helpers plus the formatters they depend on out of the page.
function lift(name) {
  const start = html.indexOf("  function " + name + "(");
  if (start < 0) throw new Error("function not found in panel: " + name);
  let depth = 0, i = html.indexOf("{", start);
  const from = i;
  for (; i < html.length; i++) {
    if (html[i] === "{") depth++;
    else if (html[i] === "}" && --depth === 0) break;
  }
  return html.slice(start, i + 1);
}

// liftVar grabs an object literal by balancing braces, so the translation
// dictionaries can be evaluated exactly rather than pattern-matched.
function liftVar(name) {
  const start = html.indexOf("  var " + name + " = {");
  if (start < 0) throw new Error("var not found in panel: " + name);
  let depth = 0, i = html.indexOf("{", start);
  const from = i;
  for (; i < html.length; i++) {
    if (html[i] === "{") depth++;
    else if (html[i] === "}" && --depth === 0) break;
  }
  return html.slice(from, i + 1);
}

const src = ["usd", "pct", "chartLabels", "buildBuckets", "agoLabel", "drawChart", "drawQuotaCurve"]
  .map(lift).join("\n");
eval(src);
const I18N = eval("(" + liftVar("I18N") + ")");

// The availability chips are built by the same page, so they are lifted the
// same way. They read the language through t(), which needs LANG and I18N in
// scope; everything else about them is a pure string transform.
let LANG = "en";
const t = eval("(" + lift("t").replace(/^  function t/, "function t") + ")");
eval(["esc", "dur", "modelStateTag", "credChip"].map(lift).join("\n"));

let failures = 0;
function check(name, got, want) {
  const ok = got === want;
  console.log("  " + name.padEnd(46) + String(got).padEnd(24) + (ok ? "OK" : "FAIL (want " + want + ")"));
  if (!ok) failures++;
}

// 48 hours: $1 spent in each of the last 24, nothing before. Percentage climbs
// one point per hour to 24%.
const series = [];
for (let ago = 47; ago >= 0; ago--) {
  series.push({ ago, usd: ago < 24 ? 1 : 0, requests: ago < 24 ? 2 : 0,
                percent: ago < 24 ? 24 - ago : undefined });
}

console.log("=".repeat(74));
console.log("G. chart geometry");
console.log("=".repeat(74));

const b = buildBuckets(series);
check("bucket span", b.span, 48);
check("last bucket is now", b.usd[47], 1);
check("oldest bucket empty", b.usd[0], 0);
check("percent captured", b.pct[47], 24);

const svg = drawChart(series, 1000, 165, true);
check("renders svg", svg.startsWith("<svg"), true);
check("one bar per bucket", (svg.match(/<rect /g) || []).length, 48);
check("has cumulative polyline", (svg.match(/<polyline /g) || []).length, 1);
check("axis shows cumulative total", svg.includes("$24.00"), true);
check("bar tooltip labels now", svg.includes("this hour"), true);

// The cumulative line must be flat while nothing is spent, then rise.
const pts = svg.match(/points='([^']+)'/)[1].split(" ").map(p => p.split(",").map(Number));
check("cumulative starts at baseline", pts[0][1] === pts[23][1], true);
check("cumulative rises after", pts[47][1] < pts[23][1], true);
check("cumulative is monotonic", pts.every((p, i) => i === 0 || p[1] <= pts[i - 1][1] + 1e-9), true);

console.log();
console.log("=".repeat(74));
console.log("H. the line is a trailing window, not a running total");
console.log("=".repeat(74));

// Fourteen days of perfectly steady load: $1 an hour, never varying. A running
// total climbs the whole way and says nothing except that time passed. The
// trailing seven-day total flattens once the window fills - which is what
// makes a change in it mean something.
const steady = [];
for (let ago = 335; ago >= 0; ago--) {
  const h = 335 - ago;                       // hours since recording began
  steady.push({ ago, usd: 1, requests: 1,
                rolling_usd: Math.min(h + 1, 168),
                rolling_partial: h + 1 < 168 });
}
const svgS = drawChart(steady, 1000, 165, true);
const runs = [...svgS.matchAll(/<polyline[^>]*points='([^']+)'/g)]
  .map(m => m[1].split(" ").map(p => p.split(",").map(Number)));
check("a provisional run and a settled one", runs.length, 2);
const settled = runs[1];
check("the settled stretch is flat", settled[1][1] === settled[settled.length - 1][1], true);
check("the provisional stretch is dashed", svgS.includes("stroke-dasharray='4 3'"), true);
check("the axis tops out at the window, not the total", svgS.includes("$168.00"), true);
check("and not at the fourteen-day total", svgS.includes("$336.00"), false);

// Load that genuinely doubles must still show as a climb.
const growing = steady.map((p, i) => ({ ...p,
  rolling_usd: i < 200 ? Math.min(i + 1, 168) : 168 + (i - 200) }));
const svgG = drawChart(growing, 1000, 165, true);
const gRuns = [...svgG.matchAll(/<polyline[^>]*points='([^']+)'/g)]
  .map(m => m[1].split(" ").map(p => p.split(",").map(Number)));
const gLast = gRuns[gRuns.length - 1];
check("real growth still rises", gLast[gLast.length - 1][1] < gLast[1][1], true);

// A payload from an older build carries no rolling figure, and must still draw
// rather than leaving the panel blank.
check("falls back when the field is absent",
  (drawChart(series, 1000, 165, true).match(/<polyline /g) || []).length, 1);

const curve = drawQuotaCurve(series, 10080, 300, 96);
check("quota curve renders", curve.startsWith("<svg"), true);
check("curve has reference line", curve.includes("stroke-dasharray"), true);
const cpts = curve.match(/<polyline[^>]*points='([^']+)'/)[1].split(" ").map(p => p.split(",").map(Number));
check("curve point per known hour", cpts.length, 24);
check("curve descends on screen as % rises", cpts[23][1] < cpts[0][1], true);

// A rollover mid-series must restart the curve, not draw a sawtooth.
const roll = series.map(p => ({ ...p }));
for (let i = 36; i < 48; i++) roll[i].percent = i - 36;   // percentage drops at ago=11
const rollCurve = drawQuotaCurve(roll, 10080, 300, 96);
const rpts = rollCurve.match(/<polyline[^>]*points='([^']+)'/)[1].split(" ").map(p => p.split(",").map(Number));
check("rollover restarts the curve", rpts.length, 12);

// One reading is not a trend and must render nothing at all.
check("single point draws nothing",
  drawQuotaCurve([{ ago: 0, usd: 1, requests: 1, percent: 5 }], 10080, 300, 96), "");
check("empty series handled", drawChart([], 300, 76, false).includes("No data"), true);

// The SVG builders take their few words as an argument rather than reading a
// language global, which is what lets this file run them outside a browser.
const zh = { noData: "暂无数据", thisHour: "本小时", hoursAgo: "小时前",
             now: "现在", total: "累计", reqs: "次" };
const zhSvg = drawChart(series, 1000, 165, true, zh);
check("labels are translatable", zhSvg.includes("本小时") && zhSvg.includes("累计"), true);
check("translated chart keeps geometry", (zhSvg.match(/<rect /g) || []).length, 48);
check("empty series translatable", drawChart([], 300, 76, false, zh).includes("暂无数据"), true);

console.log();
console.log("=".repeat(74));
console.log("H. translation dictionaries");
console.log("=".repeat(74));
const zhKeys = Object.keys(I18N.zh), enKeys = Object.keys(I18N.en);
const missingEn = zhKeys.filter(k => !(k in I18N.en));
const missingZh = enKeys.filter(k => !(k in I18N.zh));
check("languages offered", Object.keys(I18N).sort().join(","), "en,zh");
check("dictionary is substantial", zhKeys.length > 60, true);
check("no key missing from en", missingEn.join(",") || "none", "none");
check("no key missing from zh", missingZh.join(",") || "none", "none");
// A key present but left as the other language's text is worse than a missing
// key: it looks translated and is not.
const identical = zhKeys.filter(k =>
  typeof I18N.zh[k] === "string" && I18N.zh[k] === I18N.en[k] &&
  /[A-Za-z]{4}/.test(I18N.zh[k]) && k !== "locale");
check("no untranslated leftovers", identical.join(",") || "none", "none");
check("every value is a string", zhKeys.every(k => typeof I18N.zh[k] === "string"), true);

console.log();
console.log("=".repeat(74));
console.log("I. availability chips");
console.log("=".repeat(74));
// The board is what an operator reads during an outage, so the chips have to
// say which credential, what state, and how long - in the selected language.
const cooling = credChip({ credential: "cred-a.json", state: "cooling", requests: 40,
                           failed: 2, blocks: 3, cooldown_in_seconds: 3660,
                           cooldown_until: "2026-09-06T16:07:00Z", reason: "usage_limit_reached",
                           blocked_window: "5h", cooldown_estimated: false });
check("cooling chip is marked bad", cooling.includes("tag bad"), true);
check("cooling chip names the credential", cooling.includes("cred-a.json"), true);
check("cooling chip shows the countdown", cooling.includes("1h1m"), true);
check("cooling chip carries the reason", cooling.includes("usage_limit_reached"), true);
check("cooling chip carries the deadline", cooling.includes("2026-09-06T16:07:00Z"), true);

const guess = credChip({ credential: "c", state: "cooling", cooldown_in_seconds: 600,
                         cooldown_estimated: true });
check("an estimated deadline is marked", guess.includes("10m?"), true);

check("a healthy chip is not alarming",
  credChip({ credential: "c", state: "ok" }).includes("tag ok"), true);
check("a disabled chip is greyed",
  credChip({ credential: "c", state: "disabled" }).includes("off"), true);

// A credential name arrives from the auth file and is not trusted markup.
check("credential names are escaped",
  credChip({ credential: "<img src=x>", state: "ok" }).includes("&lt;img"), true);

check("a dead model reads as bad",
  modelStateTag({ state: "down", single_point: false }).includes("tag bad"), true);
check("a single credential is flagged",
  modelStateTag({ state: "ok", single_point: true }).includes("single credential"), true);

LANG = "zh";
check("chips follow the language",
  credChip({ credential: "c", state: "cooling", cooldown_in_seconds: 60 }).includes("冷却"), true);
check("state tags follow the language",
  modelStateTag({ state: "down", single_point: false }).includes("全部冷却"), true);
LANG = "en";

console.log();
console.log("=".repeat(74));
if (failures) { console.log("FAILED: " + failures + " check(s)"); process.exit(1); }
console.log("all chart checks passed");
