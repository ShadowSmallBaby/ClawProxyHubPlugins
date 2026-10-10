// solverq.go 对接 SolverQ 的同步 Turnstile 服务协议。
package challenge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

type solverQ struct {
	config Config
	http   *http.Client
}

// Solve 同步请求一次，不缓存 token，也不重放可能已创建任务的 POST。
func (c *solverQ) Solve(ctx context.Context, request Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, transportError(ctx, err)
	}
	if request.Kind != TurnstileToken {
		return Result{}, failure(UnsupportedKind, "adapter only supports turnstile_token")
	}
	if request.TargetProxy != "" || request.UserAgent != "" || len(request.Cookies) != 0 {
		return Result{}, failure(UnsupportedParameter, "SolverQ does not support target proxy, user agent or cookies")
	}
	if _, err := validateURL(request.PageURL, true); err != nil {
		return Result{}, failure(InvalidRequest, "page URL must be HTTP(S) without credentials or fragment")
	}
	if strings.TrimSpace(request.SiteKey) == "" {
		return Result{}, failure(InvalidRequest, "site key is required")
	}
	ctx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	seconds := int(math.Ceil(time.Until(deadline).Seconds()))
	if seconds < 1 {
		return Result{}, &Error{Code: Timeout, Message: "challenge deadline expired", cause: context.DeadlineExceeded}
	}
	if seconds > 85 {
		seconds = 85
	}
	if len([]rune(request.Action)) > 32 || len([]rune(request.CData)) > 255 {
		return Result{}, failure(InvalidRequest, "action or cData exceeds service limit")
	}
	body, _ := json.Marshal(struct {
		SiteKey string `json:"sitekey"`
		SiteURL string `json:"url"`
		Timeout int    `json:"timeout"`
		Action  string `json:"action,omitempty"`
		CData   string `json:"cdata,omitempty"`
	}{request.SiteKey, request.PageURL, seconds, request.Action, request.CData})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL, bytes.NewReader(body))
	if err != nil {
		return Result{}, failure(InvalidConfig, "cannot construct service request")
	}
	req.Header.Set("Content-Type", "application/json")
	if c.config.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.AccessToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, transportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		code := ServiceUnavailable
		switch resp.StatusCode {
		case 401, 403:
			code = ServiceAuthFailed
		case 402:
			code = InsufficientBalance
		case 400:
			code = InvalidRequest
		case 422:
			code = SolveFailed
		case 429:
			code = RateLimited
		case 500:
			code = SolveFailed
		}
		return Result{}, &Error{Code: code, Message: "challenge service rejected request", HTTPStatus: resp.StatusCode, Retryable: code == RateLimited || code == ServiceUnavailable}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return Result{}, transportError(ctx, err)
	}
	if len(raw) > maxResponseBytes {
		return Result{}, failure(InvalidResponse, "challenge response exceeds size limit")
	}
	var payload struct {
		Token     string          `json:"token"`
		UserAgent string          `json:"user_agent"`
		Status    string          `json:"status"`
		Code      string          `json:"code"`
		Error     json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &payload) != nil || strings.TrimSpace(payload.Token) == "" || len(payload.Error) != 0 || payload.Status == "error" || payload.Code != "" {
		return Result{}, failure(InvalidResponse, "challenge response must contain a nonempty token without an error")
	}
	return Result{Kind: TurnstileToken, Token: payload.Token, UserAgent: payload.UserAgent}, nil
}
