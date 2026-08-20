package main

import (
	"encoding/json"
	"strings"
)

type managementRequest struct {
	Method  string              `json:"Method"`
	Path    string              `json:"Path"`
	Headers map[string][]string `json:"Headers"`
	Query   map[string][]string `json:"Query"`
	Body    []byte              `json:"Body"`
}

type managementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

// HandleManagement serves both route families.
//
// /panel is reachable without the management key, so it must never contain
// data: it ships an empty shell that asks the operator for the key and then
// calls the authenticated /data route itself.
func (a *App) HandleManagement(payload []byte) json.RawMessage {
	var req managementRequest
	_ = json.Unmarshal(payload, &req)
	path := strings.TrimRight(req.Path, "/")

	switch {
	case strings.HasSuffix(path, "/panel") || path == "" || strings.HasSuffix(path, pluginID):
		return jsonResponse(200, "text/html; charset=utf-8", []byte(panelHTML))
	case strings.HasSuffix(path, "/prices"):
		return marshalResponse(a.prices.Snapshot())
	case strings.HasSuffix(path, "/data"):
		return marshalResponse(a.Report())
	default:
		return marshalResponse(map[string]any{"error": "unknown route: " + req.Path})
	}
}

func marshalResponse(v any) json.RawMessage {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		body = []byte(`{"error":"encode failed"}`)
	}
	return jsonResponse(200, "application/json; charset=utf-8", body)
}

func jsonResponse(status int, contentType string, body []byte) json.RawMessage {
	resp := managementResponse{
		StatusCode: status,
		Headers: map[string][]string{
			"content-type":  {contentType},
			"cache-control": {"no-store"},
		},
		Body: body,
	}
	raw, _ := json.Marshal(resp)
	return raw
}

