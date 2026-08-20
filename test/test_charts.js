// Chart tests. The SVG builders are pulled straight out of the served panel so
// what runs here is byte-for-byte what the browser gets, then fed a synthetic
// multi-hour series and checked coordinate by coordinate.
const fs = require("fs");

const html = fs.readFileSync(process.argv[2], "utf8");

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

const src = ["usd", "pct", "buildBuckets", "agoLabel", "drawChart", "drawQuotaCurve"]
  .map(lift).join("\n");
eval(src);

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
check("bar tooltip labels now", svg.includes("本小时"), true);

// The cumulative line must be flat while nothing is spent, then rise.
const pts = svg.match(/points='([^']+)'/)[1].split(" ").map(p => p.split(",").map(Number));
check("cumulative starts at baseline", pts[0][1] === pts[23][1], true);
check("cumulative rises after", pts[47][1] < pts[23][1], true);
check("cumulative is monotonic", pts.every((p, i) => i === 0 || p[1] <= pts[i - 1][1] + 1e-9), true);

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
check("empty series handled", drawChart([], 300, 76, false).includes("暂无数据"), true);

console.log();
console.log("=".repeat(74));
if (failures) { console.log("FAILED: " + failures + " check(s)"); process.exit(1); }
console.log("all chart checks passed");
