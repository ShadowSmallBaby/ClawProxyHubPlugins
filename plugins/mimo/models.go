// models.go — 模型目录（后端无目录接口，本地静态表）。
package main

import (
	"context"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// staticModels 静态目录（2026-09-24 上游 refresh）。
var staticModels = []*pb.ModelInfo{
	{Id: "mimo-v2.6-pro", Label: map[string]string{"en": "MiMo v2.6 Pro"}, ContextWindow: 200000, SupportsTools: true, SupportsStream: true},
	{Id: "mimo-v2.6-flash", Label: map[string]string{"en": "MiMo v2.6 Flash"}, ContextWindow: 200000, SupportsTools: true, SupportsStream: true},
	{Id: "mimo-v2.6-pro-ultraspeed", Label: map[string]string{"en": "MiMo v2.6 Pro Ultraspeed"}, ContextWindow: 200000, SupportsTools: true, SupportsStream: true},
	{Id: "mimo-x-pro-preview", Label: map[string]string{"en": "MiMo X Pro Preview"}, ContextWindow: 200000, SupportsTools: true, SupportsStream: true},
	{Id: "mimo-x-flash-preview", Label: map[string]string{"en": "MiMo X Flash Preview"}, ContextWindow: 200000, SupportsTools: true, SupportsStream: true},
}

// ListModels 本地静态表（上游无目录接口）。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	return &pb.ModelList{Models: staticModels}, nil
}
