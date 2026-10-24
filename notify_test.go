package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFailureSummaryOncePerChannelAndRequest(t *testing.T) {
	var notifications, models atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("通知格式错误")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["group"] != pluginID || body["run_id"] == "" || !strings.Contains(body["body"].(string), "2 失败") {
			t.Error("应发送整轮失败摘要")
		}
		notifications.Add(1)
		rw.WriteHeader(204)
	}))
	defer server.Close()
	w, cfg := testWarmer(t, func(method string, _, response any) error {
		if method == "host.auth.list" {
			return json.Unmarshal([]byte(`{"files":[{"id":"a","provider":"codex"},{"id":"b","provider":"codex"}]}`), response)
		}
		if method == "host.model.execute" {
			models.Add(1)
			response.(*modelResponse).StatusCode = 429
		}
		return nil
	})
	cfg.NotifyWebhookURL = server.URL + "/webhook-private-canary"
	cfg.NotifyBarkURL = server.URL + "/bark-private-canary"
	w.Configure(cfg)
	job, _, err := w.startRun("manual", runRequest{RequestID: "notification-once"})
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, job)
	_, duplicate, err := w.startRun("manual", runRequest{RequestID: "notification-once"})
	if err != nil || !duplicate || notifications.Load() != 2 || models.Load() != 2 {
		t.Fatal("同一轮每个渠道只通知一次，重复请求不得再次执行或通知")
	}
	raw, _ := json.Marshal(w.Status())
	disk, _ := os.ReadFile(cfg.StateFile)
	if strings.Contains(string(raw), "private-canary") || strings.Contains(string(disk), "private-canary") {
		t.Fatal("通知地址不能出现在状态或历史中")
	}
}

func TestNotificationSuccessIsQuietAndDeliveryDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(rw, "private-reply", 503) }))
	defer server.Close()
	statusCode := 200
	w, cfg := testWarmer(t, func(method string, _, response any) error {
		if method == "host.auth.list" {
			return json.Unmarshal([]byte(`{"files":[{"id":"a","provider":"codex"}]}`), response)
		}
		if method == "host.model.execute" {
			response.(*modelResponse).StatusCode = statusCode
		}
		return nil
	})
	cfg.NotifyWebhookURL = server.URL + "/private-url"
	w.Configure(cfg)
	_, _ = w.Run("manual")
	if calls.Load() != 0 {
		t.Fatal("成功轮次应保持安静")
	}
	statusCode = 503
	_, _ = w.Run("manual")
	if calls.Load() != 1 {
		t.Fatal("通知投递失败不应重试")
	}
	var failure bool
	for _, entry := range w.Status().History.Logs {
		if entry.Event == "notification.failed" {
			failure = true
		}
	}
	raw, _ := json.Marshal(w.Status())
	if !failure || strings.Contains(string(raw), "private-url") || strings.Contains(string(raw), "private-reply") {
		t.Fatal("通知失败应安全记录")
	}
	cfg.NotifyWebhookURL = ""
	w.Configure(cfg)
	_, _ = w.Run("manual")
	if calls.Load() != 1 {
		t.Fatal("未配置通知时不得外发")
	}
}

