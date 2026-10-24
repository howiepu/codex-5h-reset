package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	errBusy     = errors.New("预热正在运行，请稍后重试")
	errDisabled = errors.New("插件已停用")
)

type hostFunc func(method string, request, response any) error

type authEntry struct {
	ID          string `json:"id"`
	Provider    string `json:"provider"`
	Label       string `json:"label"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
	Status      string `json:"status"`
}

type modelRequest struct {
	EntryProtocol  string      `json:"entry_protocol"`
	ExitProtocol   string      `json:"exit_protocol"`
	Model          string      `json:"model"`
	Stream         bool        `json:"stream"`
	Body           []byte      `json:"body"`
	Headers        http.Header `json:"headers"`
	ForcedProvider string      `json:"forced_provider"`
	AuthID         string      `json:"auth_id"`
}

type modelResponse struct {
	StatusCode int `json:"status_code"`
}

type result struct {
	AuthID     string     `json:"auth_id"`
	Label      string     `json:"label,omitempty"`
	Model      string     `json:"model"`
	StatusCode int        `json:"status_code,omitempty"`
	Success    bool       `json:"success"`
	Error      string     `json:"error,omitempty"`
	Status     string     `json:"status,omitempty"`
	ErrorKind  string     `json:"error_kind,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS int64      `json:"duration_ms,omitempty"`
}

