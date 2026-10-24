package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func testConfig(t *testing.T) config {
	t.Helper()
	cfg := defaultConfig()
	cfg.Enabled, cfg.StateFile = true, filepath.Join(t.TempDir(), "data", "history.json")
	return cfg
}

func testWarmer(t *testing.T, host hostFunc) (*warmer, config) {
	t.Helper()
	cfg := testConfig(t)
	w := newWarmer(host)
	w.Configure(cfg)
	t.Cleanup(w.Stop)
	return w, cfg
}

func TestConfiguration(t *testing.T) {
	cfg, err := parseConfig([]byte("enabled: true\npriority: 20\nstore: {repository: example}\nschedules:\n  - {id: 中, at: '10:05'}\n"))
	if err != nil || cfg.Model != "gpt-5.6-luna" || cfg.RandomArithmetic || len(cfg.Schedules) != 1 {
		t.Fatalf("配置未保持默认值或宿主字段兼容：%+v %v", cfg, err)
	}
	for _, raw := range []string{
		"model: ''", "prompt: ' '", "timezone: invalid", "max_log_entries: 0", "max_log_entries: 2001", "state_file: ''",
		"random_arithmetic: invalid", "random_arithmetic: true\nmodel: ''",
		"notify_webhook_username: 'user:name'", "notify_webhook_password: password-without-user",
		"notify_webhook_username: \"user\\nname\"",
		"history_retention_days: 0", "history_retention_days: 366", "notify_webhook_url: ftp://example.com", "notify_bark_url: 'https://user:pass@example.com/key'", "notify_bark_url: 'https://example.com/key#fragment'",
		"schedules: [{id: a, at: '5:00'}]", "schedules: [{id: '', at: '05:00'}]",
		"schedules: [{id: a, at: '05:00'}, {id: a, at: '10:00'}]",
		"schedules: [{id: a, at: '05:00'}, {id: b, at: '05:00'}]",
	} {
		if _, err := parseConfig([]byte(raw)); err == nil {
			t.Errorf("应拒绝配置：%s", raw)
		}
	}
	if _, err := parseConfig([]byte("schedules: [{id: a, at: '05:00'}, {id: b, at: '05:00', enabled: false}]")); err != nil {
		t.Fatal(err)
	}
	cfg, err = parseConfig([]byte("random_arithmetic: true\nprompt: ' '\n"))
	if err != nil || !cfg.RandomArithmetic || cfg.Prompt != "" {
		t.Fatalf("随机算术题模式应允许固定内容留空：%+v %v", cfg, err)
	}
	cfg, err = parseConfig([]byte("notify_webhook_username: '  fake-user  '\nnotify_webhook_password: ' fake-password with spaces '\n"))
	if err != nil || cfg.NotifyWebhookUsername != "fake-user" || cfg.NotifyWebhookPassword != " fake-password with spaces " {
		t.Fatal("Webhook 用户名应去掉首尾空格，密码必须原样保留")
	}
}

func TestDailySchedule(t *testing.T) {
	cfg, err := parseConfig([]byte("schedules: [{id: 早, at: '05:00'}, {id: 中, at: '10:05'}, {id: 晚, at: '15:10'}, {id: 关, at: '10:06', enabled: false}]"))
	if err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse(time.RFC3339, "2026-10-24T10:05:00+08:00")
	runs := nextRuns(cfg, now)
	if len(runs) != 3 || runs[0].ID != "晚" || runs[1].ID != "早" || runs[2].ID != "中" {
		t.Fatalf("日程排序或停用处理错误：%+v", runs)
	}
	if runs[2].Time.Format(time.RFC3339) != "2026-10-25T10:05:00+08:00" {
		t.Fatal("已经到点的计划应安排到次日，不应启动时补发")
	}
	before := now.Add(-time.Second)
	if got := nextRuns(cfg, before)[0]; got.ID != "中" || got.Time.Sub(before) != time.Second {
		t.Fatalf("即将到点的计划错误：%+v", got)
	}
	if len(nextRuns(defaultConfig(), now)) != 0 {
		t.Fatal("未配置日程时不应自动预热")
	}
}

