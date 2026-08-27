# 发布流程

## 0. 首次：确认身份

仓库里有三处写着 `tang12306`，如果你的 GitHub 账号不是这个，全部改掉：

```sh
grep -rn tang12306 --include='*.go' --include='*.json' --include='*.md' --include=Makefile .
```

| 位置 | 作用 |
|---|---|
| `main.go` 的 `pluginAuthor` / `repository` | 插件自报的作者与仓库（`repository` 会被 CI 用 `-ldflags` 覆盖，但本地构建用得上默认值） |
| `Makefile` 的 `REPO` | 本地构建的默认仓库 |
| `registry-entry.json`、`README.md` 链接 | 商店条目与文档 |
| `LICENSE` 的版权行 | 版权归属 |

## 1. 发版

版本号只写在 git tag 里，CI 用 `-ldflags -X main.pluginVersion=` 注入，源码里的默认值只是兜底。

```sh
git tag v2.0.0
git push origin v2.0.0
```

`.github/workflows/release.yml` 会：

1. 在 5 个**原生** runner 上构建（linux amd64/arm64、darwin arm64/amd64、windows amd64）——cgo 没有目标工具链就无法交叉编译，所以只能这样。
2. 打包成商店要求的资产名：`codex-weekly-usd_<版本>_<goos>_<goarch>.zip`。
3. 生成 `checksums.txt`（标准 `sha256sum` 格式）。
4. 创建 GitHub Release 并上传。

### 打包规格（改动前务必看这里）

CLIProxyAPI 的商店安装器很挑剔，两条都必须满足，否则安装直接失败：

- **动态库必须在 zip 根目录**，且文件名恰好是 `codex-weekly-usd.so` / `.dylib` / `.dll`。放进子目录会报 `target dynamic library must be at zip root`。
- **Release 里必须有 `checksums.txt`**。缺它会报 `release asset checksums.txt not found`，哪怕 zip 本身没问题。

资产名格式来自宿主的 `ArchiveName(id, version, goos, goarch)`，是精确匹配，不是模糊查找。

## 2. 提交到插件商店

商店就是一个 GitHub 仓库里的 `registry.json`：

```
https://github.com/router-for-me/CLIProxyAPI-Plugins-Store
```

把 `registry-entry.json` 的内容追加到它的 `plugins` 数组里，提 PR。二进制不需要上传给商店——它是根据条目里的 `repository` 去找**最新 Release** 的资产。

条目里的 `version` 应当与已发布的 tag 一致（去掉 `v` 前缀）。

## 3. 发版后自查

```sh
# 资产名是否符合规格
gh release view v2.0.0 --json assets -q '.assets[].name'

# 抽一个包，确认库在 zip 根目录
unzip -l codex-weekly-usd_2.0.0_linux_amd64.zip

# 校验和可用
sha256sum -c checksums.txt
```

然后在一台真实的 CLIProxyAPI 上从商店装一次，确认面板菜单「额度美元」出现，且：

```sh
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:<端口>/v0/resource/plugins/codex-weekly-usd/panel   # 200
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:<端口>/v0/management/codex-weekly-usd/data          # 401
```

第二条返回 401 是**必须**的——它证明数据没有暴露在免鉴权的资源路由上。
