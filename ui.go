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
// data: it ships an empty shell that asks the operator for the management key
// and then calls the authenticated /data route itself.
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
<title>Codex 额度美元估算</title>
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
  h3{font-size:13px;margin:0 0 8px;color:var(--muted);font-weight:600}
  .sub{color:var(--muted);font-size:13px}
  .bar{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin:16px 0}
  input,button,select{font:inherit;padding:7px 11px;border-radius:7px;
       border:1px solid var(--line);background:var(--panel);color:var(--ink)}
  input{min-width:280px}
  button{cursor:pointer;border-color:var(--accent);color:var(--accent)}
  button:hover{background:var(--accent);color:#fff}
  button.ghost{border-color:var(--line);color:var(--muted)}
  button.ghost:hover{background:var(--line);color:var(--ink)}
  .cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px;margin-bottom:10px}
  .card{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:12px 14px}
  .card .k{color:var(--muted);font-size:12px}
  .card .v{font-size:21px;font-weight:650;margin-top:3px;font-variant-numeric:tabular-nums}
  .card .n{font-size:12px;color:var(--muted);font-weight:400}
  .box{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin-bottom:16px}
  .wrap{overflow-x:auto;background:var(--panel);border:1px solid var(--line);border-radius:10px}
  table{border-collapse:collapse;width:100%;min-width:1100px}
  th,td{padding:9px 12px;text-align:right;border-bottom:1px solid var(--line);white-space:nowrap;vertical-align:top}
  th{font-size:12px;color:var(--muted);font-weight:600;position:sticky;top:0;background:var(--panel);z-index:1}
  th:first-child,td:first-child{text-align:left}
  tbody tr:last-child>td{border-bottom:none}
  tbody tr.main:hover{background:var(--panel2)}
  td.num{font-variant-numeric:tabular-nums}
  .name{font-weight:600}
  .sub2{font-size:12px;color:var(--muted);font-weight:400}
  .wcell{display:flex;flex-direction:column;gap:3px;min-width:140px;align-items:flex-end}
  .meter{position:relative;height:7px;border-radius:4px;background:var(--line);overflow:hidden;width:132px}
  .meter>i{position:absolute;inset:0 auto 0 0;display:block;border-radius:4px}
  .mlabel{display:flex;justify-content:space-between;font-size:11px;color:var(--muted);width:132px}
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
  .wgrid{display:grid;grid-template-columns:repeat(auto-fit,minmax(330px,1fr));gap:18px;margin-bottom:16px}
  .wbox{border:1px solid var(--line);border-radius:9px;padding:12px 14px;background:var(--panel)}
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
<h1>Codex 额度美元估算</h1>
<div class="sub">按 OpenAI 官方 API 价目，推算每个凭据的每个额度窗口值多少美元</div>

<div class="bar">
  <input id="key" type="password" placeholder="管理密钥" autocomplete="off" spellcheck="false">
  <button id="load">加载</button>
  <button class="ghost" id="forget">忘记密钥</button>
  <button class="ghost" id="export">导出 JSON</button>
  <select id="sort">
    <option value="quota">按最长窗口额度排序</option>
    <option value="remain">按剩余排序</option>
    <option value="used">按已用比例排序</option>
    <option value="pace">按消耗节奏排序</option>
    <option value="name">按名称排序</option>
  </select>
  <span id="stamp" class="sub"></span>
</div>

