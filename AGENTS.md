# AGENTS.md — ClawProxyHub 插件开发指南

面向人与 AI 编程助手的插件编写规范。本仓库是 [ClawProxyHub](https://github.com/ShadowSmallBaby/ClawProxyHub) 的官方插件库，一个子目录一个插件。读完本文即可从零写出一个可被核心加载、可上架市场的插件。

> 契约真身是核心仓库的 `sdk/proto/cph.proto`（protocol v2）。本文是它的读法与落地范式，两者冲突时以 proto 为准。

---

## 1. 心智模型

- **进程模型**：每个插件是一个独立可执行文件，核心用 [hashicorp/go-plugin](https://github.com/hashicorp/go-plugin) 以 gRPC 子进程方式拉起。核心是 gRPC 客户端调用插件（`ClawPlugin` 服务），插件反向调用核心（`ClawHost` 服务）。
- **唯一耦合点**：`cph.proto`。核心不认识任何具体插件，插件只实现契约。握手时双方校验 `protocol_version`，不一致直接拒载。
- **凭据代管**：账号凭据是插件自定义格式的 opaque `blob`，核心只存不解析。插件通过 RPC 返回值把变更后的 blob 交回核心持久化。
- **实例维度（v2）**：一个插件可挂多个「实例」（= 一个站点 / 部署）。核心固定提供 `name + base_url`，插件用 `instance_schema` 声明站点特有字段。登录 / 刷新 / 设置 / 任务回调都按 `instance_id` 区分。未声明 `instances` 能力的插件只有一个默认实例。
- **统一信封**：核心网关把 OpenAI / Anthropic / Responses 三种协议入口归一化成一个 `ChatRequest` 信封投递给插件，插件转成上游方言、回吐统一 `StreamEvent` 事件流，核心再转回各协议的 SSE。

```
客户端(任意协议) → 核心网关(归一化) → ChatRequest信封 → 插件 → 上游方言
上游SSE → 插件(Parser) → StreamEvent事件流 → 核心网关(转回) → 客户端
```

---

## 2. 目录约定与文件分层

### 2.1 包目录（硬约定）

```
plugins/<name>/
├── manifest.json   # 必需：name(=目录名)/version/author/label/icon/protocol_version
├── icon.png        # 可选：正方形 PNG 128–256px
└── *.go            # package main，入口 sdk.Serve(&plugin{})
```

- `name` 必须等于目录名，全局唯一。
- 所有 `.go` 同属 `package main`，如何拆文件是插件内部自由。

### 2.2 推荐文件分层（按职责拆，不是按文件数强求）

单文件够用就单文件（见 `lobsterai` / `cline`）；逻辑变重时按下面职责切开，一个文件头一句注释写清「这个文件管什么」。命名对齐既有插件，AI 助手据此定位：

| 文件 | 职责 | RPC / 关注点 |
| --- | --- | --- |
| `main.go` | 入口 + 骨架：`sdk.Serve`、`plugin` struct、`SetHost`、`Handshake`(manifest)、HTTP client、凭据 blob 解析、站点/设置读取 | 进程生命周期、Manifest |
| `auth.go` | 登录多步流程、会话自举、token 交换 | `Login` |
| `account.go` | 登录校验、资料与余额、刷新 | `Refresh` / `Get Profile`（也可与 auth 合并） |
| `chat.go` | 统一信封 → 上游请求体 → 事件流 | `Chat` |
| `models.go` | 模型目录（静态表或动态发现） | `ListModels` |
| `task.go` / `activities.go` … | 任务能力：声明 + 执行（签到 / 成长任务等），重逻辑再细拆 | `ListTaskCapabilities` / `RunTask` |
| `fingerprint.go` | 设备指纹 / 客户端伪装（形态对齐目标 CLI） | Chat 请求头 |
| `upstream.go` | 上游 HTTP/WebSocket 客户端封装（REST 调用集中处） | 被 chat/account/task 复用 |
| `parser.go` / `envelope.go` | 上游私有协议 ↔ 统一信封转换（上游非标准 SSE 时才需要） | Chat |
| `toolproto.go` | 客户端工具调用的文本协议（上游不能执行客户端工具时） | Chat |
| `sanitizer.go` | 请求体净化 / 特征改写 | Chat |
| `*_test.go` | 指纹稳定性、协议解析、余额折算等纯函数单测 | — |

### 2.3 仓库级布局

```
plugins/<name>/     # 各插件
tools/pack/         # 打包器：交叉编译 + .cphplugin + index.json
index.json          # 市场索引（CI 生成回写，勿手改）
go.mod / go.work    # go.work 已忽略，本地开发覆盖用
```

---

## 3. manifest.json 规范

源文件只写这几个字段；`protocol_version` 打包时由 SDK 补齐并注入二进制，保证声明与实际握手版本一致。

```json
{
  "name": "<name>",
  "version": "<version>",
  "author": "<auth>",
  "label": { "zh": "<zhName>", "en": "<enName>" },
  "icon": "icon.png",
  "protocol_version": 2
}
```

- `name`：= 目录名，唯一。
- `version`：语义化版本，**唯一版本来源**。改代码必须升版本，否则 CI 跳过（已发布版本不可变）。
- `author`：贡献者应与 GitHub 用户名一致。
- `label`：多语言展示名。
- `icon`：包内相对路径。
- `protocol_version`：当前为 `2`。

其余能力（capabilities / auth_methods / schema / endpoints）在 `Handshake` 返回的 `Manifest` 里声明，不写进 json（见 §5.1）。

---

## 4. 契约总览

### 4.1 核心调用插件（`ClawPlugin`）

| RPC | 触发时机 | 必要性 |
| --- | --- | --- |
| `Handshake` | 安装 / 启动 | **必需**，返回 Manifest + 校验协议版本 |
| `Login` | 用户添加账号 | 声明 `login` 能力时必需 |
| `Refresh` | 定时 / 手动刷新凭据 | 声明 `refresh` 能力时必需 |
| `GetProfile` | 查看账号资料余额 | 声明 `account` 能力时必需 |
| `ListModels` | 同步模型目录 | 声明 `models` 能力时必需 |
| `Chat` | 每次对话请求（流式） | 声明 `chat` 能力时必需，核心链路 |
| `ListTaskCapabilities` / `RunTask` | 任务调度 | 声明 `tasks` 能力时必需 |

未声明的能力对应 RPC 可留空（用 `pb.UnimplementedClawPluginServer` 兜底）。

### 4.2 插件反向调用核心（`ClawHost`，经 `sdk.Host`）

实现 `sdk.HostAware`（`SetHost(*sdk.Host)`）后由核心注入：

| 方法 | 用途 |
| --- | --- |
| `host.Log(level, msg)` | 统一日志管道 |
| `host.StoreGet/StorePut(key, val)` | 凭据之外的小状态读写 |
| `host.InstanceSettings(plugin, instanceID)` | 读实例视图设置（插件设置 ← 实例设置 ← base_url） |
| `GetProxy`（核心自动在 blob 里带 `proxy`） | 出站代理，账号级优先 |

`SetHost` 里应立刻把 `host` 存住，并（若需要）预建连接——broker 连接信息只短暂有效。

---

## 5. 能力实现指南

### 5.0 最小骨架（main.go）

```go
package main

import (
	"context"
	"fmt"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

const pluginName = "myplugin"

// 打包时经 -ldflags "-X main.version=..." 注入；源码直跑为 dev。
var version = "dev"

func main() { sdk.Serve(&plugin{}) }

type plugin struct {
	pb.UnimplementedClawPluginServer // 未实现的 RPC 自动兜底
	host *sdk.Host
}

func (p *plugin) SetHost(host *sdk.Host) { p.host = host }
```

### 5.1 Handshake（Manifest 声明）

握手是唯一必需 RPC。先校验协议版本，再返回 Manifest。**能力全在这里声明**：

```go
func (p *plugin) Handshake(ctx context.Context, req *pb.HandshakeRequest) (*pb.HandshakeResponse, error) {
	if req.ProtocolVersion != sdk.ProtocolVersion {
		return &pb.HandshakeResponse{Error: &pb.Error{
			Code: 1, Message: fmt.Sprintf("protocol mismatch: core=%d plugin=%d", req.ProtocolVersion, sdk.ProtocolVersion),
		}}, nil
	}
	return &pb.HandshakeResponse{Manifest: &pb.Manifest{
		Name:            pluginName,
		Version:         version,
		Author:          "you",
		Label:           map[string]string{"zh": "我的插件", "en": "My Plugin"},
		ProtocolVersion: sdk.ProtocolVersion,
		Capabilities:    []string{"chat", "models", "login", "refresh", "tasks", sdk.CapabilityInstances},
		Endpoints:       []string{"chat_completions", "messages", "responses"},
		SettingsSchema:  `{"type":"object","properties":{}}`,
		InstanceSchema:  `{"type":"object","properties":{ ... }}`, // 声明 instances 能力时的站点字段
		AuthMethods:     []*pb.AuthMethod{ /* 见 §5.2 */ },
	}}, nil
}
```

**capabilities 取值**：`account` / `login` / `refresh` / `models` / `chat` / `tasks` / `instances`(= `sdk.CapabilityInstances`)。

**endpoints**：插件能处理的对外协议方言，网关据此归一化投递。空 = `chat_completions + messages`。要接 Codex / Responses 类客户端就加 `responses`。声明之外的入口会被网关拒绝。

**schema**：`SettingsSchema` 是插件级设置表单（JSON Schema，仪表盘动态渲染）；`InstanceSchema` 是每个站点实例的附加字段。核心固定提供 name + base_url，无需在 schema 里重复。

### 5.2 AuthMethod（登录方式，核心动态渲染）

核心不硬编码任何登录方式，全由插件声明字段、前端动态渲染表单：

```go
AuthMethods: []*pb.AuthMethod{
	{
		Id:           "api_key",
		Label:        map[string]string{"zh": "API 密钥", "en": "API Key"},
		Capabilities: []string{"refreshable"}, // refreshable / auto_relogin / profile
		Fields: []*pb.AuthField{
			{Name: "api_key", Label: map[string]string{"zh": "API 密钥"}, Type: "password", Required: true, Placeholder: "sk-..."},
		},
	},
	{
		Id:       "oauth",
		Label:    map[string]string{"zh": "浏览器授权"},
		Callback: "auto_wait", // auto / wait / auto_wait，见 proto AuthMethod.callback
	},
}
```

- `AuthField.type`：`text` / `password` / `textarea` / `file` / `phone`。
- `Callback`：浏览器授权回调形态。`auto`=插件自动收回调、前端轮询；`wait`=用户手动粘贴回调地址；`auto_wait`=本机访问按 auto、否则 wait。

### 5.3 Login（可多步）

`Login` 返回 `LoginResult`：要么完成（`blob` + `profile`），要么给下一步（`next`）。多步状态用 `next.state` 签发、原样回传（`req.State`）。

```go
func (p *plugin) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResult, error) {
	switch req.MethodId {
	case "api_key":
		key := req.Form["api_key"]
		// 校验 key、拉一次 profile ...
		blob, _ := json.Marshal(credential{APIKey: key})
		return &pb.LoginResult{Blob: blob, Profile: &pb.AccountProfile{DisplayName: "***"}}, nil
	case "oauth":
		if req.State == nil { // 第一步：给登录链接
			return &pb.LoginResult{Next: &pb.LoginNextStep{
				Action: "open_url", Url: authURL, Wait: true,
				State:  []byte(pkceVerifier),
			}}, nil
		}
		// 第二步：拿回调 code 换 token ...
	}
	return &pb.LoginResult{Error: &pb.Error{Code: 1, Message: "unsupported method"}}, nil
}
```

凭据 blob 格式插件自定（通常一个 `credential` struct + `json.Marshal`）。核心注入的 `instance_id` / `account_id` / `proxy` 用 `json:"-"` 标记，不参与序列化。

### 5.4 Refresh / GetProfile

- `Refresh(CredentialBlob) → RefreshResult`：刷新 token / 会话。`blob` 为空表示无需变更；变更则回传新 blob 由核心持久化。可带 `notification` 触发站内通知（如密钥更换提醒重同步模型）。
- `GetProfile(CredentialBlob) → AccountProfile`：余额与资料。`quota` 标准键 `credits` / `total_credits` / `used_credits`；精细明细走 `credits_json`（十进制字符串防精度丢失）；签到状态 / 成长计划等动态块用 `sections`（核心通用渲染 entries→descriptions、items→表格）。

刷新健壮性：静态密钥类（无可刷新态）应原样返回不报错；改名 / 会话过期要能自动重登。

### 5.5 Chat（核心链路：统一信封 → 事件流）

`Chat` 是流式 RPC。核心把请求归一化为 `ChatRequest` 信封，插件转上游方言、把上游 SSE 翻成 `StreamEvent` 回吐。**九成情况直接复用 SDK 的三个上游适配器**，不用手撸协议转换：

| 适配器 | 上游方言 | API |
| --- | --- | --- |
| `sdk/openaiup` | OpenAI `/chat/completions` | `openaiup.ChatBody(req)` → 请求体；`openaiup.NewParser(emit)` → SSE 解析 |
| `sdk/anthropicup` | Anthropic `/messages` | `anthropicup.ChatBody(req)`；`anthropicup.NewParser(emit)` |
| `sdk/responsesup` | OpenAI Responses `/responses` | `responsesup.ChatBody(req)`；`responsesup.NewParser(emit)` |

典型 Chat 实现（OpenAI 兼容上游）：

```go
func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(failed(1, err.Error()))
	}

	body := openaiup.ChatBody(req)            // 信封 → 上游请求体
	body["model"] = mapModel(req.GetModel())  // 对外短名 → 上游真实 id

	resp, err := p.postStream(ctx, cred, body) // 发上游、拿 SSE
	if err != nil {
		return stream.Send(failed(1, err.Error()))
	}
	defer resp.Body.Close()

	parser := openaiup.NewParser(func(ev *pb.StreamEvent) { stream.Send(ev) })
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		parser.Feed(sc.Text()) // 逐行喂 SSE（Feed 收 string），Parser 翻成 StreamEvent
	}
	return nil
}

// 统一的失败事件
func failed(code int32, msg string) *pb.StreamEvent {
	return &pb.StreamEvent{Event: &pb.StreamEvent_TaskFailed{
		TaskFailed: &pb.TaskFailed{Error: &pb.Error{Code: code, Message: msg}},
	}}
}
```

**StreamEvent 事件类型**（oneof）：`MessageStart`（模型 + 开头 usage）→ `ContentDelta`（文本增量）/ `ReasoningDelta`（推理增量）/ `ToolCallDelta`（工具调用增量）→ `MessageFinish`（finish_reason + usage）；出错发 `TaskFailed`。

**信封里的关键透传**（`ChatRequest.extra`）：

- `sdk.ExtraClientUserAgent`：对话应使用的 UA（核心按 路由 > 全局 > 客户端 解析后注入），按需透传上游。
- `sdk.ExtraFingerprintHeaders`：按入口协议生成的客户端指纹头（JSON map），按需采用。

**Usage 语义（重要，采用 Anthropic 语义）**：`input_tokens` 是**非缓存**输入，`cached_tokens`（缓存读）、`cache_creation_tokens`（缓存写）单列，三者之和才是总输入。OpenAI 系的 `prompt_tokens`（含缓存）由 SDK 解析器自动拆开、网关出口再合回——用 SDK Parser 就无需自己处理。

**上游是非标准协议时**（NDJSON / WebSocket / 私有事件）：SDK Parser 用不上，自写 `parser.go` 把上游事件翻成 `StreamEvent`（参考 `commandcode/parser.go` 的 NDJSON、`todofor` 的 WebSocket）。请求体转换同理自写 `envelope.go`。

**上游不能执行客户端工具时**：用文本协议教上游"用严格文本块表达工具调用"，再在 Parser 里解析回 `ToolCallDelta`（参考 `todofor/toolproto.go`）。

### 5.6 ListModels（模型目录）

静态表或动态发现皆可。动态发现拉上游 `/models`，对外用短名（冲突退回全名），Chat 时再映射回真实 id：

```go
func (p *plugin) ListModels(ctx context.Context, blob *pb.CredentialBlob) (*pb.ModelList, error) {
	return &pb.ModelList{Models: []*pb.ModelInfo{
		{Id: "my-model", Label: map[string]string{"zh": "我的模型"}, ContextWindow: 200000, SupportsTools: true, SupportsStream: true},
	}}, nil
}
```

### 5.7 Tasks（任务：核心管"何时"，插件管"做什么"）

核心负责调度，插件声明能力并执行。签到 / 成长任务是典型场景。

```go
func (p *plugin) ListTaskCapabilities(ctx context.Context, req *pb.TaskCapabilitiesRequest) (*pb.TaskCapabilities, error) {
	// req.InstanceId>0 时可按实例配置裁剪（如该实例关了签到就不声明）
	return &pb.TaskCapabilities{Capabilities: []*pb.TaskCapability{
		{Id: "checkin", Label: map[string]string{"zh": "每日签到"}, Kind: "recurring", PerAccount: true, DefaultSchedule: "daily 09:00"},
	}}, nil
}

func (p *plugin) RunTask(ctx context.Context, req *pb.RunTaskRequest) (*pb.RunTaskResponse, error) {
	// req.Credential 逐账号注入（per_account）；执行后回报
	return &pb.RunTaskResponse{
		Changed:      false,        // 有凭据变更则 true + 回传 blob
		Summary:      "签到成功",     // 写执行历史
		DetailJson:   detailJSON,   // 结构化明细快照（可选）
		Notification: nil,          // 需站内提醒时给 TaskNotification
	}, nil
}
```

- `kind`：`once` / `recurring` / `both`。`per_account`：是否逐账号执行。
- 业务性跳过（无门槛 / 今日已签）归"跳过"，别当失败——把状态写进 `summary`。

---

## 6. 指纹与请求净化（伪装类插件）

反代官方 CLI 类上游时，常需让上游"看起来"是真实客户端。两个惯用手法：

- **设备指纹**（`fingerprint.go`）：形态与哈希逐字对齐目标 CLI。信号值**不读宿主真机**，而是按凭据（如 apiKey）**确定性伪造**——同一 key 永远同一台设备。重启 / 多实例 / 停用恢复后上游都看到同一设备（换指纹本身是可疑信号）。务必写单测锁死指纹稳定性。
- **请求净化**（`sanitizer.go`）：改写 system 特征文本（如把 Claude Code 特征换成目标客户端），对合规声明高频词做零宽字符（U+200B）脱敏——打断后端关键词匹配，模型 / 人眼读起来无差别。敏感词按长度降序编译，避免短词先吃长词。

> 这类逻辑高度依赖目标上游，参考 `commandcode/fingerprint.go`、`workbuddy/sanitizer.go`。改动后必须重编二进制才生效。

---

## 7. 宿主回调与设置

实现 `SetHost` 后可反向调用核心：

```go
func (p *plugin) SetHost(host *sdk.Host) {
	p.host = host
	host.Log("info", "plugin started")

	// 读实例视图设置：插件设置 ← 实例设置 ← {"base_url": 实例地址}
	raw := host.InstanceSettings(pluginName, instanceID)

	// 凭据之外的小状态
	host.StorePut("last_run", []byte("..."))
	val, ok := host.StoreGet("last_run")
}
```

- **设置读取**：`InstanceSettings(plugin, instanceID)` 返回合并视图 JSON，结构由 `settings_schema` + `instance_schema` 定义。`instanceID=0` 等价插件级 `Settings`。建议加短 TTL 缓存（如 30s，见 `newapi` 的 `site()`）。
- **保留键**：`sdk.SettingBrowserUserAgent`（全局浏览器 UA）合并进设置视图，空则用插件内置值。
- **代理**：核心自动在 `CredentialBlob.proxy` 里带上账号所属分组 / 账号级代理，插件解析成 proxy URL 构造 `http.Client` 即可，无需主动调 `GetProxy`。
- **连接时机**：宿主 Accept 的 broker 连接信息只保留约 5s，`SetHost` 里应尽早触发建连（SDK 已自动 `go host.conn()`）。

---

## 8. 开发 · 打包 · 发布 · 测试

### 8.1 依赖 SDK

SDK 来自核心模块 `github.com/ShadowSmallBaby/ClawProxyHub`（`go.mod` 固定到某提交）。对着本地核心源码开发用 workspace 覆盖（`go.work` 已忽略，不入库）：

```bash
go work init .
go work edit -replace github.com/ShadowSmallBaby/ClawProxyHub=../ClawProxyHub
```

升级 SDK 版本（绕过 workspace）：

```bash
GOWORK=off go get github.com/ShadowSmallBaby/ClawProxyHub@main && GOWORK=off go mod tidy
```

### 8.2 本地热部署

```bash
# 编译当前平台并装进核心插件目录（核心运行中会锁二进制，先在插件页停止该插件）
go run ./tools/pack -install ../ClawProxyHub/data/plugins
```

核心运行中通常可覆盖（旧文件被改名为 `.exe~`）；核心侧改动走 GoLand 运行配置，需 IDE 重启。验证优先直读 DB（`request_logs` 等）而非只看界面。

### 8.3 打包

```bash
go run ./tools/pack                 # dist/<name>-<version>.cphplugin + dist/index.json
go run ./tools/pack -only workbuddy # 只打某个
go run ./tools/pack -skip lobsterai # 跳过已发布版本，索引沿用现有 index.json
```

- 包格式：zip，含 `manifest.json` + 图标 + `plugin-<os>-<arch>[.exe]`（windows/amd64、linux/amd64、linux/arm64、darwin/amd64、darwin/arm64）。
- 固定时间戳，同一输入产出同一 sha256（可复现）。
- **`index.json` 陷阱**：`-skip` 沿用现有索引条目，别 reset 破坏基线，否则已发布插件条目会永久丢失、CI 补不回；sha 须匹配现存 release。

### 8.4 发布（CI 驱动）

1. 改 `manifest.json` 的 `version` → 合入 `main`。
2. CI 为每个新版本创建 Release `<name>-v<version>`（资产 `<name>-<version>.cphplugin`）并回写 `index.json`。
3. 已发布版本不可变：改代码必须升版本，否则 CI 跳过该插件。
4. 核心默认市场地址：`https://raw.githubusercontent.com/ShadowSmallBaby/ClawProxyHubPlugins/main/index.json`。

### 8.5 测试

对纯函数写单测：指纹稳定性、协议解析（Parser）、余额折算、净化正确性等（见 `*_test.go`）。提交前跑：

```bash
go vet ./... && go test ./... && go run ./tools/pack -only <你的插件>
```

---

## 9. 新插件检查清单

- [ ] `plugins/<name>/`，`name` = 目录名 = `manifest.json` 的 `name`，全局唯一。
- [ ] `manifest.json` 五字段齐全，`version` 语义化，`protocol_version: 2`。
- [ ] `main.go`：`sdk.Serve(&plugin{})`、`var version`、`SetHost`、`Handshake` 校验协议版本。
- [ ] `Handshake` 声明的 `capabilities` 与实际实现的 RPC 一致；未实现的 RPC 用 `UnimplementedClawPluginServer` 兜底。
- [ ] 声明 `chat`：Chat 流式实现，优先复用 `openaiup`/`anthropicup`/`responsesup`，Usage 走 Anthropic 语义。
- [ ] 声明 `login`：blob 格式自定，核心注入字段用 `json:"-"`；多步用 `next.state`。
- [ ] 声明 `instances`：填 `instance_schema`，所有回调按 `instance_id` 区分。
- [ ] 声明 `tasks`：`ListTaskCapabilities` + `RunTask`，业务跳过不当失败。
- [ ] 纯函数有单测；`go vet ./... && go test ./...` 通过。
- [ ] `go run ./tools/pack -only <name>` 可交叉编译成包。
- [ ] `author` 与 GitHub 用户名一致，提 PR 由 CI 完整交叉编译。

---

## 10. 参考

- 契约真身：核心 `sdk/proto/cph.proto`
- SDK：核心 `sdk/sdk.go`、`sdk/openaiup`、`sdk/anthropicup`、`sdk/responsesup`
- 样板：核心 `examples/stub`（最小）；本库 `newapi`（auth/account/chat 分层）、`todofor`（WebSocket + 工具协议）、`workbuddy`（任务 + 净化 + 埋点）、`commandcode`（指纹 + NDJSON 解析）
- 许可证：与核心相同，[AGPL-3.0](LICENSE)
