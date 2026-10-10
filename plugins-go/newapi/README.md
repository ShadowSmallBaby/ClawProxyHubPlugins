# New API 插件

## 验证服务配置（0.1.7）

插件设置新增：

| 字段 | 默认值 | 含义 |
| --- | --- | --- |
| `challenge_adapter` | `disabled` | 关闭或 EzSolver HTTP |
| `challenge_service_url` | 空 | 用户部署的服务基础 URL，可含路径前缀 |
| `challenge_service_token` | 空 | 可选 Bearer 网关密钥，密码框输入 |
| `challenge_timeout_seconds` | 90 | 整次调用上限，5–300 秒 |

实例设置另有 `challenge_enabled`（默认 `false`），用于逐站点启用或停用 Turnstile 验证；服务地址、适配器和密钥统一由插件级配置提供。

这些字段仅属于插件级服务配置，读取时不使用实例合并视图，每次创建客户端重新读取；关闭时忽略连接配置校验。设置由核心原有机制保存，密码框仅隐藏显示，不新增静态加密。

**当前交付为客户端与配置准备，尚未接通自动验证签到。** 尚未提供真实目标实例版本、挑战参数及 token 提交证据，因此 `checkinByAPI` 仍沿用已有请求，不因选择 EzSolver 自动求解或重试。`challenge.go` 中的客户端入口待站点适配确认后调用。登录签到、刷新签到、人工签到等模式保持现有行为。

部署协议与服务端限制见 [shared/challenge](../../shared/challenge/README.md)。网页插件设置表单需使用同时支持 `oneOf`、`format: password`、`integer` 的新版核心前端。

## 接通与验收还需的信息

目标实例 URL、版本或分支，以及脱敏的签到请求/响应证据：验证是否必需、SiteKey 来源、PageURL、action/cData、登录态或代理要求、token 提交字段、验证失败和会话过期的区别。还需可访问的 EzSolver 部署供真实验收。

确认后在 `challenge.go` 实现该站点适配，并在签到路径中区分挑战拒绝与账号过期，禁止 token 经通用重登逻辑重复提交。EzSolver 不支持的必要上下文应明确拒绝，不能通过丢弃参数宣称支持。

本次运行 NewAPI 原有回归及新增配置/客户端测试；真实站点兼容性尚未验证。
