// 手撸 ConnectRPC 客户端：proto unary / server-stream（帧封装）与 JSON unary。
// 帧格式：1 字节 flags + 4 字节 big-endian 长度 + 消息体。
package devin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
	"google.golang.org/protobuf/proto"
)

const (
	connectProtoPath  = "/exa.api_server_pb.ApiServerService/"
	seatStatusPath    = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"
	connectHeader     = "1"
	unaryTimeout      = 120 * time.Second
	maxUnaryBodyBytes = 8 << 20
)

// connectUnary proto unary：POST 单响应，错误按 Connect JSON 透传原文。
func connectUnary(ctx context.Context, cli *http.Client, token, base, path string, reqMsg []byte, out proto.Message) error {
	ctx, cancel := context.WithTimeout(ctx, unaryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(reqMsg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Connect-Protocol-Version", connectHeader)
	req.Header.Set("Authorization", "Basic "+token+"-"+token)
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxUnaryBodyBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return connectHTTPError(resp.StatusCode, raw)
	}
	return proto.Unmarshal(raw, out)
}

// connectJSONUnary 使用 Bearer 认证发送 JSON 请求。
func connectJSONUnary(ctx context.Context, cli *http.Client, token, base, path string, body any, out *map[string]any) error {
	ctx, cancel := context.WithTimeout(ctx, unaryTimeout)
	defer cancel()
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", connectHeader)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxUnaryBodyBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return connectHTTPError(resp.StatusCode, raw)
	}
	return json.Unmarshal(raw, out)
}

// streamReader 逐帧读取 Connect 服务端流。
type streamReader struct {
	resp *http.Response
	body *bufio.Reader
}

// nextStream 打开流（首包等待受 Transport 层 ResponseHeaderTimeout 约束）。
func nextStream(ctx context.Context, cli *http.Client, token, base, path string, reqMsg []byte) (*streamReader, error) {
	var head [5]byte
	binary.BigEndian.PutUint32(head[1:], uint32(len(reqMsg)))
	framed := append(head[:], reqMsg...)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(framed))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Connect-Protocol-Version", connectHeader)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Authorization", "Basic "+token+"-"+token)
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxUnaryBodyBytes))
		resp.Body.Close()
		return nil, connectHTTPError(resp.StatusCode, raw)
	}
	return &streamReader{resp: resp, body: bufio.NewReader(resp.Body)}, nil
}

// recv 下一帧；流正常结束返回 io.EOF；end-of-stream 错误帧解出 Connect 错误。
func (s *streamReader) recv() ([]byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(s.body, head[:]); err != nil {
		return nil, err
	}
	flags := head[0]
	length := binary.BigEndian.Uint32(head[1:5])
	if length > maxUnaryBodyBytes {
		return nil, fmt.Errorf("connect frame exceeds %d bytes", maxUnaryBodyBytes)
	}
	if flags & ^byte(2) != 0 {
		return nil, fmt.Errorf("unsupported connect frame flags: %d", flags)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(s.body, payload); err != nil {
		return nil, err
	}
	if flags&0x02 != 0 { // end-of-stream 错误帧：JSON Connect 错误
		return nil, connectEndError(payload)
	}
	return payload, nil
}

func (s *streamReader) close() { s.resp.Body.Close() }

// connectHTTPError HTTP 非 200：Body 是 Connect 错误 JSON（{code,message}）或原文。
func connectHTTPError(status int, body []byte) error {
	var ce struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if len(body) > 0 && json.Unmarshal(body, &ce) == nil && ce.Code != "" {
		return shared.HTTPError{Code: int32(status), Message: ce.Code + ": " + strings.TrimSpace(ce.Message)}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return shared.HTTPError{Code: int32(status), Message: msg}
}

// connectEndError end-of-stream 帧内的 Connect 错误 JSON。
func connectEndError(payload []byte) error {
	var end struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &end); err != nil {
		return fmt.Errorf("decode connect trailer: %w", err)
	}
	if end.Error == nil {
		return io.EOF
	}
	status := int32(502)
	switch end.Error.Code {
	case "unauthenticated":
		status = 401
	case "permission_denied":
		status = 403
	case "resource_exhausted":
		status = 429
	case "invalid_argument":
		status = 400
	case "unavailable":
		status = 503
	}
	return shared.HTTPError{Code: status, Message: end.Error.Code + ": " + end.Error.Message}
}
