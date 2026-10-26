package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConnectionCacheWakeAndRestart(t *testing.T) {
	w, cfg, fixture := newRefreshFixture(t)
	fixture.auths = fixture.auths[1:2]
	refreshed := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer management-canary" {
			t.Error("后台必须复用顶部连接密钥")
		}
		fixture.rotate("b", 10*24*time.Hour)
		rw.WriteHeader(http.StatusOK)
		refreshed <- struct{}{}
	}))
	defer server.Close()
	w.Configure(cfg)
	view := w.TokenRefreshStatus()
	if view.Error == "" || view.NextAt != nil || view.Accounts[0].State != "blocked" {
		t.Fatal("缺少缓存时必须暂停，不应显示待执行队列")
	}
	body, _ := json.Marshal(map[string]string{"url": server.URL})
	reply := w.handleManagement(managementRequest{Method: "POST", Path: managementBase + "/connection", Body: body, Headers: http.Header{"Authorization": {"Bearer management-canary"}}})
	if reply.StatusCode != 200 {
		t.Fatalf("连接缓存失败：%s", reply.Body)
	}
	info, err := os.Stat(connectionCachePath(cfg.StateFile))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("连接缓存必须仅当前用户可读写")
	}
	select {
	case <-refreshed:
	case <-time.After(2 * time.Second):
		t.Fatal("连接缓存后应立即唤醒队列，无需保存或重启插件")
	}
	w.Stop()
	restarted := newWarmer(w.host)
	restarted.Configure(cfg)
	defer restarted.Stop()
	view = restarted.TokenRefreshStatus()
	if view.Error != "" || view.NextAt == nil || !view.NextAt.After(time.Now()) || !restarted.Status().Connection.Cached {
		t.Fatal("插件重启后应读取缓存并根据新凭据排期")
	}
	raw, _ := json.Marshal(restarted.Status())
	stored, _ := os.ReadFile(cfg.StateFile)
	if strings.Contains(string(raw), "management-canary") || strings.Contains(string(stored), "management-canary") {
		t.Fatal("管理密钥不得进入状态或历史")
	}
	if err := os.Chmod(connectionCachePath(cfg.StateFile), 0644); err != nil {
		t.Fatal(err)
	}
	if restarted.TokenRefreshStatus().NextAt != nil || restarted.Status().Connection.Cached {
		t.Fatal("缓存权限不安全时应暂停并提示重新连接")
	}
}

func TestConnectionValidation(t *testing.T) {
	w, cfg, _ := newRefreshFixture(t)
	for _, req := range []managementRequest{
		{Body: []byte(`{"url":"http://127.0.0.1:8317"}`)},
		{Body: []byte(`{"url":"http://127.0.0.1:8317","key":"do-not-cache"}`), Headers: http.Header{"Authorization": {"Bearer fake-key"}}},
		{Body: []byte(`{"url":"https://user:secret@example.com"}`), Headers: http.Header{"Authorization": {"Bearer fake-key"}}},
		{Body: []byte(`{"url":"https://example.com/path"}`), Headers: http.Header{"Authorization": {"Bearer fake-key"}}},
		{Body: []byte(`{"url":"https://example.com?secret=secret"}`), Headers: http.Header{"Authorization": {"Bearer fake-key"}}},
		{Body: []byte(`{"url":"http://127.0.0.1:8317"} {}`), Headers: http.Header{"Authorization": {"Bearer fake-key"}}},
		{Body: []byte(`{"url":"http://127.0.0.1:8317"}`), Headers: http.Header{"Authorization": {"Bearer fake\nkey"}}},
	} {
		if err := w.cacheConnection(req); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("无效缓存请求必须拒绝且不泄露地址或认证")
		}
	}
	if _, err := os.Stat(connectionCachePath(cfg.StateFile)); !os.IsNotExist(err) {
		t.Fatal("无效请求不得写入缓存")
	}
	parsed, err := parseConfig([]byte("token_refresh_enabled: true\ntoken_refresh_management_key: do-not-use\ntoken_refresh_management_url: https://obsolete.example"))
	if err != nil || parsed.TokenRefreshKey != "" || parsed.TokenRefreshURL != "" {
		t.Fatal("后台连接只能来自独立缓存，不能复用旧配置字段")
	}
}

func TestCachingConnectionDoesNotInterruptWarmup(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	w, _ := testWarmer(t, func(method string, request, response any) error {
		if method == "host.auth.list" {
			return json.Unmarshal([]byte(`{"files":[{"id":"a","provider":"codex"}]}`), response)
		}
		if method == "host.model.execute" {
			close(started)
			<-release
			response.(*modelResponse).StatusCode = 200
		}
		return nil
	})
	job, _, err := w.startRun("manual", runRequest{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("模拟预热未开始")
	}
	cached := make(chan error, 1)
	go func() {
		cached <- w.cacheConnection(managementRequest{Body: []byte(`{"url":"http://127.0.0.1:8317"}`), Headers: http.Header{"X-Management-Key": {"fake-top-key"}}})
	}()
	select {
	case err := <-cached:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("重新连接不应等待或停止当前预热")
	}
	if state := w.Status(); !state.Running || state.History.LastProgress.StopRequested {
		t.Fatal("缓存连接不得中断当前预热")
	}
	releaseOnce()
	waitRun(t, job)
}