type logEntry struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Event   string         `json:"event"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

type status struct {
	Plugin        string       `json:"plugin"`
	Version       string       `json:"version"`
	Running       bool         `json:"running"`
	Configuration config       `json:"configuration"`
	Schedules     []plannedRun `json:"schedules"`
	History       history      `json:"history"`
	StateError    string       `json:"state_error,omitempty"`
	ArchiveError  string       `json:"archive_error,omitempty"`
}

type warmer struct {
	lifecycle    sync.Mutex
	mu           sync.Mutex
	host         hostFunc
	cfg          config
	initialized  bool
	accepting    bool
	running      bool
	stop         chan struct{}
	done         chan struct{}
	runs         sync.WaitGroup
	history      history
	stateError   string
	stateLocked  bool
	active       *runJob
	receipts     []*runJob
	archiveError string
	archiveDay   string
}

func newWarmer(host hostFunc) *warmer {
	return &warmer{host: host, cfg: defaultConfig(), history: history{SchemaVersion: 1}}
}

func (w *warmer) Configure(cfg config) {
	w.lifecycle.Lock()
	defer w.lifecycle.Unlock()
	w.mu.Lock()
	unchanged := w.initialized && reflect.DeepEqual(w.cfg, cfg) && w.accepting == cfg.Enabled && !w.stateLocked
	w.mu.Unlock()
	if unchanged {
		return
	}
	w.stopLocked()
	w.mu.Lock()
	if !w.initialized || w.cfg.StateFile != cfg.StateFile || w.stateLocked {
		loaded, err := loadHistory(cfg.StateFile)
		w.history, w.stateError, w.stateLocked = loaded, "", err != nil
		if err != nil {
			w.stateError = "历史文件读取失败，已保留原件；请检查 state_file"
		}
	}
	w.cfg, w.initialized, w.accepting = cfg, true, cfg.Enabled
	w.archiveDay = ""
	w.trimLogsLocked()
	if cfg.Enabled {
		w.stop, w.done = make(chan struct{}), make(chan struct{})
	}
	stop, done := w.stop, w.done
	w.mu.Unlock()
	w.log("info", "configuration.updated", "插件配置已加载", nil)
	if cfg.Enabled {
		go w.scheduleLoop(cfg, stop, done)
	}
}

func (w *warmer) Stop() {
	w.lifecycle.Lock()
	defer w.lifecycle.Unlock()
	w.stopLocked()
}

func (w *warmer) stopLocked() {
	w.mu.Lock()
	w.accepting = false
	if w.active != nil {
		w.active.progress.StopRequested = true
	}
	stop, done := w.stop, w.done
	w.stop, w.done = nil, nil
	w.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if done != nil {
		<-done
	}
	w.runs.Wait()
}

func (w *warmer) scheduleLoop(cfg config, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		runs := nextRuns(cfg, time.Now())
		if len(runs) == 0 {
			return
		}
		next := runs[0]
		w.log("info", "schedule.planned", "已安排下一次预热", map[string]any{"schedule_id": next.ID, "scheduled_at": next.Time.Format(time.RFC3339)})
		timer := time.NewTimer(time.Until(next.Time))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			w.log("info", "schedule.triggered", "定时预热已触发", map[string]any{"schedule_id": next.ID})
			if _, err := w.Run("schedule:" + next.ID); err != nil {
				w.log("warn", "schedule.skipped", "本次定时预热未执行", map[string]any{"reason": err.Error()})
			}
		}
	}
}

func selectAuths(entries []authEntry) []authEntry {
	selected := make([]authEntry, 0)
	for _, entry := range entries {
		if strings.EqualFold(strings.TrimSpace(entry.Provider), "codex") && !entry.Disabled && strings.TrimSpace(entry.ID) != "" {
			selected = append(selected, entry)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return strings.ToLower(selected[i].ID) < strings.ToLower(selected[j].ID) })
	return selected
}

func randomArithmeticPrompt() string {
	const operators = "+-*/"
	operator := operators[rand.IntN(len(operators))]
	left, right := rand.IntN(9)+1, rand.IntN(9)+1
	if operator == '/' {
		left = right * (rand.IntN(9/right) + 1)
	}
	if rand.IntN(2) == 0 {
		return fmt.Sprintf("%d%c%d", left, operator, right)
	}
	return fmt.Sprintf("%d %c %d", left, operator, right)
}

func (w *warmer) warmOne(cfg config, auth authEntry) (r result) {
	started := time.Now().UTC()
	r = result{AuthID: auth.ID, Label: auth.Label, Model: cfg.Model, StartedAt: &started, Status: "failed"}
	defer func() {
		finished := time.Now().UTC()
		r.FinishedAt, r.DurationMS = &finished, finished.Sub(started).Milliseconds()
	}()
	protocol := "openai"
	headers := http.Header{"Content-Type": {"application/json"}}
	payload := map[string]any{
		"model": cfg.Model, "messages": []map[string]string{{"role": "user", "content": cfg.Prompt}}, "stream": false,
	}
	if cfg.RandomArithmetic {
		protocol = "openai-response"
		payload = map[string]any{
			"model": cfg.Model, "input": randomArithmeticPrompt(), "stream": false, "store": false, "instructions": "",
			"reasoning": map[string]string{"effort": "low", "context": "all_turns"},
			"text":      map[string]string{"verbosity": "low"}, "tools": []any{}, "tool_choice": "none",
		}
		headers.Set("Accept", "application/json")
		headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
		headers.Set("X-CLIProxyAPI-Account-Warmup", "true")
	}
	body, _ := json.Marshal(payload)
	request := modelRequest{
		EntryProtocol: protocol, ExitProtocol: protocol, Model: cfg.Model, Stream: false,
		Body: body, Headers: headers, ForcedProvider: "codex", AuthID: auth.ID,
	}
	var response modelResponse
	if err := w.host("host.model.execute", request, &response); err != nil {
		var callbackErr *hostError
		if errors.As(err, &callbackErr) {
			r.StatusCode = callbackErr.HTTPStatus
		}
		r.ErrorKind, r.Error = failureForStatus(r.StatusCode)
		return r
	}
	r.StatusCode = response.StatusCode
	// 与 Warmer 保持一致：2xx 只表示请求成功，不验证生成内容或额度窗口。
	r.Success = response.StatusCode >= 200 && response.StatusCode < 300
	if r.Success {
		r.Status = "success"
	} else {
		r.ErrorKind, r.Error = failureForStatus(r.StatusCode)
	}
	return r
}

func (w *warmer) Status() status {
	w.mu.Lock()
	defer w.mu.Unlock()
	cfg := w.cfg
	cfg.NotifyWebhookURL, cfg.NotifyBarkURL = "", ""
	cfg.Schedules = append([]schedule{}, cfg.Schedules...)
	h := w.history
	h.LastResults = append([]result{}, h.LastResults...)
	h.Logs = append([]logEntry{}, h.Logs...)
	if h.LastProgress != nil {
		p := *h.LastProgress
		h.LastProgress = &p
	}
	return status{Plugin: pluginID, Version: version, Running: w.running, Configuration: cfg, Schedules: nextRuns(cfg, time.Now()), History: h, StateError: w.stateError, ArchiveError: w.archiveError}
}

func (w *warmer) log(level, event, message string, fields map[string]any) {
	w.mu.Lock()
	previousError := w.stateError + w.archiveError
	entry := logEntry{Time: time.Now().UTC(), Level: level, Event: event, Message: message, Fields: fields}
	w.history.Logs = append(w.history.Logs, entry)
	w.trimLogsLocked()
	w.persistLocked()
	day := entry.Time.Format("2006-01-02")
	if err := appendArchive(w.cfg, entry, w.archiveDay != day); err != nil {
		w.archiveError = "日志归档失败，请检查历史目录的权限、空间或旧归档格式"
	} else {
		w.archiveError, w.archiveDay = "", day
	}
	storageError := w.stateError + w.archiveError
	w.mu.Unlock()
	hostFields := map[string]any{"plugin_id": pluginID, "event": event}
	for key, value := range fields {
		hostFields[key] = value
	}
	_ = w.host("host.log", map[string]any{"level": level, "message": message, "fields": hostFields}, &struct{}{})
	if storageError != "" && storageError != previousError {
		_ = w.host("host.log", map[string]any{"level": "error", "message": storageError, "fields": map[string]any{"plugin_id": pluginID, "event": "history.storage_failed"}}, &struct{}{})
	}
}

func (w *warmer) trimLogsLocked() {
	if n := len(w.history.Logs) - w.cfg.MaxLogEntries; n > 0 {
		w.history.Logs = append([]logEntry{}, w.history.Logs[n:]...)
	}
}

func (w *warmer) persistLocked() {
	if w.stateLocked {
		return
	}
	if err := saveHistory(w.cfg.StateFile, w.history); err != nil {
		w.stateError = "历史文件写入失败，请检查 state_file 的权限和空间"
	} else {
		w.stateError = ""
	}
}
