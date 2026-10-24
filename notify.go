package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type notificationResult struct {
	Channel string `json:"channel"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

func (w *warmer) notifyFailure(cfg config, job *runJob) {
	w.mu.Lock()
	p := job.progress
	w.mu.Unlock()
	if p.Failed == 0 && p.Error == "" {
		return
	}
	body := fmt.Sprintf("本轮 %d 成功、%d 失败、%d 未发送。", p.Succeeded, p.Failed, p.Skipped)
	if p.Error != "" {
		body += p.Error
	}
	payload, _ := json.Marshal(map[string]any{"title": pluginName + " 预热异常", "body": body, "group": pluginID, "run_id": p.ID, "source": p.Source})
	for _, delivery := range sendNotifications(cfg, payload) {
		level, event, message := "info", "notification.sent", delivery.Channel+" 异常汇总已发送"
		if !delivery.Success {
			level, event, message = "warn", "notification.failed", delivery.Channel+" 异常汇总发送失败，请检查通知配置"
		}
		w.log(level, event, message, map[string]any{"run_id": p.ID, "channel": delivery.Channel})
	}
}

func sendNotifications(cfg config, payload []byte) []notificationResult {
	results := []notificationResult{}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, channel := range []struct{ name, url string }{{"Webhook", cfg.NotifyWebhookURL}, {"Bark", cfg.NotifyBarkURL}} {
		if channel.url == "" {
			continue
		}
		delivery := notificationResult{Channel: channel.name}
		var req *http.Request
		var err error
		if channel.name == "Webhook" {
			req, err = webhookRequest(cfg, payload)
		} else {
			req, err = http.NewRequest(http.MethodPost, channel.url, bytes.NewReader(payload))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
			}
		}
		if err == nil {
			var response *http.Response
			response, err = client.Do(req)
			if response != nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
				response.Body.Close()
				if response.StatusCode < 200 || response.StatusCode >= 300 {
					delivery.Error = fmt.Sprintf("接收端返回 HTTP %d", response.StatusCode)
				}
			}
		}
		if err != nil {
			delivery.Error = "发送失败，请检查通知地址、认证信息或网络"
		}
		// URL 可能包含通知密钥，网络错误和响应正文不向外返回。
		delivery.Success = delivery.Error == ""
		results = append(results, delivery)
	}
	return results
}