// panelHTML is deliberately data-free. Everything it displays is fetched at
// runtime from the authenticated Management API using a key the operator types
// in, so exposing this page without authentication leaks nothing.
const panelHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Codex 周额度美元估算</title>
<style>
  :root {
    --bg:#f5f6f8; --panel:#fff; --panel2:#fafbfc; --ink:#15181d; --muted:#6b7280;
    --line:#e4e7ec; --accent:#2f6feb; --good:#12805c; --warn:#b45309; --bad:#c2352b;
    --grid:#eef0f3;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg:#121417; --panel:#1a1d22; --panel2:#1f2229; --ink:#e7e9ec; --muted:#98a1af;
      --line:#2a2e36; --accent:#6aa0ff; --good:#3fbf8f; --warn:#e0a34a; --bad:#f0736a;
      --grid:#23272e;
    }
  }
  *{box-sizing:border-box}
  body{margin:0;padding:22px;background:var(--bg);color:var(--ink);
       font:14px/1.55 ui-sans-serif,system-ui,-apple-system,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}
  h1{font-size:19px;margin:0 0 2px;letter-spacing:.3px}
  h2{font-size:14px;margin:0 0 10px;color:var(--muted);font-weight:600}
  .sub{color:var(--muted);font-size:13px}
  .bar{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin:16px 0}
  input,button,select{font:inherit;padding:7px 11px;border-radius:7px;
       border:1px solid var(--line);background:var(--panel);color:var(--ink)}
  input{min-width:280px}
  button{cursor:pointer;border-color:var(--accent);color:var(--accent)}
  button:hover{background:var(--accent);color:#fff}
  button.ghost{border-color:var(--line);color:var(--muted)}
  button.ghost:hover{background:var(--line);color:var(--ink)}
  .cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(158px,1fr));gap:10px;margin-bottom:16px}
  .card{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:12px 14px}
  .card .k{color:var(--muted);font-size:12px}
  .card .v{font-size:22px;font-weight:650;margin-top:3px;font-variant-numeric:tabular-nums}
  .card .n{font-size:12px;color:var(--muted);font-weight:400}
  .box{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin-bottom:16px}
  .wrap{overflow-x:auto;background:var(--panel);border:1px solid var(--line);border-radius:10px}
  table{border-collapse:collapse;width:100%;min-width:1080px}
  th,td{padding:9px 12px;text-align:right;border-bottom:1px solid var(--line);white-space:nowrap}
  th{font-size:12px;color:var(--muted);font-weight:600;position:sticky;top:0;background:var(--panel);z-index:1}
  th:first-child,td:first-child{text-align:left}
  tbody tr:last-child>td{border-bottom:none}
  tbody tr.main:hover{background:var(--panel2)}
  td.num{font-variant-numeric:tabular-nums}
  .name{font-weight:600}
  .sub2{font-size:12px;color:var(--muted);font-weight:400}
  .dual{display:flex;flex-direction:column;gap:3px;width:132px}
  .meter{position:relative;height:7px;border-radius:4px;background:var(--line);overflow:hidden}
  .meter>i{position:absolute;inset:0 auto 0 0;display:block;border-radius:4px}
  .mlabel{display:flex;justify-content:space-between;font-size:11px;color:var(--muted)}
  .tag{display:inline-block;padding:1px 7px;border-radius:999px;font-size:11px;
       border:1px solid var(--line);color:var(--muted);white-space:nowrap}
  .tag.high,.tag.ok{color:var(--good);border-color:var(--good)}
  .tag.medium{color:var(--warn);border-color:var(--warn)}
  .tag.low,.tag.none,.tag.bad{color:var(--bad);border-color:var(--bad)}
  .off{opacity:.45}
  .msg{padding:10px 13px;border-radius:8px;border:1px solid var(--line);
       background:var(--panel);margin-bottom:9px;font-size:13px}
  .msg.bad{border-color:var(--bad);color:var(--bad)}
  .msg.warn{border-color:var(--warn);color:var(--warn)}
  .foot{color:var(--muted);font-size:12px;margin-top:14px;line-height:1.8}
  tr.detail>td{background:var(--panel2);padding:14px 16px}
  tr.detail table{min-width:0;width:auto}
  tr.detail th,tr.detail td{border-bottom:1px solid var(--grid);padding:6px 14px 6px 0}
  .grid2{display:grid;grid-template-columns:minmax(420px,1.6fr) minmax(240px,1fr);gap:22px;align-items:start}
  .kv{display:grid;grid-template-columns:auto auto;gap:3px 14px;font-size:12.5px}
  .kv .k{color:var(--muted)}
  .kv .v{text-align:right;font-variant-numeric:tabular-nums}
  .toggle{cursor:pointer;user-select:none;color:var(--accent)}
  svg{display:block;max-width:100%}
  details summary{cursor:pointer;color:var(--muted);font-size:13px;margin-bottom:8px}
  .legend{font-weight:400;font-size:12px;color:var(--muted);margin-left:14px;white-space:nowrap}
  .sw{display:inline-block;vertical-align:middle;margin-right:5px}
  .sw.bar{width:9px;height:9px;border-radius:2px;background:var(--accent);opacity:.62}
  .sw.ln{width:15px;height:0;border-top:2px solid var(--good)}
  .sw.ln.warn{border-top-color:var(--warn)}
  .sw.ln.dash{border-top:2px dashed var(--muted)}
  .chartcap{font-size:12px;color:var(--muted);margin:2px 0 10px}
</style>
</head>
<body>
<h1>Codex 周额度美元估算</h1>
<div class="sub">按 OpenAI 官方 API 价目,推算每个凭据的周额度值多少美元</div>

<div class="bar">
  <input id="key" type="password" placeholder="管理密钥" autocomplete="off" spellcheck="false">
  <button id="load">加载</button>
  <button class="ghost" id="forget">忘记密钥</button>
  <button class="ghost" id="export">导出 JSON</button>
  <select id="sort">
    <option value="quota">按周额度排序</option>
    <option value="remain">按剩余排序</option>
    <option value="used">按已用比例排序</option>
    <option value="pace">按消耗节奏排序</option>
    <option value="name">按名称排序</option>
  </select>
  <span id="stamp" class="sub"></span>
</div>

<div id="alerts"></div>
<div id="cards" class="cards" hidden></div>
<div id="chartbox" class="box" hidden>
  <h2>全部凭据 · 用量走势
    <span class="legend"><i class="sw bar"></i>每小时消耗（左轴）</span>
    <span class="legend"><i class="sw ln good"></i>累计消耗（右轴）</span>
  </h2>
  <div id="chart"></div>
