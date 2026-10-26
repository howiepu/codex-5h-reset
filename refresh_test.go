package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type refreshFixture struct {
	mu      sync.Mutex
	auths   []authEntry
	data    map[string]map[string]any
	gets    map[string]int
	before  func(string, int)
	rotated int
}

func newRefreshFixture(t *testing.T) (*warmer, config, *refreshFixture) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	f := &refreshFixture{data: map[string]map[string]any{}, gets: map[string]int{}}
	for _, pair := range []struct {
		id   string
		days int
	}{{"a", 5}, {"b", 4}, {"future", 8}} {
		f.auths = append(f.auths, authEntry{ID: pair.id, Name: pair.id + ".json", AuthIndex: pair.id, Label: "模拟 " + pair.id, Provider: "codex"})
		f.data[pair.id] = map[string]any{"type": "codex", "access_token": "access-canary-" + pair.id, "refresh_token": "refresh-canary-" + pair.id, "expired": now.Add(time.Duration(pair.days) * 24 * time.Hour).Format(time.RFC3339)}
	}
	f.auths = append(f.auths, authEntry{ID: "disabled", Provider: "codex", Disabled: true}, authEntry{ID: "other", Provider: "claude"})
	w, cfg := testWarmer(t, func(method string, request, response any) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		var data any
		switch method {
		case "host.auth.list":
			data = map[string]any{"files": f.auths}
		case "host.auth.get":
			id := request.(map[string]string)["auth_index"]
			f.gets[id]++
			if f.before != nil {
				f.before(id, f.gets[id])
			}
			data = map[string]any{"json": f.data[id]}
		case "host.log":
			return nil
		default:
			return fmt.Errorf("刷新队列不应调用 %s", method)
		}
		raw, _ := json.Marshal(data)
		return json.Unmarshal(raw, response)
	})
	cfg.TokenRefreshEnabled, cfg.TokenRefreshKey = true, "management-canary"
	return w, cfg, f
}

