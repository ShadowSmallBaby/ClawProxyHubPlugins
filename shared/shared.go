// Package shared 提供各插件通用且行为一致的纯工具函数，供插件直接 import。
// 传输/SSE/代理相关已迁至 sdk（统一请求+日志层）；此处保留历史兼容薄封装。
package shared

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// RandHex 返回 n 字节的 crypto/rand 随机数十六进制串（长度 2n）。
func RandHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RandUUID 返回随机 UUID v4 字符串（client_id / trace_id 等通用）。
func RandUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// OrDefault 在 s 为空时返回 def。
func OrDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Truncate 按字节截断 s 到最长 n。
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ProxyURL 把 ProxyConfig 渲染成 http 代理 URL，未配置时返回空串。
//
// Deprecated: 用 sdk.ProxyURL。
func ProxyURL(p *pb.ProxyConfig) string { return sdk.ProxyURL(p) }

// UpstreamClient 构造访问上游的 HTTP 客户端，proxyURL 非空时走该代理。
//
// Deprecated: 用 sdk.UpstreamClient（或 StreamSSE/HTTPRequest.Proxy 让 sdk 自建）。
func UpstreamClient(proxyURL string) *http.Client { return sdk.UpstreamClient(proxyURL) }

// SSEParser 消费上游 SSE 行流的解析器契约。
//
// Deprecated: 契约已迁至 sdk.SSEParser（本别名保持既有实现零改动）。
type SSEParser = sdk.SSEParser

// ScanSSE 逐行扫描上游 SSE：无 data 事件视为空流(502)，断流(502)，正常结束 Finish。
//
// Deprecated: 用 host.StreamSSE（带统一日志）或 sdk.ScanSSE（仅扫描）。
func ScanSSE(body io.Reader, parser SSEParser) error { return sdk.ScanSSE(body, parser) }

// Failed 构造 TaskFailed 事件。
func Failed(code int32, msg string) *pb.StreamEvent {
	return &pb.StreamEvent{
		Event: &pb.StreamEvent_TaskFailed{
			TaskFailed: &pb.TaskFailed{Error: &pb.Error{Code: code, Message: msg}},
		},
	}
}

// ReadLimited 最多读取 limit 字节，出错返回已读部分。
func ReadLimited(body io.Reader, limit int64) []byte {
	if body == nil {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(body, limit))
	return raw
}

// ReadLimitedResp 读取响应体前 limit 字节，响应或体为 nil 时返回 nil。
func ReadLimitedResp(resp *http.Response, limit int64) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	return ReadLimited(resp.Body, limit)
}
