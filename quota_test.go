package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func quotaFixture(t *testing.T) (*warmer, config) {
	t.Helper()
	cfg := testConfig(t)
	cfg.QuotaGuardEnabled = true
	cfg.QuotaGuardAccounts = map[string]quotaRule{"a": {FiveHourMin: 20, WeeklyMin: 10}}
	w := newWarmer(func(string, any, any) error { return nil })
	w.cfg, w.accepting, w.initialized = cfg, true, true
	t.Cleanup(w.Stop)
	return w, cfg
}

func quotaReading(remaining float64, now time.Time) *quotaWindow {
	return &quotaWindow{Remaining: remaining, ObservedAt: now, ResetAt: now.Add(time.Hour)}
}

func TestQuotaConfiguration(t *testing.T) {
	defaults, err := parseConfig(nil)
	if err != nil || !defaults.WarmupQuotaCheck || defaults.QuotaGuardEnabled {
		t.Fatal(defaults, err)
	}
	cfg, err := parseConfig([]byte("warmup_quota_check: false\nquota_guard_accounts:\n  a: {five_hour_min_remaining: 20, weekly_min_remaining: 10}"))
	if err != nil || cfg.WarmupQuotaCheck || cfg.QuotaGuardAccounts["a"].FiveHourMin != 20 {
		t.Fatal(cfg, err)
	}
	for _, raw := range []string{
		"quota_guard_accounts: {a: {five_hour_min_remaining: -1}}",
		"quota_guard_accounts: {a: {weekly_min_remaining: 100}}",
		"quota_guard_accounts: {a: {weekly_min_remaining: .nan}}",
		"quota_guard_accounts: {a: {}}",
		"quota_guard_accounts: {' a ': {weekly_min_remaining: 10}}",
	} {
		if _, err := parseConfig([]byte(raw)); err == nil {
			t.Fatalf("应拒绝 %s", raw)
		}
	}
}

func TestQuotaWindowsAndRecovery(t *testing.T) {
	now := time.Now()
	rule := quotaRule{FiveHourMin: 20, WeeklyMin: 10}
	for _, tc := range []struct {
		name         string
		five, weekly float64
		paused       bool
		blocked      bool
	}{
		{"available", 60, 50, false, false}, {"five-hour", 20, 50, false, true}, {"weekly", 90, 10, false, true},
		{"weekly-still-held-after-five-hour-reset", 100, 5, true, true},
		{"recovery-buffer", 21, 50, true, true}, {"recovered", 22, 12, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := quotaRecord{FiveHour: quotaReading(tc.five, now), Weekly: quotaReading(tc.weekly, now), Paused: tc.paused}
			if reason := quotaBlockReason(record, rule, now); (reason != "") != tc.blocked {
				t.Fatalf("原因 %q, 期望阻止 %t", reason, tc.blocked)
			}
		})
	}
	record := quotaRecord{FiveHour: quotaReading(100, now), Weekly: quotaReading(100, now)}
	record.FiveHour.ResetAt = now
	if quotaBlockReason(record, rule, now) == "" {
		t.Fatal("仅到重置时间不得放行")
	}
	record.FiveHour.ResetAt = now.Add(time.Hour)
	record.Weekly.ObservedAt = now.Add(-quotaMaxAge - time.Second)
	if quotaBlockReason(record, rule, now) == "" {
		t.Fatal("一个窗口的新数据不得刷新另一个窗口的有效期")
	}
	record.Weekly = nil
	if quotaBlockReason(record, rule, now) == "" {
		t.Fatal("缺失已配置窗口不得推测剩余 100%")
	}
	if quotaBlockReason(record, quotaRule{FiveHourMin: 20}, now) != "" {
		t.Fatal("0 阈值应关闭对应窗口检查")
	}
}

func TestParseQuotaUsesWindowDuration(t *testing.T) {
	now := time.Now()
	raw := fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":90.25,"limit_window_seconds":604800,"reset_at":%d},"secondary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":100}}}`, now.Add(time.Hour).Unix())
	five, weekly, err := parseCodexQuota([]byte(raw), now)
	if err != nil || five.Remaining != 100 || weekly.Remaining != 9.75 || !five.ResetAt.Equal(now.Add(100*time.Second)) {
		t.Fatal(five, weekly, err)
	}
	for _, raw := range []string{`{}`, `{"rate_limit":{"primary_window":{"used_percent":101,"limit_window_seconds":18000}}}`, `{"rate_limit":{"primary_window":{"limit_window_seconds":18000}}}`} {
		if _, _, err := parseCodexQuota([]byte(raw), now); err == nil {
			t.Fatalf("应拒绝不完整或无效窗口 %s", raw)
		}
	}
	old := quotaReading(15, now)
	higher := quotaReading(80, now.Add(time.Second))
	higher.ResetAt = old.ResetAt
	if got := mergeQuotaWindow(old, higher); got.Remaining != 15 {
		t.Fatal("同窗口读数回升不得解开保护")
	}
	late := quotaReading(100, now.Add(-time.Second))
	if mergeQuotaWindow(old, late) != old {
		t.Fatal("晚到旧请求不得覆盖新证据")
	}
	newWindow := quotaReading(100, now.Add(time.Second))
	newWindow.ResetAt = old.ResetAt.Add(5 * time.Hour)
	if mergeQuotaWindow(old, newWindow).Remaining != 100 {
		t.Fatal("新窗口应接受恢复读数")
	}
	old.ResetAt, newWindow.ResetAt = time.Time{}, time.Time{}
	if mergeQuotaWindow(old, newWindow).Remaining != 100 {
		t.Fatal("上游无重置时间时，新鲜的已恢复读数不能被旧窗口永久锁住")
	}
}

