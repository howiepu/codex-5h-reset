package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCustomWebhookFormatsAndHeaderScope(t *testing.T) {
	message := map[string]string{"title": "测试标题", "body": "引号\"与换行\n中文 & = {{source}}", "source": "manual", "group": pluginID}
	payload, _ := json.Marshal(message)
	for _, format := range []string{"json", "form", "text", "get"} {
		t.Run(format, func(t *testing.T) {
			var calls atomic.Int32
			cfg := defaultConfig()
			cfg.NotifyWebhookMethod = "PATCH"
			cfg.NotifyWebhookFormat = format
			cfg.NotifyWebhookHeaders = map[string]string{"authorization": "Bearer fake-custom-auth", "X-Custom": "webhook-only"}
			cfg.NotifyWebhookBody = `{"body":"{{notification}}","title":"{{title}}","values":["{{source}}",12345678901234567890]}`
			if format == "form" {
				cfg.NotifyWebhookBody = `{"body":"{{notification}}","title":"{{title}}"}`
			} else if format == "text" {
				cfg.NotifyWebhookBody = "{{title}}\n{{notification}}"
			} else if format == "get" {
				cfg.NotifyWebhookMethod, cfg.NotifyWebhookFormat = "GET", "json"
				cfg.NotifyWebhookBody = "GET ignores this draft"
			}
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, _ := io.ReadAll(r.Body)
				if r.URL.Path == "/bark" {
					if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Custom") != "" || string(raw) != string(payload) {
						t.Error("Webhook 的方法、请求头与模板不能影响 Bark")
					}
				} else {
					if r.Method != cfg.NotifyWebhookMethod || r.Header.Get("Authorization") != "Bearer fake-custom-auth" || r.Header.Get("X-Custom") != "webhook-only" {
						t.Error("自定义方法与请求头没有正确发送")
					}
					switch format {
					case "json":
						var body struct {
							Body, Title string
							Values      []json.RawMessage
						}
						if json.Unmarshal(raw, &body) != nil || body.Body != message["body"] || body.Title != message["title"] || len(body.Values) != 2 || string(body.Values[1]) != "12345678901234567890" || r.Header.Get("Content-Type") != "application/json" {
							t.Error("JSON 需要正确转义动态内容并保留静态数字")
						}
					case "form":
						values, err := url.ParseQuery(string(raw))
						if err != nil || values.Get("body") != message["body"] || values.Get("title") != message["title"] || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
							t.Error("表单需要正确编码换行及特殊字符")
						}
					case "text":
						if string(raw) != message["title"]+"\n"+message["body"] || r.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
							t.Error("纯文本模板替换不正确")
						}
					case "get":
						if len(raw) != 0 || r.Header.Get("Content-Type") != "" {
							t.Error("GET 不应携带请求体或默认 Content-Type")
						}
					}
				}
				rw.WriteHeader(204)
			}))
			defer server.Close()
			cfg.NotifyWebhookURL, cfg.NotifyBarkURL = server.URL+"/webhook", server.URL+"/bark"
			if err := validateNotificationConfig(&cfg); err != nil {
				t.Fatal(err)
			}
			results := sendNotifications(cfg, payload)
			if len(results) != 2 || !results[0].Success || !results[1].Success || calls.Load() != 2 {
				t.Fatalf("每个渠道应只发送一次：%+v", results)
			}
		})
	}
}

func TestCustomWebhookValidation(t *testing.T) {
	for _, raw := range []string{
		`{"notify_webhook_method":"TRACE"}`, `{"notify_webhook_format":"xml"}`,
		`{"notify_webhook_headers":{"bad name":"private-canary"}}`, `{"notify_webhook_headers":{"Authorization":"private-canary\r\ninjected"}}`,
		`{"notify_webhook_headers":{"Authorization":"one","authorization":"private-canary"}}`,
		`{"notify_webhook_headers":{"Host":"private-canary"}}`, `{"notify_webhook_headers":{"Content-Length":"9"}}`,
		`{"notify_webhook_body":"{\"body\":{{notification}}}"}`,
		`{"notify_webhook_format":"form","notify_webhook_body":"{\"body\":123}"}`,
	} {
		cfg := defaultConfig()
		if json.Unmarshal([]byte(raw), &cfg) != nil {
			t.Fatal("测试配置解析失败")
		}
		err := validateNotificationConfig(&cfg)
		if err == nil || strings.Contains(err.Error(), "private-canary") {
			t.Fatalf("应安全拒绝无效配置：%v", err)
		}
	}
}
