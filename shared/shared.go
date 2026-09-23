// Package shared 提供各插件通用且行为一致的纯工具函数，供插件直接 import。
// 仅收纳可安全统一的实现；与插件内部协议耦合的逻辑（如 credFrom）留在各插件内。
package shared

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

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
func ProxyURL(p *pb.ProxyConfig) string {
	if p == nil || p.GetHost() == "" {
		return ""
	}
	u := &url.URL{
		Scheme: OrDefault(p.GetScheme(), "http"),
		Host:   fmt.Sprintf("%s:%d", p.GetHost(), p.GetPort()),
	}
	if p.GetUsername() != "" {
		u.User = url.UserPassword(p.GetUsername(), p.GetPassword())
	}
	return u.String()
}

// UpstreamClient 构造访问上游的 HTTP 客户端，proxyURL 非空时走该代理。
func UpstreamClient(proxyURL string) *http.Client {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: transport}
}

// SSEParser 消费上游 SSE 行流的解析器契约（各插件的方言 Parser 都实现它）。
type SSEParser interface {
	Feed(string)
	Finish()
	FinishWithError(int32, string)
}

// ScanSSE 逐行扫描上游 SSE：无 data 事件视为空流（502），断流报错（502），
// 正常结束调用 Finish。分块读取，对超长行无缓冲上限。
func ScanSSE(body io.Reader, parser SSEParser) error {
	tmp := make([]byte, 64*1024)
	var pending string
	sawEvent := false
	for {
		n, err := body.Read(tmp)
		if n > 0 {
			scanned := pending + string(tmp[:n])
			pending = ""
			for {
				i := strings.IndexByte(scanned, '\n')
				if i < 0 {
					break
				}
				line := strings.TrimSuffix(scanned[:i], "\r")
				scanned = scanned[i+1:]
				if strings.HasPrefix(line, "data:") && !strings.Contains(line, "[DONE]") {
					sawEvent = true
				}
				parser.Feed(line)
			}
			pending = scanned
		}
		if err != nil {
			if len(pending) > 0 {
				parser.Feed(pending)
			}
			if err != io.EOF {
				parser.FinishWithError(502, "upstream stream broken: "+err.Error())
				return nil
			}
			break
		}
	}
	if !sawEvent {
		parser.FinishWithError(502, "upstream returned an empty stream")
		return nil
	}
	parser.Finish()
	return nil
}

// Failed 构造 TaskFailed 事件。
func Failed(code int32, msg string) *pb.StreamEvent {
	return &pb.StreamEvent{
		Event: &pb.StreamEvent_TaskFailed{
			TaskFailed: &pb.TaskFailed{Error: &pb.Error{Code: code, Message: msg}},
		},
	}
}

// FailedRetryable 构造带 retryable 标记的 TaskFailed 事件。
func FailedRetryable(code int32, msg string, retryable bool) *pb.StreamEvent {
	return &pb.StreamEvent{
		Event: &pb.StreamEvent_TaskFailed{
			TaskFailed: &pb.TaskFailed{Error: &pb.Error{Code: code, Message: msg, Retryable: retryable}},
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

// ShortID 截取 ID 前 8 字符用于日志展示。
func ShortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// PostRaw 用给定头部 POST 原始字节，client 为 nil 时用默认直连客户端。
func PostRaw(ctx context.Context, client *http.Client, rawURL string, headers map[string]string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if client == nil {
		client = UpstreamClient("")
	}
	return client.Do(req)
}

// PostJSON 序列化 body 后 POST，其余同 PostRaw。
func PostJSON(ctx context.Context, client *http.Client, rawURL string, headers map[string]string, body any) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return PostRaw(ctx, client, rawURL, headers, raw)
}