<div id="alerts"></div>
<div id="wtotals"></div>
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
    <thead><tr id="head"></tr></thead>
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
  var wrap = el("wrap"), stamp = el("stamp"), foot = el("foot"), head = el("head");
  var chartbox = el("chartbox"), chart = el("chart"), pricebox = el("pricebox"), wtotals = el("wtotals");
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
    if (d > 0) return d + " 天 " + (h % 24) + " 小时";
    if (h > 0) return h + " 小时 " + Math.floor((s % 3600) / 60) + " 分";
    return Math.floor(s / 60) + " 分";
  }
  function wname(w) {
    var m = w.minutes;
    if (m % 1440 === 0) return (m / 1440) + " 天";
    if (m % 60 === 0) return (m / 60) + " 小时";
    return m + " 分";
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

  // Quota consumed versus clock elapsed. Above 1 means the window will run out
  // early at the current rate.
  function paceTag(w) {
    var r = w.pace_ratio;
    if (r === undefined || r === null) return "";
    var cls = r > 1.15 ? "bad" : (r < 0.85 ? "ok" : "");
    var word = r > 1.15 ? "偏快" : (r < 0.85 ? "宽裕" : "正常");
    return "<span class='tag " + cls + "'>" + word + " ×" + r.toFixed(2) + "</span>";
  }

  function windowCell(w) {
    if (!w) return "<td class='num'><span class='tag'>无</span></td>";
    var e = w.estimate || {};
    var waiting = e.method === "none";
    var p = w.used_percent || 0;
    var s = "<td><div class='wcell'>";
    s += "<div class='mlabel'><span>" + pct(p) + "</span><span>" +
         (w.time_progress_percent !== undefined ? "时间 " + pct(w.time_progress_percent) : "") + "</span></div>";
    s += "<div class='meter'><i style='width:" + Math.min(100, p) + "%;background:" + heat(p) + "'></i></div>";
    if (w.time_progress_percent !== undefined) {
      s += "<div class='meter'><i style='width:" + Math.min(100, w.time_progress_percent) +
           "%;background:var(--muted);opacity:.5'></i></div>";
    }
    s += "<div class='sub2'>" + (waiting ? "<span class='tag'>观测中</span>"
         : "剩 <b>" + usd(e.remaining_usd) + "</b> / " + usd(e.quota_usd)) + "</div>";
    s += "<div class='sub2'>" + paceTag(w) + " <span class='tag'>" + dur(w.reset_in_seconds) + "后重置</span></div>";
    return s + "</div></td>";
  }

  function windowDetail(w) {
    var e = w.estimate || {};
    var t = w.tokens || {};
    var kv = "<div class='kv'>" +
      row2("窗口实测消耗", usd(w.usd_observed)) +
      row2("已归属消耗", usd(e.attributed_usd) + "<span class='sub2'> / 挂账 " +
           usd((w.usd_observed || 0) - (e.attributed_usd || 0)) + "</span>") +
      row2("缓存省下", usd(w.saved_usd)) +
      row2("请求 / 失败", (w.requests || 0) + " / " + (w.failed || 0) +
           (w.failure_rate ? " (" + pct(w.failure_rate) + ")" : "")) +
      row2("均价每次", usd(w.avg_usd_per_request)) +
      row2("输入 / 输出", tok(t.InputTokens) + " / " + tok(t.OutputTokens)) +
      row2("缓存读取", tok(t.CacheReadTokens) + (w.cache_hit_rate !== undefined ? " (" + pct(w.cache_hit_rate) + ")" : "")) +
      row2("窗口法估算", e.quota_usd_by_window ? usd(e.quota_usd_by_window) : "不可用") +
      row2("步进法估算", e.quota_usd_by_delta ? usd(e.quota_usd_by_delta) : "不可用") +
      row2("校准样本", (e.samples || 0) + " 条 / 覆盖 " + pct(e.evidence_percent)) +
      row2("整窗覆盖", w.full_coverage ? "是" : "否（中途接管）") +
      row2("已观察周期", (w.cycles || 0) + " 次" +
           (w.granted_resets ? "，其中 <b>" + w.granted_resets + " 次周期内重置</b>" : "")) +
      row2("外部消耗", w.unexplained_percent ? "<b>" + pct(w.unexplained_percent) + "</b>（不在本插件账上）" : "无") +
      row2("跑满预警", w.will_exhaust_before_reset === true ? "<span class='tag bad'>会提前用完</span>" :
           (w.will_exhaust_before_reset === false ? "<span class='tag ok'>够用到重置</span>" : "—")) +
      row2("日均消耗", w.burn_usd_per_day === undefined ? "—" : usd(w.burn_usd_per_day)) +
      "</div>";

    var m = "<table><thead><tr><th>模型</th><th>请求</th><th>失败</th><th>输入</th><th>输出</th>" +
            "<th>缓存命中</th><th>单价(入/出)</th><th>均价/次</th><th>金额</th></tr></thead><tbody>";
    (w.by_model || []).forEach(function (x) {
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

    return "<div class='wbox'><h3>" + wname(w) + " 窗口 · " +
           (e.method === "none" ? "<span class='tag'>观测中</span>"
             : "<span class='tag " + esc(e.confidence) + "'>" + confWord(e.confidence) + "</span> " +
               "<span class='tag'>" + methodWord(e.method) + "</span>") + "</h3>" +
           kv + "<div style='margin-top:10px'>" + m + "</div></div>";
  }
  function row2(k, v) { return "<div class='k'>" + esc(k) + "</div><div class='v'>" + v + "</div>"; }

  function detailRow(a) {
    var boxes = (a.windows || []).map(windowDetail).join("");
    var curveW = (a.windows || []).length ? a.windows[a.windows.length - 1].minutes : 0;
    var curve = drawQuotaCurve(a.series, curveW, 300, 96);
    var charts =
      "<div style='margin-bottom:12px'>" + drawChart(a.series, 340, 80, false) +
        "<div class='chartcap'><i class='sw bar'></i>每小时消耗 " +
        "<i class='sw ln' style='margin-left:8px'></i>累计</div></div>" +
      (curve
        ? "<div>" + curve + "<div class='chartcap'><i class='sw ln warn'></i>额度百分比（" +
          (curveW ? wname({minutes: curveW}) : "") + "窗口） " +
          "<i class='sw ln dash' style='margin-left:8px'></i>匀速参考线" +
          "<br>曲线高过虚线 = 照此速度会在重置前用完</div></div>"
        : "");

    return "<div class='wgrid'>" + boxes + "</div>" + charts;
  }

  function sortAccounts(list) {
    var mode = el("sort").value;
    function longest(a) { return a.longest || {}; }
    function est(a) { return (longest(a).estimate) || {}; }
    var c = {
      quota: function (a, b) { return (est(b).quota_usd || 0) - (est(a).quota_usd || 0); },
      remain: function (a, b) { return (est(b).remaining_usd || 0) - (est(a).remaining_usd || 0); },
      used: function (a, b) { return ((b.binding || {}).used_percent || 0) - ((a.binding || {}).used_percent || 0); },
      pace: function (a, b) { return ((b.binding || {}).pace_ratio || 0) - ((a.binding || {}).pace_ratio || 0); },
      name: function (a, b) { return String(a.label).localeCompare(String(b.label)); }
    }[mode];
    return list.slice().sort(c);
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

  function render(data) {
    last = data;
    alerts.innerHTML = "";
    (data.warnings || []).forEach(function (w) { note("warn", w); });

    var accounts = data.accounts || [];
    var stale = accounts.filter(function (a) { return a.stale; });
    if (stale.length) {
      note("warn", stale.length + " 个凭据的额度读数已经过期（长时间没有流量经过），下面显示的是最后一次观测值，不代表现在。");
    }
    var waiting = accounts.filter(function (a) {
      return !a.longest || !a.longest.estimate || a.longest.estimate.method === "none";
    });
    if (waiting.length) {
      note("warn", waiting.length + " / " + accounts.length +
        " 个凭据还没有估算值。额度只有在「插件亲眼看到百分比至少走动 1 点」之后才能定价，正常使用中会自动补上。");
    }

    // One summary block per window length: the 5-hour and the weekly limit are
    // separate quotas, and the same spend counts against both.
    var wt = data.window_totals || [];
    wtotals.innerHTML = wt.map(function (t) {
      return "<div class='box'><h2>" + wname(t) + " 窗口合计</h2><div class='cards' style='margin:0'>" +
        card("凭据数", t.credentials, t.estimated + " 个已定价" + (t.at_risk ? " · " + t.at_risk + " 个逼近上限" : "")) +
        card("额度总值", usd(t.quota_usd)) +
        card("已用", usd(t.spent_usd)) +
        card("剩余", usd(t.remaining_usd)) +
        "</div></div>";
    }).join("");

    var t = data.totals || {};
    cards.innerHTML =
      card("凭据数", t.credentials) +
      card("实测消耗", usd(t.window_usd_observed), "最长窗口内，插件亲眼记账部分") +
      card("缓存省下", usd(t.cache_saved_usd), "对比全价输入") +
      card("请求数", (t.requests || 0).toLocaleString("zh-CN"), (t.failed || 0) + " 次失败");
    cards.hidden = false;

    chart.innerHTML = drawChart(data.fleet_series, 1000, 165, true);
    chartbox.hidden = false;

    // Columns follow whatever window lengths upstream is actually reporting.
    var lengths = [];
    wt.forEach(function (x) { lengths.push(x.minutes); });
    if (!lengths.length) {
      accounts.forEach(function (a) {
        (a.windows || []).forEach(function (w) {
          if (lengths.indexOf(w.minutes) < 0) lengths.push(w.minutes);
        });
      });
      lengths.sort(function (a, b) { return a - b; });
    }
    head.innerHTML = "<th>凭据</th><th>套餐</th>" +
      lengths.map(function (m) { return "<th>" + wname({minutes: m}) + " 额度</th>"; }).join("") +
      "<th>状态</th>";

    rows.innerHTML = "";
    sortAccounts(accounts).forEach(function (a, i) {
      var byLen = {};
      (a.windows || []).forEach(function (w) { byLen[w.minutes] = w; });

      var tr = document.createElement("tr");
      tr.className = "main" + (a.disabled ? " off" : "");
      var tags = "";
      if (a.disabled) tags += " <span class='tag'>已停用</span>";
      if (a.unavailable) tags += " <span class='tag bad'>不可用</span>";
      if (a.stale) tags += " <span class='tag warn'>读数陈旧</span>";
      if (a.limit_reached_type) tags += " <span class='tag bad'>" + esc(a.limit_reached_type) + "</span>";

      var html =
        "<td><span class='toggle' data-i='" + i + "'>▸</span> <span class='name'>" +
          esc(a.label || a.auth_id) + "</span>" + tags +
          "<div class='sub2'>" + (a.total_requests || 0) + " 次累计 · " + usd(a.total_usd) +
          (a.observed_age_seconds !== undefined ? " · " + dur(a.observed_age_seconds) + "前观测" : "") +
          "</div></td>" +
        "<td>" + esc(a.plan_type || "—") +
          (a.active_limit ? "<div class='sub2'>" + esc(a.active_limit) + "</div>" : "") + "</td>";
      lengths.forEach(function (m) { html += windowCell(byLen[m]); });

      var le = (a.longest && a.longest.estimate) || {};
      html += "<td>" + (le.method === "none" || !le.method
        ? "<span class='tag'>待百分比走动 1%</span>"
        : "<span class='tag " + esc(le.confidence) + "'>" + confWord(le.confidence) + "</span> " +
          "<span class='tag'>" + methodWord(le.method) + " " + pct(le.evidence_percent) + "</span>") + "</td>";
      tr.innerHTML = html;
      rows.appendChild(tr);

      var dr = document.createElement("tr");
      dr.className = "detail"; dr.hidden = true;
      dr.innerHTML = "<td colspan='" + (lengths.length + 3) + "'>" + detailRow(a) + "</td>";
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
      "<b>窗口按长度识别</b>，不按上游的 primary/secondary 标签——这两个标签的含义变过一次" +
      "（5 小时限额回归后，primary 从周窗口变成了 5 小时窗口）。同一笔花费同时计入所有窗口，" +
      "所以每个窗口各自独立估算。<br>" +
      "<b>窗口法</b>＝整周期实测消耗 ÷ 整周期百分比，只在插件看到周期从 0% 开始时可用；" +
      "<b>步进法</b>＝Σ两次读数间的消耗 ÷ Σ百分比步进，中途安装也能收敛。" +
      "校准样本有数量与时效上限，因此套餐变化后估算会跟着变，不会被旧数据永久拖住。<br>" +
      "<b>节奏</b>＝额度消耗比例 ÷ 窗口时间流逝比例，大于 1 表示照此速度会在重置前用完。" +
      "缓存读取与推理 token 分别是输入、输出的子集，不重复计费。";
    loadPrices();
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
      wtotals.innerHTML = "";
    });
  }

  el("load").addEventListener("click", load);
  el("sort").addEventListener("change", function () { if (last) render(last); });
  keyBox.addEventListener("keydown", function (e) { if (e.key === "Enter") load(); });
  el("forget").addEventListener("click", function () {
    localStorage.removeItem(STORE); keyBox.value = ""; last = null;
    cards.hidden = wrap.hidden = chartbox.hidden = pricebox.hidden = true;
    alerts.innerHTML = ""; stamp.textContent = ""; wtotals.innerHTML = "";
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
