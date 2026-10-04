// models.go — GraphQL 模型动态发现（agentMode choices，每账号缓存）+ 余额。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	pb "github.com/ShadowSmallBaby/ClawProxyHub/sdk/proto/cphv1"
	shared "github.com/ShadowSmallBaby/ClawProxyHubPlugins/shared"
)

const getFeatureModelChoicesQuery = `query GetFeatureModelChoices($requestContext: RequestContext!) {
  user(requestContext: $requestContext) {
    __typename
    ... on UserOutput {
      user {
        workspaces {
          featureModelChoice {
            agentMode {
              defaultId
              choices {
                id
                displayName
                disableReason
                contextWindow { max default }
              }
            }
          }
        }
      }
    }
    ... on UserFacingError {
      error { message }
    }
  }
}`

const getRequestLimitInfoQuery = `query GetRequestLimitInfo($requestContext: RequestContext!) {
  user(requestContext: $requestContext) {
    __typename
    ... on UserOutput {
      user {
        requestLimitInfo {
          isUnlimited
          nextRefreshTime
          requestLimit
          requestsUsedSinceLastRefresh
        }
      }
    }
    ... on UserFacingError {
      error { message }
    }
  }
}`

// requestContextPayload GraphQL RequestContext（版本只在这里出现，不进指纹头）。
func requestContextPayload() map[string]interface{} {
	return map[string]interface{}{
		"clientContext": map[string]interface{}{"version": clientVersion},
		"osContext": map[string]interface{}{
			"category":           osCategory(),
			"linuxKernelVersion": nil,
			"name":               osCategory(),
			"version":            "",
		},
	}
}

