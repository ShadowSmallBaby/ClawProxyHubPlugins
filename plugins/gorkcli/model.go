// model.go — 模型目录（Grok CLI 上游真实模型静态清单）。
package main

import (
	"context"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// grokModels Grok CLI 上游可用模型（cli-chat-proxy 真实模型 id）。
var grokModels = []string{"grok-4.5", "grok-build"}

// ListModels 静态模型目录（上游无独立 /models 端点，模型由客户端指定后透传）。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	models := make([]*pb.ModelInfo, 0, len(grokModels))
	for _, id := range grokModels {
		models = append(models, &pb.ModelInfo{
			Id: id, Label: map[string]string{"en": id},
			SupportsTools: true, SupportsStream: true,
		})
	}
	return &pb.ModelList{Models: models}, nil
}