func TestWarmerCompatibleRequests(t *testing.T) {
	var requests []modelRequest
	host := func(method string, request, response any) error {
		switch method {
		case "host.auth.list":
			raw := []byte(`{"files":[{"id":"B","provider":" CoDeX ","unavailable":true,"status":"cooldown"},{"id":"a","provider":"codex"},{"id":"c","provider":"codex"},{"id":"d","provider":"codex"},{"id":"disabled","provider":"codex","disabled":true},{"id":"other","provider":"gemini"},{"id":" ","provider":"codex"}]}`)
			return json.Unmarshal(raw, response)
		case "host.model.execute":
			req := request.(modelRequest)
			requests = append(requests, req)
			switch req.AuthID {
			case "a":
				response.(*modelResponse).StatusCode = 200
			case "B":
				response.(*modelResponse).StatusCode = 204
			case "c":
				response.(*modelResponse).StatusCode = 429
			case "d":
				return &hostError{Code: "execution_failed", Message: "sensitive-host-token", HTTPStatus: 503}
			}
		}
		return nil
	}
	w, cfg := testWarmer(t, host)
	results, err := w.Run("manual")
	if err != nil || len(results) != 4 {
		t.Fatalf("应只处理四个启用的 Codex 账号：%+v %v", results, err)
	}
	ids := []string{}
	for _, req := range requests {
		ids = append(ids, req.AuthID)
		if req.EntryProtocol != "openai" || req.ExitProtocol != "openai" || req.ForcedProvider != "codex" || req.Stream || req.Model != cfg.Model || req.Headers.Get("Content-Type") != "application/json" {
			t.Fatalf("执行参数发生偏离：%+v", req)
		}
		var body map[string]any
		if err := json.Unmarshal(req.Body, &body); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"model": cfg.Model, "stream": false, "messages": []any{map[string]any{"role": "user", "content": cfg.Prompt}}}
		if !reflect.DeepEqual(body, want) {
			t.Fatalf("消息必须与 Warmer 一致：%s", req.Body)
		}
	}
	if !reflect.DeepEqual(ids, []string{"a", "B", "c", "d"}) {
		t.Fatalf("应串行发送且不重试：%v", ids)
	}
	if !results[0].Success || !results[1].Success || results[2].Success || results[3].Success || results[3].StatusCode != 503 {
		t.Fatalf("应仅依 HTTP 2xx 判断成功，即使返回正文为空：%+v", results)
	}
	raw, _ := json.Marshal(w.Status())
	if strings.Contains(string(raw), "sensitive-host-token") {
		t.Fatal("不得把上游原始错误写入状态或日志")
	}
	if w.Status().History.LastRun == nil || w.Status().Running {
		t.Fatal("结束后应保存完成时间并解除运行状态")
	}
}

func TestRandomArithmeticRequests(t *testing.T) {
	cfg := defaultConfig()
	cfg.RandomArithmetic, cfg.Prompt = true, "fixed-content-must-not-be-sent"
	var requests []modelRequest
	w := newWarmer(func(method string, request, response any) error {
		if method != "host.model.execute" {
			t.Fatalf("不应增加其他宿主调用：%s", method)
		}
		requests = append(requests, request.(modelRequest))
		response.(*modelResponse).StatusCode = 204
		return nil
	})
	for range 256 {
		if got := w.warmOne(cfg, authEntry{ID: "test-account"}); !got.Success {
			t.Fatal("随机内容仍应仅按 HTTP 2xx 判断成功")
		}
	}
	pattern := regexp.MustCompile(`^([1-9][+*/-][1-9]|[1-9] [+*/-] [1-9])$`)
	prompts := map[string]bool{}
	for _, req := range requests {
		var body map[string]any
		if err := json.Unmarshal(req.Body, &body); err != nil {
			t.Fatal(err)
		}
		prompt, _ := body["input"].(string)
		if !pattern.MatchString(prompt) {
			t.Fatalf("应发送一道两个操作数均为 1～9 的算术题：%q", prompt)
		}
		compact := strings.ReplaceAll(prompt, " ", "")
		if compact[1] == '/' && (compact[0]-'0')%(compact[2]-'0') != 0 {
			t.Fatalf("除法必须整除：%q", prompt)
		}
		want := map[string]any{
			"model": cfg.Model, "input": prompt, "stream": false, "store": false, "instructions": "",
			"reasoning": map[string]any{"effort": "low", "context": "all_turns"},
			"text":      map[string]any{"verbosity": "low"}, "tools": []any{}, "tool_choice": "none",
		}
		if !reflect.DeepEqual(body, want) {
			t.Fatalf("随机模式必须使用低推理、禁用工具的完整 Lite 请求：%s", req.Body)
		}
		if req.AuthID != "test-account" || req.ForcedProvider != "codex" || req.EntryProtocol != "openai-response" || req.ExitProtocol != "openai-response" || req.Stream {
			t.Fatalf("随机模式必须固定账号并使用非流式 Responses：%+v", req)
		}
		if req.Headers.Get("X-OpenAI-Internal-Codex-Responses-Lite") != "true" || req.Headers.Get("X-CLIProxyAPI-Account-Warmup") != "true" || req.Headers.Get("Accept") != "application/json" {
			t.Fatalf("随机模式必须携带 Lite 和预热标记：%v", req.Headers)
		}
		prompts[prompt] = true
	}
	if len(prompts) < 2 {
		t.Fatal("每次账号请求应重新随机出题")
	}
}