// doGraphQL 发送带认证和设备标识的 GraphQL 请求。
func (p *plugin) doGraphQL(ctx context.Context, cred *credential, query string, target interface{}) error {
	body := map[string]interface{}{
		"query":     query,
		"variables": map[string]interface{}{"requestContext": requestContextPayload()},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", warpGraphQLV2+"?op="+urlQueryOp(query), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	warpHeaders(httpReq)
	httpReq.Header.Set("X-Warp-Experiment-Id", cred.DeviceID)
	httpReq.Header.Set("X-Warp-Experiment-Bucket", experimentBucket(cred.DeviceID))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "*/*")
	resp, err := p.hcFor(cred).Do(httpReq)
	if err != nil {
		return fmt.Errorf("warp graphql request: %w", err)
	}
	defer resp.Body.Close()
	data := shared.ReadLimitedResp(resp, 2<<20)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("warp graphql HTTP %d: %s", resp.StatusCode, shared.Truncate(string(data), 200))
	}
	var gql struct {
		Data struct {
			User struct {
				Type  string `json:"__typename"`
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"user"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &gql); err != nil {
		return fmt.Errorf("decode warp graphql response: %w", err)
	}
	if len(gql.Errors) > 0 {
		return fmt.Errorf("warp graphql: %s", gql.Errors[0].Message)
	}
	if !strings.EqualFold(strings.TrimSpace(gql.Data.User.Type), "UserOutput") {
		if msg := strings.TrimSpace(gql.Data.User.Error.Message); msg != "" {
			return fmt.Errorf("warp graphql: %s", msg)
		}
		return fmt.Errorf("warp graphql returned %q", strings.TrimSpace(gql.Data.User.Type))
	}
	return json.Unmarshal(data, target)
}

func urlQueryOp(query string) string {
	// operationName 从 query 头提取
	for _, prefix := range []string{"query GetFeatureModelChoices", "query GetRequestLimitInfo"} {
		if strings.Contains(query, prefix) {
			name := strings.TrimPrefix(prefix, "query ")
			return strings.ReplaceAll(name, " ", "+")
		}
	}
	return ""
}

// ---------- ListModels ----------

func (p *plugin) ListModels(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.ModelList, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	p.modelsMu.Lock()
	cached, ok := p.modelsCache[cred.RefreshToken+"|"+cred.proxyURL]
	p.modelsMu.Unlock()
	if ok && time.Since(cached.at) < modelsTTL {
		return &pb.ModelList{Models: cached.models}, nil
	}

	models, err := p.discoverModels(ctx, cred)
	if err != nil {
		// 发现失败回退静态表（auto-open 服务端自动路由）
		models = staticWarpModels()
	} else {
		p.modelsMu.Lock()
		if p.modelsCache == nil {
			p.modelsCache = make(map[string]cachedModels)
		}
		p.modelsCache[cred.RefreshToken+"|"+cred.proxyURL] = cachedModels{models: models, at: time.Now()}
		p.modelsMu.Unlock()
	}
	return &pb.ModelList{Models: models}, nil
}

// discoverModels GraphQL agentMode choices → ModelInfo。
func (p *plugin) discoverModels(ctx context.Context, cred *credential) ([]*pb.ModelInfo, error) {
	if err := p.ensureFresh(ctx, cred); err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			User struct {
				User struct {
					Workspaces []struct {
						FeatureModelChoice struct {
							AgentMode struct {
								DefaultID string `json:"defaultId"`
								Choices   []struct {
									ID            string `json:"id"`
									DisplayName   string `json:"displayName"`
									DisableReason string `json:"disableReason"`
									ContextWindow struct {
										Max     int32 `json:"max"`
										Default int32 `json:"default"`
									} `json:"contextWindow"`
								} `json:"choices"`
							} `json:"agentMode"`
						} `json:"featureModelChoice"`
					} `json:"workspaces"`
				} `json:"user"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := p.doGraphQL(ctx, cred, getFeatureModelChoicesQuery, &resp); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	models := make([]*pb.ModelInfo, 0, 8)
	var defaultChoice *pb.ModelInfo
	for _, workspace := range resp.Data.User.User.Workspaces {
		choice := workspace.FeatureModelChoice.AgentMode
		for _, m := range choice.Choices {
			if strings.TrimSpace(m.DisableReason) != "" {
				continue
			}
			if strings.TrimSpace(m.ID) == "" {
				continue
			}
			id := normalizeWarpModel(m.ID)
			if id == "" {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			name := strings.TrimSpace(m.DisplayName)
			if name == "" {
				name = m.ID
			}
			window := m.ContextWindow.Max
			if window <= 0 {
				window = m.ContextWindow.Default
			}
			if window < 0 {
				window = 0
			}
			info := &pb.ModelInfo{
				Id: id, Label: map[string]string{"en": name},
				ContextWindow: window, SupportsTools: true, SupportsStream: true,
			}
			if id == normalizeWarpModel(choice.DefaultID) && defaultChoice == nil {
				defaultChoice = info
				continue
			}
			models = append(models, info)
		}
	}
	if defaultChoice != nil {
		models = append([]*pb.ModelInfo{defaultChoice}, models...)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("warp feature model discovery returned no agent mode choices")
	}
	return models, nil
}

// staticWarpModels 静态 fallback：auto-open 服务端自动路由。
func staticWarpModels() []*pb.ModelInfo {
	return []*pb.ModelInfo{{
		Id: defaultModel, Label: map[string]string{"en": "Auto"},
		SupportsTools: true, SupportsStream: true,
	}}
}

// ---------- GetProfile（余额） ----------

func (p *plugin) GetProfile(ctx context.Context, credBlob *pb.CredentialBlob) (*pb.AccountProfile, error) {
	cred, err := credFrom(credBlob)
	if err != nil {
		return nil, err
	}
	if err := p.ensureFresh(ctx, cred); err != nil {
		return nil, err
	}
	profile := &pb.AccountProfile{
		DisplayName: shared.OrDefault(cred.Email, "warp-account"), Healthy: true, Quota: map[string]string{},
	}
	// 余额尽力而为：GraphQL 失败不当作账号不健康
	var resp struct {
		Data struct {
			User struct {
				User struct {
					RequestLimitInfo struct {
						IsUnlimited                  bool    `json:"isUnlimited"`
						NextRefreshTime              string  `json:"nextRefreshTime"`
						RequestLimit                 float64 `json:"requestLimit"`
						RequestsUsedSinceLastRefresh float64 `json:"requestsUsedSinceLastRefresh"`
					} `json:"requestLimitInfo"`
				} `json:"user"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := p.doGraphQL(ctx, cred, getRequestLimitInfoQuery, &resp); err != nil {
		return profile, nil
	}
	info := resp.Data.User.User.RequestLimitInfo
	limit, used := info.RequestLimit, info.RequestsUsedSinceLastRefresh
	if used < 0 {
		used = 0
	}
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	if info.IsUnlimited {
		profile.Quota["credits"] = "unlimited"
	} else if limit > 0 {
		profile.Quota["credits"] = fmt.Sprintf("%.0f", remaining)
		profile.Quota["total_credits"] = fmt.Sprintf("%.0f", limit)
		profile.Quota["used_credits"] = fmt.Sprintf("%.0f", used)
	}
	return profile, nil
}
