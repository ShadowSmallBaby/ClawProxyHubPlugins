// account.go — 凭据刷新（Refresh）与序列化。
package warp

import (
	"context"
	"encoding/json"
	"strings"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

// Refresh 周期续期：JWT 到期前 securetoken 轮换；凭据变更回传 blob。
func (p *plugin) Refresh(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.RefreshResult, error) {
	c, err := credFrom(credBlob)
	if err != nil {
		return &pb.RefreshResult{Error: &pb.Error{Code: 400, Message: err.Error()}}, nil
	}
	refreshErr := p.refreshJWT(ctx, c)
	if refreshErr != nil {
		code := int32(503)
		if strings.Contains(refreshErr.Error(), "HTTP 400") || strings.Contains(refreshErr.Error(), "HTTP 401") {
			code = 401
		}
		return &pb.RefreshResult{Error: &pb.Error{Code: code, Message: refreshErr.Error()}}, nil
	}
	blob, _ := json.Marshal(c)
	return &pb.RefreshResult{Blob: blob, Profile: &pb.AccountProfile{
		DisplayName: shared.OrDefault(c.Email, "warp-account"), Healthy: true, Quota: map[string]string{},
	}}, nil
}
