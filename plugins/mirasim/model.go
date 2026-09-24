// model.go — 模型目录：静态兜底 + relay catalog/roster 收窄与上下文覆盖、[1m] 长上下文别名。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// modelDef 静态兜底模型定义。
type modelDef struct {
	id      string
	label   string
	claude  bool // true=Claude(messages)，false=GPT(responses)
	context int32
	output  int32
}

// staticModels 内置兜底目录（0.0.336 客户端 catalog）。
var staticModels = []modelDef{
	{"gpt-6-astra", "GPT 6 Astra", false, 872000, 128000},
	{"gpt-5.6-sol", "GPT 5.6 Sol", false, 372000, 128000},
	{"gpt-5.6-terra", "GPT 5.6 Terra", false, 372000, 128000},
	{"gpt-5.6-luna", "GPT 5.6 Luna", false, 372000, 128000},
	{"claude-opus-5", "Claude Opus 5", true, 1000000, 128000},
	{"claude-sonnet-5", "Claude Sonnet 5", true, 1000000, 128000},
	{"claude-opus-4-8", "Claude Opus 4.8", true, 1000000, 128000},
	{"claude-opus-4-6", "Claude 4.6 Opus", true, 1000000, 128000},
	{"claude-fable-5", "Claude Fable 5", true, 1000000, 128000},
	{"claude-fable-5-1", "Claude Fable 5.1", true, 1000000, 128000},
	{"claude-haiku-4-5", "Claude 4.5 Haiku", true, 200000, 64000},
}

// isExposedModel 只暴露 claude-/gpt- 前缀，拒 -paid 变体。
func isExposedModel(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if strings.HasSuffix(id, "-paid") {
		return false
	}
	return strings.HasPrefix(id, "claude-") || strings.HasPrefix(id, "gpt-")
}

func isClaudeModel(id string) bool {
	return strings.HasPrefix(strings.ToLower(id), "claude-")
}

func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	s := p.settings(credBlob.GetInstanceId())
	rc := p.relay(c, s)

	models := p.fetchCatalog(ctx, rc)
	if len(models) == 0 {
		// 兜底：静态目录
		for _, d := range staticModels {
			models = append(models, modelInfoOf(d.id, d.label, d.context))
		}
	}
	// roster 覆盖上下文/输出 + adaptive（本移植只覆盖上下文窗口）
	p.applyRoster(ctx, rc, models)
	models = withLongContextAliases(models)
	if len(models) == 0 {
		return nil, fmt.Errorf("模型目录为空")
	}
	return &pb.ModelList{Models: models}, nil
}

// fetchCatalog GET /v1/models，收窄：丢保留占位/命名空间/dated twin（有裸名时），只留 claude-/gpt-。
func (p *plugin) fetchCatalog(ctx context.Context, rc *relayClient) []*pb.ModelInfo {
	raw, code, err := rc.controlDo(ctx, "GET", modelsPath)
	if err != nil || code < 200 || code >= 300 {
		return nil
	}
	ids := parseCatalogIDs(raw)
	if len(ids) == 0 {
		return nil
	}
	// 裸名集合（无 / 且无 dated 后缀），用于去 dated twin
	undated := map[string]bool{}
	for _, id := range ids {
		if !strings.Contains(id, "/") && !datedSuffix(id) {
			undated[strings.ToLower(id)] = true
		}
	}
	reserved := map[string]bool{"*": true, "gpt-4o-mini": true, "gpt-4o-mini-openrouter": true}
	seen := map[string]bool{}
	var models []*pb.ModelInfo
	for _, id := range ids {
		lower := strings.ToLower(id)
		if seen[lower] || reserved[lower] || strings.Contains(id, "/") {
			continue
		}
		if datedSuffix(id) {
			plain := strings.TrimSuffix(id, id[len(id)-11:]) // 去 -20YYMMDD
			if undated[strings.ToLower(plain)] {
				continue // 有裸名时丢 dated twin
			}
		}
		if !isExposedModel(id) {
			continue
		}
		seen[lower] = true
		ctxWin := staticContext(id)
		models = append(models, modelInfoOf(id, id, ctxWin))
	}
	return models
}

