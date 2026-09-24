// model.go — 模型目录（内置表，"lite" 走服务端自动路由）。
package main

import (
	"context"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// ListModels 模型清单（新版无签名 model/list 端点，用内置表；"lite" 为服务端自动路由）。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	if _, err := credFrom(credBlob); err != nil {
		return nil, err
	}
	return &pb.ModelList{Models: staticModels()}, nil
}

// defaultModels 内置模型清单（display_name 直传给新版 chat 端点；"lite" 走自动路由）。
var defaultModels = []string{
	"lite",
	"Qwen3.8-Max-Preview", "Qwen3.7-Max", "Qwen3.7-Plus", "Qwen3.6-Flash",
	"DeepSeek-V4-Pro", "DeepSeek-V4-Flash", "GLM-5.2", "Kimi-K2.7-Code", "MiniMax-M2.7",
}

// staticModels 内置模型表 → ModelInfo。
func staticModels() []*pb.ModelInfo {
	models := make([]*pb.ModelInfo, 0, len(defaultModels))
	for _, name := range defaultModels {
		models = append(models, &pb.ModelInfo{
			Id: name, Label: map[string]string{"en": name},
			SupportsTools: true, SupportsStream: true,
		})
	}
	return models
}
