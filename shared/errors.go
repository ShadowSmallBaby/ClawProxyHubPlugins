package shared

import (
	"errors"
	"fmt"
)

// HTTPError 保留上游状态码，让核心区分认证、限流和暂时故障。
type HTTPError struct {
	Code    int32
	Message string
}

func (e HTTPError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Code, e.Message) }
func ErrorStatus(err error) int32 {
	var e HTTPError
	if errors.As(err, &e) && e.Code >= 400 && e.Code <= 599 {
		return e.Code
	}
	return 502
}