func (f *refreshFixture) rotate(id string, validity time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rotated++
	f.data[id]["access_token"] = fmt.Sprintf("rotated-access-canary-%d", f.rotated)
	f.data[id]["refresh_token"] = fmt.Sprintf("rotated-refresh-canary-%d", f.rotated)
	f.data[id]["expired"] = time.Now().Add(validity).UTC().Format(time.RFC3339)
	f.data[id]["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
}

func TestObserveCPARefreshAndPersistLog(t *testing.T) {
	w, cfg, f := newRefreshFixture(t)
	f.auths = f.auths[2:3]
	cfg.TokenRefreshEnabled = false
	initial := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	f.data["future"]["last_refresh"] = initial.Format(time.RFC3339)
	w.tokenRefreshCycle(context.Background(), cfg, time.Now())
	baseline, _ := w.tokenRefreshPlan(cfg, time.Now())
	if baseline.Accounts[0].LastRefresh == nil || !baseline.Accounts[0].LastRefresh.Equal(initial) || baseline.Accounts[0].LastAttempt != nil {
		t.Fatal("已有的 CPA 刷新时间应立即展示，不产生插件尝试")
	}
	countUpdates := func(w *warmer) int {
		t.Helper()
		logs, err := w.Logs(time.Now().UTC().Format("2006-01-02"))
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, entry := range logs {
			if entry.Event == "token_refresh.observed" {
				count++
			}
		}
		return count
	}
	if countUpdates(w) != 0 {
		t.Fatal("首次读取只能建立基线，不能伪造历史刷新日志")
	}
	refreshed := time.Now().UTC().Truncate(time.Second)
	f.rotate("future", 10*24*time.Hour)
	f.data["future"]["last_refresh"] = refreshed.Format(time.RFC3339)
	w.tokenRefreshCycle(context.Background(), cfg, time.Now())
	if got := countUpdates(w); got != 1 {
		t.Fatalf("CPA 更新后即使自动刷新关闭，也应产生一条运行日志，实际 %d 条", got)
	}
	view, _ := w.tokenRefreshPlan(cfg, time.Now())
	raw, _ := json.Marshal(view.Accounts[0])
	var row map[string]any
	_ = json.Unmarshal(raw, &row)
	if row["last_refresh"] != refreshed.Format(time.RFC3339) || row["refresh_source"] != "cpa" || row["last_attempt"] != nil {
		t.Fatalf("应显示 CPA 最近刷新，不能伪造插件尝试：%s", raw)
	}
	w.tokenRefreshCycle(context.Background(), cfg, time.Now())
	w.Stop()
	restarted := newWarmer(w.host)
	restarted.Configure(cfg)
	defer restarted.Stop()
	restarted.tokenRefreshCycle(context.Background(), cfg, time.Now())
	if countUpdates(restarted) != 1 {
		t.Fatal("重复扫描和重启不得重复记录同一次更新")
	}
	f.rotate("future", 11*24*time.Hour)
	f.data["future"]["last_refresh"] = refreshed.Add(time.Minute).Format(time.RFC3339)
	restarted.tokenRefreshCycle(context.Background(), cfg, time.Now())
	if countUpdates(restarted) != 2 {
		t.Fatal("重启后新的凭据更新仍需记录")
	}
	stored, _ := os.ReadFile(cfg.StateFile)
	if strings.Contains(string(stored), "canary") {
		t.Fatal("观察记录和运行日志不得保存令牌或管理密钥")
	}
}

func TestRefreshObserverRunsWithoutConnectionOrAutoRefresh(t *testing.T) {
	w, _, f := newRefreshFixture(t)
	f.auths = f.auths[2:3]
	waitFor := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("后台观察没有完成")
	}
	w.refreshWake <- struct{}{}
	waitFor(func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.history.TokenRefresh != nil && len(w.history.TokenRefresh.Observed) == 1
	})
	f.rotate("future", 10*24*time.Hour)
	w.refreshWake <- struct{}{}
	waitFor(func() bool {
		for _, entry := range w.Status().History.Logs {
			if entry.Event == "token_refresh.observed" {
				return true
			}
		}
		return false
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cfg.TokenRefreshEnabled || len(w.history.TokenRefresh.Records) != 0 || cachedConnectionState(w.cfg.StateFile).Cached {
		t.Fatal("后台观察只应写插件日志，不应启用、触发刷新或依赖连接缓存")
	}
}

func TestRefreshObservationMetadataAndAttemptIsolation(t *testing.T) {
	for _, change := range []string{"last_refresh", "expired"} {
		t.Run(change, func(t *testing.T) {
			w, cfg, f := newRefreshFixture(t)
			f.auths, cfg.TokenRefreshEnabled = f.auths[2:3], false
			previous := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			f.data["future"]["last_refresh"] = previous.Format(time.RFC3339)
			w.tokenRefreshCycle(context.Background(), cfg, time.Now())
			credential := w.readRefreshCredential(f.auths[0])
			w.mu.Lock()
			w.refreshJournalLocked().Records["future"] = refreshRecord{Version: credential.Version, State: "failed", AttemptedAt: previous, Error: "保留上次插件失败"}
			w.mu.Unlock()
			if change == "last_refresh" {
				f.data["future"][change] = previous.Add(time.Minute).Format(time.RFC3339)
			} else {
				f.data["future"][change] = time.Now().Add(10 * 24 * time.Hour).UTC().Format(time.RFC3339)
			}
			var scans sync.WaitGroup
			for range 3 {
				scans.Go(func() { w.tokenRefreshCycle(context.Background(), cfg, time.Now()) })
			}
			scans.Wait()
			view, _ := w.tokenRefreshPlan(cfg, time.Now())
			row := view.Accounts[0]
			if row.LastRefresh == nil || row.RefreshSource != "cpa" || row.LastResult != "failed" || row.LastError != "保留上次插件失败" || !row.LastAttempt.Equal(previous) {
				t.Fatal("最近刷新与插件尝试结果应独立保留")
			}
			updates := 0
			for _, entry := range w.Status().History.Logs {
				if entry.Event == "token_refresh.observed" {
					updates++
					if entry.Fields["refresh_source"] != "cpa" || entry.Fields["refreshed_at"] == nil || entry.Fields["planned_at"] == nil {
						t.Fatal("插件运行日志应包含来源、刷新时间和重排计划")
					}
				}
			}
			if updates != 1 {
				t.Fatal("只改变有效期或刷新时间也应记录，并发扫描不能重复写入")
			}
		})
	}
}

func TestRefreshObserverPreservesBaselineOnReadFailure(t *testing.T) {
	w, cfg, _ := newRefreshFixture(t)
	cfg.TokenRefreshEnabled = false
	w.tokenRefreshCycle(context.Background(), cfg, time.Now())
	w.Stop()
	host := w.host
	w.host = func(method string, request, response any) error {
		if method == "host.auth.list" {
			return fmt.Errorf("模拟账号列表暂时不可用")
		}
		return host(method, request, response)
	}
	w.tokenRefreshCycle(context.Background(), cfg, time.Now())
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.history.TokenRefresh.Observed) != 3 {
		t.Fatal("读取列表失败不能清空观察基线")
	}
}

