// model：模型目录（官方 get_models 同步 + 内置表兜底 + 模型解析）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// imaModel 上游模型条目（主模型 + think 子模型）。
type imaModel struct {
	ID        string // 插件模型 id（slug）
	Type      int64  // 上游 model_type
	UpID      string // 上游 model_id
	Name      string // 上游 model_name
	ThinkType int64
	ThinkUpID string
}

// builtinModels 内置模型表兜底（官方接口不可用时）。
var builtinModels = []imaModel{
	{ID: "hy3-preview", Type: 0, UpID: "official_0", Name: "Tencent Hy3 preview", ThinkType: 2, ThinkUpID: "official_2"},
	{ID: "deepseek-v4-flash", Type: 3, UpID: "official_3", Name: "DeepSeek V4-Flash", ThinkType: 1, ThinkUpID: "official_1"},
	{ID: "glm-5.2", Type: 3000, UpID: "official_3000", Name: "GLM-5.2", ThinkType: 3001, ThinkUpID: "official_3001"},
}

// currentModels 模型表（10 分钟缓存；官方接口免登录，缓存过期时后台拉取）。
func (p *plugin) currentModels(ctx context.Context) []imaModel {
	p.mu.Lock()
	fresh := p.modelsFound && time.Since(p.modelsAt) < 10*time.Minute
	models := p.models
	p.mu.Unlock()
	if fresh && len(models) > 0 {
		return models
	}
	if synced, err := p.fetchUpstreamModels(ctx); err == nil && len(synced) > 0 {
		p.mu.Lock()
		p.models, p.modelsAt, p.modelsFound = synced, time.Now(), true
		p.mu.Unlock()
		return synced
	}
	if len(models) > 0 {
		return models
	}
	return builtinModels
}

// fetchUpstreamModels POST /cgi-bin/model_manage/get_models（免登录）。
func (p *plugin) fetchUpstreamModels(ctx context.Context) ([]imaModel, error) {
	resp, err := p.imaPost(ctx, nil, nil, pathModels, map[string]interface{}{}, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := ioReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out struct {
		Code   int64 `json:"code"`
		Models []struct {
			ModelName     string `json:"model_name"`
			ModelType     int64  `json:"model_type"`
			ModelID       string `json:"model_id"`
			IsDefault     bool   `json:"is_default"`
			SubModelInfos map[string]struct {
				ModelType int64  `json:"model_type"`
				ModelID   string `json:"model_id"`
			} `json:"sub_model_infos"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Code != 0 {
		return nil, fmt.Errorf("get_models 返回异常")
	}
	var models []imaModel
	for _, m := range out.Models {
		if m.ModelName == "" {
			continue
		}
		mm := imaModel{
			ID: slugModelKey(m.ModelName), Type: m.ModelType,
			UpID: shared.OrDefault(m.ModelID, fmt.Sprintf("official_%d", m.ModelType)), Name: m.ModelName,
		}
		if think, ok := m.SubModelInfos["1"]; ok && think.ModelType != m.ModelType {
			mm.ThinkType = think.ModelType
			mm.ThinkUpID = shared.OrDefault(think.ModelID, fmt.Sprintf("official_%d", think.ModelType))
		}
		models = append(models, mm)
	}
	return models, nil
}

// ListModels 模型目录：主模型 + -think 变体（IMA 思考是官方 sub_model）。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	var models []*pb.ModelInfo
	for _, m := range p.currentModels(ctx) {
		models = append(models, &pb.ModelInfo{
			Id: m.ID, Label: map[string]string{"zh": m.Name, "en": m.ID},
			SupportsTools: true, SupportsStream: true,
		})
		if m.ThinkUpID != "" {
			models = append(models, &pb.ModelInfo{
				Id: m.ID + "-think", Label: map[string]string{"zh": m.Name + " (Think)", "en": m.ID + "-think"},
				SupportsTools: true, SupportsStream: true,
			})
		}
	}
	return &pb.ModelList{Models: models}, nil
}

// resolveModel 请求 id → 上游 (model_type, model_id)；-think 后缀走思考子模型，未知回退默认。
func resolveModel(models []imaModel, requested string) (int64, string) {
	if len(models) == 0 {
		return builtinModels[0].Type, builtinModels[0].UpID
	}
	if requested != "" {
		lower := strings.ToLower(requested)
		if m := strings.TrimSuffix(lower, "-think"); m != lower {
			for _, mm := range models {
				if mm.ID == m && mm.ThinkUpID != "" {
					return mm.ThinkType, mm.ThinkUpID
				}
			}
		}
		for _, mm := range models {
			if strings.EqualFold(mm.ID, requested) || mm.Name == requested {
				return mm.Type, mm.UpID
			}
		}
	}
	def := models[0]
	for _, mm := range models {
		if strings.Contains(mm.Name, "Hy3") {
			def = mm
			break
		}
	}
	return def.Type, def.UpID
}

// slugModelKey 模型名 → 插件 id。
func slugModelKey(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
