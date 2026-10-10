package challenge

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

const maxResponseBytes = 1 << 20

func validateURL(raw string, allowQuery bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || strings.Contains(raw, "#") || (!allowQuery && (u.RawQuery != "" || u.ForceQuery)) {
		return nil, errors.New("invalid URL")
	}
	return u, nil
}

func transportError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: Timeout, Message: "challenge service timed out", Retryable: true, cause: context.DeadlineExceeded}
	}
	return &Error{Code: ServiceUnavailable, Message: "challenge service request failed", Retryable: true}
}