func TestRefreshConfigurationAndExpiry(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil || cfg.TokenRefreshEnabled || cfg.TokenRefreshHours != 120 || cfg.TokenRefreshURL != "" {
		t.Fatal("刷新功能应默认关闭并提前五天")
	}
	for _, raw := range []string{"token_refresh_before_hours: 0", "token_refresh_before_hours: .nan", "token_refresh_before_hours: .inf", "token_refresh_before_hours: 9000"} {
		if _, err := parseConfig([]byte(raw)); err == nil {
			t.Fatalf("应拒绝无效刷新配置：%s", raw)
		}
	}
	expires := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expires.Unix())))
	raw := []byte(fmt.Sprintf(`{"type":"codex","access_token":"header.%s.signature","refresh_token":"fake","expired":"2020-01-01T00:00:00Z"}`, claims))
	if got := decodeRefreshCredential(authEntry{}, raw); !got.Expires.Equal(expires) || got.Message != "" {
		t.Fatal("应优先采用外层 access token 的 JWT 到期时间")
	}
	for _, raw := range []string{`{"type":"codex","refresh_token":"fake","expired":"NaN"}`, `{"type":"codex","refresh_token":"fake","expired":"Inf"}`, `{"type":"codex","refresh_token":"fake"}`, `{"type":"codex","siwc":{"refresh_token":"fake","expired":"2030-01-01T00:00:00Z"}}`, `{"type":"claude","refresh_token":"fake","expired":"2030-01-01T00:00:00Z"}`} {
		if got := decodeRefreshCredential(authEntry{}, []byte(raw)); got.Message == "" {
			t.Fatalf("未知有效期、其他服务商或仅 SIWC 凭据应跳过：%s", raw)
		}
	}
}

func TestRefreshQueueSerialAndReschedule(t *testing.T) {
	w, cfg, f := newRefreshFixture(t)
	var calls []string
	var active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v0/management/auth-files/refresh" || r.Header.Get("Authorization") != "Bearer management-canary" {
			t.Error("应只调用带管理认证的 CPA 原生刷新接口")
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body) != 2 || body["name"] != body["auth_index"]+".json" {
			t.Error("应指定单账号，不传令牌或 all=true")
		}
		current := active.Add(1)
		if current > peak.Load() {
			peak.Store(current)
		}
		time.Sleep(10 * time.Millisecond)
		calls = append(calls, body["auth_index"])
		f.rotate(body["auth_index"], 10*24*time.Hour)
		active.Add(-1)
		_, _ = rw.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	cfg.TokenRefreshURL = server.URL
	response := w.handleManagement(managementRequest{Method: "GET", Path: managementBase + "/token-refresh"})
	if len(calls) != 0 || response.StatusCode != 200 || strings.Contains(string(response.Body), "canary") {
		t.Fatal("查看计划不得刷新账号或返回令牌")
	}
	w.tokenRefreshCycle(context.Background(), cfg, time.Now())
	if !reflect.DeepEqual(calls, []string{"b", "a"}) || peak.Load() != 1 {
		t.Fatalf("应按到期顺序串行处理，仅刷新到期的启用 Codex：%v", calls)
	}
	view, _ := w.tokenRefreshPlan(cfg, time.Now())
	if view.Error != "" || view.NextAt == nil || !view.NextAt.After(time.Now()) || len(view.Accounts) != 4 {
		t.Fatalf("应按新凭据重新排期并展示下次时间：%+v", view)
	}
	for _, row := range view.Accounts {
		if row.LastResult == "success" && (row.LastRefresh == nil || row.RefreshSource != "plugin") {
			t.Fatal("已确认的插件刷新应显示最近刷新时间与插件来源")
		}
	}
	w.tokenRefreshCycle(context.Background(), cfg, time.Now())
	if len(calls) != 2 {
		t.Fatal("下一次扫描不得重复刷新")
	}
	for _, entry := range w.Status().History.Logs {
		if entry.Event == "token_refresh.observed" {
			t.Fatal("插件发起的成功刷新不能重复记为 CPA 外部更新")
		}
	}
	raw, _ := os.ReadFile(cfg.StateFile)
	if strings.Contains(string(raw), "canary") {
		t.Fatal("持久化记录不得包含令牌、管理密钥或原始响应")
	}
	w.mu.Lock()
	w.cfg.TokenRefreshKey, w.cfg.TokenRefreshURL = cfg.TokenRefreshKey, cfg.TokenRefreshURL
	w.mu.Unlock()
	status, _ := json.Marshal(w.Status())
	if strings.Contains(string(status), "canary") || strings.Contains(string(status), server.URL) || strings.Contains(string(status), "service_hash") {
		t.Fatal("状态接口应隐藏后台管理认证和内部凭据指纹")
	}
}

