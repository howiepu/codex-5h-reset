package main

import (
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
		if strings.Contains(string(data), username) || strings.Contains(string(data), strings.TrimSpace(password)) {
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
