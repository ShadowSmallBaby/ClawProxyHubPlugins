// todofor 模型目录：动态发现上游 /models，对外用短名，请求时转 runner id。
package main

import (
	"context"
	"sort"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
)

// ListModels 动态目录：拉上游 /models，短名去重后作对外 id（冲突退回全名）。
func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	cli := newClient(p.baseURL(), c.APIKey, p.hc(c))
	infos, err := cli.models(ctx)
	if err != nil {
		return nil, err
	}
	publicIDs := publicModelIDs(infos)
	var models []*pb.ModelInfo
	for i, m := range infos {
		if m.ID == "" {
			continue
		}
		label := m.Name
		if label == "" {
			label = publicIDs[i]
		}
		models = append(models, &pb.ModelInfo{
			Id:             publicIDs[i],
			Label:          map[string]string{"en": label},
			SupportsTools:  true,
			SupportsStream: true,
		})
	}
	if len(models) == 0 {
		return nil, errEmptyModels
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Id < models[j].Id })
	return &pb.ModelList{Models: models}, nil
}

// publicModelIDs 计算对外短 id：默认取 "/" 后短名，短名冲突或会遮蔽他人全名时退回全名。
func publicModelIDs(models []modelInfo) []string {
	ids := make([]string, len(models))
	canonicalOwners := make(map[string]int, len(models))
	for i, m := range models {
		ids[i] = shortModelID(m.ID)
		canonicalOwners[m.ID] = i
	}
	for {
		counts := make(map[string]int, len(ids))
		for _, id := range ids {
			counts[id]++
		}
		changed := false
		for i, id := range ids {
			owner, shadows := canonicalOwners[id]
			if counts[id] > 1 || (shadows && owner != i) {
				if ids[i] != models[i].ID {
					ids[i] = models[i].ID
					changed = true
				}
			}
		}
		if !changed {
			return ids
		}
	}
}

// resolveRunnerModel 对外模型名 → AgentSettings 期望的 runner id。
// 优先按发现目录把短名映射回上游全 id，未命中则原样按 provider:author/model 规则转换。
func (p *plugin) resolveRunnerModel(ctx context.Context, c *client, requested string) string {
	if requested == "" {
		return requested
	}
	if infos, err := c.models(ctx); err == nil {
		publicIDs := publicModelIDs(infos)
		for i, m := range infos {
			if publicIDs[i] == requested || m.ID == requested {
				return runnerModelID(m.ID)
			}
		}
	}
	return runnerModelID(requested)
}
