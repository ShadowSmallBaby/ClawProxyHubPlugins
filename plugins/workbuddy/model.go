// model.go — 模型目录（上游产品配置 cli agent 集，失败回退静态清单）。
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// codebuddyModels 上游模型端点不可达时的兜底清单（2026-09 实测 /v3/config cli agent 集合）。
// auto 为上游路由虚拟模型。
var codebuddyModels = []string{
	"auto",
	"hy4-preview-f", "hy3", "hy3-x",
	"glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5v-turbo",
	"kimi-k3-1", "kimi-k2.8-preview", "kimi-k2.7", "kimi-k2.6",
	"deepseek-v4.1-flash", "deepseek-v4-pro",
	"minimax-m3",
}

// ListModels 拉上游产品配置模型目录（cli agent 可用集），失败回退静态清单。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return &pb.ModelList{Models: staticModels()}, nil
	}
	if models := p.fetchModels(ctx, cred); len(models) > 0 {
		return &pb.ModelList{Models: models}, nil
	}
	return &pb.ModelList{Models: staticModels()}, nil
}

// fetchModels GET /console/enterprises/{enterpriseId|personal}/models，
// 取 productConfig.models，若声明了 cli agent 则按其 model 集过滤，跳过 disabled。
func (p *plugin) fetchModels(ctx context.Context, cred *credential) []*pb.ModelInfo {
	ent := cred.Account.EnterpriseID
	if ent == "" {
		ent = "personal"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", upstreamBase+"/console/enterprises/"+ent+"/models", nil)
	if err != nil {
		return nil
	}
	for k, v := range p.headers(cred, true) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var cfg struct {
		Models []struct {
			ID string `json:"id"`
			// contextWindow 上游为新版对象形态（{defaultLength, supportedLengths}），
			// 部分条目缺失该字段，用 RawMessage 兼容两种形态
			ContextWindow  json.RawMessage `json:"contextWindow"`
			MaxInputTokens int32           `json:"maxInputTokens"`
			SupportsImages bool            `json:"supportsImages"`
			Disabled       bool            `json:"disabled"`
		} `json:"models"`
		Agents []struct {
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"agents"`
	}
	if json.Unmarshal(body, &cfg) != nil {
		return nil
	}
	var cliSet map[string]bool
	for _, a := range cfg.Agents {
		if a.Name == "cli" {
			cliSet = map[string]bool{}
			for _, id := range a.Models {
				cliSet[id] = true
			}
		}
	}
	out := make([]*pb.ModelInfo, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		if m.Disabled || m.ID == "" || (cliSet != nil && !cliSet[m.ID]) {
			continue
		}
		info := &pb.ModelInfo{Id: m.ID, ContextWindow: contextWindow(m), SupportsTools: true, SupportsStream: true}
		info.Label = map[string]string{"zh": m.ID, "en": m.ID}
		out = append(out, info)
	}
	return out
}

// contextWindow 模型上下文窗口：优先 contextWindow.defaultLength（对象形态），
// 缺失/旧形态时回退 maxInputTokens；两者都无效返回 0。
func contextWindow(m struct {
	ID             string          `json:"id"`
	ContextWindow  json.RawMessage `json:"contextWindow"`
	MaxInputTokens int32           `json:"maxInputTokens"`
	SupportsImages bool            `json:"supportsImages"`
	Disabled       bool            `json:"disabled"`
}) int32 {
	var cw struct {
		DefaultLength int32 `json:"defaultLength"`
	}
	if len(m.ContextWindow) > 0 && json.Unmarshal(m.ContextWindow, &cw) == nil && cw.DefaultLength > 0 {
		return cw.DefaultLength
	}
	return m.MaxInputTokens
}

// staticModels 上游不可达时的兜底清单。
func staticModels() []*pb.ModelInfo {
	models := make([]*pb.ModelInfo, 0, len(codebuddyModels))
	for _, id := range codebuddyModels {
		m := &pb.ModelInfo{Id: id, SupportsTools: true, SupportsStream: true}
		if id == "auto" {
			m.Label = map[string]string{"zh": "自动（上游路由）", "en": "Auto (upstream routing)"}
		}
		models = append(models, m)
	}
	return models
}
