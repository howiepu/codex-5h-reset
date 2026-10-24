package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

//go:embed web/index.html
var page []byte

var instance = newWarmer(callHost)

const managementBase = "/v0/management/plugins/" + pluginID
const resourcePath = "/v0/resource/plugins/" + pluginID + "/status"

type envelope struct {
	OK     bool       `json:"ok"`
	Result any        `json:"result,omitempty"`
	Error  *hostError `json:"error,omitempty"`
}

type hostError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (e *hostError) Error() string { return e.Code }

type managementRequest struct {
	Method, Path string
	Body         []byte
	Query        url.Values
}
type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

func dispatch(w *warmer, method string, raw []byte) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, errors.New("插件配置请求格式错误")
		}
		cfg, err := parseConfig(req.ConfigYAML)
		if err != nil {
			return nil, err
		}
		w.Configure(cfg)
		return registration(), nil
	case "plugin.quiesce", "plugin.shutdown":
		w.Stop()
		return struct{}{}, nil
	case "management.register":
		return map[string]any{
			"routes": []map[string]string{
				{"Method": "GET", "Path": "/plugins/" + pluginID + "/status"},
				{"Method": "POST", "Path": "/plugins/" + pluginID + "/run"},
				{"Method": "GET", "Path": "/plugins/" + pluginID + "/logs"},
				{"Method": "GET", "Path": "/plugins/" + pluginID + "/accounts"},
				{"Method": "POST", "Path": "/plugins/" + pluginID + "/stop"},
			},
			"resources": []map[string]string{{"Path": "/status", "Menu": pluginName, "Description": "Codex 每日预热设置与运行记录"}},
		}, nil
	case "management.handle":
		var req managementRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, errors.New("管理请求格式错误")
		}
		return w.handleManagement(req), nil
	default:
		return nil, errors.New("不支持的插件方法")
	}
}

func registration() any {
	return map[string]any{
		"schema_version": 1,
		"metadata": map[string]any{
			"Name": pluginName, "Version": version, "Author": "howiepu", "GitHubRepository": repository,
			"ConfigFields": []map[string]string{
				{"Name": "model", "Type": "string", "Description": "预热使用的 Codex 模型"},
				{"Name": "prompt", "Type": "string", "Description": "发送给每个账号的消息"},
				{"Name": "timezone", "Type": "string", "Description": "每日定时使用的 IANA 时区"},
				{"Name": "schedules", "Type": "array", "Description": "每日计划：id、at（HH:MM）、enabled"},
				{"Name": "max_log_entries", "Type": "integer", "Description": "保留的日志数，1–2000"},
				{"Name": "state_file", "Type": "string", "Description": "运行历史 JSON 路径，应位于持久化卷内"},
				{"Name": "history_retention_days", "Type": "integer", "Description": "日志归档保留天数，1–365，默认 30"},
				{"Name": "notify_webhook_url", "Type": "string", "Description": "可选：每轮失败汇总的 Webhook 地址"},
				{"Name": "notify_bark_url", "Type": "string", "Description": "可选：Bark 推送地址，含设备 key"},
			},
		},
		"capabilities": map[string]bool{"management_api": true},
	}
}

func (w *warmer) handleManagement(req managementRequest) managementResponse {
	if req.Path == resourcePath && req.Method == http.MethodGet {
		return managementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}}, Body: page}
	}
	switch req.Method + " " + req.Path {
	case "GET " + managementBase + "/status":
		return jsonResponse(http.StatusOK, w.Status())
	case "GET " + managementBase + "/logs":
		logs, err := w.Logs(req.Query.Get("day"))
		if err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return jsonResponse(http.StatusOK, map[string]any{"logs": logs})
	case "GET " + managementBase + "/accounts":
		accounts, err := w.Accounts()
		if err != nil {
			return jsonResponse(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return jsonResponse(http.StatusOK, map[string]any{"accounts": accounts})
	case "POST " + managementBase + "/stop":
		var body struct {
			RunID string `json:"run_id"`
		}
		if json.Unmarshal(req.Body, &body) != nil || body.RunID == "" {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "停止请求需要 run_id"})
		}
		if !w.StopRun(body.RunID) {
			return jsonResponse(http.StatusConflict, map[string]string{"error": "该轮已结束，请刷新状态"})
		}
		return jsonResponse(http.StatusOK, map[string]any{"status": w.Status()})
	case "POST " + managementBase + "/run":
		var body runRequest
		if len(req.Body) > 0 {
			trimmed := bytes.TrimSpace(req.Body)
			decoder := json.NewDecoder(bytes.NewReader(trimmed))
			decoder.DisallowUnknownFields()
			if !json.Valid(trimmed) || trimmed[0] != '{' || decoder.Decode(&body) != nil {
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": "运行请求格式错误或包含未知字段"})
			}
		}
		job, duplicate, err := w.startRun("manual", body)
		if err != nil {
			code := http.StatusBadGateway
			if errors.Is(err, errInvalidRun) {
				code = http.StatusBadRequest
			}
			if errors.Is(err, errBusy) || errors.Is(err, errDisabled) {
				code = http.StatusConflict
			}
			return jsonResponse(code, map[string]string{"error": err.Error()})
		}
		if !body.Async {
			<-job.done
		}
		w.mu.Lock()
		progress, results := job.progress, append([]result{}, job.results...)
		w.mu.Unlock()
		code := http.StatusOK
		if progress.FinishedAt == nil {
			code = http.StatusAccepted
		} else if progress.Error != "" {
			code = http.StatusBadGateway
			if progress.Error == errInvalidRun.Error() {
				code = http.StatusBadRequest
			}
		}
		return jsonResponse(code, map[string]any{"results": results, "progress": progress, "duplicate": duplicate, "status": w.Status(), "error": progress.Error})
	default:
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "未找到接口或请求方法不受支持"})
	}
}

func jsonResponse(code int, value any) managementResponse {
	body, err := json.Marshal(value)
	if err != nil {
		code, body = http.StatusInternalServerError, []byte(fmt.Sprintf(`{"error":%q}`, "响应编码失败"))
	}
	return managementResponse{StatusCode: code, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}}, Body: body}
}
