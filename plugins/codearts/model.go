// model.go — 模型目录（opengw gateway/config benefit 模型 ∪ snap 内置模型）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// dateSuffixRe 模型 id 末尾的日期版本后缀（-0731 等，仅 4 位数字）。
var dateSuffixRe = regexp.MustCompile(`-\d{4}$`)

// normalizeModelId 去掉 id 末尾日期后缀：chat 端点只认不带后缀的 id
//（InferHub.002002009.404 "model is not registered"）。
// ⚠️ 带后缀与不带后缀在后端是不同模型、benefit 属性相反，不能互相替代。
func normalizeModelId(id string) string {
	if len(id) > 5 && strings.HasPrefix(id[len(id)-5:], "-") && dateSuffixRe.MatchString(id) {
		return id[:len(id)-5]
	}
	return id
}

func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var models []*pb.ModelInfo

	// 1. opengw gateway/config — benefit 模型（glm-5.3-flash 等）
	if obj, err := p.snapRequest(ctx, cred, "GET", pathGatewayCfg, nil, nil); err == nil {
		if result, ok := obj["result"].(map[string]interface{}); ok {
			if arr, ok := result["models"].([]interface{}); ok {
				for _, raw := range arr {
					m, _ := raw.(map[string]interface{})
					if m == nil {
						continue
					}
					if mi := p.modelInfo(m, seen); mi != nil {
						models = append(models, mi)
					}
				}
			}
		}
	}

	// 2. snap-access /v1/model/builtin — 常规模型（GLM-5.2、openpangu 等）
	if obj, err := p.snapRequest(ctx, cred, "GET", snapBase+pathBuiltin, nil, nil); err == nil {
		if arr, ok := obj["builtinModels"].([]interface{}); ok {
			for _, raw := range arr {
				m, _ := raw.(map[string]interface{})
				if m == nil {
					continue
				}
				if mi := p.modelInfo(m, seen); mi != nil {
					models = append(models, mi)
				}
			}
		}
	}

	if len(models) == 0 {
		return nil, fmt.Errorf("模型目录为空（AK/SK 可能无权限或凭据失效）")
	}
	return &pb.ModelList{Models: models}, nil
}

// modelInfo 单条模型解析：normalize 后过滤 VL 多模态（上下文小、不支持工具）。
func (p *plugin) modelInfo(m map[string]interface{}, seen map[string]bool) *pb.ModelInfo {
	rawID := strField(m, "model_id")
	if rawID == "" {
		return nil
	}
	id := normalizeModelId(rawID)
	if strings.Contains(id, "-VL-") || strings.HasSuffix(id, "-VL") {
		return nil
	}
	name := normalizeModelId(strField(m, "model_name"))
	if name == "" {
		name = id
	}
	if seen[id] {
		return nil
	}
	seen[id] = true
	// SupportsTools=false 表示 anthropic 方言（本插件全部走 chat/completions，
	// 统一 openai 方言，故 SupportsTools=true）
	return &pb.ModelInfo{
		Id: id, Label: map[string]string{"en": shared.OrDefault(name, id)},
		SupportsTools: true, SupportsStream: true,
	}
}

func marshalPK(m map[string]string) string {
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}
