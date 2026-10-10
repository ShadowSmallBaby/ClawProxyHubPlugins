// model.go — 模型目录：Loomy Web 规范模型 id + 请求别名解析。
package loomy

import (
	"context"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// loomyModels Loomy Web 规范模型 id（对外即真实 id，去重、不含别名）。
var loomyModels = []string{
	"deepseek-v4-flash-0731",
	"MiniMax-M3",
	"Kimi-k2.6",
	"qwen-3.8-max",
	"qwen3.8-flash",
	"GLM-5.3-Flash",
	"spark-x",
	"doubao-seed-2.0-mini",
	"mimo-v2.5",
	"qwen3.5-flash",
}

// modelAliases 请求别名 → 规范 id（别名可用于请求，但不出现在 ListModels）。
var modelAliases = map[string]string{
	"deepseek-v4":       "deepseek-v4-flash-0731",
	"deepseek-v4-flash": "deepseek-v4-flash-0731",
	"minimax":           "MiniMax-M3",
	"minimax-m3":        "MiniMax-M3",
	"kimi":              "Kimi-k2.6",
	"kimi-k2.6":         "Kimi-k2.6",
	"qwen":              "qwen-3.8-max",
	"glm-5.3-flash":     "GLM-5.3-Flash",
	"spark":             "spark-x",
	"doubao":            "doubao-seed-2.0-mini",
	"mimo":              "mimo-v2.5",
}

// resolveModel 请求模型名 → 规范 id；无法识别返回空串。
func resolveModel(requested string) string {
	if requested == "" {
		return ""
	}
	if m, ok := modelAliases[requested]; ok {
		return m
	}
	for _, m := range loomyModels {
		if m == requested {
			return m
		}
	}
	return ""
}

// ListModels 静态目录：Loomy Web 接口只有单 content 字段，无工具位，故均不支持工具。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	models := make([]*pb.ModelInfo, 0, len(loomyModels))
	for _, id := range loomyModels {
		models = append(models, &pb.ModelInfo{
			Id:             id,
			Label:          map[string]string{"zh": id, "en": id},
			SupportsTools:  false,
			SupportsStream: true,
		})
	}
	return &pb.ModelList{Models: models}, nil
}
