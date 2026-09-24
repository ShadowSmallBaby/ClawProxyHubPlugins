// 模型目录：静态列表（插件设置可覆盖）。
package main

import (
	"context"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// ListModels 静态模型目录（设置可覆盖）。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	var models []*pb.ModelInfo
	for _, id := range p.modelIDs() {
		models = append(models, &pb.ModelInfo{
			Id: id, Label: map[string]string{"en": id},
			SupportsTools: false, SupportsStream: true,
		})
	}
	return &pb.ModelList{Models: models}, nil
}
