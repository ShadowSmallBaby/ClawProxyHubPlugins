// model.go — 模型目录（llm/v2/model_catalog，visible 模型 + 倍率展示名）。
package main

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// fallbackModels 兜底目录（6 个 visible 模型）。
// 顺序照抄远端返回顺序；展示名由倍率规则固化（免费 / x原价→x折后价 / x生效价）。
var fallbackModels = []struct {
	id, name string
}{
	{"sn-sensenova-6-8-flash", "SenseNova-6.8-Flash · 免费"},
	{"sn-sensenova-6-8-flash-lite", "SenseNova-6.8-Flash-Lite · 免费"},
	{"sn-glm-5-3", "GLM-5-3 · x0.75"},
	{"sn-kimi-k3", "Kimi-K3 · x1"},
	{"sn-glm-5-3-flash", "GLM-5-3-Flash · x0.2→x0.1"},
	{"sn-deepseek-v4-1-flash", "DeepSeek-V4.1-Flash · x0.25"},
}

func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	data, err := p.bizRequest(ctx, cred, "GET", apiBase+llmPrefix+"/model_catalog", nil)
	if err != nil {
		// 远端不可用时回退兜底目录（不阻断模型选择）
		var models []*pb.ModelInfo
		for _, m := range fallbackModels {
			models = append(models, &pb.ModelInfo{
				Id: m.id, Label: map[string]string{"en": m.name},
				SupportsTools: true, SupportsStream: true,
			})
		}
		return &pb.ModelList{Models: models}, nil
	}
	models := parseCatalog(data)
	if len(models) == 0 {
		for _, m := range fallbackModels {
			models = append(models, &pb.ModelInfo{
				Id: m.id, Label: map[string]string{"en": m.name},
				SupportsTools: true, SupportsStream: true,
			})
		}
	}
	return &pb.ModelList{Models: models}, nil
}

// parseCatalog 解析 model_catalog：visible:true 的条目，倍率拼进展示名。
func parseCatalog(data map[string]interface{}) []*pb.ModelInfo {
	items, ok := data["models"].([]interface{})
	if !ok {
		return nil
	}
	var models []*pb.ModelInfo
	for _, raw := range items {
		m, _ := raw.(map[string]interface{})
		if m == nil || m["visible"] != true {
			continue
		}
		id := strField(m, "id")
		if id == "" {
			continue
		}
		models = append(models, &pb.ModelInfo{
			Id: id, Label: map[string]string{"en": displayName(m, id)},
			SupportsTools: true, SupportsStream: true,
		})
	}
	return models
}

// displayName 最终展示名：
// 生效价 0 → 免费；生效价 < 原价 → x原价→x折后价；其余（含 1 倍）→ x生效价。
func displayName(m map[string]interface{}, id string) string {
	name := strField(m, "description")
	if name == "" {
		name = id
	}
	effective, hasEff := m["billing_effective_multiplier"].(float64)
	if !hasEff || math.IsNaN(effective) || effective < 0 {
		return name
	}
	if effective == 0 {
		return name + " · 免费"
	}
	base, hasBase := m["billing_multiplier"].(float64)
	if hasBase && base > effective && base > 0 {
		return fmt.Sprintf("%s · x%s→x%s", name, fmtMult(base), fmtMult(effective))
	}
	return fmt.Sprintf("%s · x%s", name, fmtMult(effective))
}

// fmtMult 倍率格式化：最多 4 位小数去尾 0（1 → "1"，0.75 → "0.75"）。
func fmtMult(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

var _ = strings.TrimSpace
