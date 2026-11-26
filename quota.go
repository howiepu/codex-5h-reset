package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const quotaPollInterval = time.Minute
const quotaMaxAge = 2 * time.Minute

type quotaRule struct {
	FiveHourMin float64 `yaml:"five_hour_min_remaining" json:"five_hour_min_remaining"`
	WeeklyMin   float64 `yaml:"weekly_min_remaining" json:"weekly_min_remaining"`
}

type quotaWindow struct {
	Remaining  float64   `json:"remaining"`
	ResetAt    time.Time `json:"reset_at"`
	ObservedAt time.Time `json:"observed_at"`
}

type quotaRecord struct {
	FiveHour  *quotaWindow `json:"five_hour,omitempty"`
	Weekly    *quotaWindow `json:"weekly,omitempty"`
	Paused    bool         `json:"paused"`
	Reason    string       `json:"reason,omitempty"`
	Error     string       `json:"error,omitempty"`
	CheckedAt time.Time    `json:"checked_at"`
	NextCheck time.Time    `json:"next_check"`
}

type quotaAccountStatus struct {
	ID string `json:"id"`
	quotaRecord
	Rule quotaRule `json:"rule"`
}

type quotaStatus struct {
	Enabled  bool                 `json:"enabled"`
	Accounts []quotaAccountStatus `json:"accounts"`
	Error    string               `json:"error,omitempty"`
}

type quotaFetchError struct {
	Message string
	Hold    bool
	RetryAt time.Time
}

func (e *quotaFetchError) Error() string { return e.Message }

func quotaRetryAt(headers http.Header, now time.Time) time.Time {
	value := ""
	for key, values := range headers {
		if strings.EqualFold(key, "Retry-After") && len(values) > 0 {
			value = values[0]
			break
		}
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 && seconds < 365*24*3600 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at
	}
	return now.Add(5 * time.Minute)
}

func validateQuotaRules(rules map[string]quotaRule) error {
	if len(rules) > 200 {
		return fmt.Errorf("额度保护最多配置 200 个账号")
	}
	for id, rule := range rules {
		if id == "" || strings.TrimSpace(id) != id || len(id) > 512 || strings.ContainsAny(id, "\r\n\t") {
			return fmt.Errorf("额度保护必须使用有效的 CPA 凭证 ID")
		}
		for _, value := range []float64{rule.FiveHourMin, rule.WeeklyMin} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 99 {
				return fmt.Errorf("额度下限必须在 0 到 99%% 之间，0 表示不检查该窗口")
			}
		}
		if rule.FiveHourMin == 0 && rule.WeeklyMin == 0 {
			return fmt.Errorf("受保护账号至少需要设置一个非零额度下限")
		}
	}
	return nil
}

func quotaBlockReason(record quotaRecord, rule quotaRule, now time.Time) string {
	for _, item := range []struct {
		name    string
		minimum float64
		window  *quotaWindow
	}{
		{"5 小时", rule.FiveHourMin, record.FiveHour}, {"周", rule.WeeklyMin, record.Weekly},
	} {
		if item.minimum == 0 {
			continue
		}
		if item.window == nil {
			return "等待确认" + item.name + "额度；上游未提供该窗口时请关闭该窗口检查"
		}
		window := item.window
		if window.ObservedAt.IsZero() || now.Sub(window.ObservedAt) > quotaMaxAge || window.ObservedAt.After(now.Add(time.Minute)) {
			return item.name + "额度读数已过期，等待复查"
		}
		if !window.ResetAt.IsZero() && !window.ResetAt.After(now) {
			return item.name + "窗口已到重置时间，等待上游确认"
		}
		if window.Remaining <= item.minimum {
			return fmt.Sprintf("%s剩余 %.2f%%，已达到保留下限 %.2f%%", item.name, window.Remaining, item.minimum)
		}
		// 留出固定恢复缓冲，避免临界读数反复暂停和恢复。
		if record.Paused && window.Remaining < math.Min(100, item.minimum+2) {
			return item.name + "额度尚未超过恢复缓冲"
		}
	}
	return ""
}

func (w *warmer) quotaReasonLocked(id string, now time.Time) string {
	if !w.accepting || !w.cfg.QuotaGuardEnabled {
		return ""
	}
	rule, exists := w.cfg.QuotaGuardAccounts[id]
	if !exists {
		return ""
	}
	if w.quotaHold != "" {
		return w.quotaHold
	}
	if w.stateLocked || w.stateError != "" {
		return "额度保护状态不可读写，等待恢复"
	}
	if !w.quotaVerified[id] {
		return "等待本次启动或配置更新后的额度复查"
	}
	return quotaBlockReason(w.history.QuotaGuard[id], rule, now)
}

func (w *warmer) quotaReason(id string, now time.Time) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.quotaReasonLocked(id, now)
}