</div>
<div id="wrap" class="wrap" hidden>
  <table>
    <thead>
      <tr>
        <th>凭据</th><th>套餐</th><th>额度 / 时间进度</th><th>节奏</th>
        <th>周额度</th><th>已用</th><th>剩余</th><th>日均</th><th>重置倒计时</th><th>依据</th>
      </tr>
    </thead>
    <tbody id="rows"></tbody>
  </table>
</div>
<div id="pricebox" class="box" hidden>
  <details>
    <summary>当前生效价目表（美元 / 百万 token）</summary>
    <div id="prices"></div>
  </details>
</div>
<div id="foot" class="foot"></div>

<script>
(function () {
  var STORE = "cwu.key";
  var el = function (id) { return document.getElementById(id); };
  var keyBox = el("key"), alerts = el("alerts"), rows = el("rows"), cards = el("cards");
  var wrap = el("wrap"), stamp = el("stamp"), foot = el("foot");
  var chartbox = el("chartbox"), chart = el("chart"), pricebox = el("pricebox");
  var last = null, timer = null;

  keyBox.value = localStorage.getItem(STORE) || "";

  function base() {
    var i = location.pathname.indexOf("/v0/");
    return i >= 0 ? location.pathname.slice(0, i) : "";
  }
  function esc(s) {
    return String(s === null || s === undefined ? "" : s)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
  }
  function usd(v) {
    if (v === null || v === undefined || isNaN(v)) return "—";
    var a = Math.abs(v);
    if (a !== 0 && a < 0.01) return "$" + v.toFixed(4);
    return "$" + Number(v).toLocaleString("zh-CN", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  }
  function pct(v) { return (v === null || v === undefined) ? "—" : Number(v).toFixed(1) + "%"; }
  function tok(v) {
    if (!v) return "0";
    if (v >= 1e6) return (v / 1e6).toFixed(2) + "M";
    if (v >= 1e3) return (v / 1e3).toFixed(1) + "K";
    return String(v);
  }
  function dur(s) {
    if (s === null || s === undefined) return "—";
    var h = Math.floor(s / 3600), d = Math.floor(h / 24);
    return d > 0 ? d + " 天 " + (h % 24) + " 小时" : h + " 小时 " + Math.floor((s % 3600) / 60) + " 分";
  }
  function heat(p) { return p >= 90 ? "var(--bad)" : p >= 70 ? "var(--warn)" : "var(--good)"; }
  function note(cls, text) {
    var d = document.createElement("div");
    d.className = "msg " + cls; d.textContent = text; alerts.appendChild(d);
  }

  // Flatten a series into fixed hourly buckets ending at "now".
  function buildBuckets(series) {
    var maxAgo = 0, i;
    for (i = 0; i < series.length; i++) maxAgo = Math.max(maxAgo, series[i].ago);
    var span = Math.max(maxAgo, 23) + 1;
    var usdB = new Array(span), pctB = new Array(span), reqB = new Array(span);
    for (i = 0; i < span; i++) { usdB[i] = 0; pctB[i] = null; reqB[i] = 0; }
    for (i = 0; i < series.length; i++) {
      var idx = span - 1 - series[i].ago;
      if (idx < 0 || idx >= span) continue;
      usdB[idx] += series[i].usd || 0;
      reqB[idx] += series[i].requests || 0;
      if (series[i].percent !== undefined && series[i].percent !== null) pctB[idx] = series[i].percent;
    }
    return { span: span, usd: usdB, pct: pctB, req: reqB };
  }

  function agoLabel(ago) { return ago === 0 ? "本小时" : ago + " 小时前"; }

  // Hourly spend as bars, with a cumulative-spend line on its own right-hand
  // axis. Inline SVG only, so the page needs no external chart library.
  function drawChart(series, w, h, showAxis) {
    if (!series || !series.length) return "<div class='sub'>暂无数据</div>";
    var b = buildBuckets(series), span = b.span, i;

    var maxU = 0, total = 0;
    for (i = 0; i < span; i++) { maxU = Math.max(maxU, b.usd[i]); total += b.usd[i]; }
    if (maxU <= 0) maxU = 1;
    var cum = new Array(span), run = 0;
    for (i = 0; i < span; i++) { run += b.usd[i]; cum[i] = run; }
    var maxC = total > 0 ? total : 1;

    var padL = showAxis ? 50 : 4, padR = showAxis ? 54 : 4;
    var padB = showAxis ? 18 : 12, padT = 8;
    var iw = w - padL - padR, ih = h - padB - padT;
    var bw = iw / span;
    var cx = function (k) { return padL + k * bw + bw / 2; };
    var svg = "<svg viewBox='0 0 " + w + " " + h + "' width='100%' height='" + h + "'>";

    if (showAxis) {
      for (i = 0; i <= 2; i++) {
        var y = padT + ih - ih * i / 2;
        svg += "<line x1='" + padL + "' y1='" + y.toFixed(1) + "' x2='" + (w - padR) +
               "' y2='" + y.toFixed(1) + "' stroke='var(--grid)' stroke-width='1'/>";
        svg += "<text x='" + (padL - 6) + "' y='" + (y + 4).toFixed(1) + "' text-anchor='end' " +
               "font-size='10' fill='var(--accent)'>" + usd(maxU * i / 2) + "</text>";
        svg += "<text x='" + (w - padR + 6) + "' y='" + (y + 4).toFixed(1) + "' " +
               "font-size='10' fill='var(--good)'>" + usd(maxC * i / 2) + "</text>";
      }
    }

    for (i = 0; i < span; i++) {
      var bh = Math.max(b.usd[i] > 0 ? 1.5 : 0, ih * b.usd[i] / maxU);
      svg += "<rect x='" + (padL + i * bw + bw * 0.12).toFixed(2) + "' y='" + (padT + ih - bh).toFixed(2) +
             "' width='" + Math.max(0.6, bw * 0.76).toFixed(2) + "' height='" + bh.toFixed(2) +
             "' rx='1' fill='var(--accent)' opacity='.62'><title>" + agoLabel(span - 1 - i) +
             " · " + usd(b.usd[i]) + " · " + b.req[i] + " 次</title></rect>";
    }

    // Cumulative spend. Flat stretches are idle hours; a steepening slope is
    // spend accelerating.
    var pts = [];
    for (i = 0; i < span; i++) {
      pts.push(cx(i).toFixed(1) + "," + (padT + ih - ih * cum[i] / maxC).toFixed(1));
    }
    svg += "<polyline fill='none' stroke='var(--good)' stroke-width='2' " +
           "stroke-linejoin='round' points='" + pts.join(" ") + "'/>";
    svg += "<circle cx='" + cx(span - 1).toFixed(1) + "' cy='" +
           (padT + ih - ih * cum[span - 1] / maxC).toFixed(1) +
           "' r='3' fill='var(--good)'><title>累计 " + usd(total) + "</title></circle>";

    if (showAxis) {
      svg += "<text x='" + padL + "' y='" + (h - 4) + "' font-size='10' fill='var(--muted)'>" +
             span + " 小时前</text>";
      svg += "<text x='" + (w - padR) + "' y='" + (h - 4) + "' text-anchor='end' font-size='10' " +
             "fill='var(--muted)'>现在</text>";
    }
    return svg + "</svg>";
  }

  // Quota percentage over time against a constant-rate reference. A curve above
  // the dashed line is the visual form of a pace ratio greater than one: the
  // window will be exhausted before it resets.
  function drawQuotaCurve(series, windowMinutes, w, h) {
    if (!series || !series.length) return "";
    var b = buildBuckets(series), span = b.span, i;

    // Forward-fill: a percentage stays where it was until the next reading.
    var fill = new Array(span), seen = null, any = false;
    for (i = 0; i < span; i++) {
      if (b.pct[i] !== null) { seen = b.pct[i]; any = true; }
      fill[i] = seen;
    }
    if (!any) return "";

    var padL = 34, padR = 6, padB = 14, padT = 8;
    var iw = w - padL - padR, ih = h - padB - padT;
    var cx = function (k) { return padL + iw * (span === 1 ? 0 : k / (span - 1)); };
    var cy = function (v) { return padT + ih - ih * Math.min(100, Math.max(0, v)) / 100; };
    var svg = "<svg viewBox='0 0 " + w + " " + h + "' width='100%' height='" + h + "'>";

    for (i = 0; i <= 2; i++) {
      var y = padT + ih - ih * i / 2;
      svg += "<line x1='" + padL + "' y1='" + y.toFixed(1) + "' x2='" + (w - padR) + "' y2='" +
             y.toFixed(1) + "' stroke='var(--grid)'/>";
      svg += "<text x='" + (padL - 5) + "' y='" + (y + 4).toFixed(1) + "' text-anchor='end' " +
             "font-size='10' fill='var(--muted)'>" + (i * 50) + "%</text>";
    }

    var first = 0;
    while (first < span && fill[first] === null) first++;
    // A falling percentage means the window rolled over. Only the current
    // window is drawn, otherwise the curve is a sawtooth and the reference line
    // gets anchored in a window that has already closed.
    for (i = first + 1; i < span; i++) {
      if (fill[i] < fill[i - 1] - 0.001) first = i;
    }
    // One point is not a curve; drawing it would show an empty box with a
    // caption promising a trend that is not there yet.
    if (span - first < 2) return "";

    // Constant-rate reference: 100% spread evenly across the whole window.
    if (windowMinutes > 0) {
      var slope = 100 / (windowMinutes / 60);
      var start = fill[first], endV = start + slope * (span - 1 - first);
      svg += "<line x1='" + cx(first).toFixed(1) + "' y1='" + cy(start).toFixed(1) +
             "' x2='" + cx(span - 1).toFixed(1) + "' y2='" + cy(endV).toFixed(1) +
             "' stroke='var(--muted)' stroke-width='1.5' stroke-dasharray='4 3' opacity='.7'>" +
             "<title>匀速参考线</title></line>";
    }

    var pts = [];
    for (i = first; i < span; i++) pts.push(cx(i).toFixed(1) + "," + cy(fill[i]).toFixed(1));
    svg += "<polyline fill='none' stroke='var(--warn)' stroke-width='2' stroke-linejoin='round' " +
           "points='" + pts.join(" ") + "'/>";
    svg += "<circle cx='" + cx(span - 1).toFixed(1) + "' cy='" + cy(fill[span - 1]).toFixed(1) +
           "' r='3' fill='var(--warn)'><title>" + pct(fill[span - 1]) + "</title></circle>";
    return svg + "</svg>";
  }

  function dualMeter(a) {
    var q = a.used_percent || 0, t = a.time_progress_percent;
    var s = "<div class='dual'>";
    s += "<div class='mlabel'><span>额度</span><span>" + pct(q) + "</span></div>";
    s += "<div class='meter'><i style='width:" + Math.min(100, q) + "%;background:" + heat(q) + "'></i></div>";
    if (t !== undefined && t !== null) {
      s += "<div class='mlabel'><span>时间</span><span>" + pct(t) + "</span></div>";
      s += "<div class='meter'><i style='width:" + Math.min(100, t) +
           "%;background:var(--muted);opacity:.55'></i></div>";
    }
    return s + "</div>";
  }

  // Quota consumed versus clock elapsed. Above 1 means the window will run out
  // early at the current rate.
  function paceTag(a) {
    var r = a.pace_ratio;
    if (r === undefined || r === null) return "<span class='tag'>—</span>";
    var cls = r > 1.15 ? "bad" : (r < 0.85 ? "ok" : "");
    var word = r > 1.15 ? "偏快" : (r < 0.85 ? "宽裕" : "正常");
    return "<span class='tag " + cls + "'>" + word + " ×" + r.toFixed(2) + "</span>";
  }

  function detailRow(a) {
    var e = a.estimate || {};
    var m = "<table><thead><tr><th>模型</th><th>请求</th><th>失败</th><th>输入</th><th>输出</th>" +
            "<th>缓存命中</th><th>单价(入/出)</th><th>均价/次</th><th>金额</th></tr></thead><tbody>";
    (a.by_model || []).forEach(function (x) {
      var p = x.price || {};
      m += "<tr><td>" + esc(x.model) + (x.unpriced ? " <span class='tag bad'>无价目</span>" : "") + "</td>" +
           "<td class='num'>" + x.requests + "</td>" +
           "<td class='num'>" + (x.failed || 0) + "</td>" +
           "<td class='num'>" + tok(x.tokens.InputTokens) + "</td>" +
           "<td class='num'>" + tok(x.tokens.OutputTokens) +
             (x.tokens.ReasoningTokens ? " <span class='sub2'>(含推理 " + tok(x.tokens.ReasoningTokens) + ")</span>" : "") + "</td>" +
           "<td class='num'>" + (x.cache_hit_rate === undefined ? "—" : pct(x.cache_hit_rate)) + "</td>" +
           "<td class='num sub2'>" + (p.input === undefined ? "—" : "$" + p.input + " / $" + p.output) + "</td>" +
           "<td class='num'>" + (x.avg_usd === undefined ? "—" : usd(x.avg_usd)) + "</td>" +
           "<td class='num'>" + usd(x.usd) + "</td></tr>";
    });
    m += "</tbody></table>";

    var t = a.window_tokens || {};
    var kv = "<div class='kv'>" +
      row2("窗口实测消耗", usd(a.window_usd_observed)) +
      row2("已归属消耗", usd(e.attributed_usd) + "<span class='sub2'> / 挂账 " +
           usd((a.window_usd_observed || 0) - (e.attributed_usd || 0)) + "</span>") +
      row2("缓存省下", usd(a.window_saved_usd)) +
      row2("请求 / 失败", (a.window_requests || 0) + " / " + (a.window_failed || 0) +
           (a.failure_rate ? " (" + pct(a.failure_rate) + ")" : "")) +
      row2("均价每次", usd(a.avg_usd_per_request)) +
      row2("输入 / 输出", tok(t.InputTokens) + " / " + tok(t.OutputTokens)) +
      row2("缓存读取", tok(t.CacheReadTokens) + (a.cache_hit_rate !== undefined ? " (" + pct(a.cache_hit_rate) + ")" : "")) +
      row2("窗口法估算", e.quota_usd_by_window ? usd(e.quota_usd_by_window) : "不可用") +
      row2("步进法估算", e.quota_usd_by_delta ? usd(e.quota_usd_by_delta) : "不可用") +
      row2("校准样本", (a.calibration || {}).samples + " 次 / 累计 " + pct((a.calibration || {}).percent)) +
      row2("整窗覆盖", a.window_full_coverage ? "是" : "否（中途接管）") +
      row2("跑满预警", a.will_exhaust_before_reset === true ? "<span class='tag bad'>会提前用完</span>" :
           (a.will_exhaust_before_reset === false ? "<span class='tag ok'>够用到重置</span>" : "—")) +
      row2("续航", a.runway_days === undefined ? "—" : a.runway_days + " 天") +
      row2("最后观测", a.observed_age_seconds === undefined ? "—" : dur(a.observed_age_seconds) + "前") +
      "</div>";

    var curve = drawQuotaCurve(a.series, a.window_minutes, 300, 96);
    var charts =
      "<div style='margin-bottom:12px'>" + drawChart(a.series, 300, 76, false) +
        "<div class='chartcap'><i class='sw bar'></i>每小时消耗 " +
        "<i class='sw ln' style='margin-left:8px'></i>累计</div></div>" +
      (curve
        ? "<div style='margin-bottom:12px'>" + curve +
          "<div class='chartcap'><i class='sw ln warn'></i>额度百分比 " +
          "<i class='sw ln dash' style='margin-left:8px'></i>匀速参考线" +
          "<br>曲线高过虚线 = 照此速度会在重置前用完</div></div>"
        : "");

    return "<div class='grid2'><div>" + m + "</div><div>" + charts + kv + "</div></div>";
  }
  function row2(k, v) { return "<div class='k'>" + esc(k) + "</div><div class='v'>" + v + "</div>"; }

  function sortAccounts(list) {
    var mode = el("sort").value;
    var c = {
      quota: function (a, b) { return (b.estimate.quota_usd || 0) - (a.estimate.quota_usd || 0); },
      remain: function (a, b) { return (b.estimate.remaining_usd || 0) - (a.estimate.remaining_usd || 0); },
      used: function (a, b) { return (b.used_percent || 0) - (a.used_percent || 0); },
      pace: function (a, b) { return (b.pace_ratio || 0) - (a.pace_ratio || 0); },
      name: function (a, b) { return String(a.label).localeCompare(String(b.label)); }
    }[mode];
    return list.slice().sort(c);
  }

  function render(data) {
    last = data;
    alerts.innerHTML = "";
    (data.warnings || []).forEach(function (w) { note("warn", w); });

    var accounts = data.accounts || [];
    var waitingList = accounts.filter(function (a) {
      return !a.estimate || a.estimate.method === "none";
    });
    if (waitingList.length) {
      note("warn", waitingList.length + " / " + accounts.length +
        " 个凭据还没有估算值。额度只有在「插件亲眼看到百分比至少走动 1 点」之后才能定价,正常使用中会自动补上。");
    }

    var t = data.totals || {};
    cards.innerHTML =
      card("凭据数", t.credentials, (t.estimated || 0) + " 个已定价" +
           (t.at_risk ? " · " + t.at_risk + " 个逼近上限" : "")) +
      card("周额度总值", usd(t.quota_usd), "折合官方 API 计费") +
      card("已用", usd(t.spent_usd), "按百分比推算") +
      card("剩余", usd(t.remaining_usd), "本窗口内") +
      card("实测消耗", usd(t.window_usd_observed), "插件亲眼记账部分") +
      card("缓存省下", usd(t.cache_saved_usd), "对比全价输入") +
      card("请求数", (t.window_requests || 0).toLocaleString("zh-CN"),
           (t.window_failed || 0) + " 次失败");
    cards.hidden = false;

    chart.innerHTML = drawChart(data.fleet_series, 1000, 165, true);
    chartbox.hidden = false;

    rows.innerHTML = "";
    sortAccounts(accounts).forEach(function (a, i) {
      var e = a.estimate || {};
      var waiting = e.method === "none";
      var money = function (v) { return waiting ? "<span class='tag'>观测中</span>" : usd(v); };

      var tr = document.createElement("tr");
      tr.className = "main" + (a.disabled ? " off" : "");
      tr.innerHTML =
        "<td><span class='toggle' data-i='" + i + "'>▸</span> <span class='name'>" +
          esc(a.label || a.auth_id) + "</span>" +
          (a.disabled ? " <span class='tag'>已停用</span>" : "") +
          (a.unavailable ? " <span class='tag bad'>不可用</span>" : "") +
          "<div class='sub2'>" + (a.by_model || []).length + " 个模型 · " +
          (a.window_requests || 0) + " 次请求" +
          (a.active_limit ? " · " + esc(a.active_limit) : "") + "</div></td>" +
        "<td>" + esc(a.plan_type || "—") + "<div class='sub2'>" + esc(a.window_label || "") + " 窗口</div></td>" +
        "<td>" + dualMeter(a) + "</td>" +
        "<td>" + paceTag(a) + "</td>" +
        "<td class='num'>" + money(e.quota_usd) + "</td>" +
        "<td class='num'>" + money(e.spent_usd) + "</td>" +
        "<td class='num'>" + money(e.remaining_usd) + "</td>" +
        "<td class='num'>" + (waiting || a.burn_usd_per_day === undefined ? "—" : usd(a.burn_usd_per_day)) + "</td>" +
        "<td class='num'>" + dur(a.reset_in_seconds) + "</td>" +
        "<td>" + (waiting
          ? "<span class='tag'>待百分比走动 1%</span>"
          : "<span class='tag " + esc(e.confidence) + "'>" + confWord(e.confidence) + "</span> " +
            "<span class='tag'>" + methodWord(e.method) + " " + pct(e.evidence_percent) + "</span>") + "</td>";
      rows.appendChild(tr);

      var dr = document.createElement("tr");
      dr.className = "detail"; dr.hidden = true;
      dr.innerHTML = "<td colspan='10'>" + detailRow(a) + "</td>";
      rows.appendChild(dr);

      tr.querySelector(".toggle").addEventListener("click", function (ev) {
        dr.hidden = !dr.hidden;
        ev.target.textContent = dr.hidden ? "▸" : "▾";
      });
    });
    wrap.hidden = false;

    stamp.textContent = "更新于 " + new Date().toLocaleTimeString("zh-CN");
    var via = { host: "经 CPA 代理", direct: "直连" }[data.price_transport] || "";
    foot.innerHTML =
      "价目来源：" + esc(data.price_source || "内置表") +
      (via ? "（" + via + "）" : "") +
      (data.price_fetched ? "，同步于 " + esc(data.price_fetched) : "") + "<br>" +
      "<b>窗口法</b>＝整窗实测消耗 ÷ 整窗百分比，只在插件看到窗口从 0% 开始时可用；" +
      "<b>步进法</b>＝Σ两次读数间的消耗 ÷ Σ百分比步进，中途安装也能收敛。" +
      "置信度反映插件亲眼看着消耗掉的额度比例。<br>" +
      "<b>节奏</b>＝额度消耗比例 ÷ 窗口时间流逝比例，大于 1 表示照此速度会在重置前用完。" +
      "缓存读取与推理 token 分别是输入、输出的子集，不重复计费。";
    loadPrices();
  }

  function confWord(c) {
    return { high: "高置信", medium: "中置信", low: "低置信", none: "无估算" }[c] || c;
  }
  function methodWord(m) { return { window: "窗口法", delta: "步进法", none: "—" }[m] || m; }

  function card(k, v, n) {
    return "<div class='card'><div class='k'>" + esc(k) + "</div><div class='v'>" +
      (v === undefined || v === null ? "—" : v) + "</div>" +
      (n ? "<div class='n'>" + esc(n) + "</div>" : "") + "</div>";
  }

  function api(path) {
    return fetch(base() + "/v0/management/codex-weekly-usd/" + path, {
      headers: { "X-Management-Key": keyBox.value.trim() }, cache: "no-store"
    }).then(function (r) {
      if (r.status === 401 || r.status === 403) throw new Error("管理密钥被拒绝。");
      if (!r.ok) throw new Error("请求失败：HTTP " + r.status);
      return r.json();
    });
  }

  function loadPrices() {
    api("prices").then(function (p) {
      var h = "<table><thead><tr><th>模型</th><th>输入</th><th>输出</th><th>缓存读</th><th>缓存写</th></tr></thead><tbody>";
      (p.models || []).forEach(function (m) {
        h += "<tr><td>" + esc(m.model) + (m.overridden ? " <span class='tag'>已覆盖</span>" : "") + "</td>" +
             "<td class='num'>$" + m.input + "</td><td class='num'>$" + m.output + "</td>" +
             "<td class='num'>" + (m.cache_read ? "$" + m.cache_read : "—") + "</td>" +
             "<td class='num'>" + (m.cache_write ? "$" + m.cache_write : "—") + "</td></tr>";
      });
      el("prices").innerHTML = h + "</tbody></table>";
      pricebox.hidden = false;
    }).catch(function () { pricebox.hidden = true; });
  }

  function load() {
    if (!keyBox.value.trim()) { alerts.innerHTML = ""; note("warn", "请先填入管理密钥。"); return; }
    localStorage.setItem(STORE, keyBox.value.trim());
    api("data").then(render).catch(function (err) {
      alerts.innerHTML = ""; note("bad", err.message);
      cards.hidden = wrap.hidden = chartbox.hidden = pricebox.hidden = true;
    });
  }

  el("load").addEventListener("click", load);
  el("sort").addEventListener("change", function () { if (last) render(last); });
  keyBox.addEventListener("keydown", function (e) { if (e.key === "Enter") load(); });
  el("forget").addEventListener("click", function () {
    localStorage.removeItem(STORE); keyBox.value = ""; last = null;
    cards.hidden = wrap.hidden = chartbox.hidden = pricebox.hidden = true;
    alerts.innerHTML = ""; stamp.textContent = "";
    if (timer) { clearInterval(timer); timer = null; }
  });
  el("export").addEventListener("click", function () {
    if (!last) { note("warn", "请先加载数据。"); return; }
    var blob = new Blob([JSON.stringify(last, null, 2)], { type: "application/json" });
    var url = URL.createObjectURL(blob), a = document.createElement("a");
    a.href = url; a.download = "codex-weekly-usd-" + new Date().toISOString().slice(0, 10) + ".json";
    a.click(); URL.revokeObjectURL(url);
  });

  if (keyBox.value) load();
  timer = setInterval(function () { if (keyBox.value.trim()) load(); }, 60000);
})();
</script>
</body>
</html>`
