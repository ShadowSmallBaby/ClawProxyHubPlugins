# ClawProxyHubPlugins

[ClawProxyHub](https://github.com/ShadowSmallBaby/ClawProxyHub) 的官方插件仓库：每个子目录一个插件，合入 main 后由 CI 构建、发布 Release 并更新市场索引，核心的「插件市场」默认从本仓库安装。

> 想写插件？先读 **[AGENTS.md](AGENTS.md)** —— 面向人与 AI 助手的完整开发指南（契约、分层、各能力实操、打包发布）。

全部插件已升级到契约 **protocol v2**（实例维度）。

插件有两种运行时：**Go 插件**（编译二进制，`plugins/plugins/`）与 **Lua 插件**（脚本，零编译，`plugins/plugins-lua/`，由核心内置的 LuaHost 运行时加载，见 [AGENTS.md](AGENTS.md) §11）。

## 插件清单

### Go 插件（`plugins/plugins/`）

| 插件 | 说明 | 能力 |
| --- | --- | --- |
| `lobsterai` | 网易有道 LobsterAI：浏览器 OAuth / 凭据文件登录，每日签到 | chat / models / login / tasks |
| `workbuddy` | 腾讯 WorkBuddy / CodeBuddy：手机验证码 / 浏览器授权 / 凭据文件登录，签到、盲盒、旅行、成长任务 | chat / models / login / refresh / tasks |
| `newapi` | New API（QuantumNous/new-api）：API 密钥 / 密码 / 凭据文件登录，余额折算与每日签到；多实例（不同站点各建实例填 `base_url`） | chat / models / login / refresh / tasks / instances |
| `gorkcli` | Grok CLI：xAI OIDC refresh_token / 凭据文件登录，反代 cli-chat-proxy.grok.com（Responses 协议），access_token 自动刷新 | chat / models / login / refresh |
| `commandcode` | Command Code：私有协议（NDJSON）反代 + 设备指纹伪装（按 key 确定性伪造，形态对齐官方 CLI） | chat / models / login / refresh |
| `todofor` | todofor.ai：REST + 前端 WebSocket 订阅的私有协议，客户端工具走文本协议 | chat / models / login |
| `notion` | Notion AI：`/api/v3` NDJSON 流私有协议，静态 Cookie（token_v2 + space_id）登录，无刷新态 | chat / models / login / refresh |
| `qoder` | Qoder：PAT → jobToken 签名换取，OpenAI 兼容端点，token 到期前自动轮换 | chat / models / login / refresh |
| `mirasim` | Mirasim 私有中继：邮件验证码 / 凭据导入登录，Ed25519 签名 + 封密元数据，Claude 走 messages、GPT 走 responses | chat / models / login / refresh |
| `cline` | Cline（api.cline.bot）：WorkOS 设备码授权 → refreshToken，OpenAI 兼容上游 | chat / models / login / refresh |
| `opencode` | OpenCode Zen：Zen / Zen Go 双池，API Key 直填（免费模型可匿名） | chat / models / login |
| `chatjimmy` | ChatJimmy（chatjimmy.ai）：匿名一键建档免 KEY，私有一次性纯文本响应 | chat / models / login |
| `improvado` | Improvado Agent：浏览器 Cookie 登录，SSE 纯文本流（无工具调用） | chat / login |
| `postman` | Postman Agent Mode：API Key（PMAK/PAT）/ 会话 Cookie 登录，反代团队子域网关 `/_gw/chat`（私有 SSE）；多团队（各团队子域建实例填 `base_url`） | chat / models / login / refresh / account / instances |
| `codebuff` | Codebuff（Freebuff 免费层）：粘贴 Bearer token（裸 token / curl / HAR 嗅探），OpenAI 兼容 + session/run 编排 | chat / models / login / refresh |
| `doubao` | 豆包（www.doubao.com）：Cookie 导入免 KEY，桌面客户端 SSE 私有协议 | chat / models / login |
| `ima` | 腾讯 ima：微信扫码 / Cookie 导入，SSE 私有协议（带刷新 token 自动续期） | chat / models / login / refresh |
| `joycode` | JoyCode（京东 AI 编程助手）：JD pt_key + userId 登录，color gateway HMAC 签名 OpenAI 兼容端点 | chat / models / login / refresh |
| `mimo` | Xiaomi MiMo：passToken 凭据导入 → 小米 SSO 换 serviceToken（401 自动刷新），OpenAI 兼容直通透传 | chat / models / login / refresh |
| `puter` | Puter 驱动调用反代：粘贴浏览器 auth_token（whoami 校验 + 月用量），NDJSON 流 | chat / models / login / refresh |
| `warp` | Warp 多代理 API：设备授权登录 → Firebase refresh token 周期续期，官方 ConnectRPC 协议 | chat / models / login / refresh |
| `zcode` | ZCode Proxy（GLM 编码套餐）：OAuth 设备码登录，Anthropic 端点直连（双密钥）+ JWT 网关 | chat / models / login / refresh / account |

### Lua 插件（`plugins/plugins-lua/`）

零 Go、零编译：一个目录一个插件，只需 `manifest.json`（name/version/author/label/icon）+ `main.lua`，CI 打平台无关 `.cphplugin`，由核心内置的 [LuaHost](../hosts/luahost) 运行时加载执行。

| 插件 | 说明 | 能力 |
| --- | --- | --- |
| `autoclaw` | AutoClaw（智谱 AutoGLM 加速上游）：手机验证码 / 凭据导入登录，token 自持（refresh_token 换 access_token，无需桌面端常驻），钱包余额 | chat / models / login / refresh |

### 开发中 / 规划中

| 插件 | 说明 | 状态 |
| --- | --- | --- |
| `devin` | Devin / Windsurf 反代 | 📋 开发中 |
| `loomy` | Loomy 反代 | 📋 规划中 |

## 目录约定

```
plugins/
├── plugins/<name>/       # Go 插件
│   ├── manifest.json     # name（= 目录名）、version、author、label、icon
│   ├── icon.png          # 可选，正方形 PNG 128–256px
│   └── *.go              # package main，入口 sdk.Serve(&plugin{})
├── plugins-lua/<name>/   # Lua 插件（零编译）
│   ├── manifest.json     # 同上（无需 protocol_version，打包时由 SDK 补）
│   ├── icon.png
│   └── main.lua          # 约定函数 return M（handshake/chat/models/login/refresh/profile）
├── tools/pack/           # 打包器：Go 交叉编译 / Lua 平台无关包，统一 .cphplugin + index.json
└── index.json            # 市场索引（CI 生成回写，勿手改；条目带 runtime 字段）
```

Go 插件实现 `pb.ClawPluginServer`（契约见核心 `sdk/proto/cph.proto`），复用 `sdk/openaiup` / `sdk/anthropicup` / `sdk/responsesup` 适配 OpenAI / Anthropic / Responses 方言上游；宿主回调（日志 / 存储 / 代理 / 设置）实现 `sdk.HostAware`。

Lua 插件跑在核心内置的 LuaHost 沙箱 VM 里：约定函数 `handshake/chat/models/login/refresh/profile`（与 Go 插件 Handshake 同构），宿主能力 `cph.*`（http/json/hash/time/random/log/openai）承接一切出站与日志。

契约细节、能力实现范式、多实例说明与文件分层建议见 **[AGENTS.md](AGENTS.md)**。参考核心 `examples/stub` 与既有插件。

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

- 包格式：统一 `.cphplugin`（zip 容器），含 `manifest.json`、图标与 `plugin-<os>-<arch>[.exe]`（windows/amd64、linux/amd64、linux/arm64、darwin/amd64、darwin/arm64）；Lua 插件跳过 go build、产平台无关 `.cphplugin`（包内只含 `main.lua` + 可选 `lib/*.lua`）；固定时间戳，同一输入产出同一 sha256。市场条目带 `runtime` 字段（`go` / `lua`）
- 发布：改 `manifest.json` 的 `version` → 合入 main → CI 为每个新版本创建 Release `<name>-v<version>`（资产 `<name>-<version>.cphplugin`）并回写 `index.json`
- 已发布版本不可变：改代码必须升版本，否则 CI 跳过该插件
- 核心默认市场地址：`https://raw.githubusercontent.com/ShadowSmallBaby/ClawProxyHubPlugins/main/index.json`

## 贡献

1. fork → `plugins/plugins/<你的插件>/`（Go）或 `plugins/plugins-lua/<你的插件>/`（Lua）开发（`manifest.json` 的 `author` 与 GitHub 用户名一致）
2. Go：`go vet ./... && go test ./... && go run ./tools/pack -only <你的插件>` 确认可构建；Lua：`go run ./tools/pack -only <你的插件>` 可打包即可
3. 提 PR，CI 会完整交叉编译一遍

## 许可证

与核心相同，[AGPL-3.0](LICENSE)。
