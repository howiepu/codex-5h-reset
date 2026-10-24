package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

var errInvalidRun = errors.New("请求 ID 或账号选择无效，请刷新账号列表")
var requestIDPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{8,128}$`)

type runRequest struct {
	AuthIDs   []string `json:"auth_ids"`
	RequestID string   `json:"request_id"`
	Async     bool     `json:"async"`
}

type runProgress struct {
	ID            string     `json:"id"`
	Source        string     `json:"source"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	Total         int        `json:"total"`
	Completed     int        `json:"completed"`
	Succeeded     int        `json:"succeeded"`
	Failed        int        `json:"failed"`
	Skipped       int        `json:"skipped"`
	StopRequested bool       `json:"stop_requested"`
	Error         string     `json:"error,omitempty"`
}

type runJob struct {
	request  runRequest
	progress runProgress
	results  []result
	err      error
	done     chan struct{}
}

func (w *warmer) Run(source string) ([]result, error) {
	job, _, err := w.startRun(source, runRequest{})
	if err != nil {
		return nil, err
	}
	<-job.done
	return job.results, job.err
}

func (w *warmer) startRun(source string, req runRequest) (*runJob, bool, error) {
	if req.RequestID != "" && !requestIDPattern.MatchString(req.RequestID) {
		return nil, false, errInvalidRun
	}
	if req.AuthIDs != nil {
		if len(req.AuthIDs) == 0 || len(req.AuthIDs) > 2000 {
			return nil, false, errInvalidRun
		}
		ids := map[string]bool{}
		for _, id := range req.AuthIDs {
			if strings.TrimSpace(id) == "" || ids[id] {
				return nil, false, errInvalidRun
			}
			ids[id] = true
		}
		req.AuthIDs = append([]string{}, req.AuthIDs...)
		sort.Strings(req.AuthIDs)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// shortcut: 仅在本进程内保留最近 64 次手动触发，跨重启去重需持久化请求回执。
	if req.RequestID != "" {
		for _, previous := range w.receipts {
			if previous.request.RequestID == req.RequestID {
				if !reflect.DeepEqual(previous.request.AuthIDs, req.AuthIDs) {
					return nil, false, errInvalidRun
				}
				return previous, true, nil
			}
		}
	}
	if !w.accepting {
		return nil, false, errDisabled
	}
	if w.running {
		return nil, false, errBusy
	}
	job := &runJob{request: req, progress: runProgress{ID: rand.Text(), Source: source, StartedAt: time.Now().UTC()}, results: []result{}, done: make(chan struct{})}
	if req.RequestID != "" {
		w.receipts = append(w.receipts, job)
		if len(w.receipts) > 64 {
			w.receipts = w.receipts[1:]
		}
	}
	w.active, w.running = job, true
	w.runs.Add(1)
	w.history.LastSource, w.history.LastResults, w.history.LastProgress = source, []result{}, &job.progress
	go w.executeRun(w.cfg, job)
	return job, false, nil
}

func (w *warmer) StopRun(runID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active == nil || w.active.progress.ID != runID {
		return false
	}
	w.active.progress.StopRequested = true
	return true
}

func (w *warmer) executeRun(cfg config, job *runJob) {
	defer func() {
		w.mu.Lock()
		now := time.Now().UTC()
		job.progress.FinishedAt = &now
		if job.err != nil {
			job.progress.Error = job.err.Error()
		}
		w.mu.Unlock()
		w.notifyFailure(cfg, job)
		w.mu.Lock()
		w.history.LastRun, w.running, w.active = &now, false, nil
		w.persistLocked()
		w.mu.Unlock()
		w.runs.Done()
		close(job.done)
	}()
	runLog := func(level, event, message string, fields map[string]any) {
		if fields == nil {
			fields = map[string]any{}
		}
		fields["run_id"], fields["source"] = job.progress.ID, job.progress.Source
		w.log(level, event, message, fields)
	}
	runLog("info", "warmup.started", "开始逐账号预热", map[string]any{"model": cfg.Model})
	var listed struct {
		Files []authEntry `json:"files"`
	}
	if err := w.host("host.auth.list", struct{}{}, &listed); err != nil {
		job.err = errors.New("读取 CPA 凭证列表失败")
		runLog("error", "warmup.finished", job.err.Error(), nil)
		return
	}
	codex, disabled, missing, unavailable, inactive := 0, 0, 0, 0, 0
	for _, a := range listed.Files {
		if !strings.EqualFold(strings.TrimSpace(a.Provider), "codex") {
			continue
		}
		codex++
		switch {
		case a.Disabled:
			disabled++
		case strings.TrimSpace(a.ID) == "":
			missing++
		default:
			if a.Unavailable {
				unavailable++
			}
			if !strings.EqualFold(strings.TrimSpace(a.Status), "active") {
				inactive++
			}
		}
	}
	auths := selectAuths(listed.Files)
	if job.request.AuthIDs != nil {
		ids := map[string]bool{}
		for _, id := range job.request.AuthIDs {
			ids[id] = true
		}
		selected := []authEntry{}
		for _, auth := range auths {
			if ids[auth.ID] {
				selected = append(selected, auth)
			}
		}
		if len(selected) != len(ids) {
			job.err = errInvalidRun
			runLog("error", "warmup.finished", job.err.Error(), nil)
			return
		}
		auths = selected
	}
	runLog("info", "warmup.selection", fmt.Sprintf("本轮选择 %d 个账号，跳过禁用 %d 个、缺少 ID %d 个", len(auths), disabled, missing), map[string]any{
		"auth_total": len(listed.Files), "codex_total": codex, "selected_count": len(auths), "skipped_disabled": disabled, "skipped_missing_id": missing, "reported_unavailable": unavailable, "reported_inactive": inactive,
	})
	if len(auths) == 0 {
		message := "没有可执行的 Codex 账号，请检查 CPA 凭证"
		runLog("warn", "warmup.no_credentials", message, nil)
		if strings.HasPrefix(job.progress.Source, "schedule:") {
			job.err = errors.New(message)
			runLog("error", "warmup.finished", message, nil)
			return
		}
	}
	w.mu.Lock()
	job.progress.Total = len(auths)
	for _, auth := range auths {
		job.results = append(job.results, result{AuthID: auth.ID, Label: auth.Label, Model: cfg.Model, Status: "queued"})
	}
	w.history.LastResults = job.results
	w.mu.Unlock()
	for i, auth := range auths {
		w.mu.Lock()
		if job.progress.StopRequested {
			for j := i; j < len(job.results); j++ {
				job.results[j].Status = "stopped"
			}
			job.progress.Skipped = len(auths) - i
			w.mu.Unlock()
			break
		}
		job.results[i].Status = "running"
		started := time.Now().UTC()
		job.results[i].StartedAt = &started
		w.mu.Unlock()
		runLog("info", "warmup.credential_started", "开始执行账号请求", map[string]any{"auth_id": auth.ID, "label": auth.Label})
		r := w.warmOne(cfg, auth)
		w.mu.Lock()
		job.results[i] = r
		job.progress.Completed++
		if r.Success {
			job.progress.Succeeded++
		} else {
			job.progress.Failed++
		}
		w.mu.Unlock()
		level, message := "warn", r.Error
		if r.Success {
			level, message = "info", "请求成功"
		}
		runLog(level, "warmup.credential", fmt.Sprintf("%s（HTTP %d，耗时 %d ms）", message, r.StatusCode, r.DurationMS), map[string]any{"auth_id": r.AuthID, "label": r.Label, "status_code": r.StatusCode, "success": r.Success, "duration_ms": r.DurationMS, "error_kind": r.ErrorKind})
	}
	w.mu.Lock()
	p := job.progress
	w.mu.Unlock()
	runLog("info", "warmup.finished", fmt.Sprintf("本轮结束：%d 成功、%d 失败、%d 未发送", p.Succeeded, p.Failed, p.Skipped), map[string]any{"credential_count": p.Total, "success_count": p.Succeeded, "failure_count": p.Failed, "skipped_count": p.Skipped})
}

func failureForStatus(code int) (string, string) {
	switch code {
	case 401, 403:
		return "authorization", "账号认证失败，请检查 CPA 凭证"
	case 429:
		return "rate_limit", "账号限流或额度不足，由 CPA 判断冷却状态"
	case 400, 404, 422:
		return "request", "模型或请求被拒绝，请检查模型配置"
	case 408, 504:
		return "uncertain", "请求超时，结果未确认，可能已消耗额度；插件不会自动重发"
	case 0:
		return "uncertain", "CPA 未返回可确认的结果，可能已消耗额度；请检查 CPA 日志"
	default:
		if code >= 500 {
			return "upstream", "CPA 或上游服务异常，请检查 CPA 日志"
		}
		return "http", fmt.Sprintf("请求返回 HTTP %d", code)
	}
}

func (w *warmer) Accounts() ([]authEntry, error) {
	var listed struct {
		Files []authEntry `json:"files"`
	}
	if err := w.host("host.auth.list", struct{}{}, &listed); err != nil {
		return nil, errors.New("读取 CPA 凭证列表失败")
	}
	accounts := []authEntry{}
	for _, a := range listed.Files {
		if strings.EqualFold(strings.TrimSpace(a.Provider), "codex") {
			accounts = append(accounts, a)
		}
	}
	sort.Slice(accounts, func(i, j int) bool { return strings.ToLower(accounts[i].ID) < strings.ToLower(accounts[j].ID) })
	return accounts, nil
}
