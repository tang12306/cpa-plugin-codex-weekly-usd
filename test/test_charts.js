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

const src = ["usd", "pct", "chartLabels", "buildBuckets", "agoLabel", "drawChart"]
  .map(lift).join("\n");
eval(src);
const I18N = eval("(" + liftVar("I18N") + ")");

// The table cells are built by the same page, so they are lifted the same way.
// They read the language through t(), which needs LANG and I18N in scope;
// everything else about them is a pure string transform.
let LANG = "en";
const t = eval("(" + lift("t").replace(/^  function t/, "function t") + ")");
eval(["esc", "dur", "row2", "perModelRows", "heat", "paceTag", "meterRow", "windowCell"].map(lift).join("\n"));

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
console.log("I. the per-model quota table");
console.log("=".repeat(74));
// One pool of quota shown at every model's price. The expensive model buys
// less window, so the two figures have to sit side by side: which one applies
// is decided by what the credential is about to be asked to serve.
const perModel = perModelRows({
  quota_usd_by_model: { "gpt-5.6-sol": 200, "gpt-6-astra": 100 },
  remaining_usd_by_model: { "gpt-5.6-sol": 140, "gpt-6-astra": 70 },
  quota_model_source: { "gpt-5.6-sol": "measured", "gpt-6-astra": "carried from gpt-5.6-sol" },
  dominant_model: "gpt-6-astra",
});
check("both models are listed", /gpt-5\.6-sol[\s\S]*gpt-6-astra|gpt-6-astra[\s\S]*gpt-5\.6-sol/.test(perModel), true);
check("the model that buys more window is listed first",
  perModel.indexOf("gpt-5.6-sol") < perModel.indexOf("gpt-6-astra"), true);
check("the model actually in use is marked", /gpt-6-astra <span class='tag ok'>/.test(perModel), true);
check("a carried figure is not passed off as measured",
  perModel.includes("carried"), true);
check("and is visually distinguished", perModel.includes("pms dim"), true);
check("remaining is shown beside the quota", perModel.includes("$70"), true);
// A carried figure is only as good as the ratio behind it, so it says so.
const backed = perModelRows({
  quota_usd_by_model: { "gpt-5.6-sol": 200, "gpt-6-astra": 100 },
  quota_model_source: { "gpt-5.6-sol": "measured", "gpt-6-astra": "carried from gpt-5.6-sol" },
  quota_model_windows: { "gpt-6-astra": 5 },
});
check("a carried figure says how many windows back it", backed.includes("carried · 5 windows"), true);
check("and what it was carried from", backed.includes("its measured gpt-5.6-sol figure"), true);
// Short of that, no figure at all - and the line says how far short.
const short = perModelRows({
  quota_usd_by_model: { "gpt-6-astra": 67.4 },
  quota_model_source: { "gpt-6-astra": "measured", "gpt-5.6-terra": "insufficient" },
  quota_model_windows: { "gpt-5.6-terra": 3 },
  quota_model_rule: { windows: 5, points: 10 },
});
check("a model without enough data is still listed", short.includes("gpt-5.6-terra"), true);
check("says so instead of a number", short.includes("not enough data · 3/5"), true);
check("and comes after the priced ones",
  short.indexOf("gpt-6-astra") < short.indexOf("gpt-5.6-terra"), true);
check("its tooltip gives the bar", short.includes("at least 5 windows"), true);
check("a window with no figure at all still lists its models",
  perModelRows({ quota_model_source: { "gpt-5.6-luna": "insufficient" } }).includes("gpt-5.6-luna"), true);
// A window nothing has priced renders nothing, not an empty table.
check("no estimate, no table", perModelRows({}), "");
// Model names come from upstream and are not trusted markup.
check("model names are escaped",
  perModelRows({ quota_usd_by_model: { "<img src=x>": 10 } }).includes("&lt;img"), true);

console.log();
console.log("=".repeat(74));
console.log("J. each bar is labelled");
console.log("=".repeat(74));
// The two bars once shared a single line of figures, and which number belonged
// to which bar was left to guess. Each row now carries its own word and value.
const cellRows = w => windowCell(w).match(/<div class='mrow'>.*?<\/span><\/div>/g) || [];
const week = { used_percent: 85, time_progress_percent: 34.3, pace_ratio: 2.48,
               reset_in_seconds: 396000,
               estimate: { method: "delta", quota_usd: 65.87, remaining_usd: 9.88 } };
const wr = cellRows(week);
check("one row per bar", wr.length, 2);
check("the used bar names itself", /<span>used<\/span>/.test(wr[0]), true);
check("and carries its own figure", wr[0].includes("85.0%"), true);
check("the time bar names itself", /<span>time<\/span>/.test(wr[1]), true);
check("and carries its own figure", wr[1].includes("34.3%"), true);
check("neither figure strays into the other row",
  !wr[0].includes("34.3%") && !wr[1].includes("85.0%"), true);
check("a reading from before the reset is dimmed",
  cellRows({ ...week, used_percent_stale: true })[0].includes("opacity:.35"), true);
check("no clock, no time bar",
  cellRows({ ...week, time_progress_percent: undefined }).length, 1);
LANG = "zh";
const zr = cellRows(week);
check("labels follow the language", zr[0].includes("已用") && zr[1].includes("时间"), true);
LANG = "en";

console.log();
console.log("=".repeat(74));
console.log("K. alerts");
console.log("=".repeat(74));
// Alerts are built from DOM nodes rather than markup, so a stand-in document
// that records what was built is all note() needs.
const document = {
  createElement: tag => ({ tag, children: [], className: "", textContent: "",
                           appendChild(c) { this.children.push(c); } }),
  createTextNode: text => ({ text }),
};
const alerts = { children: [], appendChild(c) { this.children.push(c); } };
eval(["note", "warnText"].map(lift).join("\n"));
const flat = m => m.children.map(n => n.tag === "b" ? "<b>" + n.textContent + "</b>" : n.text).join("");
note("warn", "requests **are unaffected** for now");
check("emphasis is bold, not asterisks", flat(alerts.children[0]), "requests <b>are unaffected</b> for now");
note("bad", "<img src=x> is **refused**");
check("text around it stays text", alerts.children[1].children[0].text, "<img src=x> is ");

LANG = "zh";
const lowZh = warnText({ code: "rotator_low", reserves: 3, in_seconds: 30 * 3600 });
check("running low says what takes over", lowZh.includes("还有 3 个"), true);
check("and when the first window resets", lowZh.includes("1天"), true);
check("chinese sentences run on unspaced", lowZh.includes("。 "), false);
check("says so when nothing is behind them",
  warnText({ code: "rotator_low", reserves: 0 }).includes("将没有凭据可用"), true);
LANG = "en";
check("english sentences are spaced",
  warnText({ code: "rotator_low", reserves: 2 }).includes("unaffected.** After that"), true);

console.log();
console.log("=".repeat(74));
if (failures) { console.log("FAILED: " + failures + " check(s)"); process.exit(1); }
console.log("all chart checks passed");
