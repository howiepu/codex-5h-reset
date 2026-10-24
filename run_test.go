package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitRun(t *testing.T, job *runJob) {
	t.Helper()
	select {
	case <-job.done:
	case <-time.After(3 * time.Second):
		t.Fatal("预热未及时结束")
	}
}

func TestAccountsRegisteredModelsAreAdvisory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy bool
	}{{"registered", false}, {"legacy_host", true}} {
		t.Run(tc.name, func(t *testing.T) {
			modelCalls := 0
			w := newWarmer(func(method string, _, response any) error {
				switch method {
				case "host.auth.list":
					return json.Unmarshal([]byte(`{"files":[{"id":"b","provider":"codex","disabled":true},{"id":"a","provider":"codex"},{"id":"c","provider":"codex","unavailable":true},{"id":"other","provider":"gemini"}]}`), response)
				case "host.auth.models":
					modelCalls++
					if tc.legacy {
						return errors.New("unsupported-private-host-error")
					}
					return json.Unmarshal([]byte(`{"credentials":[{"id":"a","provider":"codex","models":["model-z","model-a"]},{"id":"b","provider":"codex","models":[]},{"id":"c","provider":"gemini","models":["foreign-model"]},{"id":"missing","provider":"codex","models":["unlisted-model"]}]}`), response)
				default:
					t.Fatalf("读取注册信息不应执行模型或查询额度：%s", method)
				}
				return nil
			})
			accounts, err := w.Accounts()
			if err != nil || len(accounts) != 3 || modelCalls != 1 || accounts[0].ID != "a" || accounts[1].ID != "b" || accounts[2].ID != "c" {
				t.Fatalf("模型提示不能改变账号列表：%+v, %v", accounts, err)
			}
			if !tc.legacy && (strings.Join(accounts[0].Models, ",") != "model-z,model-a" || accounts[1].Models == nil || len(accounts[1].Models) != 0) {
				t.Fatalf("应按账号 ID 匹配模型，并区分空列表和未读取：%+v", accounts)
			}
			if accounts[2].Models != nil {
				t.Fatal("不能匹配其他提供方的模型")
			}
			if tc.legacy && (accounts[0].Models != nil || accounts[1].Models != nil) {
				t.Fatal("旧宿主不支持模型列表时应保留未知状态")
			}
		})
	}
}

