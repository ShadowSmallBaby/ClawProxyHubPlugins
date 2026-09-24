// 模型目录：modelList 接口拉取。
package main

import (
	"context"
	"fmt"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// ListModels modelList 接口拉目录（id 用 modelId，空则 label）。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	resp, err := p.postJSON(ctx, c, "models", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	data, ok := resp["data"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("模型目录为空：响应缺少 data 数组")
	}
	var models []*pb.ModelInfo
	for _, item := range data {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := m["modelId"].(string)
		label, _ := m["label"].(string)
		if id == "" {
			id = label
		}
		if id == "" {
			continue
		}
		stream := true
		if s, ok := m["supportStream"].(bool); ok {
			stream = s
		}
		models = append(models, &pb.ModelInfo{
			Id: id, Label: map[string]string{"en": shared.OrDefault(label, id)},
			SupportsTools: true, SupportsStream: stream,
		})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("模型目录为空：无可用模型")
	}
	return &pb.ModelList{Models: models}, nil
}
