// 模型目录：免费层可用模型静态快照。
package main

import (
	"context"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// availableModels 免费模式当前可用的模型（仅 available=true）。
var availableModels = []string{
	"z-ai/glm-5.3-flash",
	"google/gemini-3.1-flash-lite",
	"google/gemini-3.5-flash-lite",
	"deepseek/deepseek-v4-flash",
	"deepseek/deepseek-v4-flash-max",
	"deepseek/deepseek-v4-pro-max",
	"openai/gpt-5.6-luna",
	"openai/gpt-5.6-luna-es",
	"openai/gpt-5.6-luna-max",
	"upstage/solar-pro4",
	"meta/muse-spark-1.2-contributor",
	"anthropic/claude-fable-5",
	"crof/kimi-k3-eco",
}

func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	var models []*pb.ModelInfo
	for _, id := range availableModels {
		models = append(models, &pb.ModelInfo{
			Id: id, Label: map[string]string{"en": id},
			SupportsTools: true, SupportsStream: true,
		})
	}
	return &pb.ModelList{Models: models}, nil
}