func TestRefreshRechecksCredentialsBeforeSending(t *testing.T) {
	w, cfg, f := newRefreshFixture(t)
	f.before = func(id string, count int) {
		if id == "b" && count == 2 {
			f.data[id]["access_token"] = "updated-by-cpa"
			f.data[id]["expired"] = time.Now().Add(10 * 24 * time.Hour).UTC().Format(time.RFC3339)
		}
	}
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, body["auth_index"])
		f.rotate(body["auth_index"], 10*24*time.Hour)
		_, _ = rw.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	cfg.TokenRefreshURL = server.URL
	w.tokenRefreshCycle(context.Background(), cfg, time.Now())
	if !reflect.DeepEqual(calls, []string{"a"}) {
		t.Fatalf("CPA 已更新的凭据不应重复刷新：%v", calls)
	}
}

func TestRefreshBackoffAndPermanentFailure(t *testing.T) {
	for _, kind := range []string{"temporary", "invalid_grant", "unauthorized", "short_validity", "unconfirmed"} {
		t.Run(kind, func(t *testing.T) {
			w, cfg, f := newRefreshFixture(t)
			f.auths = f.auths[1:2]
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				switch kind {
				case "temporary":
					if n == 1 {
						rw.WriteHeader(500)
						_, _ = rw.Write([]byte(`{"error":"response-secret-canary"}`))
						return
					}
				case "invalid_grant":
					rw.WriteHeader(500)
					_, _ = rw.Write([]byte(`{"error":"invalid_grant response-secret-canary"}`))
					return
				case "unauthorized":
					rw.WriteHeader(401)
					return
				case "short_validity":
					f.rotate(body["auth_index"], time.Hour)
					_, _ = rw.Write([]byte(`{"ok":true}`))
					return
				case "unconfirmed":
					_, _ = rw.Write([]byte(`{"ok":true}`))
					return
				}
				f.rotate(body["auth_index"], 10*24*time.Hour)
				_, _ = rw.Write([]byte(`{"ok":true}`))
			}))
			defer server.Close()
			cfg.TokenRefreshURL = server.URL
			w.tokenRefreshCycle(context.Background(), cfg, time.Now())
			w.tokenRefreshCycle(context.Background(), cfg, time.Now().Add(30*time.Second))
			if calls.Load() != 1 {
				t.Fatal("失败或短有效期不应立即重复调用")
			}
			w.tokenRefreshCycle(context.Background(), cfg, time.Now().Add(2*time.Minute))
			if kind == "temporary" && calls.Load() != 2 {
				t.Fatal("临时失败应按退避计划重新尝试")
			}
			if kind != "temporary" && calls.Load() != 1 {
				t.Fatal("认证失败、短有效期或结果未确认应暂停或等待核对")
			}
			if kind == "unauthorized" {
				w.mu.Lock()
				w.history, _ = loadHistory(cfg.StateFile)
				w.mu.Unlock()
				w.tokenRefreshCycle(context.Background(), cfg, time.Now().Add(time.Hour))
				if calls.Load() != 1 {
					t.Fatal("重启后也应保留管理认证暂停状态，避免反复触发封禁")
				}
				cfg.TokenRefreshKey = "new-management-canary"
				w.tokenRefreshCycle(context.Background(), cfg, time.Now().Add(time.Hour))
				if calls.Load() != 2 {
					t.Fatal("更新后台认证后应允许重新尝试")
				}
				off := cfg
				off.TokenRefreshEnabled = false
				if err := writePrivateJSON(connectionCachePath(cfg.StateFile), managementConnection{URL: cfg.TokenRefreshURL, Key: cfg.TokenRefreshKey}); err != nil {
					t.Fatal(err)
				}
				w.Configure(off)
				w.Configure(cfg)
				view, _ := w.tokenRefreshPlan(cfg, time.Now())
				if view.Error != "" {
					t.Fatal("手动关闭后重新启用应解除认证暂停状态")
				}
			}
			raw, _ := os.ReadFile(cfg.StateFile)
			if strings.Contains(string(raw), "canary") {
				t.Fatal("失败响应中的敏感信息不得进入日志或历史")
			}
		})
	}
}

