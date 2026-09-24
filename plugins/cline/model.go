// model.go — 模型目录（官方 recommended-models 免费池，需 accessToken）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// ListModels 免费模型目录（官方 recommended-models 动态拉取；需 accessToken）。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	if err := p.ensureToken(ctx, c); err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", apiBase+"/ai/cline/recommended-models", nil)
	for k, v := range p.headers(c, fmt.Sprintf("sess_models_%d", time.Now().UnixMilli())) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(c).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("models endpoint returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Free []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"free"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return nil, fmt.Errorf("invalid models response")
	}
	var models []*pb.ModelInfo
	for _, m := range payload.Free {
		if m.ID == "" {
			continue
		}
		models = append(models, &pb.ModelInfo{
			Id: m.ID, Label: map[string]string{"en": shared.OrDefault(m.Name, m.ID)},
			SupportsTools: true, SupportsStream: true,
		})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("models endpoint returned an empty list")
	}
	return &pb.ModelList{Models: models}, nil
}
