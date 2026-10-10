# ClawProxyHubPlugins

[ClawProxyHub](https://github.com/ShadowSmallBaby/ClawProxyHub) 的官方插件仓库：每个子目录一个插件，合入 main 后由 CI 构建、发布 Release 并更新市场索引，核心的「插件市场」默认从本仓库安装。

> 想写插件？先读 **[AGENTS.md](AGENTS.md)** —— 面向人与 AI 助手的完整开发指南（契约、分层、各能力实操、打包发布）。

插件使用契约 **protocol v2**（实例维度）。

插件有两种运行时：**Go 插件**（编译二进制，`plugins-go/`）与 **Lua 插件**（脚本，零编译，`plugins-lua/`，由宿主提供的 Lua Host 运行时加载，见 [AGENTS.md](AGENTS.md) §11）。

## 插件清单

### Go 插件（`plugins-go/`）

| 插件 | 说明 | 能力 |
| --- | --- | --- |
| `cnb` | CNB 仓库 AI 网关：流水线 Token、仓库实例、OpenAI SSE 与原生工具调用 | chat / models / login / instances |
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
| `codearts` | 华为云 CodeArts Agent：IAM AK/SK 凭据、签名请求与每日签到 | chat / models / login / refresh / account / tasks |
| `devin` | Devin：会话认证、模型目录与对话转发 | chat / models / login / refresh / account |
| `loomy` | 讯飞 Loomy：Web Cookie 认证、纯文本对话 | chat / models / login / refresh / account / tasks |
| `raccoon` | 商汤 Raccoon Work：扫码登录、Bearer 认证与对话转发 | chat / models / login / refresh / account / tasks |
| `trae` | TRAE CN：浏览器授权、Token 导入与自动续期 | chat / models / login / refresh / account / tasks |

### Lua 插件（`plugins-lua/`）

零 Go、零编译：一个目录一个插件，只需 `manifest.json`（name/version/author/label/icon）+ `main.lua`，CI 打平台无关 `.cphplugin`，由宿主提供的 [Lua Host](https://github.com/ShadowSmallBaby/ClawProxyHub/tree/develop/hosts/luahost) 运行时加载执行。

| 插件 | 说明 | 能力 |
| --- | --- | --- |
| `autoclaw` | AutoClaw（智谱 AutoGLM 加速上游）：手机验证码 / 凭据导入登录，token 自持（refresh_token 换 access_token，无需桌面端常驻），钱包余额 | chat / models / login / refresh |

## 目录约定

```
plugins/
├── plugins-go/<name>/       # Go 插件
│   ├── manifest.json     # name（= 目录名）、version、author、label、icon
│   ├── icon.png          # 可选，正方形 PNG 128–256px
│   ├── *.go              # 可导入业务包，提供 New(version)
│   └── cmd/main.go       # 桌面 package main 入口
├── plugins-lua/<name>/   # Lua 插件（零编译）
│   ├── manifest.json     # 同上（无需 protocol_version，打包时由 SDK 补）
│   ├── icon.png
│   └── main.lua          # 约定函数 return M（handshake/chat/models/login/refresh/profile）
├── tools/pack/           # 打包器：Go 交叉编译 / Lua 平台无关包，统一 .cphplugin + index.json
├── android/              # 独立 Android 原生插件构建、签名与包校验
└── index.json            # 市场索引（CI 生成回写，勿手改；条目带 runtime 字段）
```

Go 插件实现 `pb.ClawPluginServer`（契约见核心 `sdk/proto/cph.proto`），复用 `sdk/openaiup` / `sdk/anthropicup` / `sdk/responsesup` 适配 OpenAI / Anthropic / Responses 方言上游；宿主回调（日志 / 存储 / 代理 / 设置）实现 `sdk.HostAware`。

Go 插件业务包位于插件根目录，提供 `New(version)`，桌面入口位于 `cmd/main.go`。手动编译 newapi 使用 `go build -o build/newapi ./plugins-go/newapi/cmd`，Android 入口直接导入 `plugins-go/newapi` 包。打包器兼容根目录为 `package main` 的桌面插件。

Lua 插件跑在宿主提供的 Lua Host 沙箱 VM 里：约定函数 `handshake/chat/models/login/refresh/profile`（与 Go 插件 Handshake 同构），宿主能力 `cph.*`（http/json/hash/time/random/log/openai）承接一切出站与日志。

契约细节、能力实现范式、多实例说明与文件分层建议见 **[AGENTS.md](AGENTS.md)**。参考核心 `examples/stub` 与既有插件。

## 开发

SDK 来自核心模块 `github.com/ShadowSmallBaby/ClawProxyHub`，`go.mod` 固定到已发布的版本 tag，正常构建直接使用远程依赖，无需本地 `replace`、workspace 或 SDK 源码副本。

```bash
go build ./... && go test ./...

# 编译当前平台并装进核心的插件目录（核心运行中会锁住二进制，先在插件页停止该插件）
go run ./tools/pack -install ../ClawProxyHub/data/plugins
```

上例安装路径适用于两个仓库并列检出；作为核心的 `plugins/` 子模块开发时，使用 `-install ../data/plugins`。

升级 SDK 时，先发布核心 tag，再执行 `go get github.com/ShadowSmallBaby/ClawProxyHub@vX.Y.Z` 和 `go mod tidy`（将 `vX.Y.Z` 换成实际发布的 tag）。验证后递增已发布插件的 `manifest.json` 补丁版本，未发布插件无需单独递增版本。

## 打包与发布

```bash
go run ./tools/pack            # build/<name>-<version>.cphplugin + build/index.json
go run ./tools/pack -only workbuddy
```

- 包格式：统一 `.cphplugin`（zip 容器），含 `manifest.json`、图标与 `plugin-<os>-<arch>[.exe]`（windows/amd64、linux/amd64、linux/arm64、darwin/amd64、darwin/arm64）；Lua 插件跳过 go build、产平台无关 `.cphplugin`（包内含清单、`main.lua`、可选图标及 `lib/*.lua`）；固定时间戳，同一输入产出同一 sha256。市场条目带 `runtime` 字段（`go` / `lua`）
- 发布：改 `manifest.json` 的 `version` → 合入 main → CI 为每个新版本创建 Release `<name>-v<version>`（资产 `<name>-<version>.cphplugin`）并回写 `index.json`
- 已发布版本不可变：改代码必须升版本，否则 CI 跳过该插件
- 核心默认市场地址：`https://raw.githubusercontent.com/ShadowSmallBaby/ClawProxyHubPlugins/main/index.json`

## 贡献

1. fork → `plugins-go/<你的插件>/`（Go）或 `plugins-lua/<你的插件>/`（Lua）开发（`manifest.json` 的 `author` 与 GitHub 用户名一致）
2. Go：`go vet ./... && go test ./... && go run ./tools/pack -only <你的插件>` 确认可构建；Lua：`go run ./tools/pack -only <你的插件>` 可打包即可
3. 提 PR，CI 会完整交叉编译一遍

## 许可证

与核心相同，[AGPL-3.0](LICENSE)。

## Android 原生插件发布

`build.yml` 同时构建桌面包与 Android arm64-v8a 的独立 `.cphplugin`。本库 [android/](android/README.md) 独立维护入口生成、原生编译、签名与包校验，复用业务工厂，不产生插件 APK，也不检出主 APP 工程。Lua 包继续平台无关。x86_64 原生插件只用于本地开发、模拟器测试，不在 CI 中构建。

两个仓库的 GitHub Actions Secrets 必须配置同一套 RSA keystore：`CPH_ANDROID_KEYSTORE_BASE64`、`CPH_ANDROID_STORE_PASSWORD`、`CPH_ANDROID_KEY_ALIAS`、`CPH_ANDROID_KEY_PASSWORD`。主 APP 仅信任与自身发行证书相同的原生插件。PR 使用 debug 身份做验证，不发布。

SDK 由本库 `go.mod` 统一锁定，构建器关闭 Go workspace，要求依赖版本包含 `sdk/androidplugin`。`checkAndroidSdk` 会提前提示缺失依赖；升级方法、构建命令与环境变量见 [Android 构建说明](android/README.md)。

每个插件使用 `<name>-v<version>` Release：桌面兼容资产 `<name>-<version>.cphplugin`，Android 资产 `<name>-<version>-android-arm64.cphplugin`。已发布资产只校验复用，禁止覆盖。工作流只在 main 发布，全部构建、校验与资产上传成功后才回写唯一的 `index.json`。

脚本回归：`python3 -m unittest discover -s tools -p '*_test.py'`。


### 平台发布清单

每个 Release 的 `manifest.json` 列出各平台包，`index.json` 通过 `release_manifest` 指向它，并保留兼容包地址。宿主验证清单后选择平台包；没有发布清单的旧索引使用兼容包，已声明清单的校验失败不会降级下载。字段定义、完整示例和生成流程见 [PACKAGING.md](PACKAGING.md)。
