// Chat 编排：CC 请求发送 + NDJSON 流泵 + idle 看门狗。
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// defaultStreamIdle 上游读空闲超时（长思考合法停顿可达数百秒，默认 90s 宽容值）。
const defaultStreamIdle = 90 * time.Second

// streamIdle 从设置读空闲超时（stream_idle_seconds，非法回退 90s）。
func (p *plugin) streamIdle() time.Duration {
	if n := shared.OrDefault(p.settingStr("stream_idle_seconds"), ""); n != "" {
		var v int
		if _, err := fmt.Sscanf(n, "%d", &v); err == nil && v > 0 {
			return time.Duration(v) * time.Second
		}
	}
	return defaultStreamIdle
}

// convAnchor 对话锚点：system 消息 hash（同账号多对话据此派生不同 threadId）。
func convAnchor(req *pb.ChatRequest) string {
	for _, m := range req.Messages {
		if m.Role == "system" && m.Text != "" {
			return hashThreadKey(m.Text)
		}
	}
	if len(req.Messages) > 0 {
		return hashThreadKey(req.Messages[0].Text)
	}
	return ""
}

// hashThreadKey 文本 → UUID v4 形状（threadId 须合法 UUID）。
func hashThreadKey(s string) string {
	h := sha256.Sum256([]byte(s))
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hex := fmt.Sprintf("%x", b)
	return hex[0:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:32]
}

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	cred, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(failed(401, err.Error()))
	}
	cfg := p.ccConfigFrom()
	p.ensureInitialized(ctx, cred, cfg)

	sess := p.sessionID(cred.Key)
	body := buildCcBody(req, cfg)
	body = orderedBody(body, sess, convAnchor(req))
	raw, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.apiBase()+"/alpha/generate", bytes.NewReader(raw))
	if err != nil {
		return stream.Send(failed(502, err.Error()))
	}
	for k, v := range chatHeaders(cfg, cred, sess) {
		httpReq.Header.Set(k, v)
	}

	resp, err := p.hc(cred).Do(httpReq)
	if err != nil {
		return stream.Send(failed(502, err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return stream.Send(failed(mapCcStatus(resp.StatusCode), fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(errBody), 300))))
	}

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.Model},
	}}); err != nil {
		return err
	}
	parser := newParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	return scanNDJSON(ctx, resp.Body, parser, p.streamIdle())
}

// scanNDJSON 逐行读 CC NDJSON，每行喂给 parser。
// idle 超时只计「无新数据的等待」，每收到数据重置；触发即取消。
func scanNDJSON(ctx context.Context, body io.Reader, parser interface {
	Feed(string)
	Finish()
	FinishWithError(int32, string)
}, idle time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.AfterFunc(idle, cancel)
	defer timer.Stop()

	tmp := make([]byte, 64*1024)
	var pending string
	for {
		n, err := body.Read(tmp)
		if n > 0 {
			timer.Reset(idle)
			scanned := pending + string(tmp[:n])
			pending = ""
			for {
				i := strings.IndexByte(scanned, '\n')
				if i < 0 {
					break
				}
				line := strings.TrimSuffix(scanned[:i], "\r")
				scanned = scanned[i+1:]
				parser.Feed(line)
			}
			pending = scanned
		}
		if err != nil {
			if len(pending) > 0 {
				parser.Feed(pending)
			}
			if ctx.Err() != nil {
				parser.FinishWithError(429, "upstream idle timeout: no data for "+idle.String())
				return nil
			}
			if err != io.EOF {
				parser.FinishWithError(502, "upstream stream broken: "+err.Error())
				return nil
			}
			break
		}
	}
	parser.Finish()
	return nil
}

// failed 失败事件。
func failed(code int32, msg string) *pb.StreamEvent {
	return shared.Failed(code, msg)
}
