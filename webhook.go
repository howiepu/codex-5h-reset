package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var webhookHeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func prepareWebhookConfig(cfg *config) error {
	cfg.NotifyWebhookMethod = strings.ToUpper(strings.TrimSpace(cfg.NotifyWebhookMethod))
	if cfg.NotifyWebhookMethod == "" {
		cfg.NotifyWebhookMethod = http.MethodPost
	}
	switch cfg.NotifyWebhookMethod {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return errors.New("Webhook 请求方法必须为 GET、POST、PUT、PATCH 或 DELETE")
	}
	cfg.NotifyWebhookFormat = strings.ToLower(strings.TrimSpace(cfg.NotifyWebhookFormat))
	if cfg.NotifyWebhookFormat == "" {
		cfg.NotifyWebhookFormat = "json"
	}
	if cfg.NotifyWebhookFormat != "json" && cfg.NotifyWebhookFormat != "form" && cfg.NotifyWebhookFormat != "text" {
		return errors.New("Webhook Body 格式必须为 json、form 或 text")
	}
	headers := make(map[string]string, len(cfg.NotifyWebhookHeaders))
	for name, value := range cfg.NotifyWebhookHeaders {
		if !webhookHeaderName.MatchString(name) || strings.ContainsFunc(value, func(r rune) bool { return r < 32 && r != '\t' || r == 127 }) {
			return errors.New("Webhook 请求头的名称或值不合法")
		}
		name = http.CanonicalHeaderKey(name)
		if name == "Host" || name == "Content-Length" || name == "Transfer-Encoding" {
			return errors.New("Webhook 的 Host、Content-Length 和 Transfer-Encoding 由客户端管理")
		}
		if _, exists := headers[name]; exists {
			return errors.New("Webhook 请求头不能重复，名称不区分大小写")
		}
		headers[name] = value
	}
	// 旧配置自动转换为请求头，避免升级后已有认证失效。
	if _, exists := headers["Authorization"]; !exists {
		user := strings.TrimSpace(cfg.NotifyWebhookUsername)
		if strings.ContainsAny(user, ":\r\n") {
			return errors.New("Webhook 用户名不能包含冒号或换行")
		}
		if cfg.NotifyWebhookPassword != "" && user == "" {
			return errors.New("Webhook 配置密码时必须填写用户名")
		}
		if user != "" {
			req := &http.Request{Header: http.Header{}}
			req.SetBasicAuth(user, cfg.NotifyWebhookPassword)
			headers["Authorization"] = req.Header.Get("Authorization")
		}
	}
	cfg.NotifyWebhookHeaders = headers
	cfg.NotifyWebhookUsername, cfg.NotifyWebhookPassword = "", ""
	if cfg.NotifyWebhookMethod != http.MethodGet {
		_, err := renderWebhookBody(cfg.NotifyWebhookFormat, cfg.NotifyWebhookBody, map[string]string{})
		return err
	}
	return nil
}

func renderWebhookBody(format, template string, message map[string]string) ([]byte, error) {
	replacer := strings.NewReplacer("{{notification}}", message["body"], "{{body}}", message["body"], "{{title}}", message["title"], "{{group}}", message["group"], "{{run_id}}", message["run_id"], "{{source}}", message["source"])
	if format == "text" {
		if template == "" {
			return []byte(message["body"]), nil
		}
		return []byte(replacer.Replace(template)), nil
	}
	var value any
	if template == "" {
		value = message
	} else {
		if !json.Valid([]byte(template)) {
			return nil, errors.New("Webhook 请求体必须是有效 JSON，变量需要放在字符串引号内")
		}
		decoder := json.NewDecoder(strings.NewReader(template))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("Webhook 请求体无法解析")
		}
		var replace func(any) any
		replace = func(item any) any {
			switch item := item.(type) {
			case string:
				return replacer.Replace(item)
			case map[string]any:
				for key, entry := range item {
					item[key] = replace(entry)
				}
			case []any:
				for i, entry := range item {
					item[i] = replace(entry)
				}
			}
			return item
		}
		value = replace(value)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("Webhook 请求体无法编码")
	}
	if format == "form" {
		var fields map[string]string
		if json.Unmarshal(encoded, &fields) != nil || fields == nil {
			return nil, errors.New("Webhook 表单请求体必须是值均为字符串的 JSON 对象")
		}
		form := url.Values{}
		for name, value := range fields {
			form.Set(name, value)
		}
		return []byte(form.Encode()), nil
	}
	return encoded, nil
}

func webhookRequest(cfg config, payload []byte) (*http.Request, error) {
	var message map[string]string
	if json.Unmarshal(payload, &message) != nil {
		return nil, errors.New("通知内容无法解析")
	}
	var body []byte
	if cfg.NotifyWebhookMethod != http.MethodGet {
		var err error
		body, err = renderWebhookBody(cfg.NotifyWebhookFormat, cfg.NotifyWebhookBody, message)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequest(cfg.NotifyWebhookMethod, cfg.NotifyWebhookURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if cfg.NotifyWebhookMethod != http.MethodGet {
		req.Header.Set("Content-Type", map[string]string{"json": "application/json", "form": "application/x-www-form-urlencoded", "text": "text/plain; charset=utf-8"}[cfg.NotifyWebhookFormat])
	}
	for name, value := range cfg.NotifyWebhookHeaders {
		req.Header.Set(name, value)
	}
	return req, nil
}