func (w *warmer) quotaStatusLocked(now time.Time) quotaStatus {
	view := quotaStatus{Enabled: w.accepting && w.cfg.QuotaGuardEnabled, Accounts: []quotaAccountStatus{}, Error: w.quotaHold}
	for id, rule := range w.cfg.QuotaGuardAccounts {
		record := w.history.QuotaGuard[id]
		record.Reason = w.quotaReasonLocked(id, now)
		record.Paused = record.Reason != ""
		view.Accounts = append(view.Accounts, quotaAccountStatus{ID: id, quotaRecord: record, Rule: rule})
	}
	sort.Slice(view.Accounts, func(i, j int) bool { return view.Accounts[i].ID < view.Accounts[j].ID })
	return view
}

func (w *warmer) allowWarmup(id string) string {
	// 使用宿主内部 caller_scope 识别本次预热，不信任客户端可伪造的请求头。
	scope := pluginID + ":" + rand.Text()
	w.mu.Lock()
	w.warmupScopes[scope] = id
	w.mu.Unlock()
	return scope
}

func (w *warmer) finishWarmup(scope string) {
	w.mu.Lock()
	delete(w.warmupScopes, scope)
	w.mu.Unlock()
}

func (w *warmer) quotaPick(raw []byte) (any, error) {
	var req struct {
		Candidates []struct{ ID, Provider string }
		Options    struct{ Metadata map[string]any }
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("额度保护调度请求格式无效")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	scope, _ := req.Options.Metadata["caller_scope"].(string)
	bypassID := w.warmupScopes[scope]
	excluded := []string{}
	for _, candidate := range req.Candidates {
		if candidate.Provider == "codex" && (bypassID == "" || candidate.ID != bypassID) && w.quotaReasonLocked(candidate.ID, time.Now()) != "" {
			excluded = append(excluded, candidate.ID)
		}
	}
	return map[string]any{"handled": false, "exclude_candidates": excluded}, nil
}

func mergeQuotaWindow(previous, next *quotaWindow) *quotaWindow {
	if next == nil {
		return previous
	}
	if previous == nil {
		return next
	}
	if next.ObservedAt.Before(previous.ObservedAt) {
		return previous
	}
	if !previous.ResetAt.IsZero() && !next.ResetAt.IsZero() && next.ResetAt.Before(previous.ResetAt.Add(-90*time.Second)) {
		return previous
	}
	sameWindow := !previous.ResetAt.IsZero() && !next.ResetAt.IsZero() && math.Abs(next.ResetAt.Sub(previous.ResetAt).Seconds()) <= 90
	if sameWindow && next.Remaining > previous.Remaining {
		copy := *next
		copy.Remaining = previous.Remaining
		return &copy
	}
	return next
}

func (w *warmer) storeQuota(id string, five, weekly *quotaWindow, checked time.Time, failure string, verified bool) {
	w.mu.Lock()
	if _, exists := w.cfg.QuotaGuardAccounts[id]; !exists || !w.accepting || !w.cfg.QuotaGuardEnabled {
		w.mu.Unlock()
		return
	}
	if w.history.QuotaGuard == nil {
		w.history.QuotaGuard = map[string]quotaRecord{}
	}
	record := w.history.QuotaGuard[id]
	previousReason := record.Reason
	record.FiveHour, record.Weekly = mergeQuotaWindow(record.FiveHour, five), mergeQuotaWindow(record.Weekly, weekly)
	if !checked.IsZero() {
		if _, attempted := w.quotaVerified[id]; !attempted {
			w.quotaVerified[id] = false
		}
		record.CheckedAt, record.NextCheck, record.Error = checked, checked.Add(quotaPollInterval), failure
		for _, window := range []*quotaWindow{record.FiveHour, record.Weekly} {
			if window != nil && window.ResetAt.After(checked) && window.ResetAt.Before(record.NextCheck) {
				record.NextCheck = window.ResetAt.Add(2 * time.Second)
			}
		}
	}
	if verified {
		w.quotaVerified[id] = true
	}
	record.Reason = quotaBlockReason(record, w.cfg.QuotaGuardAccounts[id], time.Now())
	record.Paused = record.Reason != ""
	w.history.QuotaGuard[id] = record
	var entry *logEntry
	storageError := ""
	if previousReason != record.Reason {
		event, message := "quota_guard.recovered", "额度复查通过，账号已解除保护"
		if record.Paused {
			event, message = "quota_guard.paused", record.Reason
		}
		item := logEntry{Time: time.Now().UTC(), Level: "info", Event: event, Message: message, Fields: map[string]any{"auth_id": id}}
		storageError, entry = w.logLocked(item), &item
	} else {
		w.persistLocked()
	}
	w.mu.Unlock()
	if entry != nil {
		w.emitLog(*entry, storageError)
	}
}

func parseQuotaWindow(raw json.RawMessage, observed time.Time) (*quotaWindow, int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, 0, nil
	}
	var value struct {
		Used       *float64 `json:"used_percent"`
		Seconds    int      `json:"limit_window_seconds"`
		ResetAt    int64    `json:"reset_at"`
		ResetAfter *float64 `json:"reset_after_seconds"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Used == nil || math.IsNaN(*value.Used) || math.IsInf(*value.Used, 0) || *value.Used < 0 || *value.Used > 100 {
		return nil, 0, fmt.Errorf("上游额度窗口格式无效")
	}
	window := &quotaWindow{Remaining: 100 - *value.Used, ObservedAt: observed}
	if value.ResetAt > 0 {
		window.ResetAt = time.Unix(value.ResetAt, 0).UTC()
	} else if value.ResetAfter != nil && *value.ResetAfter > 0 && *value.ResetAfter <= 8*24*3600 {
		window.ResetAt = observed.Add(time.Duration(*value.ResetAfter * float64(time.Second)))
	}
	return window, value.Seconds, nil
}

func parseCodexQuota(raw []byte, observed time.Time) (five, weekly *quotaWindow, err error) {
	var data struct {
		RateLimit struct {
			Primary   json.RawMessage `json:"primary_window"`
			Secondary json.RawMessage `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return nil, nil, fmt.Errorf("上游额度响应格式无效")
	}
	for _, rawWindow := range []json.RawMessage{data.RateLimit.Primary, data.RateLimit.Secondary} {
		window, seconds, parseErr := parseQuotaWindow(rawWindow, observed)
		if parseErr != nil {
			return nil, nil, parseErr
		}
		switch seconds {
		case 18000:
			five = window
		case 604800:
			weekly = window
		}
	}
	if five == nil && weekly == nil {
		return nil, nil, fmt.Errorf("上游未返回可识别的 5 小时或周额度窗口")
	}
	return five, weekly, nil
}

