// model.go — 模型目录（双池并集）与档案。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// listModels 拉上游 /v1/models 模型 id 列表。
func (p *plugin) listModels(ctx context.Context, cred *credential, base, key string) ([]string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(base, "/")+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", p.userAgentStr())
	req.Header.Set("x-opencode-client", "cli")
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
	var out []string
	for _, m := range payload.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models endpoint returned an empty list")
	}
	return out, nil
}

// refreshCatalog 双池目录并发拉取（缓存 5 分钟）；失败沿用旧目录。
func (p *plugin) refreshCatalog(ctx context.Context) {
	p.modelsMu.Lock()
	if !p.modelsAt.IsZero() && time.Since(p.modelsAt) < 5*time.Minute {
		p.modelsMu.Unlock()
		return
	}
	p.modelsMu.Unlock()

	zen, goModels := p.fetchTier(ctx, zenBase), p.fetchTier(ctx, goBase)
	p.modelsMu.Lock()
	if zen != nil {
		p.zenModels = zen
	}
	if goModels != nil {
		p.goModels = goModels
	}
	p.modelsAt = time.Now()
	p.modelsMu.Unlock()
}

// fetchTier 拉 tier 目录，失败返回 nil（沿用旧值）。
func (p *plugin) fetchTier(ctx context.Context, base string) map[string]bool {
	// 先用匿名 public 探测（免费模型）；失败再用无凭据直连兜底
	ids, err := p.listModels(ctx, nil, base, anonZenKey)
	if err != nil {
		return nil
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// inferProtocol 模型名推断上游原生协议
func inferProtocol(model string) string {
	m := strings.ToLower(model)
	if m == "deepseek-v4-flash-free" {
		return "chat"
	}
	for _, prefix := range []string{"claude-", "qwen"} {
		if strings.HasPrefix(m, prefix) {
			return "anthropic"
		}
	}
	for _, prefix := range []string{"gpt-", "o1", "o3", "o4", "grok-", "muse-"} {
		if strings.HasPrefix(m, prefix) {
			return "responses"
		}
	}
	return "chat"
}

// isFreeModel 免费模型判定：名称含 free（大小写不敏感）。
func isFreeModel(model string) bool { return strings.Contains(strings.ToLower(model), "free") }

// ListModels 双池并集；匿名场景只透出免费模型。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	p.refreshCatalog(ctx)
	p.modelsMu.Lock()
	seen := make(map[string]bool, len(p.zenModels)+len(p.goModels))
	for m := range p.zenModels {
		seen[m] = true
	}
	for m := range p.goModels {
		seen[m] = true
	}
	p.modelsMu.Unlock()

	cred, _ := credFrom(credBlob)
	var models []*pb.ModelInfo
	for id := range seen {
		// 无凭据（匿名模式）：只透出免费模型
		if (cred == nil || cred.Key == anonZenKey) && !isFreeModel(id) {
			continue
		}
		models = append(models, &pb.ModelInfo{
			Id: id, Label: map[string]string{"en": id},
			SupportsTools: true, SupportsStream: true,
		})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("模型目录为空：上游目录刷新失败或无可用模型")
	}
	return &pb.ModelList{Models: models}, nil
}
