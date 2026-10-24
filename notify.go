package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

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
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, channel := range []struct{ name, url string }{{"Webhook", cfg.NotifyWebhookURL}, {"Bark", cfg.NotifyBarkURL}} {
		if channel.url == "" {
			continue
		}
		level, event, message := "info", "notification.sent", channel.name+" 异常汇总已发送"
		req, err := http.NewRequest(http.MethodPost, channel.url, bytes.NewReader(payload))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			if channel.name == "Webhook" && cfg.NotifyWebhookUsername != "" {
				req.SetBasicAuth(cfg.NotifyWebhookUsername, cfg.NotifyWebhookPassword)
			}
			var response *http.Response
			response, err = client.Do(req)
			if response != nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
				response.Body.Close()
				if response.StatusCode < 200 || response.StatusCode >= 300 {
					err = fmt.Errorf("通知未被接收")
				}
			}
		}
		if err != nil {
			level, event, message = "warn", "notification.failed", channel.name+" 异常汇总发送失败，请检查通知配置"
		}
		// URL 可能包含通知密钥，网络错误和响应正文都不写入日志。
		w.log(level, event, message, map[string]any{"run_id": p.ID, "channel": channel.name})
	}
}
