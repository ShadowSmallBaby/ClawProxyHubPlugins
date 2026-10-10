package challenge

import (
	"net/http"
	"strings"
	"time"
)

// Adapters 每次返回独立元数据，调用方按需要的结果类型构造自己的设置表单。
func Adapters() []AdapterInfo {
	return []AdapterInfo{{
		ID:                 "ezsolver",
		Name:               map[string]string{"zh": "EzSolver HTTP", "en": "EzSolver HTTP"},
		Kinds:              []Kind{TurnstileToken},
		RequiredParameters: []string{"page_url", "site_key"},
	}, {ID: "solverq", Name: map[string]string{"zh": "SolverQ (solver.000.moe)", "en": "SolverQ (solver.000.moe)"}, Kinds: []Kind{TurnstileToken}, RequiredParameters: []string{"page_url", "site_key"}, OptionalParameters: []string{"action", "cdata"}}}
}

func NewClient(config Config) (Client, error) {
	if config.Adapter != "ezsolver" && config.Adapter != "solverq" {
		return nil, failure(InvalidConfig, "unknown challenge adapter")
	}
	u, err := validateURL(config.BaseURL, false)
	if err != nil {
		return nil, failure(InvalidConfig, "service URL must be HTTP(S), without credentials, query or fragment")
	}
	if config.Timeout == 0 {
		config.Timeout = 90 * time.Second
	}
	if config.Timeout < 5*time.Second || config.Timeout > 300*time.Second {
		return nil, failure(InvalidConfig, "timeout must be between 5 and 300 seconds")
	}
	for _, c := range config.AccessToken {
		if c < 0x21 || c > 0x7e {
			return nil, failure(InvalidConfig, "access token must contain visible ASCII characters only")
		}
	}
	config.BaseURL = strings.TrimRight(u.String(), "/") + "/solve"
	if config.Adapter == "solverq" {
		if config.AccessToken == "" {
			return nil, failure(InvalidConfig, "SolverQ API key is required")
		}
		return &solverQ{config: config, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
	}
	return &ezSolver{config: config, http: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}