func codexAccountID(raw []byte) string {
	var data struct {
		AccountID string `json:"account_id"`
		IDToken   string `json:"id_token"`
	}
	if len(raw) > 2<<20 || json.Unmarshal(raw, &data) != nil {
		return ""
	}
	if data.AccountID != "" {
		return data.AccountID
	}
	parts := strings.Split(data.IDToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.AccountID
}

func (w *warmer) fetchQuota(ctx context.Context, connection managementConnection, auth authEntry, observed time.Time) (*quotaWindow, *quotaWindow, error) {
	if auth.AuthIndex == "" {
		return nil, nil, fmt.Errorf("该账号缺少 auth_index")
	}
	var stored struct {
		JSON json.RawMessage `json:"json"`
	}
	if w.host("host.auth.get", map[string]string{"auth_index": auth.AuthIndex}, &stored) != nil {
		return nil, nil, fmt.Errorf("无法读取 CPA 账号信息")
	}
	accountID := codexAccountID(stored.JSON)
	if accountID == "" {
		return nil, nil, fmt.Errorf("CPA 账号缺少可确认的 ChatGPT account_id")
	}
	body, _ := json.Marshal(map[string]any{"auth_index": auth.AuthIndex, "method": "GET", "url": "https://chatgpt.com/backend-api/wham/usage", "header": map[string]string{"Authorization": "Bearer $TOKEN$", "Chatgpt-Account-Id": accountID, "User-Agent": "codex_cli_rs/0.101.0", "Accept": "application/json"}})
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, connection.URL+"/v8/management/requests/api-call", bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("CPA 后台管理地址无效")
	}
	req.Header.Set("Authorization", "Bearer "+connection.Key)
	req.Header.Set("Content-Type", "application/json")
	response, err := tokenRefreshClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("CPA 额度查询未确认，等待复查")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
	if err != nil || len(raw) > 2<<20 {
		return nil, nil, fmt.Errorf("CPA 额度响应无法读取")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 404 {
			return nil, nil, &quotaFetchError{Message: fmt.Sprintf("CPA 额度管理接口不可用（HTTP %d），后台查询已暂停；请在顶部重新连接并检查版本", response.StatusCode), Hold: true}
		}
		if response.StatusCode == 429 {
			return nil, nil, &quotaFetchError{Message: "CPA 额度查询被限流，等待退避后复查", RetryAt: quotaRetryAt(response.Header, time.Now())}
		}
		return nil, nil, fmt.Errorf("CPA 拒绝额度查询（HTTP %d），请检查后台连接与版本", response.StatusCode)
	}
	var result struct {
		StatusCode int         `json:"status_code"`
		Body       string      `json:"body"`
		Header     http.Header `json:"header"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return nil, nil, fmt.Errorf("CPA 额度响应格式无效")
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		if result.StatusCode == 429 {
			return nil, nil, &quotaFetchError{Message: "上游额度接口被限流，等待退避后复查", RetryAt: quotaRetryAt(result.Header, time.Now())}
		}
		return nil, nil, fmt.Errorf("上游额度查询失败（HTTP %d），等待复查；401 请检查令牌刷新", result.StatusCode)
	}
	return parseCodexQuota([]byte(result.Body), observed)
}

func (w *warmer) quotaLoop(ctx context.Context, cfg config, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		w.quotaCycle(ctx, cfg, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.quotaWake:
		}
	}
}

func (w *warmer) quotaCycle(ctx context.Context, cfg config, now time.Time) {
	if ctx.Err() != nil || !cfg.Enabled || !cfg.QuotaGuardEnabled || len(cfg.QuotaGuardAccounts) == 0 {
		return
	}
	w.mu.Lock()
	held := w.quotaHold != ""
	w.mu.Unlock()
	if held {
		return
	}
	connection, connectionErr := readConnection(cfg.StateFile)
	var listed struct {
		Files []authEntry `json:"files"`
	}
	listErr := w.host("host.auth.list", struct{}{}, &listed)
	byID := map[string]authEntry{}
	for _, auth := range listed.Files {
		if auth.Provider == "codex" {
			byID[auth.ID] = auth
		}
	}
	ids := make([]string, 0, len(cfg.QuotaGuardAccounts))
	for id := range cfg.QuotaGuardAccounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		w.mu.Lock()
		next := w.history.QuotaGuard[id].NextCheck
		_, attempted := w.quotaVerified[id]
		w.mu.Unlock()
		if attempted && next.After(now) {
			continue
		}
		auth, exists := byID[id]
		var failure error
		switch {
		case connectionErr != nil:
			failure = connectionErr
		case listErr != nil:
			failure = fmt.Errorf("读取 CPA 账号列表失败")
		case !exists:
			failure = fmt.Errorf("CPA 中暂未发现该 Codex 账号")
		case auth.Disabled:
			failure = fmt.Errorf("账号已在 CPA 停用，额度保护不会启用它")
		}
		if failure != nil {
			w.storeQuota(id, nil, nil, now, failure.Error(), false)
			continue
		}
		started := time.Now()
		five, weekly, err := w.fetchQuota(ctx, connection, auth, started)
		if ctx.Err() != nil {
			return
		}
		message := ""
		if err != nil {
			message = err.Error()
		}
		w.storeQuota(id, five, weekly, time.Now(), message, err == nil)
		var fetchError *quotaFetchError
		if errors.As(err, &fetchError) {
			w.mu.Lock()
			if fetchError.Hold {
				w.quotaHold = fetchError.Message
			}
			if fetchError.RetryAt.After(time.Now()) {
				record := w.history.QuotaGuard[id]
				record.NextCheck = fetchError.RetryAt
				w.history.QuotaGuard[id] = record
				w.persistLocked()
			}
			w.mu.Unlock()
			if fetchError.Hold {
				return
			}
		}
	}
}

func (w *warmer) observeQuotaUsage(raw []byte) error {
	var usage struct {
		Provider, AuthID string
		RequestedAt      time.Time
		ResponseHeaders  http.Header
	}
	if json.Unmarshal(raw, &usage) != nil {
		return fmt.Errorf("额度用量记录格式无效")
	}
	if usage.Provider != "codex" || usage.AuthID == "" || usage.RequestedAt.IsZero() {
		return nil
	}
	headers := http.Header{}
	for key, values := range usage.ResponseHeaders {
		for _, value := range values {
			headers.Add(key, value)
		}
	}
	var five, weekly *quotaWindow
	for _, name := range []string{"primary", "secondary"} {
		prefix := "X-Codex-" + name
		used, err := strconv.ParseFloat(headers.Get(prefix+"-Used-Percent"), 64)
		if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
			continue
		}
		minutes, _ := strconv.Atoi(headers.Get(prefix + "-Window-Minutes"))
		reset, err := strconv.ParseInt(headers.Get(prefix+"-Reset-At"), 10, 64)
		if err != nil || reset <= 0 {
			after, afterErr := strconv.ParseFloat(headers.Get(prefix+"-Reset-After-Seconds"), 64)
			if afterErr != nil || math.IsNaN(after) || math.IsInf(after, 0) || after <= 0 || after > 8*24*3600 {
				continue
			}
			reset = usage.RequestedAt.Add(time.Duration(after * float64(time.Second))).Unix()
		}
		window := &quotaWindow{Remaining: 100 - used, ResetAt: time.Unix(reset, 0).UTC(), ObservedAt: usage.RequestedAt}
		switch minutes {
		case 300:
			five = window
		case 10080:
			weekly = window
		}
	}
	if five != nil || weekly != nil {
		w.storeQuota(usage.AuthID, five, weekly, time.Time{}, "", false)
	}
	return nil
}
