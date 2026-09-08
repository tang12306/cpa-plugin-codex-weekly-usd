package main

import (
	"encoding/json"
	"strings"
	"time"
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
	case strings.HasSuffix(path, "/rotate"):
		// Evaluate the pool now instead of waiting for the next check. POST
		// only: this one can change which credentials are enabled, and a route
		// that acts must not be reachable by following a link. It sits under
		// /v0/management, so it carries the management key like everything else
		// that is not the panel shell.
		if !strings.EqualFold(req.Method, "POST") {
			return jsonResponse(405, "application/json; charset=utf-8",
				[]byte(`{"error":"POST required"}`))
		}
		cfg := a.rot.config()
		board := func(c RotatorConfig) []candidate {
			return a.rot.board(a.authMetadata(), c, time.Now())
		}
		if !cfg.Enabled {
			return marshalResponse(map[string]any{
				"error": "rotator is disabled", "rotator": a.rot.Report(cfg, board(cfg))})
		}
		a.rot.tick(true)
		cfg = a.rot.config()
		return marshalResponse(a.rot.Report(cfg, board(cfg)))

	case strings.HasSuffix(path, "/refresh"):
		// Re-read every credential now. Nothing automatic does this, and
		// nothing should: the rotator asks only when it has a reason to, and a
		// quota reset granted out of band gives it none. This is the operator
		// saying the stored numbers are wrong. POST, because it makes upstream
		// requests and rewrites what the plugin believes.
		if !strings.EqualFold(req.Method, "POST") {
			return jsonResponse(405, "application/json; charset=utf-8",
				[]byte(`{"error":"POST required"}`))
		}
		now := time.Now()
		entries := a.authMetadata()
		cfg := a.rot.config()
		out := a.rot.refreshAll(entries, cfg, now)
		out["rotator"] = a.rot.Report(cfg, a.rot.board(entries, cfg, now))
		return marshalResponse(out)
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
<title>Codex Quota USD</title>
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
  .tag.medium,.tag.warn{color:var(--warn);border-color:var(--warn)}
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
  .mwrap{overflow-x:auto}
  .mwrap table{min-width:0}
  .mwrap th,.mwrap td{padding:7px 14px 7px 0}
  .mwrap td:last-child{white-space:normal;min-width:280px}
  .chip{display:inline-block;margin:2px 5px 2px 0}
</style>
</head>
<body>
<h1 id="t-title"></h1>
<div class="sub" id="t-sub"></div>

<div class="bar">
  <input id="key" type="password" autocomplete="off" spellcheck="false">
  <button id="load"></button>
  <button class="ghost" id="forget"></button>
  <button class="ghost" id="export"></button>
  <select id="sort">
    <option value="quota"></option>
    <option value="remain"></option>
    <option value="used"></option>
    <option value="pace"></option>
    <option value="name"></option>
  </select>
  <select id="lang">
    <option value="zh">中文</option>
    <option value="en">English</option>
  </select>
  <span id="stamp" class="sub"></span>
</div>

<div id="alerts"></div>
<div id="modelbox" class="box" hidden>
  <h2 id="t-models"></h2>
  <div class="mwrap"><table id="modeltable"></table></div>
  <div class="chartcap" id="t-modelhint"></div>
</div>
<div id="rotbox" class="box" hidden>
  <h2><span id="t-rot"></span> <span class="legend" id="rot-status"></span></h2>
  <div class="mwrap"><table id="rottable"></table></div>
  <div class="chartcap" id="t-rothint"></div>
  <details id="rotlogbox" hidden>
    <summary id="t-rotlog"></summary>
    <div class="mwrap"><table id="rotlog"></table></div>
  </details>
</div>
<div id="wtotals"></div>
<div id="cards" class="cards" hidden></div>
<div id="chartbox" class="box" hidden>
  <h2><span id="t-chart"></span>
    <span class="legend"><i class="sw bar"></i><span id="t-lg1"></span></span>
    <span class="legend"><i class="sw ln good"></i><span id="t-lg2"></span></span>
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
    <summary id="t-prices"></summary>
    <div id="prices"></div>
  </details>
</div>
<div id="foot" class="foot"></div>

<script>
(function () {
  var STORE = "cwu.key", LANGKEY = "cwu.lang";

  // Every user-facing string lives here. Backend warnings arrive as structured
  // codes rather than prose for the same reason: the panel, not the plugin,
  // decides what language the operator reads.
  var I18N = {
    zh: {
      title: "Codex 额度美元估算",
      sub: "按 OpenAI 官方 API 价目，推算每个凭据的每个额度窗口值多少美元",
      keyPlaceholder: "管理密钥", load: "加载", forget: "忘记密钥", exportJson: "导出 JSON",
      sortQuota: "按最长窗口额度排序", sortRemain: "按剩余排序", sortUsed: "按已用比例排序",
      sortPace: "按消耗节奏排序", sortName: "按名称排序",
      chartTitle: "全部凭据 · 用量走势", legendBars: "每小时消耗（左轴）", legendLine: "近 7 天消耗（右轴）",
      pricesTitle: "当前生效价目表（美元 / 百万 token）",
      modelsTitle: "模型可用性",
      modelsHint: "CPA 的冷却是按「凭据 × 模型」的：上游对每个模型有独立额度，所以一个凭据完全可以" +
        "「astra 已耗尽、其它模型照跑」。某个模型的凭据全部冷却时，外部表现就是「只有这一个模型用不了」，" +
        "而凭据状态和渠道状态都会显示正常。冷却到期时间取自上游返回的 reset，与代理实际使用的是同一个。",
      mhModel: "模型", mhState: "状态", mhCap: "可用 / 凭据", mhBack: "最快恢复",
      staleCycle: "窗口已重置，读数待确认",
      mhTraffic: "请求 / 失败", mhCreds: "各凭据状态", mhNone: "还没有模型可用性数据。",
      msDown: "全部冷却", msDegraded: "部分冷却", msOK: "正常", msSingle: "单点凭据",
      csCooling: "冷却", csRecovering: "待验证", csOK: "正常", csDisabled: "已停用",
      csRejected: "凭据失效", rmRemoved: "{0} 个凭据已删除，不再显示：{1}",
      mhEstimated: "估计", mhBlocks: "累计 {0} 次冷却", mhReason: "原因：{0}",
      rotTitle: "凭据轮换",
      rotHint: "轮换器维持固定数量的凭据处于启用状态，在成员快耗尽之前把它换掉。" +
        "代理的 fill-first 会在一个凭据被拒时于同一请求内改用下一个，所以只要替补已经在池子里，" +
        "换人对调用方是无感的。<b>判定用的是实探读数</b>——已停用的凭据仍可能被别处消耗，存档数据只能算线索。" +
        "「可撑」按剩余美元 ÷ 当前消耗速度估算；百分比在不同套餐上不等值，所以排序不看百分比。" +
        "同时合格时优先用<b>更早作废</b>的额度。",
      rotOff: "未启用", rotDry: "空跑（只决策不写入）", rotOn: "运行中",
      rotPool: "池 {0} 个", rotFloor: "阈值 {0}%", rotSwept: "{0}前评估",
      rotChanges: "今日 {0}/{1} 次改动", rotProbes: "今日探测 {0} 次",
      rotRefused: "{0} 个凭据已被上游拒绝",
      rtCred: "凭据", rtRole: "角色", rtHead: "余量", rtWindow: "绑定窗口",
      rtReset: "重置", rtServe: "可撑", rtVerdict: "判定",
      rtActive: "启用中", rtStandby: "备用",
      rtOK: "合格", rtSafe: "合格 · 可撑住",
      skDead: "token 失效", skExhausted: "已耗尽", skFloor: "低于阈值",
      skProbe: "探测失败", skExcluded: "配置排除", skRecent: "刚轮换过",
      rotLogTitle: "轮换记录", rlAt: "时间", rlAction: "动作", rlCred: "凭据", rlWhy: "原因",
      rlEnable: "启用", rlDisable: "停用", rlDry: "空跑",
      warnRotStuck: "轮换器判定**池子里已经没有任何凭据能服务**，且没有备用凭据合格，" +
        "因此没有动任何东西——现有凭据保持启用。请补充可用账号，或重新登录已失效的号。",
      warnRotStuckEta: "轮换器判定**池子里已经没有任何凭据能服务**，且暂时没有合格备用。" +
        "最快 {0} 后会有候选恢复额度。",
      warnRotDegraded: "轮换器没能把池子补满（{0}/{1} 个仍在服务），暂时没有合格备用凭据。" +
        "**当前请求不受影响**——代理会在被拒时自动改用池中还能用的那个。",
      warnRotDegradedEta: "轮换器没能把池子补满（{0}/{1} 个仍在服务），暂时没有合格备用。" +
        "**当前请求不受影响**；最快 {2} 后会有候选恢复额度。",
      warnRotDry: "轮换器处于空跑模式：它会照常判断并记录，但不会真的改写凭据。确认记录无误后把 dry_run 关掉。",
      mhWindowFull: "{0} 窗口已打满", mhLastOK: "最后成功于 {0}前",
      updatedAt: "更新于 {0}",
      needKey: "请先填入管理密钥。", keyRejected: "管理密钥被拒绝。", reqFailed: "请求失败：HTTP {0}",
      loadFirst: "请先加载数据。",
      warnStale: "{0} 个凭据的额度读数已经过期（长时间没有流量经过），下面显示的是最后一次观测值，不代表现在。",
      warnWaiting: "{0} / {1} 个凭据还没有估算值。额度只有在「插件亲眼看到百分比至少走动 1 点」之后才能定价，正常使用中会自动补上。",
      warnExternal: "{0} 的 {1} 窗口有 {2} 的额度被消耗但不在本插件账上，可能有其它客户端在共用该凭据",
      warnUnpriced: "{0} 没有公开价目（凭据 {1}），其花费未计入估算",
      warnPrice: "价目刷新失败：{0}",
      warnModelDown: "模型 {0} 当前没有可用凭据：{1} 个全部在冷却中，最快 {2} 后恢复。" +
        "这类故障只打这一个模型，凭据状态与渠道状态都会显示正常。",
      warnModelDownNoEta: "模型 {0} 当前没有可用凭据：{1} 个全部在冷却中。",
      warnModelSingle: "模型 {0} 只有 1 个可用凭据（{1}），它一旦撞 429，该模型就整体不可用。",
      warnModelSingleOff: "模型 {0} 只有 1 个可用凭据（{1}），另有 {2} 个已停用。" +
        "它一旦撞 429，该模型就整体不可用。",
      totalsTitle: "{0} 窗口合计",
      cCreds: "凭据数", cQuota: "额度总值", cSpent: "已用", cRemain: "剩余",
      cPriced: "{0} 个已定价", cAtRisk: "{0} 个逼近上限",
      cObserved: "实测消耗", cObservedN: "最长窗口内，插件亲眼记账部分",
      cSaved: "缓存省下", cSavedN: "对比全价输入",
      cRequests: "请求数", cFailedN: "{0} 次失败",
      thCred: "凭据", thPlan: "套餐", thQuota: "{0} 额度", thStatus: "状态",
      tagDisabled: "已停用", tagUnavailable: "不可用", tagStale: "读数陈旧",
      rowTotals: "{0} 次累计 · {1}", rowObserved: "{0}前观测",
      watching: "观测中", needTick: "待百分比走动 1%",
      left: "剩", resetsIn: "{0}后重置",
      paceFast: "偏快", paceSlow: "宽裕", paceNormal: "正常",
      confHigh: "高置信", confMedium: "中置信", confLow: "低置信", confNone: "无估算",
      mWindow: "窗口法", mDelta: "步进法",
      wTitle: "{0} 窗口",
      dObserved: "窗口实测消耗", dAttributed: "已归属消耗", dPending: " / 挂账 {0}",
      dSaved: "缓存省下", dReqFail: "请求 / 失败", dAvg: "均价每次",
      dInOut: "输入 / 输出", dCacheRead: "缓存读取",
      dByWindow: "窗口法估算", dByDelta: "步进法估算", dNA: "不可用",
      dSamples: "校准样本", dSamplesV: "{0} 条 / 覆盖 {1}",
      dCoverage: "整窗覆盖", dYes: "是", dNoMid: "否（中途接管）",
      dCycles: "已观察周期", dCyclesV: "{0} 次", dGranted: "，其中 {0} 次周期内重置",
      dExternal: "外部消耗", dExternalV: "{0}（不在本插件账上）", dNone: "无",
      dExhaust: "跑满预警", dWillExhaust: "会提前用完", dLasts: "够用到重置",
      dPerDay: "日均消耗",
      mtModel: "模型", mtReq: "请求", mtFail: "失败", mtIn: "输入", mtOut: "输出",
      mtCache: "缓存命中", mtPrice: "单价(入/出)", mtAvg: "均价/次", mtUsd: "金额",
      mtNoPrice: "无价目", mtReasoning: "(含推理 {0})",
      capHourly: "每小时消耗", capCumulative: "近 7 天",
      capCurve: "额度百分比（{0}窗口）", capRef: "匀速参考线",
      capCurveHint: "曲线高过虚线 = 照此速度会在重置前用完",
      pModel: "模型", pIn: "输入", pOut: "输出", pCacheR: "缓存读", pCacheW: "缓存写",
      pOverridden: "已覆盖",
      footPrices: "价目来源：{0}", footVia: "（{0}）", footFetched: "，同步于 {0}",
      viaHost: "经 CPA 代理", viaDirect: "直连",
      footBody: "<b>窗口按长度识别</b>，不按上游的 primary/secondary 标签——这两个标签的含义变过一次" +
        "（5 小时限额回归后，primary 从周窗口变成了 5 小时窗口）。同一笔花费同时计入所有窗口，" +
        "所以每个窗口各自独立估算。<br>" +
        "<b>窗口法</b>＝整周期实测消耗 ÷ 整周期百分比，只在插件看到周期从 0% 开始时可用；" +
        "<b>步进法</b>＝Σ两次读数间的消耗 ÷ Σ百分比步进，中途安装也能收敛。" +
        "校准样本有数量与时效上限，因此套餐变化后估算会跟着变，不会被旧数据永久拖住。<br>" +
        "<b>节奏</b>＝额度消耗比例 ÷ 窗口时间流逝比例，大于 1 表示照此速度会在重置前用完。" +
        "缓存读取与推理 token 分别是输入、输出的子集，不重复计费。",
      noData: "暂无数据", thisHour: "本小时", hoursAgo: "小时前", now: "现在",
      total: "近 7 天", reqs: "次",
      unitDay: "天", unitHour: "小时", unitMin: "分",
      wDays: "{0} 天", wHours: "{0} 小时", wMins: "{0} 分",
      locale: "zh-CN"
    },
    en: {
      title: "Codex Quota USD",
      sub: "What each credential's quota window is worth at public OpenAI API pricing",
      keyPlaceholder: "Management key", load: "Load", forget: "Forget key", exportJson: "Export JSON",
      sortQuota: "Sort by longest-window quota", sortRemain: "Sort by remaining", sortUsed: "Sort by used %",
      sortPace: "Sort by burn pace", sortName: "Sort by name",
      chartTitle: "All credentials · usage", legendBars: "Hourly spend (left axis)", legendLine: "Trailing 7 days (right axis)",
      pricesTitle: "Active rate card (USD per 1M tokens)",
      modelsTitle: "Model availability",
      modelsHint: "The proxy cools a credential down per model: upstream keeps a separate allowance " +
        "for each one, so a credential can be out of astra while every other model keeps working. " +
        "When every credential for a model is cooling at once it looks from the outside like just " +
        "that one model is broken, while the credential and channel status both read as healthy. " +
        "Deadlines come from the reset upstream reported, the same one the proxy cools down to.",
      mhModel: "Model", mhState: "State", mhCap: "Available / credentials", mhBack: "First back",
      staleCycle: "window has reset; reading not yet confirmed",
      mhTraffic: "Requests / failed", mhCreds: "Per credential", mhNone: "No model availability data yet.",
      msDown: "all cooling", msDegraded: "partly cooling", msOK: "healthy", msSingle: "single credential",
      csCooling: "cooling", csRecovering: "unverified", csOK: "ok", csDisabled: "disabled",
      csRejected: "credential rejected", rmRemoved: "{0} deleted credential(s) not shown: {1}",
      mhEstimated: "estimated", mhBlocks: "{0} lockouts so far", mhReason: "reason: {0}",
      rotTitle: "Credential rotation",
      rotHint: "The rotator holds a fixed number of credentials enabled and replaces a member " +
        "before it runs out. The proxy's fill-first selector retries a refused request on the " +
        "next enabled credential, so a handover is invisible to callers as long as the " +
        "replacement is already in the pool. <b>Decisions use a live reading</b>: a disabled " +
        "credential can still be spent elsewhere, so stored percentages are evidence rather " +
        "than fact. \"Covers\" is remaining dollars divided by the current spend rate - a " +
        "percentage point is worth different money on different plans, so ranking never uses " +
        "percentages. Between candidates that both qualify, the allowance that <b>expires " +
        "soonest</b> is spent first.",
      rotOff: "off", rotDry: "dry run (decides, writes nothing)", rotOn: "running",
      rotPool: "pool of {0}", rotFloor: "floor {0}%", rotSwept: "evaluated {0} ago",
      rotChanges: "{0}/{1} changes today", rotProbes: "{0} probes today",
      rotRefused: "{0} credential(s) refused upstream",
      rtCred: "Credential", rtRole: "Role", rtHead: "Headroom", rtWindow: "Binding window",
      rtReset: "Resets", rtServe: "Covers", rtVerdict: "Verdict",
      rtActive: "enabled", rtStandby: "standby",
      rtOK: "eligible", rtSafe: "eligible · covers the horizon",
      skDead: "token rejected", skExhausted: "exhausted", skFloor: "below floor",
      skProbe: "probe failed", skExcluded: "excluded", skRecent: "just rotated",
      rotLogTitle: "Rotation log", rlAt: "When", rlAction: "Action", rlCred: "Credential", rlWhy: "Why",
      rlEnable: "enable", rlDisable: "disable", rlDry: "dry run",
      warnRotStuck: "**Nothing in the pool can serve** and no standby qualified, so the rotator " +
        "changed nothing - whatever is enabled stays enabled. Add a usable account, or log back " +
        "in to the rejected ones.",
      warnRotStuckEta: "**Nothing in the pool can serve** and no standby qualifies yet. The " +
        "soonest candidate gets its allowance back in {0}.",
      warnRotDegraded: "The rotator could not fill the pool ({0}/{1} still serving) and no standby " +
        "qualifies. **Requests are unaffected** - the proxy moves to the member that still works " +
        "when one is refused.",
      warnRotDegradedEta: "The rotator could not fill the pool ({0}/{1} still serving) and no " +
        "standby qualifies yet. **Requests are unaffected**; the soonest candidate recovers in {2}.",
      warnRotDry: "The rotator is in dry run: it decides and records as usual but never rewrites a " +
        "credential. Turn dry_run off once the log looks right.",
      mhWindowFull: "{0} window is full", mhLastOK: "last success {0} ago",
      updatedAt: "updated {0}",
      needKey: "Enter the management key first.", keyRejected: "Management key rejected.",
      reqFailed: "Request failed: HTTP {0}", loadFirst: "Load the data first.",
      warnStale: "{0} credential(s) have stale quota readings (no traffic for a while). The figures below are the last observation, not the present.",
      warnWaiting: "{0} of {1} credentials have no estimate yet. A quota can only be priced once its used-percentage has been watched moving at least one point, so these fill in during normal use.",
      warnExternal: "{1} window of {0} shows {2} consumed that this plugin did not bill — another client may be sharing this credential",
      warnUnpriced: "No public price for {0} (credential {1}); its spend is missing from the estimate",
      warnPrice: "Price refresh is failing: {0}",
      warnModelDown: "Model {0} has no credential left: all {1} are cooling down, the first back in {2}. " +
        "An outage like this hits only this model, while credential and channel status both look fine.",
      warnModelDownNoEta: "Model {0} has no credential left: all {1} are cooling down.",
      warnModelSingle: "Model {0} runs on a single credential ({1}). Its next 429 takes the whole model down.",
      warnModelSingleOff: "Model {0} runs on a single credential ({1}), with {2} more disabled. " +
        "Its next 429 takes the whole model down.",
      totalsTitle: "{0} window totals",
      cCreds: "Credentials", cQuota: "Quota value", cSpent: "Spent", cRemain: "Remaining",
      cPriced: "{0} priced", cAtRisk: "{0} near the limit",
      cObserved: "Observed spend", cObservedN: "What the plugin billed itself, longest window",
      cSaved: "Cache savings", cSavedN: "vs paying full input price",
      cRequests: "Requests", cFailedN: "{0} failed",
      thCred: "Credential", thPlan: "Plan", thQuota: "{0} quota", thStatus: "Status",
      tagDisabled: "disabled", tagUnavailable: "unavailable", tagStale: "stale reading",
      rowTotals: "{0} total · {1}", rowObserved: "observed {0} ago",
      watching: "watching", needTick: "needs 1% of movement",
      left: "left", resetsIn: "resets in {0}",
      paceFast: "fast", paceSlow: "comfortable", paceNormal: "normal",
      confHigh: "high", confMedium: "medium", confLow: "low", confNone: "none",
      mWindow: "window", mDelta: "delta",
      wTitle: "{0} window",
      dObserved: "Observed spend this cycle", dAttributed: "Attributed", dPending: " / pending {0}",
      dSaved: "Cache savings", dReqFail: "Requests / failed", dAvg: "Avg per request",
      dInOut: "Input / output", dCacheRead: "Cache reads",
      dByWindow: "Window estimate", dByDelta: "Delta estimate", dNA: "n/a",
      dSamples: "Calibration", dSamplesV: "{0} samples / {1} covered",
      dCoverage: "Full-cycle coverage", dYes: "yes", dNoMid: "no (joined mid-cycle)",
      dCycles: "Cycles observed", dCyclesV: "{0}", dGranted: ", {0} granted mid-cycle",
      dExternal: "External usage", dExternalV: "{0} (not on this ledger)", dNone: "none",
      dExhaust: "Exhaustion", dWillExhaust: "runs out early", dLasts: "lasts to reset",
      dPerDay: "Per day",
      mtModel: "Model", mtReq: "Req", mtFail: "Failed", mtIn: "Input", mtOut: "Output",
      mtCache: "Cache hit", mtPrice: "Rate (in/out)", mtAvg: "Avg/req", mtUsd: "Spend",
      mtNoPrice: "no price", mtReasoning: "(incl. reasoning {0})",
      capHourly: "hourly spend", capCumulative: "trailing 7d",
      capCurve: "quota % ({0} window)", capRef: "constant-rate reference",
      capCurveHint: "above the dashed line = runs out before reset at this rate",
      pModel: "Model", pIn: "Input", pOut: "Output", pCacheR: "Cache read", pCacheW: "Cache write",
      pOverridden: "overridden",
      footPrices: "Prices: {0}", footVia: " ({0})", footFetched: ", fetched {0}",
      viaHost: "via the CPA proxy", viaDirect: "direct",
      footBody: "<b>Windows are keyed by length</b>, not by the upstream primary/secondary label — " +
        "those swapped meaning once (when the 5-hour limit returned, primary went from the weekly " +
        "window to the 5-hour one). The same spend counts against every window, so each is " +
        "estimated independently.<br>" +
        "<b>window</b> = observed spend for the cycle ÷ the cycle's percentage, available only when " +
        "the plugin saw the cycle open at 0%; <b>delta</b> = Σ spend between readings ÷ Σ percentage " +
        "steps, which converges even for a mid-cycle install. Calibration is bounded in count and " +
        "age, so the estimate follows a plan change instead of being held back by old data.<br>" +
        "<b>Pace</b> = quota consumed ÷ clock elapsed; above 1 means it runs out before it resets. " +
        "Cache reads and reasoning tokens are subsets of input and output and are never billed twice.",
      noData: "No data", thisHour: "this hour", hoursAgo: "h ago", now: "now",
      total: "Trailing 7d", reqs: "req",
      unitDay: "d", unitHour: "h", unitMin: "m",
      wDays: "{0}-day", wHours: "{0}-hour", wMins: "{0}-min",
      locale: "en-US"
    }
  };

  var LANG = localStorage.getItem(LANGKEY);
  if (!LANG) {
    LANG = (String(navigator.language || "").toLowerCase().indexOf("zh") === 0) ? "zh" : "en";
  }
  function t(k) {
    var s = (I18N[LANG] && I18N[LANG][k]) || (I18N.en[k]) || k;
    for (var i = 1; i < arguments.length; i++) {
      s = s.split("{" + (i - 1) + "}").join(String(arguments[i]));
    }
    return s;
  }

  var el = function (id) { return document.getElementById(id); };
  var keyBox = el("key"), alerts = el("alerts"), rows = el("rows"), cards = el("cards");
  var wrap = el("wrap"), stamp = el("stamp"), foot = el("foot"), head = el("head");
  var chartbox = el("chartbox"), chart = el("chart"), pricebox = el("pricebox"), wtotals = el("wtotals");
  var modelbox = el("modelbox"), modeltable = el("modeltable");
  var rotbox = el("rotbox"), rottable = el("rottable");
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

  // Money formatting is deliberately locale-independent: zh-CN and en-US group
  // and punctuate numbers identically, and keeping it free of the language
  // dictionary lets the chart tests exercise these helpers in isolation.
  function usd(v) {
    if (v === null || v === undefined || isNaN(v)) return "—";
    var a = Math.abs(v);
    if (a !== 0 && a < 0.01) return "$" + v.toFixed(4);
    return "$" + Number(v).toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  }
  function pct(v) { return (v === null || v === undefined) ? "—" : Number(v).toFixed(1) + "%"; }

  // chartLabels supplies the few words the SVG builders need. Passing them in
  // keeps those builders self-contained, which is what lets the chart test lift
  // them straight out of the served page and run them on their own.
  function chartLabels(L) {
    return L || { noData: "No data", thisHour: "this hour", hoursAgo: "h ago",
                  now: "now", total: "Trailing 7d", reqs: "req" };
  }

  function tok(v) {
    if (!v) return "0";
    if (v >= 1e6) return (v / 1e6).toFixed(2) + "M";
    if (v >= 1e3) return (v / 1e3).toFixed(1) + "K";
    return String(v);
  }
  function dur(s) {
    if (s === null || s === undefined) return "—";
    var h = Math.floor(s / 3600), d = Math.floor(h / 24);
    var sep = LANG === "zh" ? " " : "";
    if (d > 0) return d + t("unitDay") + sep + (h % 24) + t("unitHour");
    if (h > 0) return h + t("unitHour") + sep + Math.floor((s % 3600) / 60) + t("unitMin");
    return Math.floor(s / 60) + t("unitMin");
  }
  function wname(w) {
    var m = w.minutes;
    if (m % 1440 === 0) return t("wDays", m / 1440);
    if (m % 60 === 0) return t("wHours", m / 60);
    return t("wMins", m);
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
    var rollB = new Array(span), partB = new Array(span);
    for (i = 0; i < span; i++) {
      usdB[i] = 0; pctB[i] = null; reqB[i] = 0; rollB[i] = null; partB[i] = false;
    }
    for (i = 0; i < series.length; i++) {
      var idx = span - 1 - series[i].ago;
      if (idx < 0 || idx >= span) continue;
      usdB[idx] += series[i].usd || 0;
      reqB[idx] += series[i].requests || 0;
      if (series[i].percent !== undefined && series[i].percent !== null) pctB[idx] = series[i].percent;
      if (series[i].rolling_usd !== undefined && series[i].rolling_usd !== null) {
        rollB[idx] = series[i].rolling_usd;
        partB[idx] = !!series[i].rolling_partial;
      }
    }
    return { span: span, usd: usdB, pct: pctB, req: reqB, roll: rollB, partial: partB };
  }

  function agoLabel(ago, L) {
    L = chartLabels(L);
    return ago === 0 ? L.thisHour : ago + " " + L.hoursAgo;
  }

  // Hourly spend as bars, with a cumulative-spend line on its own right-hand
  // axis. Inline SVG only, so the page needs no external chart library.
  function drawChart(series, w, h, showAxis, L) {
    L = chartLabels(L);
    if (!series || !series.length) return "<div class='sub'>" + L.noData + "</div>";
    var b = buildBuckets(series), span = b.span, i;

    var maxU = 0, total = 0;
    for (i = 0; i < span; i++) { maxU = Math.max(maxU, b.usd[i]); total += b.usd[i]; }
    if (maxU <= 0) maxU = 1;
    // A trailing seven-day total, not a running one. Spend accumulated since
    // the chart began only ever rises, so the line carried no information
    // beyond "time passed"; this one is flat while load is steady and moves
    // only when load does. Older builds sent no rolling figure, so fall back
    // to the running total rather than drawing nothing.
    var cum = new Array(span), maxC = 0, run = 0, haveRoll = false;
    for (i = 0; i < span; i++) { if (b.roll[i] !== null) { haveRoll = true; break; } }
    for (i = 0; i < span; i++) {
      run += b.usd[i];
      cum[i] = haveRoll ? (b.roll[i] === null ? null : b.roll[i]) : run;
      if (cum[i] !== null) maxC = Math.max(maxC, cum[i]);
    }
    if (maxC <= 0) maxC = 1;

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
             "' rx='1' fill='var(--accent)' opacity='.62'><title>" + agoLabel(span - 1 - i, L) +
             " · " + usd(b.usd[i]) + " · " + b.req[i] + " " + L.reqs + "</title></rect>";
    }

    // Trailing spend. A flat line is steady load, a rising one is load
    // genuinely growing. Where the seven days reach back past anything
    // recorded the figure is short by an unknown amount, so that stretch is
    // drawn dashed rather than presented as a real climb.
    var cy = function (k) { return padT + ih - ih * cum[k] / maxC; };
    var solid = [], dashed = [], lastY = null;
    for (i = 0; i < span; i++) {
      if (cum[i] === null) continue;
      var pt = cx(i).toFixed(1) + "," + cy(i).toFixed(1);
      if (b.partial[i]) {
        dashed.push(pt);
      } else {
        // Join the two runs so the line has no gap where it becomes complete.
        if (!solid.length && dashed.length) solid.push(dashed[dashed.length - 1]);
        solid.push(pt);
      }
      lastY = cy(i);
    }
    if (dashed.length > 1) {
      svg += "<polyline fill='none' stroke='var(--good)' stroke-width='2' opacity='.45' " +
             "stroke-dasharray='4 3' stroke-linejoin='round' points='" + dashed.join(" ") + "'/>";
    }
    if (solid.length > 1) {
      svg += "<polyline fill='none' stroke='var(--good)' stroke-width='2' " +
             "stroke-linejoin='round' points='" + solid.join(" ") + "'/>";
    }
    if (lastY !== null) {
      var lastV = cum[span - 1] === null ? 0 : cum[span - 1];
      svg += "<circle cx='" + cx(span - 1).toFixed(1) + "' cy='" + lastY.toFixed(1) +
             "' r='3' fill='var(--good)'><title>" + L.total + " " + usd(lastV) + "</title></circle>";
    }

    if (showAxis) {
      svg += "<text x='" + padL + "' y='" + (h - 4) + "' font-size='10' fill='var(--muted)'>" +
             agoLabel(span, L) + "</text>";
      svg += "<text x='" + (w - padR) + "' y='" + (h - 4) + "' text-anchor='end' font-size='10' " +
             "fill='var(--muted)'>" + L.now + "</text>";
    }
    return svg + "</svg>";
  }

  // Quota percentage over time against a constant-rate reference. A curve above
  // the dashed line is the visual form of a pace ratio greater than one: the
  // window will be exhausted before it resets.
  function drawQuotaCurve(series, windowMinutes, w, h, L) {
    L = chartLabels(L);
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
             "' stroke='var(--muted)' stroke-width='1.5' stroke-dasharray='4 3' opacity='.7'/>";
    }

    var pts = [];
    for (i = first; i < span; i++) pts.push(cx(i).toFixed(1) + "," + cy(fill[i]).toFixed(1));
    svg += "<polyline fill='none' stroke='var(--warn)' stroke-width='2' stroke-linejoin='round' " +
           "points='" + pts.join(" ") + "'/>";
    svg += "<circle cx='" + cx(span - 1).toFixed(1) + "' cy='" + cy(fill[span - 1]).toFixed(1) +
           "' r='3' fill='var(--warn)'><title>" + pct(fill[span - 1]) + "</title></circle>";
    return svg + "</svg>";
  }

  function L() {
    return { noData: t("noData"), thisHour: t("thisHour"), hoursAgo: t("hoursAgo"),
             now: t("now"), total: t("total"), reqs: t("reqs") };
  }

  function paceTag(w) {
    var r = w.pace_ratio;
    if (r === undefined || r === null) return "";
    var cls = r > 1.15 ? "bad" : (r < 0.85 ? "ok" : "");
    var word = r > 1.15 ? t("paceFast") : (r < 0.85 ? t("paceSlow") : t("paceNormal"));
    return "<span class='tag " + cls + "'>" + word + " ×" + r.toFixed(2) + "</span>";
  }

  function windowCell(w) {
    if (!w) return "<td class='num'><span class='tag'>—</span></td>";
    var e = w.estimate || {};
    var waiting = e.method === "none";
    var p = w.used_percent || 0;
    var stale = !!w.used_percent_stale;
    var s = "<td><div class='wcell'>";
    s += "<div class='mlabel'><span>" + pct(p) + "</span><span>" +
         (w.time_progress_percent !== undefined ? pct(w.time_progress_percent) : "") + "</span></div>";
    s += "<div class='meter'><i style='width:" + Math.min(100, p) + "%;background:" + heat(p) +
         (stale ? ";opacity:.35" : "") + "'></i></div>";
    if (w.time_progress_percent !== undefined) {
      s += "<div class='meter'><i style='width:" + Math.min(100, w.time_progress_percent) +
           "%;background:var(--muted);opacity:.5'></i></div>";
    }
    s += "<div class='sub2'>" + (waiting ? "<span class='tag'>" + t("watching") + "</span>"
         : t("left") + " <b>" + usd(e.remaining_usd) + "</b> / " + usd(e.quota_usd)) + "</div>";
    s += "<div class='sub2'>" + paceTag(w) + " <span class='tag'>" +
         t("resetsIn", dur(w.reset_in_seconds)) + "</span>" +
         // The window rolled over while nothing was watching, so the
         // percentage above describes the cycle before last. Saying so beats
         // showing it as current, and the next check replaces it with a real
         // reading rather than a guess.
         (w.reset_inferred ? " <span class='tag warn' title='" + esc(t("staleCycle")) +
                             "'>?</span>" : "") + "</div>";
    return s + "</div></td>";
  }

  function row2(k, v) { return "<div class='k'>" + esc(k) + "</div><div class='v'>" + v + "</div>"; }

  function windowDetail(w) {
    var e = w.estimate || {};
    var tk = w.tokens || {};
    var kv = "<div class='kv'>" +
      row2(t("dObserved"), usd(w.usd_observed)) +
      row2(t("dAttributed"), usd(e.attributed_usd) + "<span class='sub2'>" +
           t("dPending", usd((w.usd_observed || 0) - (e.attributed_usd || 0))) + "</span>") +
      row2(t("dSaved"), usd(w.saved_usd)) +
      row2(t("dReqFail"), (w.requests || 0) + " / " + (w.failed || 0) +
           (w.failure_rate ? " (" + pct(w.failure_rate) + ")" : "")) +
      row2(t("dAvg"), usd(w.avg_usd_per_request)) +
      row2(t("dInOut"), tok(tk.InputTokens) + " / " + tok(tk.OutputTokens)) +
      row2(t("dCacheRead"), tok(tk.CacheReadTokens) +
           (w.cache_hit_rate !== undefined ? " (" + pct(w.cache_hit_rate) + ")" : "")) +
      row2(t("dByWindow"), e.quota_usd_by_window ? usd(e.quota_usd_by_window) : t("dNA")) +
      row2(t("dByDelta"), e.quota_usd_by_delta ? usd(e.quota_usd_by_delta) : t("dNA")) +
      row2(t("dSamples"), t("dSamplesV", e.samples || 0, pct(e.evidence_percent))) +
      row2(t("dCoverage"), w.full_coverage ? t("dYes") : t("dNoMid")) +
      row2(t("dCycles"), t("dCyclesV", w.cycles || 0) +
           (w.granted_resets ? "<b>" + t("dGranted", w.granted_resets) + "</b>" : "")) +
      row2(t("dExternal"), w.unexplained_percent
           ? "<b>" + t("dExternalV", pct(w.unexplained_percent)) + "</b>" : t("dNone")) +
      row2(t("dExhaust"), w.will_exhaust_before_reset === true
           ? "<span class='tag bad'>" + t("dWillExhaust") + "</span>"
           : (w.will_exhaust_before_reset === false
              ? "<span class='tag ok'>" + t("dLasts") + "</span>" : "—")) +
      row2(t("dPerDay"), w.burn_usd_per_day === undefined ? "—" : usd(w.burn_usd_per_day)) +
      "</div>";

    var m = "<table><thead><tr><th>" + t("mtModel") + "</th><th>" + t("mtReq") + "</th><th>" +
            t("mtFail") + "</th><th>" + t("mtIn") + "</th><th>" + t("mtOut") + "</th><th>" +
            t("mtCache") + "</th><th>" + t("mtPrice") + "</th><th>" + t("mtAvg") + "</th><th>" +
            t("mtUsd") + "</th></tr></thead><tbody>";
    (w.by_model || []).forEach(function (x) {
      var p = x.price || {};
      m += "<tr><td>" + esc(x.model) +
           (x.unpriced ? " <span class='tag bad'>" + t("mtNoPrice") + "</span>" : "") + "</td>" +
           "<td class='num'>" + x.requests + "</td>" +
           "<td class='num'>" + (x.failed || 0) + "</td>" +
           "<td class='num'>" + tok(x.tokens.InputTokens) + "</td>" +
           "<td class='num'>" + tok(x.tokens.OutputTokens) +
             (x.tokens.ReasoningTokens ? " <span class='sub2'>" +
              t("mtReasoning", tok(x.tokens.ReasoningTokens)) + "</span>" : "") + "</td>" +
           "<td class='num'>" + (x.cache_hit_rate === undefined ? "—" : pct(x.cache_hit_rate)) + "</td>" +
           "<td class='num sub2'>" + (p.input === undefined ? "—" : "$" + p.input + " / $" + p.output) + "</td>" +
           "<td class='num'>" + (x.avg_usd === undefined ? "—" : usd(x.avg_usd)) + "</td>" +
           "<td class='num'>" + usd(x.usd) + "</td></tr>";
    });
    m += "</tbody></table>";

    return "<div class='wbox'><h3>" + t("wTitle", wname(w)) + " · " +
           (e.method === "none" ? "<span class='tag'>" + t("watching") + "</span>"
             : "<span class='tag " + esc(e.confidence) + "'>" + confWord(e.confidence) + "</span> " +
               "<span class='tag'>" + methodWord(e.method) + "</span>") + "</h3>" +
           kv + "<div style='margin-top:10px'>" + m + "</div></div>";
  }

  function detailRow(a) {
    var boxes = (a.windows || []).map(windowDetail).join("");
    var curveW = (a.windows || []).length ? a.windows[a.windows.length - 1].minutes : 0;
    var curve = drawQuotaCurve(a.series, curveW, 300, 96, L());
    var charts =
      "<div style='margin-bottom:12px'>" + drawChart(a.series, 340, 80, false, L()) +
        "<div class='chartcap'><i class='sw bar'></i>" + t("capHourly") +
        " <i class='sw ln' style='margin-left:8px'></i>" + t("capCumulative") + "</div></div>" +
      (curve
        ? "<div>" + curve + "<div class='chartcap'><i class='sw ln warn'></i>" +
          t("capCurve", curveW ? wname({ minutes: curveW }) : "") +
          " <i class='sw ln dash' style='margin-left:8px'></i>" + t("capRef") +
          "<br>" + t("capCurveHint") + "</div></div>"
        : "");

    return "<div class='wgrid'>" + boxes + "</div>" + charts;
  }

  function sortAccounts(list) {
    var mode = el("sort").value;
    function est(a) { return ((a.longest || {}).estimate) || {}; }
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
    return { high: t("confHigh"), medium: t("confMedium"), low: t("confLow"), none: t("confNone") }[c] || c;
  }
  function methodWord(m) { return { window: t("mWindow"), delta: t("mDelta"), none: "—" }[m] || m; }

  function card(k, v, n) {
    return "<div class='card'><div class='k'>" + esc(k) + "</div><div class='v'>" +
      (v === undefined || v === null ? "—" : v) + "</div>" +
      (n ? "<div class='n'>" + esc(n) + "</div>" : "") + "</div>";
  }

  // Backend warnings arrive as codes plus fields, so they render in whichever
  // language is selected rather than whichever one the plugin was compiled with.
  function warnText(w) {
    switch (w.code) {
      case "external_usage": return t("warnExternal", w.credential, w.window, pct(w.percent));
      case "unpriced_models": return t("warnUnpriced", (w.models || []).join(", "), w.credential);
      case "price_refresh_failed": return t("warnPrice", w.message);
      case "model_unavailable":
        return w.in_seconds === undefined
          ? t("warnModelDownNoEta", w.model, w.credentials)
          : t("warnModelDown", w.model, w.credentials, dur(w.in_seconds));
      case "rotator_stuck":
        return w.in_seconds === undefined ? t("warnRotStuck") : t("warnRotStuckEta", dur(w.in_seconds));
      case "rotator_degraded":
        return w.in_seconds === undefined
          ? t("warnRotDegraded", w.serving, w.target)
          : t("warnRotDegradedEta", w.serving, w.target, dur(w.in_seconds));
      case "rotator_dry_run": return t("warnRotDry");
      case "model_single_point":
        return w.disabled
          ? t("warnModelSingleOff", w.model, w.credential, w.disabled)
          : t("warnModelSingle", w.model, w.credential);
      default: return w.message || w.code || "";
    }
  }

  // A model with nothing left to serve it is an outage, not an advisory.
  function warnClass(w) {
    return (w.code === "model_unavailable" || w.code === "rotator_stuck") ? "bad" : "warn";
  }

  function modelStateTag(m) {
    var cls = { down: "bad", degraded: "warn", ok: "ok" }[m.state] || "";
    var word = { down: t("msDown"), degraded: t("msDegraded"), ok: t("msOK") }[m.state] || m.state;
    var s = "<span class='tag " + cls + "'>" + word + "</span>";
    if (m.single_point) s += " <span class='tag warn'>" + t("msSingle") + "</span>";
    return s;
  }

  // One chip per credential. The title carries the detail an operator would
  // otherwise have to dig out of the proxy log: why it is out, which window
  // filled up, and when it comes back.
  function credChip(c) {
    var cls = { cooling: "bad", recovering: "warn", ok: "ok", disabled: "",
                rejected: "bad" }[c.state] || "";
    var word = { cooling: t("csCooling"), recovering: t("csRecovering"),
                 ok: t("csOK"), disabled: t("csDisabled"),
                 rejected: t("csRejected") }[c.state] || c.state;
    var label = esc(c.credential) + " · " + word;
    if (c.state === "cooling") {
      label += " " + dur(c.cooldown_in_seconds) + (c.cooldown_estimated ? "?" : "");
    }
    // A rejection does not expire, so a countdown would be a lie. What matters
    // is how long it has been refused and why.
    if (c.state === "rejected" && c.rejected_age_seconds !== undefined) {
      label += " " + dur(c.rejected_age_seconds);
    }
    var tip = [];
    if (c.reason) tip.push(t("mhReason", c.reason));
    if (c.blocked_window) tip.push(t("mhWindowFull", c.blocked_window));
    if (c.cooldown_until) tip.push(c.cooldown_until + (c.cooldown_estimated ? " (" + t("mhEstimated") + ")" : ""));
    if (c.last_ok_age_seconds !== undefined) tip.push(t("mhLastOK", dur(c.last_ok_age_seconds)));
    if (c.blocks) tip.push(t("mhBlocks", c.blocks));
    tip.push(t("mhTraffic") + ": " + (c.requests || 0) + " / " + (c.failed || 0));
    return "<span class='chip tag " + cls + (c.state === "disabled" ? " off" : "") +
           "' title='" + esc(tip.join("\n")) + "'>" + label + "</span>";
  }

  var SKIPS = { dead_token: "skDead", exhausted: "skExhausted", below_floor: "skFloor",
                probe_failed: "skProbe", excluded_by_config: "skExcluded",
                recently_rotated: "skRecent" };

  function rotVerdict(c) {
    if (c.skipped) {
      var cls = c.skipped === "dead_token" ? "bad" : "";
      return "<span class='tag " + cls + "'>" + t(SKIPS[c.skipped] || c.skipped) + "</span>";
    }
    return "<span class='tag ok'>" + t(c.safe ? "rtSafe" : "rtOK") + "</span>";
  }

  function renderRotator(r) {
    if (!r) { rotbox.hidden = true; return; }
    var state = !r.enabled ? "<span class='tag'>" + t("rotOff") + "</span>"
      : (r.dry_run ? "<span class='tag warn'>" + t("rotDry") + "</span>"
                   : "<span class='tag ok'>" + t("rotOn") + "</span>");
    state += " <span class='tag'>" + t("rotPool", r.keep_enabled) + "</span>" +
             " <span class='tag'>" + t("rotFloor", r.switch_at_percent) + "</span>";
    if (r.last_sweep_age_seconds !== undefined) {
      state += " <span class='tag'>" + t("rotSwept", dur(r.last_sweep_age_seconds)) + "</span>";
    }
    if (r.max_changes_daily) {
      state += " <span class='tag'>" + t("rotChanges", r.changes_today || 0, r.max_changes_daily) + "</span>";
    }
    // On a working system this reads zero for days at a time. If it does not,
    // the estimator has stopped being able to answer from stored readings and
    // that is worth seeing before upstream sees it.
    if (r.probes_today !== undefined) {
      state += " <span class='tag" + (r.probes_today > 20 ? " warn" : "") + "'>" +
               t("rotProbes", r.probes_today) + "</span>";
    }
    if (r.probes_refused_credentials) {
      state += " <span class='tag bad'>" + t("rotRefused", r.probes_refused_credentials) + "</span>";
    }
    el("rot-status").innerHTML = state;

    var rows = r.candidates || [];
    if (!rows.length) {
      rottable.innerHTML = "<tbody><tr><td>" + t("mhNone") + "</td></tr></tbody>";
    } else {
      var h = "<thead><tr><th>" + t("rtCred") + "</th><th>" + t("rtRole") + "</th><th>" +
              t("rtHead") + "</th><th>" + t("rtWindow") + "</th><th>" + t("rtReset") +
              "</th><th>" + t("rtServe") + "</th><th>" + t("rtVerdict") + "</th></tr></thead><tbody>";
      rows.forEach(function (c) {
        h += "<tr><td><span class='name'>" + esc(c.credential || c.file) + "</span>" +
               (c.plan_type ? "<div class='sub2'>" + esc(c.plan_type) + "</div>" : "") + "</td>" +
             "<td><span class='tag" + (c.enabled ? " ok" : "") + "'>" +
               t(c.enabled ? "rtActive" : "rtStandby") + "</span></td>" +
             "<td class='num'>" + pct(c.headroom_percent) + "</td>" +
             "<td class='num'>" + (c.binding_window_minutes
               ? wname({ minutes: c.binding_window_minutes }) : "—") + "</td>" +
             "<td class='num'>" + (c.binding_reset_in_hours
               ? dur(Math.round(c.binding_reset_in_hours * 3600)) : "—") + "</td>" +
             "<td class='num'>" + (c.service_hours
               ? dur(Math.round(c.service_hours * 3600)) : "—") + "</td>" +
             "<td>" + rotVerdict(c) + "</td></tr>";
      });
      rottable.innerHTML = h + "</tbody>";
    }

    var log = r.log || [];
    el("rotlogbox").hidden = !log.length;
    if (log.length) {
      var lh = "<thead><tr><th>" + t("rlAt") + "</th><th>" + t("rlAction") + "</th><th>" +
               t("rlCred") + "</th><th>" + t("rlWhy") + "</th></tr></thead><tbody>";
      log.forEach(function (e) {
        lh += "<tr><td>" + esc(e.at) + "</td><td><span class='tag " +
              (e.action === "enable" ? "ok" : "warn") + "'>" +
              t(e.action === "enable" ? "rlEnable" : "rlDisable") + "</span>" +
              (e.dry_run ? " <span class='tag'>" + t("rlDry") + "</span>" : "") + "</td>" +
              "<td>" + esc(e.file) + "</td><td>" + esc(e.reason) + "</td></tr>";
      });
      el("rotlog").innerHTML = lh + "</tbody>";
    }
    rotbox.hidden = false;
  }

  function renderModels(models) {
    if (!models || !models.length) {
      modeltable.innerHTML = "<tbody><tr><td>" + t("mhNone") + "</td></tr></tbody>";
      modelbox.hidden = false;
      return;
    }
    var h = "<thead><tr><th>" + t("mhModel") + "</th><th>" + t("mhState") + "</th><th>" +
            t("mhCap") + "</th><th>" + t("mhBack") + "</th><th>" + t("mhTraffic") + "</th><th>" +
            t("mhCreds") + "</th></tr></thead><tbody>";
    models.forEach(function (m) {
      h += "<tr><td><span class='name'>" + esc(m.model) + "</span></td>" +
           "<td>" + modelStateTag(m) + "</td>" +
           "<td class='num'>" + m.available + " / " + m.credentials +
             (m.disabled ? " <span class='sub2'>+" + m.disabled + " " + t("csDisabled") + "</span>" : "") + "</td>" +
           "<td class='num'>" + (m.next_recovery_in_seconds === undefined
             ? "—" : dur(m.next_recovery_in_seconds)) + "</td>" +
           "<td class='num'>" + (m.requests || 0) + " / " + (m.failed || 0) + "</td>" +
           "<td>" + (m.by_credential || []).map(credChip).join("") + "</td></tr>";
    });
    modeltable.innerHTML = h + "</tbody>";
    modelbox.hidden = false;
  }

  function applyStatic() {
    document.documentElement.lang = LANG === "zh" ? "zh-CN" : "en";
    el("t-title").textContent = t("title");
    el("t-sub").textContent = t("sub");
    keyBox.placeholder = t("keyPlaceholder");
    el("load").textContent = t("load");
    el("forget").textContent = t("forget");
    el("export").textContent = t("exportJson");
    el("t-chart").textContent = t("chartTitle");
    el("t-lg1").textContent = t("legendBars");
    el("t-lg2").textContent = t("legendLine");
    el("t-prices").textContent = t("pricesTitle");
    el("t-models").textContent = t("modelsTitle");
    el("t-rot").textContent = t("rotTitle");
    el("t-rothint").innerHTML = t("rotHint");
    el("t-rotlog").textContent = t("rotLogTitle");
    el("t-modelhint").innerHTML = t("modelsHint");
    var opts = el("sort").options;
    var names = ["sortQuota", "sortRemain", "sortUsed", "sortPace", "sortName"];
    for (var i = 0; i < opts.length; i++) opts[i].textContent = t(names[i]);
    el("lang").value = LANG;
  }

  function render(data) {
    last = data;
    alerts.innerHTML = "";
    (data.warnings || []).forEach(function (w) { note(warnClass(w), warnText(w)); });
    renderModels(data.models);
    renderRotator(data.rotator);

    var accounts = data.accounts || [];
    var stale = accounts.filter(function (a) { return a.stale; });
    if (stale.length) note("warn", t("warnStale", stale.length));
    var waiting = accounts.filter(function (a) {
      return !a.longest || !a.longest.estimate || a.longest.estimate.method === "none";
    });
    if (waiting.length) note("warn", t("warnWaiting", waiting.length, accounts.length));

    // Credentials whose auth file was deleted are no longer listed among the
    // live ones - a deleted account read as a working one is worse than not
    // seeing it - but saying so beats them vanishing without explanation.
    var gone = data.removed || [];
    if (gone.length) {
      note("", t("rmRemoved", gone.length, gone.map(function (r) {
        return String(r.auth_id).replace(/^codex-/, "").replace(/\.json$/, "");
      }).join(", ")));
    }

    // One summary block per window length: the 5-hour and the weekly limit are
    // separate quotas, and the same spend counts against both.
    var wt = data.window_totals || [];
    wtotals.innerHTML = wt.map(function (x) {
      return "<div class='box'><h2>" + t("totalsTitle", wname(x)) + "</h2><div class='cards' style='margin:0'>" +
        card(t("cCreds"), x.credentials,
             t("cPriced", x.estimated) + (x.at_risk ? " · " + t("cAtRisk", x.at_risk) : "")) +
        card(t("cQuota"), usd(x.quota_usd)) +
        card(t("cSpent"), usd(x.spent_usd)) +
        card(t("cRemain"), usd(x.remaining_usd)) +
        "</div></div>";
    }).join("");

    var tot = data.totals || {};
    cards.innerHTML =
      card(t("cCreds"), tot.credentials) +
      card(t("cObserved"), usd(tot.window_usd_observed), t("cObservedN")) +
      card(t("cSaved"), usd(tot.cache_saved_usd), t("cSavedN")) +
      card(t("cRequests"), (tot.requests || 0).toLocaleString(t("locale")), t("cFailedN", tot.failed || 0));
    cards.hidden = false;

    chart.innerHTML = drawChart(data.fleet_series, 1000, 165, true, L());
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
    head.innerHTML = "<th>" + t("thCred") + "</th><th>" + t("thPlan") + "</th>" +
      lengths.map(function (m) { return "<th>" + t("thQuota", wname({ minutes: m })) + "</th>"; }).join("") +
      "<th>" + t("thStatus") + "</th>";

    rows.innerHTML = "";
    sortAccounts(accounts).forEach(function (a, i) {
      var byLen = {};
      (a.windows || []).forEach(function (w) { byLen[w.minutes] = w; });

      var tr = document.createElement("tr");
      tr.className = "main" + (a.disabled ? " off" : "");
      var tags = "";
      if (a.disabled) tags += " <span class='tag'>" + t("tagDisabled") + "</span>";
      if (a.unavailable) tags += " <span class='tag bad'>" + t("tagUnavailable") + "</span>";
      if (a.stale) tags += " <span class='tag warn'>" + t("tagStale") + "</span>";
      if (a.limit_reached_type) tags += " <span class='tag bad'>" + esc(a.limit_reached_type) + "</span>";

      var html =
        "<td><span class='toggle' data-i='" + i + "'>▸</span> <span class='name'>" +
          esc(a.label || a.auth_id) + "</span>" + tags +
          "<div class='sub2'>" + t("rowTotals", a.total_requests || 0, usd(a.total_usd)) +
          (a.observed_age_seconds !== undefined
            ? " · " + t("rowObserved", dur(a.observed_age_seconds)) : "") +
          "</div></td>" +
        "<td>" + esc(a.plan_type || "—") +
          (a.active_limit ? "<div class='sub2'>" + esc(a.active_limit) + "</div>" : "") + "</td>";
      lengths.forEach(function (m) { html += windowCell(byLen[m]); });

      var le = (a.longest && a.longest.estimate) || {};
      html += "<td>" + (le.method === "none" || !le.method
        ? "<span class='tag'>" + t("needTick") + "</span>"
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

    stamp.textContent = t("updatedAt", new Date().toLocaleTimeString(t("locale")));
    var via = data.price_transport === "host" ? t("viaHost")
            : (data.price_transport === "direct" ? t("viaDirect") : "");
    foot.innerHTML =
      t("footPrices", esc(data.price_source || "builtin")) +
      (via ? t("footVia", via) : "") +
      (data.price_fetched ? t("footFetched", esc(data.price_fetched)) : "") + "<br>" +
      t("footBody");
    loadPrices();
  }

  function api(path) {
    return fetch(base() + "/v0/management/codex-weekly-usd/" + path, {
      headers: { "X-Management-Key": keyBox.value.trim() }, cache: "no-store"
    }).then(function (r) {
      if (r.status === 401 || r.status === 403) throw new Error(t("keyRejected"));
      if (!r.ok) throw new Error(t("reqFailed", r.status));
      return r.json();
    });
  }

  function loadPrices() {
    api("prices").then(function (p) {
      var h = "<table><thead><tr><th>" + t("pModel") + "</th><th>" + t("pIn") + "</th><th>" +
              t("pOut") + "</th><th>" + t("pCacheR") + "</th><th>" + t("pCacheW") +
              "</th></tr></thead><tbody>";
      (p.models || []).forEach(function (m) {
        h += "<tr><td>" + esc(m.model) +
             (m.overridden ? " <span class='tag'>" + t("pOverridden") + "</span>" : "") + "</td>" +
             "<td class='num'>$" + m.input + "</td><td class='num'>$" + m.output + "</td>" +
             "<td class='num'>" + (m.cache_read ? "$" + m.cache_read : "—") + "</td>" +
             "<td class='num'>" + (m.cache_write ? "$" + m.cache_write : "—") + "</td></tr>";
      });
      el("prices").innerHTML = h + "</tbody></table>";
      pricebox.hidden = false;
    }).catch(function () { pricebox.hidden = true; });
  }

  function load() {
    if (!keyBox.value.trim()) { alerts.innerHTML = ""; note("warn", t("needKey")); return; }
    localStorage.setItem(STORE, keyBox.value.trim());
    api("data").then(render).catch(function (err) {
      alerts.innerHTML = ""; note("bad", err.message);
      cards.hidden = wrap.hidden = chartbox.hidden = pricebox.hidden = true;
      modelbox.hidden = rotbox.hidden = true;
      wtotals.innerHTML = "";
    });
  }

  el("load").addEventListener("click", load);
  el("sort").addEventListener("change", function () { if (last) render(last); });
  el("lang").addEventListener("change", function (ev) {
    LANG = ev.target.value;
    try { localStorage.setItem(LANGKEY, LANG); } catch (e) { /* private mode */ }
    applyStatic();
    if (last) render(last);
  });
  keyBox.addEventListener("keydown", function (e) { if (e.key === "Enter") load(); });
  el("forget").addEventListener("click", function () {
    localStorage.removeItem(STORE); keyBox.value = ""; last = null;
    cards.hidden = wrap.hidden = chartbox.hidden = pricebox.hidden = true;
    modelbox.hidden = rotbox.hidden = true;
    alerts.innerHTML = ""; stamp.textContent = ""; wtotals.innerHTML = "";
    if (timer) { clearInterval(timer); timer = null; }
  });
  el("export").addEventListener("click", function () {
    if (!last) { note("warn", t("loadFirst")); return; }
    var blob = new Blob([JSON.stringify(last, null, 2)], { type: "application/json" });
    var url = URL.createObjectURL(blob), a = document.createElement("a");
    a.href = url; a.download = "codex-quota-usd-" + new Date().toISOString().slice(0, 10) + ".json";
    a.click(); URL.revokeObjectURL(url);
  });

  applyStatic();
  if (keyBox.value) load();
  timer = setInterval(function () { if (keyBox.value.trim()) load(); }, 60000);
})();
</script>
</body>
</html>`
