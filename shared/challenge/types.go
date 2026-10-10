// Package challenge 为插件提供独立于宿主的验证服务客户端。
package challenge

import (
	"context"
	"net/http"
	"time"
)

type Kind string

const (
	TurnstileToken  Kind = "turnstile_token"
	ClearanceCookie Kind = "clearance_cookie"
)

type AdapterInfo struct {
	ID                 string
	Name               map[string]string
	Kinds              []Kind
	RequiredParameters []string
	OptionalParameters []string
}

func (a AdapterInfo) Supports(kind Kind) bool {
	for _, k := range a.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

type Config struct {
	Adapter     string
	BaseURL     string
	AccessToken string
	Timeout     time.Duration
}

type Request struct {
	Kind        Kind
	PageURL     string
	SiteKey     string
	Action      string
	CData       string
	TargetProxy string
	UserAgent   string
	Cookies     []*http.Cookie
}

type Result struct {
	Kind      Kind
	Token     string
	Cookies   []*http.Cookie
	UserAgent string
	ExpiresAt *time.Time
}

type Client interface {
	Solve(context.Context, Request) (Result, error)
}

type Code string

const (
	InvalidConfig        Code = "invalid_config"
	InvalidRequest       Code = "invalid_request"
	UnsupportedKind      Code = "unsupported_kind"
	UnsupportedParameter Code = "unsupported_parameter"
	ServiceAuthFailed    Code = "service_auth_failed"
	ServiceUnavailable   Code = "service_unavailable"
	InsufficientBalance  Code = "insufficient_balance"
	RateLimited          Code = "rate_limited"
	Timeout              Code = "timeout"
	SolveFailed          Code = "solve_failed"
	InvalidResponse      Code = "invalid_response"
)

// Error 只包含本地生成的消息，避免回显服务响应中的 token、Cookie 或地址凭据。
type Error struct {
	Code       Code
	Message    string
	HTTPStatus int
	Retryable  bool
	cause      error
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }
func (e *Error) Unwrap() error { return e.cause }

func failure(code Code, message string) *Error { return &Error{Code: code, Message: message} }
