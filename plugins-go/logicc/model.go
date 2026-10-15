// model:模型目录(上游动态发现 + 5 分钟缓存)。
package logicc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// upstreamModel 上游模型条目(id 与 name 字段在不同版本下命名不一,全部兼容)。
type upstreamModel struct {
	ID        string `json:"id"`
	ModelID   string `json:"modelId"`
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
	Provider  struct {
		Name string `json:"name"`
	} `json:"provider"`
}

func (m upstreamModel) modelID() string {
	if m.ID != "" {
		return m.ID
	}
	return m.ModelID
}

// fetchModels 拉上游 /api/models?language=en。登录校验与模型目录共用。
func (p *plugin) fetchModels(ctx context.Context, c *credential) ([]*pb.ModelInfo, error) {
	resp, err := p.upstream(ctx, c, "models?language=en", reqOpts{})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var payload struct {
		Models []upstreamModel `json:"models"`
		Data   []upstreamModel `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("上游模型列表格式不正确: %w", err)
	}
	list := payload.Models
	if len(list) == 0 {
		list = payload.Data
	}
	var out []*pb.ModelInfo
	for _, m := range list {
		id := m.modelID()
		if id == "" {
			continue
		}
		label := m.Name
		if label == "" {
			label = id
		}
		out = append(out, &pb.ModelInfo{
			Id: id, Label: map[string]string{"en": label},
			SupportsTools: true, SupportsStream: true,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("上游没有返回可用模型")
	}
	return out, nil
}

// ListModels 上游动态目录(缓存 5 分钟)。失败时回退到缓存,无缓存则报错。
func (p *plugin) ListModels(ctx context.Context, blob *pb.CredentialBlob) (*pb.ModelList, error) {
	c, err := credFrom(blob)
	if err != nil {
		return nil, err
	}
	p.modelsMu.Lock()
	if !p.modelsAt.IsZero() && time.Since(p.modelsAt) < 5*time.Minute && len(p.models) > 0 {
		models := p.models
		p.modelsMu.Unlock()
		return &pb.ModelList{Models: models}, nil
	}
	p.modelsMu.Unlock()

	models, err := p.fetchModels(ctx, c)
	if err != nil {
		p.modelsMu.Lock()
		cached := p.models
		p.modelsMu.Unlock()
		if len(cached) > 0 {
			return &pb.ModelList{Models: cached}, nil
		}
		return nil, err
	}
	p.modelsMu.Lock()
	p.models, p.modelsAt = models, time.Now()
	p.modelsMu.Unlock()
	return &pb.ModelList{Models: models}, nil
}

// pickDefaultModel 客户端未指定模型时,取上游目录里标记默认的模型,否则取首个。
func (p *plugin) pickDefaultModel(ctx context.Context, c *credential) (string, error) {
	resp, err := p.upstream(ctx, c, "models?language=en", reqOpts{})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var payload struct {
		Models []upstreamModel `json:"models"`
		Data   []upstreamModel `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return "", fmt.Errorf("上游模型列表格式不正确: %w", err)
	}
	list := payload.Models
	if len(list) == 0 {
		list = payload.Data
	}
	for _, m := range list {
		if m.IsDefault && m.modelID() != "" {
			return m.modelID(), nil
		}
	}
	for _, m := range list {
		if m.modelID() != "" {
			return m.modelID(), nil
		}
	}
	return "", shared.HTTPError{Code: 502, Message: "上游没有返回可用模型"}
}