func excludedBy(t *testing.T, w *warmer, scope string) []string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"Candidates": []map[string]string{{"ID": "a", "Provider": "codex"}, {"ID": "other", "Provider": "codex"}, {"ID": "claude", "Provider": "claude"}}, "Options": map[string]any{"Metadata": map[string]string{"caller_scope": scope}}})
	value, err := dispatch(w, "scheduler.pick", raw)
	if err != nil {
		t.Fatal(err)
	}
	response := value.(map[string]any)
	if response["handled"].(bool) {
		t.Fatal("只应排除凭证，不能接管账号选择")
	}
	return response["exclude_candidates"].([]string)
}

func TestQuotaWarmupSwitchAndOrdinaryCalls(t *testing.T) {
	for _, check := range []bool{true, false} {
		t.Run(fmt.Sprint(check), func(t *testing.T) {
			w, cfg := quotaFixture(t)
			cfg.WarmupQuotaCheck = check
			w.cfg = cfg
			now := time.Now()
			w.storeQuota("a", quotaReading(15, now), quotaReading(60, now), now, "", true)
			calls, scope := 0, ""
			w.host = func(method string, request, response any) error {
				if method == "host.model.execute" {
					calls++
					r := request.(modelRequest)
					scope = r.CallerScope
					if blocked := excludedBy(t, w, scope); len(blocked) != 0 {
						t.Fatalf("本次授权预热仍被拦截：%v", blocked)
					}
					response.(*modelResponse).StatusCode = 204
				}
				return nil
			}
			result := w.warmOne(cfg, authEntry{ID: "a", Provider: "codex"})
			if check && (calls != 0 || result.Status != "skipped" || result.ErrorKind != "quota_guard") {
				t.Fatal(calls, result)
			}
			if !check && (calls != 1 || !result.Success || scope == "") {
				t.Fatal(calls, result)
			}
			if got := excludedBy(t, w, scope); len(got) != 1 || got[0] != "a" {
				t.Fatal("预热结束后普通调用或重放不得绕过保护", got)
			}
			if got := excludedBy(t, w, "forged-warmup"); len(got) != 1 {
				t.Fatal("客户端标记不得授权绕过", got)
			}
		})
	}
}