func TestBusyAndStopWaitForRequests(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	host := func(method string, request, response any) error {
		if method == "host.auth.list" {
			return json.Unmarshal([]byte(`{"files":[{"id":"a","provider":"codex"}]}`), response)
		}
		if method == "host.model.execute" {
			close(started)
			<-release
			response.(*modelResponse).StatusCode = 200
		}
		return nil
	}
	w, _ := testWarmer(t, host)
	runDone := make(chan struct{})
	go func() { defer close(runDone); _, _ = w.Run("manual") }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("请求未启动")
	}
	if _, err := w.Run("manual"); !errors.Is(err, errBusy) {
		t.Fatalf("并行请求应返回冲突：%v", err)
	}
	stopDone := make(chan struct{})
	go func() { w.Stop(); close(stopDone) }()
	select {
	case <-stopDone:
		t.Fatal("请求未结束时不得完成卸载")
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("请求未结束")
	}
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("停止时发生死锁")
	}
	if _, err := w.Run("manual"); !errors.Is(err, errDisabled) {
		t.Fatalf("停止后不得接受新请求：%v", err)
	}
}

func TestPersistenceAndLogLimit(t *testing.T) {
	host := func(method string, _, response any) error {
		if method == "host.auth.list" {
			return json.Unmarshal([]byte(`{"files":[]}`), response)
		}
		return nil
	}
	cfg := testConfig(t)
	cfg.MaxLogEntries = 3
	w := newWarmer(host)
	w.Configure(cfg)
	if _, err := w.Run("manual"); err != nil {
		t.Fatal(err)
	}
	w.Stop()
	restarted := newWarmer(host)
	restarted.Configure(cfg)
	defer restarted.Stop()
	s := restarted.Status()
	if s.History.LastRun == nil || s.History.LastSource != "manual" || len(s.History.Logs) != 3 || s.StateError != "" {
		t.Fatalf("重启未恢复历史或日志未限长：%+v", s)
	}
	for path, mode := range map[string]os.FileMode{cfg.StateFile: 0600, filepath.Dir(cfg.StateFile): 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("历史权限错误：%s %v", path, err)
		}
	}
}

func TestCorruptHistoryIsPreserved(t *testing.T) {
	cfg := testConfig(t)
	if err := os.MkdirAll(filepath.Dir(cfg.StateFile), 0700); err != nil {
		t.Fatal(err)
	}
	broken := []byte("{broken-history")
	if err := os.WriteFile(cfg.StateFile, broken, 0600); err != nil {
		t.Fatal(err)
	}
	w := newWarmer(func(string, any, any) error { return nil })
	w.Configure(cfg)
	defer w.Stop()
	_, _ = w.Run("manual")
	raw, err := os.ReadFile(cfg.StateFile)
	if err != nil || string(raw) != string(broken) || w.Status().StateError == "" {
		t.Fatalf("损坏文件被覆盖，或错误未显示：%q %v", raw, err)
	}
	if err := os.WriteFile(cfg.StateFile, []byte(`{"schema_version":1,"last_source":"restored","last_results":[],"logs":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	w.Configure(cfg)
	if s := w.Status(); s.StateError != "" || s.History.LastSource != "restored" {
		t.Fatal("修复后用相同配置重载，应重新读取历史")
	}
}

func TestWriteFailureIsVisible(t *testing.T) {
	w, cfg := testWarmer(t, func(string, any, any) error { return nil })
	if err := os.Rename(cfg.StateFile, cfg.StateFile+".backup"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfg.StateFile, 0700); err != nil {
		t.Fatal(err)
	}
	_, _ = w.Run("manual")
	if !strings.Contains(w.Status().StateError, "写入失败") {
		t.Fatal("必须报告历史文件不可写")
	}
	if err := os.Remove(cfg.StateFile); err != nil {
		t.Fatal(err)
	}
	_, _ = w.Run("manual")
	if w.Status().StateError != "" {
		t.Fatal("写入恢复后应清除错误")
	}
}
