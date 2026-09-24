// Chat 编排：session（拿 instance_id）→ startRun → OpenAI 兼容对话 → 收尾 run。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk/openaiup"
	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

func (p *plugin) Chat(req *pb.ChatRequest, stream pb.ClawPlugin_ChatServer) error {
	ctx := stream.Context()
	c, err := credFrom(req.GetCredential())
	if err != nil {
		return stream.Send(shared.Failed(401, err.Error()))
	}
	model := shared.OrDefault(req.Model, defaultModel)

	// 1) 确保会话（拿 instance_id）
	instanceID, err := p.ensureSession(ctx, c, model)
	if err != nil {
		code := int32(502)
		if strings.Contains(err.Error(), "失效") {
			code = 401
		} else if strings.Contains(err.Error(), "排队") || strings.Contains(err.Error(), "queued") {
			code = 429
		}
		return stream.Send(shared.Failed(code, "session: "+err.Error()))
	}

	// 2) 启动 run
	runID, err := p.startRun(ctx, c)
	if err != nil {
		return stream.Send(shared.Failed(502, "run: "+err.Error()))
	}

	// 3) 组请求体：OpenAI 兼容 + codebuff_metadata 注入
	body := openaiup.ChatBody(req)
	body["model"] = model
	body["codebuff_metadata"] = map[string]interface{}{
		"run_id":               runID,
		"cost_mode":            "free",
		"client_id":            randClientID(),
		"freebuff_instance_id": instanceID,
	}
	raw, _ := json.Marshal(body)

	resp, err := postJSON(ctx, p.hc(c), apiBase+"/chat/completions", p.authHeaders(c), raw)
	if err != nil {
		return stream.Send(shared.Failed(502, err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		code := int32(502)
		switch resp.StatusCode {
		case 401, 403:
			code = 401
		case 429:
			code = 429
		}
		return stream.Send(shared.Failed(code, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(errBody), 300))))
	}

	// 4) 成功建连后 best-effort 收尾 run（释放上游 run 计数，失败不影响转发）
	go func() { _ = p.finishRun(context.Background(), c, runID) }()

	if err := stream.Send(&pb.StreamEvent{Event: &pb.StreamEvent_MessageStart{
		MessageStart: &pb.MessageStart{Model: req.Model},
	}}); err != nil {
		return err
	}
	parser := openaiup.NewParser(func(ev *pb.StreamEvent) { _ = stream.Send(ev) })
	return sdk.ScanSSE(resp.Body, parser)
}

// ensureSession POST /freebuff/session 建会话；queued 短暂轮询，active 返回 instance_id。
func (p *plugin) ensureSession(ctx context.Context, c *credential, model string) (string, error) {
	deadline := time.Now().Add(20 * time.Second)
	for {
		headers := p.authHeaders(c)
		headers[hdrModel] = model
		headers[hdrMultiSession] = "1"
		resp, err := postJSON(ctx, p.hc(c), apiBase+"/freebuff/session", headers, []byte("{}"))
		if err != nil {
			return "", err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return "", fmt.Errorf("token 已失效（HTTP %d）", resp.StatusCode)
		}
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 200))
		}
		var s struct {
			Status        string `json:"status"`
			InstanceID    string `json:"instance_id"`
			InstanceIDAlt string `json:"instanceId"`
			EstimatedWait int64  `json:"estimated_wait_ms"`
			Position      int64  `json:"position"`
			Message       string `json:"message"`
			Error         string `json:"error"`
		}
		_ = json.Unmarshal(raw, &s)
		id := shared.OrDefault(s.InstanceID, s.InstanceIDAlt)
		switch s.Status {
		case "active":
			if id == "" {
				return "", fmt.Errorf("会话 active 但缺 instance_id")
			}
			c.InstanceID = id
			return id, nil
		case "queued":
			if time.Now().After(deadline) {
				return "", fmt.Errorf("排队超时（position %d）", s.Position)
			}
			wait := time.Duration(s.EstimatedWait) * time.Millisecond
			if wait < time.Second {
				wait = time.Second
			}
			if wait > 5*time.Second {
				wait = 5 * time.Second
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(wait):
			}
			continue
		case "disabled":
			return "", fmt.Errorf("账号无可用免费额度（disabled）")
		default:
			if time.Now().After(deadline) {
				return "", fmt.Errorf("会话未就绪（status=%s）: %s", s.Status, shared.OrDefault(s.Message, s.Error))
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
	}
}

// startRun POST /agent-runs 启动根 run，返回 run_id。
func (p *plugin) startRun(ctx context.Context, c *credential) (string, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"action":         "START",
		"agentId":        rootAgentID,
		"ancestorRunIds": []string{},
	})
	resp, err := postJSON(ctx, p.hc(c), apiBase+"/agent-runs", p.authHeaders(c), body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, shared.Truncate(string(raw), 200))
	}
	var r struct {
		RunID    string `json:"run_id"`
		RunIDAlt string `json:"runId"`
	}
	_ = json.Unmarshal(raw, &r)
	id := shared.OrDefault(r.RunID, r.RunIDAlt)
	if id == "" {
		return "", fmt.Errorf("start run 响应缺 runId")
	}
	return id, nil
}

// finishRun POST /agent-runs 收尾 run（best-effort）。
func (p *plugin) finishRun(ctx context.Context, c *credential, runID string) error {
	body, _ := json.Marshal(map[string]interface{}{
		"action":        "FINISH",
		"runId":         runID,
		"status":        "completed",
		"totalSteps":    1,
		"directCredits": 0,
		"totalCredits":  0,
	})
	resp, err := postJSON(ctx, p.hc(c), apiBase+"/agent-runs", p.authHeaders(c), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return nil
}