func TestQuotaPersistenceRequiresStartupVerification(t *testing.T) {
	w, cfg := quotaFixture(t)
	now := time.Now()
	w.storeQuota("a", quotaReading(100, now), quotaReading(100, now), now, "", true)
	if reason := w.quotaReason("a", now); reason != "" {
		t.Fatal(reason)
	}
	loaded, err := loadHistory(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newWarmer(w.host)
	restarted.cfg, restarted.history, restarted.accepting = cfg, loaded, true
	if restarted.quotaReason("a", now) == "" {
		t.Fatal("重启不能仅凭磁盘快照直接放行")
	}
	raw, _ := json.Marshal(w.Status())
	if strings.Contains(string(raw), `"history":{"quota_guard"`) {
		t.Fatal("历史快照应由独立状态视图输出")
	}
}

func TestQuotaNativeAPICallAndFailure(t *testing.T) {
	w, cfg := quotaFixture(t)
	used, calls := 90.0, 0
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		calls++
		if req.URL.Path != "/v8/management/requests/api-call" || req.Header.Get("Authorization") != "Bearer fake-management-key" {
			t.Error(req.URL.Path, req.Header)
		}
		var body struct {
			AuthIndex string            `json:"auth_index"`
			URL       string            `json:"url"`
			Header    map[string]string `json:"header"`
		}
		if json.NewDecoder(req.Body).Decode(&body) != nil || body.AuthIndex != "index-a" || body.Header["Authorization"] != "Bearer $TOKEN$" || body.Header["Chatgpt-Account-Id"] != "account-a" {
			t.Errorf("没有复用 CPA 原生 Token 替换与账号选择：%+v", body)
		}
		payload := fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%g,"limit_window_seconds":18000,"reset_at":%d},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%d}}}`, used, time.Now().Add(time.Hour).Unix(), time.Now().Add(7*24*time.Hour).Unix())
		_ = json.NewEncoder(rw).Encode(map[string]any{"status_code": 200, "body": payload})
	}))
	defer server.Close()
	connection := managementConnection{URL: server.URL, Key: "fake-management-key"}
	if err := writePrivateJSON(connectionCachePath(cfg.StateFile), connection); err != nil {
		t.Fatal(err)
	}
	w.host = func(method string, _ any, response any) error {
		switch method {
		case "host.auth.list":
			return json.Unmarshal([]byte(`{"files":[{"id":"a","provider":"codex","auth_index":"index-a"}]}`), response)
		case "host.auth.get":
			return json.Unmarshal([]byte(`{"json":{"account_id":"account-a"}}`), response)
		case "host.log":
			return nil
		default:
			t.Fatal("不得直接修改凭证或额外发送模型请求", method)
		}
		return nil
	}
	now := time.Now()
	w.quotaCycle(context.Background(), cfg, now)
	if calls != 1 || w.quotaReason("a", time.Now()) == "" {
		t.Fatal("读到低额度后应保护账号")
	}
	w.quotaCycle(context.Background(), cfg, now.Add(time.Second))
	if calls != 1 {
		t.Fatal("状态读取不得重复查询上游")
	}
	used = 0
	w.quotaCycle(context.Background(), cfg, now.Add(quotaPollInterval+time.Second))
	if w.quotaReason("a", time.Now()) == "" {
		t.Fatal("同一窗口更高读数不应解除保护")
	}
	w.host = func(string, any, any) error { return errors.New("secret-error-must-not-leak") }
	w.quotaCycle(context.Background(), cfg, now.Add(3*quotaPollInterval))
	if w.quotaReason("a", now.Add(3*quotaPollInterval)) == "" {
		t.Fatal("网络失败不得当成恢复")
	}
	raw, _ := os.ReadFile(cfg.StateFile)
	for _, secret := range []string{"fake-management-key", "account-a", "secret-error-must-not-leak"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("状态泄漏凭据信息", secret)
		}
	}
}

func TestQuotaManagementFailureStopsQueries(t *testing.T) {
	w, cfg := quotaFixture(t)
	cfg.QuotaGuardAccounts["b"] = quotaRule{WeeklyMin: 10}
	calls, denied := 0, true
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		calls++
		if denied {
			rw.WriteHeader(403)
			return
		}
		_, _ = rw.Write([]byte(`{"status_code":429,"header":{"retry-after":["600"]},"body":"limited"}`))
	}))
	defer server.Close()
	if err := writePrivateJSON(connectionCachePath(cfg.StateFile), managementConnection{URL: server.URL, Key: "mock-key"}); err != nil {
		t.Fatal(err)
	}
	w.host = func(method string, _ any, response any) error {
		switch method {
		case "host.auth.list":
			return json.Unmarshal([]byte(`{"files":[{"id":"a","provider":"codex","auth_index":"a"},{"id":"b","provider":"codex","auth_index":"b"}]}`), response)
		case "host.auth.get":
			return json.Unmarshal([]byte(`{"json":{"account_id":"mock-account"}}`), response)
		}
		return nil
	}
	now := time.Now()
	w.quotaCycle(context.Background(), cfg, now)
	w.quotaCycle(context.Background(), cfg, now.Add(5*time.Minute))
	if calls != 1 || w.Status().QuotaGuard.Error == "" {
		t.Fatal("管理认证失败后应暂停整个队列", calls)
	}
	w.mu.Lock()
	w.quotaHold = ""
	w.quotaVerified = map[string]bool{}
	w.mu.Unlock()
	denied = false
	w.quotaCycle(context.Background(), cfg, time.Now())
	if calls != 3 {
		t.Fatal(calls)
	}
	w.quotaCycle(context.Background(), cfg, now.Add(2*time.Minute))
	if calls != 3 {
		t.Fatal("必须遵守上游 Retry-After，不得按固定分钟继续请求", calls)
	}
}

func TestQuotaPassiveHeadersAndSkipProgress(t *testing.T) {
	w, _ := quotaFixture(t)
	now := time.Now()
	w.storeQuota("a", quotaReading(80, now), quotaReading(80, now), now, "", true)
	headers := map[string][]string{"x-codex-primary-used-percent": {"85"}, "x-codex-primary-window-minutes": {"300"}, "x-codex-primary-reset-after-seconds": {"3600"}}
	raw, _ := json.Marshal(map[string]any{"Provider": "codex", "AuthID": "a", "RequestedAt": now.Add(time.Second), "ResponseHeaders": headers})
	if _, err := dispatch(w, "usage.handle", raw); err != nil {
		t.Fatal(err)
	}
	if w.quotaReason("a", now.Add(time.Second)) == "" {
		t.Fatal("用量回调应及时收紧保护")
	}
	w.host = func(method string, _ any, response any) error {
		if method == "host.auth.list" {
			return json.Unmarshal([]byte(`{"files":[{"id":"a","provider":"codex"}]}`), response)
		}
		if method == "host.model.execute" {
			t.Fatal("额度保护必须在预热前生效")
		}
		return nil
	}
	results, err := w.Run("schedule:test")
	if err != nil || len(results) != 1 || results[0].Status != "skipped" {
		t.Fatal(results, err)
	}
	progress := w.Status().History.LastProgress
	if progress.Skipped != 1 || progress.Failed != 0 || progress.Completed != 0 {
		t.Fatal(progress)
	}
	w.cfg.QuotaGuardEnabled = false
	if len(excludedBy(t, w, "")) != 0 {
		t.Fatal("关闭保护后应交回 CPA 调度")
	}
}
