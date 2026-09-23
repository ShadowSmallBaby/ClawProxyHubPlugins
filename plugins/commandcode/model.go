// model：模型目录（上游动态 + 硬编码回退）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// hardcodedModels 上游目录拉取失败时的回退清单。
var hardcodedModels = []string{
	"claude-sonnet-4-6", "claude-opus-4-8", "claude-opus-4-7", "claude-haiku-4-5-20251001",
	"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex",
	"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash",
	"moonshotai/Kimi-K2.6", "moonshotai/Kimi-K2.5",
	"zai-org/GLM-5.1", "zai-org/GLM-5",
	"MiniMaxAI/MiniMax-M3", "MiniMaxAI/MiniMax-M2.7", "MiniMaxAI/MiniMax-M2.5",
	"Qwen/Qwen3.6-Max-Preview", "Qwen/Qwen3.6-Plus", "Qwen/Qwen3.7-Max",
	"stepfun/Step-3.7-Flash", "stepfun/Step-3.5-Flash",
	"xiaomi/mimo-v2.5-pro", "xiaomi/mimo-v2.5",
	"google/gemini-3.5-flash", "google/gemini-3.1-flash-lite",
}

// fetchModels 拉上游 /provider/v1/models（Bearer + CC 版本头），模型目录校验 + 登录共用。
func (p *plugin) fetchModels(ctx context.Context, cred *credential) ([]*pb.ModelInfo, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", p.apiBase()+"/provider/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+cred.Key)
	req.Header.Set("x-cli-environment", "production")
	req.Header.Set("x-command-code-version", ccProtocolVersion)
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("models endpoint returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	var out []*pb.ModelInfo
	for _, m := range payload.Data {
		if m.ID != "" {
			out = append(out, &pb.ModelInfo{Id: m.ID, Label: map[string]string{"en": m.ID}, SupportsTools: true, SupportsStream: true})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models endpoint returned an empty list")
	}
	return out, nil
}

// ListModels 上游动态目录（缓存 5 分钟），失败回退硬编码清单。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	p.modelsMu.Lock()
	if !p.modelsAt.IsZero() && time.Since(p.modelsAt) < 5*time.Minute && len(p.models) > 0 {
		models := p.models
		p.modelsMu.Unlock()
		return &pb.ModelList{Models: models}, nil
	}
	p.modelsMu.Unlock()

	models, err := p.fetchModels(ctx, c)
	if err != nil {
		// 回退硬编码清单
		for _, id := range hardcodedModels {
			models = append(models, &pb.ModelInfo{Id: id, Label: map[string]string{"en": id}, SupportsTools: true, SupportsStream: true})
		}
	}
	p.modelsMu.Lock()
	p.models, p.modelsAt = models, time.Now()
	p.modelsMu.Unlock()
	return &pb.ModelList{Models: models}, nil
}
