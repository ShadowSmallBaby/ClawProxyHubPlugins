// model.go — 模型目录（apiFormat 决定 anthropic 直通 / openai 转换）。
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	for k, v := range keyfrom(cred, p.clientVersion()) {
		if v != "" {
			q.Set(k, v)
		}
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", serverBase+pathModels+"?"+q.Encode(), nil)
	for k, v := range p.authHeaders(cred) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil, err
	}
	data, err := envelope(resp)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ModelID   string `json:"modelId"`
		ModelName string `json:"modelName"`
		APIFormat string `json:"apiFormat"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var models []*pb.ModelInfo
	for _, m := range raw {
		if m.ModelID == "" {
			continue
		}
		isAnthropic := m.APIFormat == "anthropic"
		anthropicModels.Store(m.ModelID, isAnthropic) // Chat 直通判定缓存
		models = append(models, &pb.ModelInfo{
			Id: m.ModelID, Label: map[string]string{"en": shared.OrDefault(m.ModelName, m.ModelID)},
			SupportsTools: !isAnthropic, SupportsStream: true,
		})
	}
	return &pb.ModelList{Models: models}, nil
}