// parseCatalogIDs 解析 {data:[...]} 或 {models:[...]}，条目可为字符串或 {id}。
func parseCatalogIDs(raw []byte) []string {
	var env struct {
		Data   []json.RawMessage `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return nil
	}
	entries := env.Data
	if len(entries) == 0 {
		entries = env.Models
	}
	var ids []string
	for _, e := range entries {
		var s string
		if json.Unmarshal(e, &s) == nil && s != "" {
			ids = append(ids, s)
			continue
		}
		var obj struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(e, &obj) == nil && obj.ID != "" {
			ids = append(ids, obj.ID)
		}
	}
	return ids
}

// datedSuffix 匹配 -20YYMMDD 结尾（dated twin，如 claude-haiku-4-5-20251001）。
func datedSuffix(id string) bool {
	if len(id) < 11 {
		return false
	}
	tail := id[len(id)-11:]
	if tail[0] != '-' || tail[1:3] != "20" {
		return false
	}
	for _, ch := range tail[1:] {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// staticContext 静态目录里该 id 的上下文窗口（不在表中回 0）。
func staticContext(id string) int32 {
	lower := strings.ToLower(id)
	for _, d := range staticModels {
		if d.id == lower {
			return d.context
		}
	}
	return 0
}

// applyRoster GET /v1/model-roster，用 contextWindow 覆盖已列模型的上下文窗口。
func (p *plugin) applyRoster(ctx context.Context, rc *relayClient, models []*pb.ModelInfo) {
	raw, code, err := rc.controlDo(ctx, "GET", rosterPath)
	if err != nil || code < 200 || code >= 300 {
		return
	}
	var env struct {
		Version string `json:"version"`
		Agents  map[string][]struct {
			ID            string `json:"id"`
			Label         string `json:"label"`
			ContextWindow int64  `json:"contextWindow"`
		} `json:"agents"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Version == "" {
		return
	}
	specs := map[string]int32{}
	for _, family := range []string{"claude", "codex"} {
		for _, sp := range env.Agents[family] {
			id := strings.ToLower(strings.TrimSpace(sp.ID))
			if id == "" || sp.ContextWindow <= 0 || strings.HasSuffix(id, "-paid") {
				continue
			}
			specs[id] = int32(sp.ContextWindow)
		}
	}
	for _, m := range models {
		if win, ok := specs[strings.ToLower(m.Id)]; ok {
			m.ContextWindow = win
		}
	}
}

// withLongContextAliases 为 ≥1M 的 Claude 追加 [1m] 选择器别名。
func withLongContextAliases(models []*pb.ModelInfo) []*pb.ModelInfo {
	seen := map[string]bool{}
	for _, m := range models {
		seen[strings.ToLower(m.Id)] = true
	}
	out := append([]*pb.ModelInfo(nil), models...)
	for _, m := range models {
		if !isClaudeModel(m.Id) || strings.Contains(m.Id, "[") || m.ContextWindow < 1000000 {
			continue
		}
		alias := m.Id + "[1m]"
		if seen[strings.ToLower(alias)] {
			continue
		}
		seen[strings.ToLower(alias)] = true
		label := map[string]string{}
		for k, v := range m.Label {
			label[k] = v + " [1m]"
		}
		out = append(out, &pb.ModelInfo{
			Id: alias, Label: label, ContextWindow: m.ContextWindow,
			SupportsTools: true, SupportsStream: true,
		})
	}
	return out
}

// modelInfoOf 组 ModelInfo（label 缺失用 id）。
func modelInfoOf(id, label string, ctxWin int32) *pb.ModelInfo {
	return &pb.ModelInfo{
		Id: id, Label: map[string]string{"en": shared.OrDefault(label, id)},
		ContextWindow: ctxWin, SupportsTools: true, SupportsStream: true,
	}
}
