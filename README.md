# codex-weekly-usd

[English](README.en.md) · 简体中文

一个 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 插件，只回答一个问题：

> **这个 Codex 账号的额度，按 OpenAI 官方 API 计费，值多少美元？**

商店里已有的用量类插件大多停在 token 数和百分比。这个插件把两者**除一下**，给出美元。

一个凭据同时受**多个窗口**限制（目前是 5 小时 + 一周），同一笔花费同时计入所有窗口，
所以每个窗口各自独立记账、各自估算。

## 原理

Codex 每次响应都带着这个账号的额度状态，而每次请求都会产生 token 计数。把两者配对：

```
分子   UsageRecord.Detail 里的 token 数 × 官方价目
分母   该窗口的已用百分比响应头

窗口额度(美元) = 花费 / (已用百分比 / 100)
```

上游 `chatgpt.com/backend-api/codex` 的真实响应头：

```
x-codex-primary-window-minutes:   300      ← 5 小时
x-codex-primary-used-percent:     100
x-codex-secondary-window-minutes: 10080    ← 7 天
x-codex-secondary-used-percent:   63
x-codex-plan-type: team
x-codex-active-limit: premium
```

这些头在 **200 响应上就有**，不是只在 429 限流时才返回。

### ⚠ 窗口按**长度**识别，不按 primary/secondary 标签

这两个标签的含义**变过一次**：5 小时限额回归之前，`primary` 是周窗口、`secondary` 不存在；
之后 `primary` 变成 5 小时窗口，周窗口挪到了 `secondary`。

把 `primary` 当成"那个窗口"会出两种错：周窗口的历史整段丢失，而且**周窗口的 1% 和 5 小时
窗口的 1% 代表的钱差着数量级**，混在同一个校准里平均出来的数毫无意义。所以本插件一律按
`window_minutes` 给窗口建档。

### 两个互相独立的估算器

| 依据 | 算法 | 何时可用 |
|---|---|---|
| `窗口法` | 整窗花费 ÷ 整窗百分比 | 只在插件亲眼看到窗口从 0% 开始时 |
| `步进法` | Σ两次读数间的花费 ÷ Σ百分比步进 | 第一次百分比跳动之后，随时 |

烧掉 3% 以上时优先用**窗口法**——它拿整窗的钱对整窗的百分比，没有归属上的猜测。**步进法**是兜底，它不在乎插件是什么时候装的，所以**中途安装也能收敛**，而不是给出一堆离谱数字。

`confidence`（置信度）反映插件**亲眼看着**消耗掉的额度有多少：≥25 个点为高，≥8 为中，更低为低，第一次跳动之前为无。

**校准样本有上限**（数量 160 条、时效 21 天）。无上限累加的话，套餐调整、窗口语义变化、
模型改价之后，旧证据会永远压过新证据，而"证据量"还会一路涨到几百个点，让置信度**看起来
越来越高、实际越来越错**。有界之后估算会跟着现实走。

### 归属：为什么要挂账

额度头是在上游响应流打开时发出的，描述的是**本次请求计入之前**的状态。所以本次花费先记入 `pending` 挂账，到**下一次**读数才和百分比步进配对。

`attributed_usd` 是最新百分比已经有机会涵盖的花费；未扣挂账的窗口原始总额单独报为 `window_usd_observed`。**窗口法必须用前者**，否则会系统性高估。

### token 口径

CLIProxyAPI 把 OpenAI 的计数器定义为子集关系：

- `CacheReadTokens`、`CacheCreationTokens` **在** `InputTokens` **里面**
- `ReasoningTokens` **在** `OutputTokens` **里面**

所以缓存 token 要从输入桶里**扣出来**按缓存价算，推理 token **绝不**在输出之上再加一遍。任何一个重复计费都会让整体估算虚高。

没有公开缓存写入价的模型，那部分 token 会**折回按新鲜输入计价**，而不是当成免费。

### 上游中途送重置

OpenAI 经常在周期没结束时把额度还回来：**百分比掉回去，但 `reset_at` 不变**。只按
`reset_at` 判断周期翻转会漏掉这种情况，于是重置前的花费仍留在账上、却要除以重置后的
一个很小的百分比——估算直接炸掉。因此**百分比回退也算周期翻转**，并单独计数（面板显示
"周期内重置 N 次"）。

### 别人也在用这个凭据

如果百分比涨了、而本插件这一段没有任何花费，说明有别的客户端在共用该凭据。这种情况下
样本会被跳过（否则会污染校准），同时累加到 `unexplained_percent` 并在面板告警——因为它
同时意味着**本插件的账是少记的**。

### 重启安全

状态是持久化的，重启不会丢窗口。但插件停机期间跑掉的流量**在百分比里、不在账里**——所以重启后第一次读数如果百分比涨了，窗口法的覆盖标记会作废，陈旧挂账会丢弃，而不是拿去和一段它没有造成的百分比步进配对。

## 面板

界面为中文，每个凭据一行：

- **额度 / 时间双进度条** —— 额度消耗与窗口时间流逝并排。谁烧太快，一眼就看出来。
- **节奏** —— `已用百分比 ÷ 时间流逝百分比`。>1.15 标「偏快」，<0.85 标「宽裕」。**大于 1 就意味着会在重置前用完。**
- **周额度 / 已用 / 剩余 / 日均**（美元），外加重置倒计时。
- **依据** —— 这个数字来自哪个估算器、背后有多少证据支撑。

展开任意一行，还有按模型的完整明细（请求数、失败数、输入输出 token 并标出推理子集、缓存命中率、所用价目、每次均价、金额）、该凭据的用量走势图，以及完整的推导过程：已归属 vs 挂账、两个估算器各自的值、校准样本数、整窗覆盖与否、续航天数与跑满预警。

