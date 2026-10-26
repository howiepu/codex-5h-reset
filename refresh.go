package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const refreshScanInterval = time.Minute

type refreshRecord struct {
	Version     string    `json:"version"`
	LeadHours   float64   `json:"lead_hours"`
	State       string    `json:"state"`
	Error       string    `json:"error,omitempty"`
	AttemptedAt time.Time `json:"attempted_at"`
	FinishedAt  time.Time `json:"finished_at"`
	RetryAt     time.Time `json:"retry_at"`
	Failures    int       `json:"failures"`
	Permanent   bool      `json:"permanent,omitempty"`
}

type refreshJournal struct {
	Records     map[string]refreshRecord      `json:"records"`
	Observed    map[string]refreshObservation `json:"observed,omitempty"`
	ServiceHash string                        `json:"service_hash,omitempty"`
	Hold        string                        `json:"hold,omitempty"`
}

type refreshObservation struct {
	Version     string    `json:"version"`
	ExpiresAt   time.Time `json:"expires_at"`
	RefreshedAt time.Time `json:"refreshed_at"`
}

type refreshAccount struct {
	ID            string     `json:"id"`
	Label         string     `json:"label"`
	State         string     `json:"state"`
	Message       string     `json:"message,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	PlannedAt     *time.Time `json:"planned_at,omitempty"`
	NextAt        *time.Time `json:"next_at,omitempty"`
	LastRefresh   *time.Time `json:"last_refresh,omitempty"`
	RefreshSource string     `json:"refresh_source,omitempty"`
	LastAttempt   *time.Time `json:"last_attempt,omitempty"`
	LastResult    string     `json:"last_result,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

type refreshStatus struct {
	Enabled     bool             `json:"enabled"`
	LeadHours   float64          `json:"lead_hours"`
	Running     bool             `json:"running"`
	ScannedAt   time.Time        `json:"scanned_at"`
	NextAt      *time.Time       `json:"next_at,omitempty"`
	NextAccount string           `json:"next_account,omitempty"`
	Error       string           `json:"error,omitempty"`
	Accounts    []refreshAccount `json:"accounts"`
}

type refreshCredential struct {
	Auth        authEntry
	Expires     time.Time
	Version     string
	Message     string
	RefreshedAt time.Time
}

func (c refreshCredential) observation() refreshObservation {
	return refreshObservation{Version: c.Version, ExpiresAt: c.Expires, RefreshedAt: c.RefreshedAt}
}

func refreshServiceHash(cfg config) string {
	sum := sha256.Sum256([]byte(cfg.TokenRefreshURL + "\x00" + cfg.TokenRefreshKey))
	return hex.EncodeToString(sum[:])
}

func (w *warmer) refreshJournalLocked() *refreshJournal {
	if w.history.TokenRefresh == nil {
		w.history.TokenRefresh = &refreshJournal{Records: map[string]refreshRecord{}}
	}
	if w.history.TokenRefresh.Records == nil {
		w.history.TokenRefresh.Records = map[string]refreshRecord{}
	}
	if w.history.TokenRefresh.Observed == nil {
		w.history.TokenRefresh.Observed = map[string]refreshObservation{}
	}
	return w.history.TokenRefresh
}

func credentialTime(raw json.RawMessage) (time.Time, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if value, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return value.UTC(), value.Unix() > 0
		}
	} else {
		text = string(raw)
	}
	seconds, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > 253402300799000 {
		return time.Time{}, false
	}
	if seconds > 1e12 {
		seconds /= 1000
	}
	value := time.Unix(int64(seconds), 0).UTC()
	return value, value.Year() <= 9999
}