func TestRefreshRedirectAndStateProtection(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	cfg := defaultConfig()
	cfg.TokenRefreshURL, cfg.TokenRefreshKey = origin.URL, "redirect-secret-canary"
	outcome := requestTokenRefresh(context.Background(), cfg, authEntry{Name: "a.json", AuthIndex: "a"})
	if !outcome.Hold || forwarded.Load() != 0 || strings.Contains(outcome.Error, "canary") {
		t.Fatal("管理密钥不得随重定向转发")
	}
	w, cfg, _ := newRefreshFixture(t)
	cfg.TokenRefreshURL = target.URL
	w.mu.Lock()
	w.stateLocked = true
	w.mu.Unlock()
	w.tokenRefreshCycle(context.Background(), cfg, time.Now())
	if forwarded.Load() != 0 {
		t.Fatal("无法可靠保存历史时不得刷新令牌")
	}
}

func TestRefreshLifecycleCancellationAndRestart(t *testing.T) {
	w, cfg, f := newRefreshFixture(t)
	f.auths = f.auths[1:2]
	started := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls.Add(1)
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer server.Close()
	cfg.TokenRefreshURL = server.URL
	if err := writePrivateJSON(connectionCachePath(cfg.StateFile), managementConnection{URL: cfg.TokenRefreshURL, Key: cfg.TokenRefreshKey}); err != nil {
		t.Fatal(err)
	}
	w.Configure(cfg)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("后台应在页面未打开时执行到期账号")
	}
	stopped := make(chan struct{})
	go func() { w.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("插件退出应取消等待并保存未确认状态")
	}
	loaded, err := loadHistory(cfg.StateFile)
	if err != nil || loaded.TokenRefresh.Records["b"].State != "uncertain" {
		t.Fatal("取消时必须保留未确认记录")
	}
	w.tokenRefreshCycle(context.Background(), cfg, time.Now())
	if calls.Load() != 1 {
		t.Fatal("重启恢复不能直接重放未确认刷新")
	}
	changed := cfg
	changed.TokenRefreshHours = 144
	w.tokenRefreshCycle(context.Background(), changed, time.Now())
	if calls.Load() != 1 {
		t.Fatal("修改提前时长也不能绕过未确认请求的等待期")
	}
	f.rotate("b", 10*24*time.Hour)
	view, _ := w.tokenRefreshPlan(cfg, time.Now())
	if view.Accounts[0].LastResult != "uncertain" || view.Accounts[0].LastRefresh == nil || view.Accounts[0].RefreshSource != "cpa" || view.NextAt == nil || !view.NextAt.After(time.Now()) {
		t.Fatal("未确认请求若已更新凭据，应采用新有效期")
	}
}