func TestWebhookBasicAuthIsScopedRedactedAndNotRedirected(t *testing.T) {
	const username, password = "fake-webhook-user-canary", " fake-webhook-password-canary "
	var webhooks, barks, redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bark" {
			barks.Add(1)
			if r.Header.Get("Authorization") != "" {
				t.Error("Webhook 认证不得发送到 Bark")
			}
		} else {
			webhooks.Add(1)
			user, pass, ok := r.BasicAuth()
			if !ok || user != username || pass != password {
				t.Error("Webhook Basic Auth 不正确，密码必须保留空格")
			}
		}
		if r.URL.Path == "/redirect" {
			rw.Header().Set("Location", target.URL)
			rw.WriteHeader(http.StatusFound)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	w, cfg := testWarmer(t, func(method string, _, response any) error {
		if method == "host.auth.list" {
			return json.Unmarshal([]byte(`{"files":[{"id":"a","provider":"codex"}]}`), response)
		}
		if method == "host.model.execute" {
			response.(*modelResponse).StatusCode = 429
		}
		return nil
	})
	cfg.NotifyWebhookURL, cfg.NotifyBarkURL = server.URL+"/webhook", server.URL+"/bark"
	cfg.NotifyWebhookUsername, cfg.NotifyWebhookPassword = username, password
	cfg.NotifyWebhookBody = `{"body":"{{notification}}","private":"private-body-canary"}`
	if err := validateNotificationConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	w.Configure(cfg)
	_, _ = w.Run("manual")
	cfg.NotifyWebhookURL = server.URL + "/redirect"
	w.Configure(cfg)
	_, _ = w.Run("manual")
	if webhooks.Load() != 2 || barks.Load() != 2 || redirected.Load() != 0 {
		t.Fatal("每个渠道只发送一次，不得把认证跟随重定向发送")
	}
	raw, _ := json.Marshal(w.Status())
	disk, _ := os.ReadFile(cfg.StateFile)
	archived, err := w.Logs(w.Status().History.LastRun.Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	logs, _ := json.Marshal(archived)
	for _, data := range [][]byte{raw, disk, logs} {
		if strings.Contains(string(data), username) || strings.Contains(string(data), strings.TrimSpace(password)) || strings.Contains(string(data), cfg.NotifyWebhookHeaders["Authorization"]) || strings.Contains(string(data), "private-body-canary") {
			t.Fatal("Webhook 认证不得出现在状态、历史或日志中")
		}
	}
}

func TestScheduledEmptyAccountsNotifiesOncePerChannel(t *testing.T) {
	for name, files := range map[string]string{
		"empty":    `{"files":[]}`,
		"disabled": `{"files":[{"id":"a","provider":"codex","disabled":true},{"id":"other","provider":"gemini"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var notifications atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				var body struct{ Body string }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !strings.Contains(body.Body, "没有可执行的 Codex 账号") {
					t.Error("通知应说明定时预热没有可执行账号")
				}
				notifications.Add(1)
				rw.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			w, cfg := testWarmer(t, func(method string, _, response any) error {
				if method == "host.auth.list" {
					return json.Unmarshal([]byte(files), response)
				}
				if method == "host.model.execute" {
					t.Error("没有可执行账号时不得发送模型请求")
				}
				return nil
			})
			cfg.NotifyWebhookURL, cfg.NotifyBarkURL = server.URL+"/webhook", server.URL+"/bark"
			w.Configure(cfg)
			if _, err := w.Run("manual"); err != nil || notifications.Load() != 0 {
				t.Fatal("手动空账号检查仍应只记录警告")
			}
			if results, err := w.Run("schedule:morning"); err == nil || len(results) != 0 || notifications.Load() != 2 {
				t.Fatal("定时空账号应返回异常，每个渠道通知一次")
			}
			progress := w.Status().History.LastProgress
			if progress.Total != 0 || progress.Failed != 0 || progress.Error == "" || progress.FinishedAt == nil {
				t.Fatal("整轮应保存零账号异常，不应伪造账号失败数")
			}
		})
	}
}

func TestNotificationTestUsesDraftWithoutWarmingOrSaving(t *testing.T) {
	const username, password = "fake-draft-user", " fake-draft-password "
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Error("测试通知应发送 JSON POST")
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["source"] != "test" || payload["group"] != pluginID || !strings.Contains(payload["title"], "测试通知") || payload["run_id"] != "" {
			t.Error("测试消息应有明确标记，不得伪造预热轮次")
		}
		switch r.URL.Path {
		case "/draft-webhook":
			user, pass, ok := r.BasicAuth()
			if !ok || user != username || pass != password {
				t.Error("测试应使用当前输入的认证，密码保留首尾空格")
			}
			rw.WriteHeader(http.StatusNoContent)
		case "/draft-bark":
			if r.Header.Get("Authorization") != "" {
				t.Error("Webhook 认证不得发送给 Bark")
			}
			http.Error(rw, "private-receiver-body", http.StatusUnauthorized)
		default:
			t.Error("不能使用已保存的通知地址")
		}
	}))
	defer server.Close()
	w, cfg := testWarmer(t, func(method string, _, _ any) error {
		if method != "host.log" {
			t.Errorf("测试通知不能读取账号或执行模型：%s", method)
		}
		return nil
	})
	cfg.NotifyWebhookURL = server.URL + "/saved-webhook"
	w.Configure(cfg)
	before, _ := json.Marshal(w.Status())
	diskBefore, _ := os.ReadFile(cfg.StateFile)
	body, _ := json.Marshal(map[string]any{"notify_webhook_url": " " + server.URL + "/draft-webhook ", "notify_bark_url": server.URL + "/draft-bark", "notify_webhook_method": "POST", "notify_webhook_format": "json", "notify_webhook_headers": map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))}})
	response := w.handleManagement(managementRequest{Method: "POST", Path: managementBase + "/test-notification", Body: body})
	var decoded struct {
		Results []notificationResult `json:"results"`
	}
	if err := json.Unmarshal(response.Body, &decoded); err != nil || response.StatusCode != 200 || len(decoded.Results) != 2 || calls.Load() != 2 {
		t.Fatalf("每个渠道应各发送一次并独立返回结果：%s", response.Body)
	}
	if !decoded.Results[0].Success || decoded.Results[0].Channel != "Webhook" || decoded.Results[1].Success || decoded.Results[1].Channel != "Bark" || decoded.Results[1].Error != "接收端返回 HTTP 401" {
		t.Fatalf("应保留部分成功结果和安全的 HTTP 状态：%s", response.Body)
	}
	for _, secret := range []string{server.URL, username, strings.TrimSpace(password), "private-receiver-body"} {
		if strings.Contains(string(response.Body), secret) {
			t.Fatal("测试结果不得暴露地址、认证或接收端正文")
		}
	}
	after, _ := json.Marshal(w.Status())
	diskAfter, _ := os.ReadFile(cfg.StateFile)
	if string(before) != string(after) || string(diskBefore) != string(diskAfter) || w.cfg.NotifyWebhookURL != cfg.NotifyWebhookURL {
		t.Fatal("测试不能保存配置或修改运行历史")
	}
}

func TestNotificationTestRejectsInvalidInput(t *testing.T) {
	w := newWarmer(func(string, any, any) error { t.Fatal("无效测试请求不能调用宿主"); return nil })
	for _, body := range []string{
		"", `null`, `[]`, `{`, `{}`, `{} {}`, `{"unknown":"private-input-canary"}`, `{"notify_webhook_url":true}`,
		`{"notify_webhook_url":"file:///private-input-canary"}`,
		`{"notify_webhook_url":"https://user:private-input-canary@example.com"}`,
		`{"notify_webhook_url":"https://example.com/#private-input-canary"}`,
		`{"notify_webhook_username":"user:private-input-canary"}`,
		`{"notify_webhook_password":"private-input-canary"}`,
	} {
		response := w.handleManagement(managementRequest{Method: "POST", Path: managementBase + "/test-notification", Body: []byte(body)})
		if response.StatusCode != 400 || strings.Contains(string(response.Body), "private-input-canary") {
			t.Fatalf("应安全拒绝错误的通知测试请求：%s", response.Body)
		}
	}
}