func decodeRefreshCredential(auth authEntry, raw json.RawMessage) refreshCredential {
	credential := refreshCredential{Auth: auth}
	var data map[string]json.RawMessage
	if len(raw) > 2<<20 || json.Unmarshal(raw, &data) != nil {
		credential.Message = "凭据格式无效"
		return credential
	}
	var provider, accessToken, refreshToken string
	var disabled bool
	_ = json.Unmarshal(data["type"], &provider)
	_ = json.Unmarshal(data["disabled"], &disabled)
	if disabled || !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		credential.Message = "账号已停用或不再是 Codex 凭据"
		return credential
	}
	credential.RefreshedAt, _ = credentialTime(data["last_refresh"])
	_ = json.Unmarshal(data["access_token"], &accessToken)
	_ = json.Unmarshal(data["refresh_token"], &refreshToken)
	if strings.TrimSpace(refreshToken) == "" {
		credential.Message = "缺少外层 refresh token，交由 CPA 原有策略处理"
		return credential
	}
	sum := sha256.Sum256([]byte(accessToken + "\x00" + refreshToken))
	credential.Version = hex.EncodeToString(sum[:])
	parts := strings.Split(accessToken, ".")
	if len(parts) == 3 {
		payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
		var claims map[string]json.RawMessage
		if err == nil && json.Unmarshal(payload, &claims) == nil {
			if expiry, ok := credentialTime(claims["exp"]); ok {
				credential.Expires = expiry
			}
		}
	}
	if credential.Expires.IsZero() {
		for _, key := range []string{"expired", "expire", "expires_at", "expiresAt", "expiry", "expires"} {
			if expiry, ok := credentialTime(data[key]); ok {
				credential.Expires = expiry
				break
			}
		}
	}
	if credential.Expires.IsZero() {
		credential.Message = "无法读取外层令牌到期时间"
	}
	return credential
}

func (w *warmer) readRefreshCredential(auth authEntry) refreshCredential {
	credential := refreshCredential{Auth: auth}
	if auth.Disabled {
		credential.Message = "账号已停用"
		return credential
	}
	if auth.RuntimeOnly || auth.AuthIndex == "" || auth.Name == "" {
		credential.Message = "缺少可读取的账号文件或 auth_index"
		return credential
	}
	var response struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := w.host("host.auth.get", map[string]string{"auth_index": auth.AuthIndex}, &response); err != nil {
		credential.Message = "读取账号凭据失败"
		return credential
	}
	return decodeRefreshCredential(auth, response.JSON)
}