func TestSelectedRunProgressStopAndDedup(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	var sent atomic.Int32
	w, cfg := testWarmer(t, func(method string, request, response any) error {
		switch method {
		case "host.auth.list":
			return json.Unmarshal([]byte(`{"files":[{"id":"b","provider":"codex","unavailable":true},{"id":"a","provider":"codex"},{"id":"c","provider":"codex"}]}`), response)
		case "host.model.execute":
			if request.(modelRequest).AuthID != "a" {
				t.Error("停止后不应再执行其他账号")
			}
			sent.Add(1)
			close(started)
			<-release
			response.(*modelResponse).StatusCode = 204
		}
		return nil
	})
	req := runRequest{RequestID: "one-manual-request", AuthIDs: []string{"b", "a"}, Async: true}
	job, duplicate, err := w.startRun("manual", req)
	if err != nil || duplicate {
		t.Fatalf("启动失败：%v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("请求未启动")
	}
	s := w.Status()
	if !s.Running || s.History.LastProgress.Total != 2 || s.History.LastProgress.Completed != 0 || len(s.History.LastResults) != 2 || s.History.LastResults[0].Status != "running" || s.History.LastResults[1].Status != "queued" {
		t.Fatalf("进度错误：%+v", s.History)
	}
	var stored history
	raw, err := os.ReadFile(cfg.StateFile)
	if err != nil || json.Unmarshal(raw, &stored) != nil || len(stored.LastResults) != 2 || stored.LastResults[0].Status != "running" {
		t.Fatal("CPA 执行前应先持久化在途账号状态")
	}
	repeated, duplicate, err := w.startRun("manual", runRequest{RequestID: req.RequestID, AuthIDs: []string{"a", "b"}})
	if err != nil || !duplicate || repeated != job {
		t.Fatal("重复请求应找到正在执行的同一轮")
	}
	if _, _, err := w.startRun("manual", runRequest{RequestID: req.RequestID, AuthIDs: []string{"c"}}); !errors.Is(err, errInvalidRun) {
		t.Fatal("同一请求 ID 不能改成另一个账号")
	}
	if w.StopRun("stale-round") || !w.StopRun(job.progress.ID) {
		t.Fatal("停止必须只匹配当前轮次")
	}
	if sent.Load() != 1 {
		t.Fatal("不得并发发送")
	}
	for i := 0; i < 20; i++ {
		if _, err := json.Marshal(w.Status()); err != nil {
			t.Fatal(err)
		}
	}
	releaseOnce()
	waitRun(t, job)
	s = w.Status()
	p := s.History.LastProgress
	if s.Running || p.Completed != 1 || p.Succeeded != 1 || p.Failed != 0 || p.Skipped != 1 || p.FinishedAt == nil || s.History.LastResults[1].Status != "stopped" || sent.Load() != 1 {
		t.Fatalf("停止后的结果错误：%+v", s.History)
	}
	_, duplicate, err = w.startRun("manual", req)
	if err != nil || !duplicate || sent.Load() != 1 {
		t.Fatal("已完成请求不能再次发送")
	}
}

func TestInvalidSelectionNeverSends(t *testing.T) {
	var sent atomic.Int32
	w, _ := testWarmer(t, func(method string, _, response any) error {
		if method == "host.auth.list" {
			return json.Unmarshal([]byte(`{"files":[{"id":"a","provider":"codex"},{"id":"disabled","provider":"codex","disabled":true},{"id":"other","provider":"gemini"}]}`), response)
		}
		if method == "host.model.execute" {
			sent.Add(1)
		}
		return nil
	})
	for _, req := range []runRequest{{AuthIDs: []string{}}, {AuthIDs: []string{"a", "a"}}, {AuthIDs: []string{" "}}, {RequestID: "short"}} {
		if _, _, err := w.startRun("manual", req); !errors.Is(err, errInvalidRun) {
			t.Fatalf("应拒绝：%+v", req)
		}
	}
	for _, id := range []string{"missing", "disabled", "other"} {
		job, _, err := w.startRun("manual", runRequest{AuthIDs: []string{"a", id}})
		if err != nil {
			t.Fatal(err)
		}
		waitRun(t, job)
		if !errors.Is(job.err, errInvalidRun) {
			t.Fatalf("应拒绝混入无效账号：%s", id)
		}
	}
	if sent.Load() != 0 {
		t.Fatal("账号选择无效时，整轮都不应发送")
	}
}

func TestSafeErrorClassificationAndDuration(t *testing.T) {
	for _, tc := range []struct {
		code int
		kind string
	}{{401, "authorization"}, {403, "authorization"}, {429, "rate_limit"}, {400, "request"}, {404, "request"}, {422, "request"}, {408, "uncertain"}, {504, "uncertain"}, {503, "upstream"}, {0, "uncertain"}, {302, "http"}} {
		w := newWarmer(func(method string, _, response any) error {
			if method != "host.model.execute" {
				t.Fatalf("不应在此查询额度：%s", method)
			}
			time.Sleep(2 * time.Millisecond)
			if tc.code == 0 {
				return errors.New("private-token-canary")
			}
			response.(*modelResponse).StatusCode = tc.code
			return nil
		})
		r := w.warmOne(defaultConfig(), authEntry{ID: "a"})
		if r.Success || r.ErrorKind != tc.kind || r.StartedAt == nil || r.FinishedAt == nil || r.DurationMS < 1 || strings.Contains(r.Error, "private-token-canary") {
			t.Fatalf("HTTP %d 分类或耗时错误：%+v", tc.code, r)
		}
	}
}

func TestSelectionDetailsAndEmptyWarning(t *testing.T) {
	w, _ := testWarmer(t, func(method string, _, response any) error {
		if method == "host.auth.list" {
			return json.Unmarshal([]byte(`{"files":[{"id":"","provider":"codex"},{"id":"d","provider":"codex","disabled":true},{"id":"other","provider":"gemini"}]}`), response)
		}
		if method == "host.model.execute" {
			t.Fatal("没有可用账号时不应请求")
		}
		return nil
	})
	if _, err := w.Run("manual"); err != nil {
		t.Fatal(err)
	}
	var selection, warn bool
	for _, entry := range w.Status().History.Logs {
		if entry.Event == "warmup.selection" {
			selection = entry.Fields["codex_total"] == 2 && entry.Fields["skipped_disabled"] == 1 && entry.Fields["skipped_missing_id"] == 1 && entry.Fields["selected_count"] == 0
		}
		if entry.Event == "warmup.no_credentials" && entry.Level == "warn" {
			warn = true
		}
	}
	if !selection || !warn {
		t.Fatal("应明确记录跳过原因，并在零账号时发出警告")
	}
}
