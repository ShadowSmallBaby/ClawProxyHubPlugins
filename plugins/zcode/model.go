// Package main — zcode 模型目录：静态表（对齐上游 registry MODELS，plan 维度过滤）+ gRPC ListModels。
package main

import (
	"context"
	"slices"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// glmModel 静态目录条目（与上游 registry MODELS 同源）。
type glmModel struct {
	ID        string
	Label     string
	CtxWindow int32
	MaxOut    int32
	Reasoning bool
	Vision    bool
	// Plans 该模型可用的套餐；空 = 全部套餐可用。
	Plans []string
}

// glmModels 上游权威目录（coding-plan / start-plan 维度）。
var glmModels = []glmModel{
	{ID: "glm-4.6", Label: "GLM-4.6", CtxWindow: 200000, MaxOut: 128000, Reasoning: true, Plans: []string{"coding-plan", "start-plan"}},
	{ID: "glm-4.5-air", Label: "GLM-4.5-Air", CtxWindow: 128000, MaxOut: 96000, Reasoning: true, Plans: []string{"coding-plan", "start-plan"}},
	{ID: "glm-4.5v", Label: "GLM-4.5V", CtxWindow: 64000, MaxOut: 16000, Vision: true, Plans: []string{"coding-plan"}},
}

// modelAllowed 该模型是否对当前套餐开放（Plans 空 = 全开放）。
func modelAllowed(m glmModel, plan string) bool {
	if len(m.Plans) == 0 {
		return true
	}
	return slices.Contains(m.Plans, plan)
}

// listModels 静态目录按套餐过滤 → ModelInfo（凭据有效性由 verifyCredential 判定）。
func (p *plugin) listModels(_ context.Context, c *credential) ([]*pb.ModelInfo, error) {
	plan := "coding-plan"
	if c != nil && c.Plan != "" {
		plan = c.Plan
	}
	var out []*pb.ModelInfo
	for _, m := range glmModels {
		if !modelAllowed(m, plan) {
			continue
		}
		out = append(out, &pb.ModelInfo{
			Id:             m.ID,
			Label:          map[string]string{"en": m.Label},
			ContextWindow:  m.CtxWindow,
			SupportsTools:  true,
			SupportsStream: true,
		})
	}
	return out, nil
}

// ListModels gRPC：SyncModels / RefreshCatalog 拉取目录（凭据 blob 决定套餐维度）。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	models, err := p.listModels(ctx, c)
	if err != nil {
		return nil, err
	}
	return &pb.ModelList{Models: models}, nil
}
