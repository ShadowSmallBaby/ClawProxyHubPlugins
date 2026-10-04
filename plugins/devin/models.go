// 模型目录（GetCascadeModelConfigs unary，TTL 缓存按凭据）。
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	devinproto "github.com/ShadowSmallBaby/ClawProxyHubPlugins/plugins/devin/devinproto"
	"github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
	"google.golang.org/protobuf/proto"
)

const modelsTTL = 5 * time.Minute

// ListModels GetCascadeModelConfigs：disabled 过滤、uid 去重、provider 后缀作 owned_by。
func (p *plugin) ListModels(ctx context.Context, blob *pb.CredentialBlob) (*pb.ModelList, error) {
	c, err := credFrom(blob)
	if err != nil {
		return nil, err
	}
	key := p.baseURL() + "|" + c.Token + "|" + c.proxyURL
	p.modelsMu.Lock()
	if p.models != nil && p.modelsKey == key && time.Since(p.modelsAt) < modelsTTL {
		models := p.models
		p.modelsMu.Unlock()
		return &pb.ModelList{Models: models}, nil
	}
	p.modelsMu.Unlock()

	configs, err := p.fetchModelConfigs(ctx, c)
	if err != nil {
		return nil, err
	}
	models := make([]*pb.ModelInfo, 0, len(configs))
	seen := map[string]struct{}{}
	for _, cfg := range configs {
		if cfg.GetDisabled() {
			continue
		}
		uid := cfg.GetModelUid()
		if uid == "" && cfg.GetModelOrAlias() != nil {
			uid = cfg.GetModelOrAlias().GetModelUid()
		}
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		ownedBy := "devin"
		if prov := cfg.GetProvider().String(); prov != "" {
			if i := strings.LastIndex(prov, "_"); i >= 0 && i+1 < len(prov) {
				ownedBy = strings.ToLower(prov[i+1:])
			}
		}
		models = append(models, &pb.ModelInfo{
			Id:             uid,
			Label:          map[string]string{"en": shared.OrDefault(cfg.GetLabel(), ownedBy+"/"+uid)},
			ContextWindow:  200000,
			SupportsTools:  true,
			SupportsStream: true,
		})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("GetCascadeModelConfigs returned an empty list")
	}
	p.modelsMu.Lock()
	p.models, p.modelsKey, p.modelsAt = models, key, time.Now()
	p.modelsMu.Unlock()
	return &pb.ModelList{Models: models}, nil
}

// metadataFor chisel 伪装 metadata（无指纹；指纹仅对话请求注入）。
func metadataFor(token string) *devinproto.ExaCodeiumCommonPb_Metadata {
	return &devinproto.ExaCodeiumCommonPb_Metadata{
		ApiKey:           proto.String(token),
		ExtensionName:    proto.String(clientName),
		ExtensionVersion: proto.String(clientVersion),
		IdeName:          proto.String(clientName),
		IdeVersion:       proto.String(clientVersion),
		Locale:           proto.String("en"),
		Os:               proto.String("win"),
	}
}
