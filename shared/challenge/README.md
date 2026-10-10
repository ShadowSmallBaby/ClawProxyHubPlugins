# 验证服务客户端

独立 Go 包，不依赖 SDK、宿主设置、数据库或扩展发现。客户端不会启动浏览器或验证服务。

```go
client, err := challenge.NewClient(challenge.Config{
    Adapter: "ezsolver",
    BaseURL: "http://127.0.0.1:8191",
    Timeout: 90 * time.Second,
})
if err != nil { return err }
result, err := client.Solve(ctx, challenge.Request{
    Kind: challenge.TurnstileToken,
    PageURL: "https://example.com/checkin",
    SiteKey: siteKey,
})
```

调用方使用 `Adapters()` 与 `Supports(kind)` 筛选服务选项；`disabled` 由插件处理，不是适配器。每次返回的元数据可以独立修改。

## EzSolver 协议

固定源码版本：[ismoiloffS/EzSolver@db9d061](https://github.com/ismoiloffS/EzSolver/blob/db9d061409a6bc913ddbc779eeaae1bd58994f4e/service.py)，于 2026-10-10 核查。

- 请求：`POST <BaseURL>/solve`，JSON 为 `sitekey`、`siteurl`、整数秒 `timeout`。
- 成功：HTTP 200，非空字符串 `token`；`elapsed` 不作为过期时间。
- 失败：原服务 HTTP 500，`error` 字段；客户端不回显远端错误正文。
- 仅支持 Turnstile token；拒绝 action、cData、目标代理、指定 User-Agent、Cookie 和 clearance 请求。
- 可选 `AccessToken` 发送为 Bearer 头，用于前置网关；原服务不提供内置认证。
- 前缀 URL 如 `https://solver.example/prefix` 请求 `/prefix/solve`，需反向代理剥离前缀后转给原服务。

BaseURL 仅允许 HTTP(S)，支持回环、内网及路径前缀，拒绝用户名密码、查询与 fragment；PageURL 可含查询，不应包含敏感会话信息。服务地址从插件进程所在机器或容器访问。

## 超时与错误

配置超时默认 90 秒，范围 5–300 秒；每次调用取配置与 context 剩余期限的较小值，覆盖连接、服务排队等待及读取响应。HTTP 不跟随重定向，响应上限 1 MiB，不自动重试 POST，不缓存 token。服务请求与目标代理互相独立。

`*Error` 提供稳定 Code、脱敏 Message、HTTPStatus 和 Retryable。取消可用 `errors.Is(err, context.Canceled)` 判断，超时可用 `errors.Is(err, context.DeadlineExceeded)` 判断。Retryable 仅供调度决策，不代表可重放已有签到请求或旧 token。

**服务端限制：**上述 EzSolver 版本的 semaphore 排队与浏览器任务不感知客户端断开。客户端超时会立即结束等待，但不能保证服务端任务随即释放；服务端队列和资源回收由部署者负责。原服务自身会记录页面 URL、sitekey 和部分 token，客户端的日志脱敏不改变服务端行为。

## 验证记录

本地 `httptest` 已覆盖请求协议、路径前缀、认证、元数据隔离、不支持参数、状态码、无效 JSON、超大响应、取消、期限传递、禁止重定向及不缓存/重试。

尚无指定的真实 NewAPI 实例和可用服务部署，未运行浏览器求解与真实签到验收。协议测试通过不代表站点接受求解结果。