func (w *warmer) tokenRefreshPlan(cfg config, now time.Time) (refreshStatus, []refreshCredential) {
	view := refreshStatus{Enabled: cfg.Enabled && cfg.TokenRefreshEnabled, LeadHours: cfg.TokenRefreshHours, ScannedAt: now.UTC(), Accounts: []refreshAccount{}}
	if view.Enabled && cfg.TokenRefreshKey == "" {
		view.Error = "请在顶部连接设置中连接，后台连接将自动缓存"
	}
	w.mu.Lock()
	records := map[string]refreshRecord{}
	if journal := w.history.TokenRefresh; journal != nil {
		for id, record := range journal.Records {
			records[id] = record
		}
		if journal.ServiceHash == refreshServiceHash(cfg) && journal.Hold != "" {
			view.Error = journal.Hold
		}
	}
	active := w.refreshActive
	if w.stateLocked || w.stateError != "" {
		view.Error = "历史状态不可读写，自动刷新已暂停；请检查历史文件"
	}
	w.mu.Unlock()
	view.Running = active != ""
	var listed struct {
		Files []authEntry `json:"files"`
	}
	if err := w.host("host.auth.list", struct{}{}, &listed); err != nil {
		view.Error = "读取 CPA 账号列表失败"
		return view, nil
	}
	credentials := []refreshCredential{}
	seen := map[string]bool{}
	for _, auth := range listed.Files {
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || strings.TrimSpace(auth.ID) == "" || seen[auth.ID] {
			continue
		}
		seen[auth.ID] = true
		credential := w.readRefreshCredential(auth)
		row := refreshAccount{ID: auth.ID, Label: auth.Label, State: "skipped", Message: credential.Message}
		record := records[auth.ID]
		if !credential.RefreshedAt.IsZero() {
			row.LastRefresh, row.RefreshSource = &credential.RefreshedAt, "cpa"
		}
		if record.Version == credential.Version && record.State == "success" && !record.FinishedAt.IsZero() && !credential.RefreshedAt.After(record.FinishedAt) {
			row.RefreshSource = "plugin"
			if row.LastRefresh == nil {
				row.LastRefresh = &record.FinishedAt
			}
		}
		if !record.AttemptedAt.IsZero() {
			row.LastAttempt, row.LastResult, row.LastError = &record.AttemptedAt, record.State, record.Error
			if record.State == "running" && active != auth.ID {
				row.LastResult = "uncertain"
			}
		}
		if credential.Message == "" {
			planned := credential.Expires.Add(-time.Duration(cfg.TokenRefreshHours * float64(time.Hour)))
			row.ExpiresAt, row.PlannedAt, row.NextAt, row.State = &credential.Expires, &planned, &planned, "scheduled"
			if !planned.After(now) {
				row.State = "queued"
			}
			if record.Version == credential.Version {
				if record.Permanent && record.LeadHours == cfg.TokenRefreshHours {
					row.State, row.Message, row.NextAt = "blocked", record.Error, nil
				} else if record.RetryAt.After(now) && record.RetryAt.After(planned) {
					row.State, row.NextAt, row.Message = "retry", &record.RetryAt, record.Error
				}
			}
			if active == auth.ID {
				row.State, row.NextAt = "running", nil
			} else if view.Error != "" {
				row.State, row.Message, row.NextAt = "blocked", view.Error, nil
			} else if !view.Enabled {
				row.State = "paused"
			}
			credentials = append(credentials, credential)
		}
		view.Accounts = append(view.Accounts, row)
	}
	sort.SliceStable(view.Accounts, func(i, j int) bool {
		a, b := view.Accounts[i], view.Accounts[j]
		if a.NextAt == nil || b.NextAt == nil {
			if a.NextAt != b.NextAt {
				return a.NextAt != nil
			}
		} else if !a.NextAt.Equal(*b.NextAt) {
			return a.NextAt.Before(*b.NextAt)
		}
		return a.ID < b.ID
	})
	for _, row := range view.Accounts {
		if row.NextAt != nil && row.State != "paused" {
			view.NextAt, view.NextAccount = row.NextAt, row.Label
			if view.NextAccount == "" {
				view.NextAccount = row.ID
			}
			break
		}
	}
	return view, credentials
}

func (w *warmer) TokenRefreshStatus() refreshStatus {
	w.mu.Lock()
	cfg := w.cfg
	w.mu.Unlock()
	cfg, connection := refreshConnectionConfig(cfg)
	view, _ := w.tokenRefreshPlan(cfg, time.Now())
	if view.Enabled && !connection.Cached {
		view.Error = connection.Error
	}
	return view
}

func (w *warmer) tokenRefreshLoop(ctx context.Context, cfg config, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(refreshScanInterval)
	defer ticker.Stop()
	if !cfg.TokenRefreshEnabled {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.refreshWake:
		}
	}
	for {
		current, _ := refreshConnectionConfig(cfg)
		w.tokenRefreshCycle(ctx, current, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.refreshWake:
		}
	}
}

