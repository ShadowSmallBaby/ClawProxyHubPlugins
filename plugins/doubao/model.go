// model.go — 模型目录：doubao（快速）/ doubao-think（思考）/ doubao-expert（专家）。
package main

import (
	"context"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// ListModels 后两者映射上游 need_deep_think=1/3。工具调用以提示词注入模拟。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	models := []*pb.ModelInfo{
		{Id: "doubao", Label: map[string]string{"zh": "豆包", "en": "doubao"}, SupportsTools: true, SupportsStream: true},
		{Id: "doubao-think", Label: map[string]string{"zh": "豆包·思考", "en": "doubao-think"}, SupportsTools: true, SupportsStream: true},
		{Id: "doubao-expert", Label: map[string]string{"zh": "豆包·专家", "en": "doubao-expert"}, SupportsTools: true, SupportsStream: true},
	}
	return &pb.ModelList{Models: models}, nil
}