全局还有汇总卡片（周额度总值、已用、剩余、实测消耗、缓存省下、请求与失败数）、走势图、生效价目表、排序和 JSON 导出。

### 图表

全部是内联 SVG，不引任何外部库，页面自包含。

- **用量走势** —— 柱状为每小时消耗（左轴），叠加**累计消耗折线**（自带右轴）。平段是空闲，斜率变陡就是消耗在加速。
- **额度曲线**（每个凭据）—— 已用百分比随时间的走势，配一条**匀速参考虚线**（把 100% 均摊到整个窗口）。**曲线高过虚线，就是「节奏 > 1」的可视化形态。**

额度曲线只画**当前窗口**：百分比下跌意味着窗口翻转，曲线从那里重新开始，否则会画成锯齿，而且参考线会锚在一个已经关闭的窗口上。只有一个读数时**什么都不画**，而不是给一张空图配一句承诺趋势的说明。

## 安装

### 插件商店

在 CLIProxyAPI 管理面板的插件商店里搜 `codex-weekly-usd` 安装。

### 手动

从 [Releases](https://github.com/tang12306/cpa-plugin-codex-weekly-usd/releases) 下载对应平台的包，把动态库放到：

```
<CLIProxyAPI 工作目录>/plugins/<goos>/<goarch>/codex-weekly-usd.so
```

**文件名必须等于插件 id。** 另外注意这个路径是相对 systemd 的 `WorkingDirectory` 解析的——如果 service 文件里没写 `WorkingDirectory=`，systemd 默认是 `/`，插件会被去 `/plugins/...` 里找，然后你会看到「已安装 0」。

## 配置

```yaml
plugins:
  enabled: true
  configs:
    codex-weekly-usd:
      enabled: true
      priority: 130
      data_dir: /var/lib/cliproxyapi/data/codex-weekly-usd
      price_source_url: https://models.dev/api.json
      price_refresh_hours: 24
      event_log: true
      event_log_keep_days: 60
      flush_seconds: 15
      # 超过该 prompt 规模套用长上下文加价，0 为关闭
      # long_context_threshold: 272000
      # long_context_multiplier: 2
      # 手工价目覆盖，优先级高于一切
      # price_overrides:
      #   gpt-5.6-sol:
      #     input: 5
      #     output: 30
      #     cache_read: 0.5
      #     cache_write: 6.25
```

### 价目

每天从 `https://models.dev/api.json` 的 openai provider 刷新，缓存到 `prices.json`，并内置一份表作为离线兜底。

**拉取优先走宿主的 HTTP 回调**（`host.http.do`），这样 CLIProxyAPI 里配好的代理会自动生效——网络受限环境不用再单独配一遍。宿主拒绝该回调时才退回直连。面板页脚会显示这次是「经 CPA 代理」还是「直连」。

没有公开价目的模型会单独计入 `unpriced_requests` 并给出告警——它们的花费是**缺失**，而不是被悄悄当成 0。

## 路由与鉴权

| 路由 | 鉴权 | 内容 |
|---|---|---|
| `/v0/resource/plugins/codex-weekly-usd/panel` | **无** | 不含数据的 HTML 壳 |
| `/v0/management/codex-weekly-usd/data` | 管理密钥 | 完整报表 |
| `/v0/management/codex-weekly-usd/prices` | 管理密钥 | 生效价目 |

CLIProxyAPI 的插件资源路由**不过管理鉴权**，所以本插件的面板刻意**不含任何数据**：它只是个壳，向使用者索取管理密钥后，自己去调带鉴权的路由。测试固化了这一点——面板返回 200 且不含任何凭据标识，`/data` 无密钥返回 401。

管理接口**只认 header**（`Authorization: Bearer` 或 `X-Management-Key`），不认 query 参数，所以浏览器无法直接打开受保护路由；这正是壳存在的理由。

## 数据文件

```
data_dir/
  state.json               每个凭据的累加器（原子写入）
  prices.json              价目缓存
  events/YYYY-MM-DD.jsonl  每请求一行，用于事后核对估算
```

`state.json` 和事件日志含有按凭据的用量，**不要提交进版本库**（`.gitignore` 已覆盖）。

## 从源码构建

CLIProxyAPI 的插件 ABI 是**纯 C ABI + JSON**，不是 Go 的 `plugin` 包。因此插件**不需要**和宿主的 Go 版本一致，也**完全不 import** CLIProxyAPI 模块。

```sh
make build        # 产出 codex-weekly-usd.so
make test         # 77 项断言
```

需要 Go（cgo，所以要 gcc/clang）、Python 3 和 Node（图表测试用）。

cgo 无法在没有目标工具链的情况下交叉编译，所以各平台由 GitHub Actions 在各自的原生 runner 上构建。

## 测试

`test/harness.c` 用 `dlopen` 加载 `.so`，通过**真实的 C ABI** 喂脚本化 JSON，因此不需要跑起一个 CLIProxyAPI 就能验算法。

场景是算术而非意见：每个请求恰好花费 $2.00（gpt-5.6-sol 官方价），百分比恰好走 1 点，所以答案**必须**是 $200。

覆盖：两个估算器互相印证、缓存 token 扣除、推理 token 不重复计费、未知模型被标记、带停机间隔的重启、窗口翻转、经宿主回调拉取价目、以及面板零泄露。

`test/test_charts.js` 把 SVG 生成函数**从插件真实返回的面板里**抠出来，喂 48 小时合成序列，逐坐标断言（柱数、累计单调性、每小时一个点、翻转重启、退化情况）——**测的就是浏览器拿到的那份字节**。

## 许可

MIT。本插件不包含 CLIProxyAPI 的任何代码。