func (w *warmer) observeTokenRefreshes(cfg config, credentials []refreshCredential, now time.Time) {
	type observedLog struct {
		entry        logEntry
		storageError string
	}
	var logs []observedLog
	w.mu.Lock()
	if w.stateLocked || cfg.StateFile != w.cfg.StateFile {
		w.mu.Unlock()
		return
	}
	journal := w.refreshJournalLocked()
	dirty := false
	for _, credential := range credentials {
		id, current := credential.Auth.ID, credential.observation()
		previous, exists := journal.Observed[id]
		if current == previous || w.refreshActive == id {
			continue
		}
		journal.Observed[id] = current
		dirty = true
		if !exists {
			continue
		}
		fields := map[string]any{"auth_id": id, "label": credential.Auth.Label, "refresh_source": "cpa", "expires_at": credential.Expires.Format(time.RFC3339), "planned_at": credential.Expires.Add(-time.Duration(cfg.TokenRefreshHours * float64(time.Hour))).Format(time.RFC3339)}
		if !credential.RefreshedAt.IsZero() {
			fields["refreshed_at"] = credential.RefreshedAt.Format(time.RFC3339)
		}
		entry := logEntry{Time: now.UTC(), Level: "info", Event: "token_refresh.observed", Message: "检测到 CPA 凭据更新，已重新计算刷新计划", Fields: fields}
		logs = append(logs, observedLog{entry: entry, storageError: w.logLocked(entry)})
		dirty = false
	}
	if dirty || w.stateError != "" {
		w.persistLocked()
	}
	w.mu.Unlock()
	for _, item := range logs {
		w.emitLog(item.entry, item.storageError)
	}
}

func (w *warmer) tokenRefreshCycle(ctx context.Context, cfg config, now time.Time) {
	if ctx.Err() != nil || !cfg.Enabled {
		return
	}
	view, credentials := w.tokenRefreshPlan(cfg, now)
	if credentials == nil || ctx.Err() != nil {
		return
	}
	w.observeTokenRefreshes(cfg, credentials, now)
	w.mu.Lock()
	if journal := w.history.TokenRefresh; journal != nil {
		removed := false
		present := map[string]bool{}
		for _, row := range view.Accounts {
			present[row.ID] = true
		}
		for id := range journal.Records {
			if !present[id] {
				delete(journal.Records, id)
				removed = true
			}
		}
		for id := range journal.Observed {
			if !present[id] {
				delete(journal.Observed, id)
				removed = true
			}
		}
		if removed {
			w.persistLocked()
		}
	}
	w.mu.Unlock()
	if !view.Enabled || view.Error != "" || ctx.Err() != nil {
		return
	}
	byID := map[string]refreshCredential{}
	for _, credential := range credentials {
		byID[credential.Auth.ID] = credential
	}
	for _, row := range view.Accounts {
		if ctx.Err() != nil {
			return
		}
		if (row.State != "queued" && row.State != "retry") || row.NextAt == nil || row.NextAt.After(now) {
			continue
		}
		previous := byID[row.ID]
		current := w.readRefreshCredential(previous.Auth)
		if current.Message != "" || current.Version != previous.Version || !current.Expires.Equal(previous.Expires) {
			continue
		}
		w.mu.Lock()
		journal := w.refreshJournalLocked()
		record := journal.Records[row.ID]
		if record.Version != current.Version || record.LeadHours != cfg.TokenRefreshHours {
			record = refreshRecord{}
		}
		record.Version, record.LeadHours, record.State = current.Version, cfg.TokenRefreshHours, "running"
		record.AttemptedAt, record.RetryAt = time.Now().UTC(), time.Now().UTC().Add(15*time.Minute)
		record.FinishedAt, record.Error, record.Permanent = time.Time{}, "", false
		journal.Records[row.ID] = record
		w.refreshActive = row.ID
		w.persistLocked()
		canWrite := !w.stateLocked && w.stateError == ""
		w.mu.Unlock()
		if !canWrite {
			w.mu.Lock()
			w.refreshActive = ""
			w.mu.Unlock()
			return
		}
		w.log("info", "token_refresh.started", "开始调用 CPA 刷新账号令牌", map[string]any{"auth_id": row.ID, "label": row.Label})
		outcome := requestTokenRefresh(ctx, cfg, current.Auth)
		var updated refreshCredential
		record.FinishedAt = time.Now().UTC()
		record.Error, record.Permanent = outcome.Error, outcome.Permanent
		if outcome.Error == "" {
			updated = w.readRefreshCredential(current.Auth)
			if updated.Message != "" || updated.Version == current.Version {
				outcome.Uncertain = true
				record.Error = "CPA 已返回成功，但尚未确认凭据更新；稍后重新核对"
			} else {
				record.Version = updated.Version
				if !updated.Expires.After(record.FinishedAt.Add(time.Duration(cfg.TokenRefreshHours * float64(time.Hour)))) {
					record.Error, record.Permanent = "刷新后的有效期仍不足提前时长，请缩短提前时间后保存", true
				}
			}
		}
		if record.Error == "" {
			record.State, record.Failures, record.RetryAt = "success", 0, time.Time{}
		} else {
			record.State, record.Failures = "failed", record.Failures+1
			backoff := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
			record.RetryAt = record.FinishedAt.Add(backoff[min(record.Failures-1, len(backoff)-1)])
			if outcome.Uncertain || ctx.Err() != nil {
				record.State, record.RetryAt = "uncertain", record.FinishedAt.Add(15*time.Minute)
			}
		}
		w.mu.Lock()
		journal = w.refreshJournalLocked()
		journal.Records[row.ID] = record
		if updated.Version != "" && updated.Message == "" {
			journal.Observed[row.ID] = updated.observation()
		}
		if outcome.Hold {
			journal.ServiceHash, journal.Hold = refreshServiceHash(cfg), record.Error
		}
		w.refreshActive = ""
		w.persistLocked()
		w.mu.Unlock()
		level, event, message := "info", "token_refresh.success", "CPA 已刷新账号令牌，下一次按新到期时间排期"
		if record.Error != "" {
			level, event, message = "warn", "token_refresh.failed", record.Error
		}
		w.log(level, event, message, map[string]any{"auth_id": row.ID, "label": row.Label})
		if outcome.Hold {
			return
		}
	}
}

