// 上游 HTTP 工具：JSON POST 与随机 client_id。
package main

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"net/http"

	sdk "github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

func postJSON(ctx context.Context, client *http.Client, rawURL string, headers map[string]string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if client == nil {
		client = sdk.UpstreamClient("")
	}
	return client.Do(req)
}

// randClientID codebuff_metadata.client_id：随机客户端会话 id。
func randClientID() string {
	b := make([]byte, 8)
	crand.Read(b)
	return hex.EncodeToString(b)
}
