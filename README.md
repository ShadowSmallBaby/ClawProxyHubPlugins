# ClawProxyHubPlugins

[ClawProxyHub](https://github.com/ShadowSmallBaby/ClawProxyHub) 的官方插件仓库：每个子目录一个插件，合入 main 后由 CI 构建、发布 Release 并更新市场索引，核心的「插件市场」默认从本仓库安装。

| 插件 | 说明 |
| --- | --- |
| `lobsterai` | 网易有道 LobsterAI：浏览器 OAuth / 凭据文件登录，每日签到 |
| `workbuddy` | 腾讯 WorkBuddy / CodeBuddy：手机验证码 / 浏览器授权 / 凭据文件登录，签到、盲盒、旅行、成长任务 |
| `newapi` | New API：API 密钥 / 密码 / 凭据文件登录，余额折算与每日签到；多实例（不同站点各建实例填 `base_url`） |

全部插件已升级到契约 **protocol v2**（实例维度）。

## 目录约定

```
plugins/<name>/
├── manifest.json   # name（= 目录名）、version、author、label、icon、protocol_version、capabilities、instance_schema
├── icon.png        # 可选，正方形 PNG 128–256px
└── main.go         # 入口：sdk.Serve(&plugin{})，Handshake 回报 version 变量
tools/pack/         # 打包器：交叉编译 + .cphplugin + index.json
index.json          # 市场索引（CI 生成并回写，勿手改）
```

插件实现 `pb.ClawPluginServer`（契约见核心 `sdk/proto/cph.proto`），复用 `sdk/openaiup` / `sdk/anthropicup` 适配 OpenAI / Anthropic 方言上游；宿主回调（日志 / 存储 / 代理）实现 `sdk.HostAware`。参考核心 `examples/stub` 与既有插件。

**多实例（protocol v2）**：`manifest.json` 声明 `capabilities` 含 `instances`（`sdk.CapabilityInstances`）+ `protocol_version: 2`，站点可配字段由 `instance_schema`（JSON Schema）声明，核心固定提供 name + base_url。登录 / 刷新 / 设置 / 任务能力回调按 `instance_id` 区分；`GetProxy` 可用 `account_id` 优先账号级代理。未声明该能力的插件只有一个默认实例。

## 开发

SDK 来自核心模块 `github.com/ShadowSmallBaby/ClawProxyHub`（go.mod 固定到某个提交）。要对着本地核心源码开发，用 workspace 覆盖（`go.work` 已忽略，不入库）：

```bash
go work init .
go work edit -replace github.com/ShadowSmallBaby/ClawProxyHub=../ClawProxyHub

go build ./... && go test ./...

# 编译当前平台并装进核心的插件目录（核心运行中会锁住二进制，先在插件页停止该插件）
go run ./tools/pack -install ../ClawProxyHub/data/plugins
```

升级 SDK 版本：`GOWORK=off go get github.com/ShadowSmallBaby/ClawProxyHub@main && GOWORK=off go mod tidy`。

## 打包与发布

```bash
go run ./tools/pack            # dist/<name>-<version>.cphplugin + dist/index.json
go run ./tools/pack -only workbuddy
```

- 包格式：zip，含 `manifest.json`、图标与 `plugin-<os>-<arch>[.exe]`（windows/amd64、linux/amd64、linux/arm64、darwin/amd64、darwin/arm64）；固定时间戳，同一输入产出同一 sha256
- 发布：改 `manifest.json` 的 `version` → 合入 main → CI 为每个新版本创建 Release `<name>-v<version>`（资产 `<name>-<version>.cphplugin`）并回写 `index.json`
- 已发布版本不可变：改代码必须升版本，否则 CI 跳过该插件
- 核心默认市场地址：`https://raw.githubusercontent.com/ShadowSmallBaby/ClawProxyHubPlugins/main/index.json`

## 贡献

1. fork → `plugins/<你的插件>/` 开发（`manifest.json` 的 `author` 与 GitHub 用户名一致）
2. `go vet ./... && go test ./... && go run ./tools/pack -only <你的插件>` 确认可构建
3. 提 PR，CI 会完整交叉编译一遍

## 许可证

与核心相同，[AGPL-3.0](LICENSE)。