type refreshOutcome struct {
	Error     string
	Permanent bool
	Hold      bool
	Uncertain bool
}

var tokenRefreshClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{Transport: transport, Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}()

func requestTokenRefresh(ctx context.Context, cfg config, auth authEntry) refreshOutcome {
	body, _ := json.Marshal(map[string]string{"name": auth.Name, "auth_index": auth.AuthIndex})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenRefreshURL+"/v0/management/auth-files/refresh", bytes.NewReader(body))
	if err != nil {
		return refreshOutcome{Error: "CPA 后台管理地址无效", Hold: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.TokenRefreshKey)
	response, err := tokenRefreshClient.Do(req)
	if err != nil {
		return refreshOutcome{Error: "CPA 刷新请求未确认，请检查后台地址、网络或 CPA 日志", Uncertain: true}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return refreshOutcome{Error: "CPA 刷新响应未确认，稍后重新核对账号凭据", Uncertain: true}
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return refreshOutcome{Error: "CPA 拒绝后台管理认证，队列已暂停；请在顶部连接设置中重新连接", Hold: true}
	}
	if response.StatusCode == http.StatusNotFound || (response.StatusCode >= 300 && response.StatusCode < 400) {
		return refreshOutcome{Error: "CPA 刷新接口不可用或发生重定向，队列已暂停；请检查后台管理地址和 CPA 版本", Hold: true}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		lower := strings.ToLower(string(raw))
		if strings.Contains(lower, "invalid_grant") || strings.Contains(lower, "refresh_token_reused") || strings.Contains(lower, "refresh_token_expired") || strings.Contains(lower, "refresh_token_invalidated") {
			return refreshOutcome{Error: "账号刷新凭据已失效，请在 CPA 中重新登录该账号", Permanent: true}
		}
		return refreshOutcome{Error: fmt.Sprintf("CPA 刷新失败（HTTP %d），将退避重试", response.StatusCode)}
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(raw, &result) != nil || !result.OK {
		return refreshOutcome{Error: "CPA 刷新结果格式异常，稍后重新核对账号凭据", Uncertain: true}
	}
	return refreshOutcome{}
}
