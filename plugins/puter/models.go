// models.go — 公开模型目录（api.puter.com/puterai/chat/models/details）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

type puterModelDetailsResponse struct {
	Models []puterModelDetails `json:"models"`
}

type puterModelDetails struct {
	ID       string `json:"id"`
	PuterID  string `json:"puterId"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	if _, err := credFrom(credBlob); err != nil {
		return nil, err
	}
	p.modelsMu.Lock()
	cached := p.modelsCache
	cacheAt := p.modelsCacheAt
	p.modelsMu.Unlock()
	if cached != nil && time.Since(cacheAt) < modelsFetchTTL {
		return &pb.ModelList{Models: cached}, nil
	}

	models, err := p.fetchModels(ctx)
	if err != nil {
		if cached != nil {
			// 拉取失败回退上次缓存
			return &pb.ModelList{Models: cached}, nil
		}
		return nil, err
	}
	p.modelsMu.Lock()
	p.modelsCache, p.modelsCacheAt = models, time.Now()
	p.modelsMu.Unlock()
	return &pb.ModelList{Models: models}, nil
}

// fetchModels 公开目录即可用性来源（不过本地白名单收窄）。
func (p *plugin) fetchModels(ctx context.Context) ([]*pb.ModelInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, puterModelsURL, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("puter model details fetch failed: %d", resp.StatusCode)
	}
	var payload puterModelDetailsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	models := make([]*pb.ModelInfo, 0, len(payload.Models))
	seen := make(map[string]struct{}, len(payload.Models))
	for _, raw := range payload.Models {
		id := strings.ToLower(strings.TrimSpace(raw.ID))
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		name := strings.TrimSpace(raw.Name)
		if name == "" {
			name = id
		}
		models = append(models, &pb.ModelInfo{
			Id: id, Label: map[string]string{"en": name},
			SupportsTools: true, SupportsStream: true,
		})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("puter model details contained no current gateway models")
	}
	sort.Slice(models, func(i, j int) bool { return models[i].GetId() < models[j].GetId() })
	return models, nil
}
