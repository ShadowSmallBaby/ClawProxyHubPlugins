// notion 模型目录：对外模型名 → Notion 后台模型 key 的映射。
package main

import (
	"context"
	"encoding/json"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// defaultModelMap 内置映射：对外模型名 → Notion 后台模型（runInference config.model）。
// vertex-*（Gemini）走 markdown-chat 线程，其余走 workflow 线程。
var defaultModelMap = map[string]string{
	"claude-sonnet-4.5": "anthropic-sonnet-alt",
	"claude-opus-4.1":   "anthropic-opus-4.1",
	"gpt-5":             "openai-turbo",
	"gpt-4.1":           "openai-gpt-4.1",
	"gemini-2.5-pro":    "vertex-gemini-2.5-pro",
	"gemini-2.5-flash":  "vertex-gemini-2.5-flash",
}

// modelOrder 目录展示顺序（map 遍历无序，固定顺序保证 UI 稳定）。
var modelOrder = []string{
	"claude-sonnet-4.5", "claude-opus-4.1", "gpt-5", "gpt-4.1",
	"gemini-2.5-pro", "gemini-2.5-flash",
}

// modelMap 生效映射：settings.model_map（JSON）覆盖内置表，解析失败回退内置。
func (p *plugin) modelMap() map[string]string {
	raw := p.settingStr("model_map")
	if raw == "" {
		return defaultModelMap
	}
	var m map[string]string
	if json.Unmarshal([]byte(raw), &m) != nil || len(m) == 0 {
		return defaultModelMap
	}
	return m
}

// resolveModel 对外模型名 → Notion 后台模型；未命中回退 sonnet。
func (p *plugin) resolveModel(model string) string {
	m := p.modelMap()
	if v, ok := m[model]; ok && v != "" {
		return v
	}
	// 允许直接传后台模型 key
	for _, v := range m {
		if v == model {
			return model
		}
	}
	return defaultModelMap["claude-sonnet-4.5"]
}

// ListModels 静态目录（映射表的 key 集合，按内置顺序 + settings 追加）。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	if _, err := credFrom(credBlob); err != nil {
		return nil, err
	}
	m := p.modelMap()
	seen := map[string]bool{}
	var models []*pb.ModelInfo
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		models = append(models, &pb.ModelInfo{
			Id: id, Label: map[string]string{"en": id},
			SupportsStream: true,
		})
	}
	for _, id := range modelOrder {
		if _, ok := m[id]; ok {
			add(id)
		}
	}
	for id := range m { // settings 里的额外条目
		add(id)
	}
	return &pb.ModelList{Models: models}, nil
}
