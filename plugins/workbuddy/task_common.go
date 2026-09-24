// task_common.go — 成长活动公共层：端点常量 + 宽松 envelope 请求 helper。
// 上游把业务错误放在 HTTP 400 的 JSON body 里，code 数字/字符串混用，统一走 actData 宽松判定；
// 无猫 / 门槛未达属账号状态，归跳过不算失败。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	actEnergy      = "/activity/growth/energy"
	actBlindbox    = "/activity/growth/buddy/open"
	actQuota       = "/activity/growth/buddy/quota"
	actTravelStat  = "/activity/growth/buddy/travel/status"
	actTravelGo    = "/activity/growth/buddy/travel/depart"
	actTravelWin   = "/activity/growth/buddy/travel/claim"
	actTravelCfg   = "/activity/growth/buddy/travel/config"
	actBuddyInfo   = "/activity/growth/buddy/info"
	actBuddyFirst  = "/activity/growth/buddy/first"
	actBuddyAgree  = "/activity/growth/buddy/agreement"
	actTasksList   = "/v2/activity/growth/tasks"
	actTasksAccept = "/activity/growth/tasks/accept"
)

// actGet GET + 宽松 envelope。
func (p *plugin) actGet(ctx context.Context, cred *credential, path, label string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", upstreamBase+path, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range p.headers(cred, true) {
		req.Header.Set(k, v)
	}
	resp, err := p.hc(cred).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s接口网络失败: %w", label, err)
	}
	defer resp.Body.Close()
	return actData(resp, label)
}

// actPost POST + 宽松 envelope。
func (p *plugin) actPost(ctx context.Context, cred *credential, path string, body interface{}, label string) (json.RawMessage, error) {
	resp, err := postJSON(ctx, p.hc(cred), upstreamBase+path, p.headers(cred, true), body)
	if err != nil {
		return nil, fmt.Errorf("%s接口网络失败: %w", label, err)
	}
	defer resp.Body.Close()
	return actData(resp, label)
}

// actData 宽松判定：code 为 0（数字或字符串）即成功，data 缺失回空对象。
func actData(resp *http.Response, label string) (json.RawMessage, error) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var e struct {
		Code interface{}     `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, fmt.Errorf("%s响应非 JSON（HTTP %d）", label, resp.StatusCode)
	}
	switch v := e.Code.(type) {
	case float64:
		if v != 0 {
			return nil, fmt.Errorf("%s失败：code=%v %s", label, v, strings.TrimSpace(e.Msg))
		}
	case string:
		if v != "0" {
			return nil, fmt.Errorf("%s失败：code=%v %s", label, v, strings.TrimSpace(e.Msg))
		}
	}
	if len(e.Data) == 0 {
		return json.RawMessage("{}"), nil
	}
	return e.Data, nil
}

// isBuddyStateError 无猫 / 领养门槛未达——账号状态而非故障，应跳过不重试。
func isBuddyStateError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no active buddy") || strings.Contains(msg, "first_buddy task not completed")
}
