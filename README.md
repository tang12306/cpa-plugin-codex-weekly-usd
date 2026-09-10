# codex-weekly-usd

[English](README.en.md) · 简体中文

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 插件。把每个 Codex 凭据的额度窗口（5 小时 / 一周）按 OpenAI 官方 API 价换算成美元。

- **值多少钱**：每个窗口的额度、已用、剩余，按模型分别给出
- **现在能不能用**：每个模型还有几个凭据可用、谁在冷却、何时恢复
- **自动轮换**（可选）：凭据快耗尽前换下一个

## 原理

Codex 每个响应都带额度头（`x-codex-*-used-percent`、`window-minutes`、`reset-at`），每个请求都有 token 数。按官方价给 token 计价，再除以百分比：

```
窗口额度(美元) = 花费 / (已用百分比 / 100)
```

- 窗口按长度识别，不按 primary / secondary 标签（上游调换过一次）
- 同一窗口是一个额度池，所有模型共用同一块表；但**每美元吃掉的额度因模型而异**（实测 gpt-6-astra 约为 gpt-5.6-sol 的 1.4 倍），所以额度按模型分别估算。模型间比值在同一窗口内测量，再跨账号推算到没用过该模型的凭据
- 校准只用当前周期的证据，不够时才回溯上个周期

## 安装

插件商店搜 `codex-weekly-usd`，或从 [Releases](https://github.com/tang12306/cpa-plugin-codex-weekly-usd/releases) 下载，放到：

```
<CLIProxyAPI 工作目录>/plugins/<goos>/<goarch>/codex-weekly-usd.so
```

文件名也可以是 `codex-weekly-usd-v<版本>.so`。路径相对 systemd 的 `WorkingDirectory`，没写这一项时默认是 `/`，插件会找不到。

## 配置

```yaml
plugins:
  enabled: true
  configs:
    codex-weekly-usd:
      enabled: true
      data_dir: /var/lib/cliproxyapi/data/codex-weekly-usd
      rotator:
        enabled: false        # 唯一会改写凭据文件的功能，默认关闭
        dry_run: false        # true = 只判断和记录，不写入
        keep_enabled: 2       # 同时启用的凭据数
        switch_at_percent: 10 # 最紧窗口余量低于此值即换
        probe_model: gpt-5.6-sol
```

其余项都有默认值，见 [config.go](config.go)。价目每天从 `models.dev` 拉取（走 CPA 配好的代理），离线时用内置表；没有价目的模型会告警，不会按 0 计。

## 轮换

- 维持 `keep_enabled` 个凭据启用。一个被拒时，CPA 在同一个请求内换到下一个，所以换人对调用方无感
- 替换进来的凭据会写一个低于所有在役凭据的 `priority`，排到队尾。CPA 的 fill-first 在同一优先级内按文件名排序，不这样做的话，名字靠后的备胎永远轮不到
- 平时靠推算，不访问上游；只在换人前和窗口重置后各探测一次。探测走凭据自己的 `proxy_url`
- 上游拒绝的凭据（401/403）会被停用；重新登录换了 token 后自动重新进入候选
- 直接写凭据文件（`disabled`、`priority`），由 CPA 的文件监听生效

## 路由

| 路由 | 鉴权 | 用途 |
|---|---|---|
| `GET /v0/resource/plugins/codex-weekly-usd/panel` | 无 | 面板（不含数据的空壳） |
| `GET /v0/management/codex-weekly-usd/data` | 管理密钥 | 完整报表 |
| `GET /v0/management/codex-weekly-usd/prices` | 管理密钥 | 生效价目 |
| `POST /v0/management/codex-weekly-usd/rotate` | 管理密钥 | 立即执行一次轮换判断 |
| `POST /v0/management/codex-weekly-usd/refresh` | 管理密钥 | 重读所有凭据额度（带外重置后用） |

插件资源路由不过鉴权，所以面板只是个壳，向你要管理密钥后再调受保护路由。管理密钥只认 header（`Authorization: Bearer` 或 `X-Management-Key`）。

## 数据

```
data_dir/
  state.json               累加器
  prices.json              价目缓存
  events/YYYY-MM-DD.jsonl  逐请求记录
```

含按凭据的用量，别提交进版本库。

## 构建与测试

插件 ABI 是 C ABI + JSON，不依赖 CLIProxyAPI 模块，也不要求 Go 版本一致。

```sh
make build   # codex-weekly-usd.so
make test    # 需要 Go(cgo)、Python 3、Node
```

测试通过真实 C ABI 加载 `.so` 验算法，不需要跑 CLIProxyAPI。

## 许可

MIT
