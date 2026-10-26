package main

import (
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"

	"gopkg.in/yaml.v3"
)

const (
	pluginID   = "codex-5h-reset"
	pluginName = "Codex 5H Reset"
	repository = "https://github.com/howiepu/codex-5h-reset"
)

var version = "0.1.5"

type config struct {
	Enabled               bool              `yaml:"enabled" json:"enabled"`
	Model                 string            `yaml:"model" json:"model"`
	Prompt                string            `yaml:"prompt" json:"prompt"`
	RandomArithmetic      bool              `yaml:"random_arithmetic" json:"random_arithmetic"`
	Timezone              string            `yaml:"timezone" json:"timezone"`
	Schedules             []schedule        `yaml:"schedules" json:"schedules"`
	MaxLogEntries         int               `yaml:"max_log_entries" json:"max_log_entries"`
	StateFile             string            `yaml:"state_file" json:"state_file"`
	HistoryRetentionDays  int               `yaml:"history_retention_days" json:"history_retention_days"`
	TokenRefreshEnabled   bool              `yaml:"token_refresh_enabled" json:"token_refresh_enabled"`
	TokenRefreshHours     float64           `yaml:"token_refresh_before_hours" json:"token_refresh_before_hours"`
	TokenRefreshURL       string            `yaml:"-" json:"-"`
	TokenRefreshKey       string            `yaml:"-" json:"-"`
	NotifyWebhookURL      string            `yaml:"notify_webhook_url" json:"notify_webhook_url"`
	NotifyWebhookMethod   string            `yaml:"notify_webhook_method" json:"notify_webhook_method"`
	NotifyWebhookFormat   string            `yaml:"notify_webhook_format" json:"notify_webhook_format"`
	NotifyWebhookHeaders  map[string]string `yaml:"notify_webhook_headers" json:"notify_webhook_headers"`
	NotifyWebhookBody     string            `yaml:"notify_webhook_body" json:"notify_webhook_body"`
	NotifyWebhookUsername string            `yaml:"notify_webhook_username,omitempty" json:"notify_webhook_username,omitempty"`
	NotifyWebhookPassword string            `yaml:"notify_webhook_password,omitempty" json:"notify_webhook_password,omitempty"`
	NotifyBarkURL         string            `yaml:"notify_bark_url" json:"notify_bark_url"`
}

type schedule struct {
	ID      string `yaml:"id" json:"id"`
	At      string `yaml:"at" json:"at"`
	Enabled *bool  `yaml:"enabled" json:"enabled,omitempty"`
}

type plannedRun struct {
	ID   string    `json:"id"`
	At   string    `json:"at"`
	Time time.Time `json:"time"`
}

func defaultConfig() config {
	return config{
		Model: "gpt-5.6-luna", Prompt: "Reply with exactly: quota window activated",
		Timezone: "Asia/Singapore", MaxLogEntries: 200,
		HistoryRetentionDays: 30,
		TokenRefreshHours:    120,
		NotifyWebhookMethod:  "POST", NotifyWebhookFormat: "json",
		StateFile: filepath.Join("plugins", "data", pluginID, "history.json"),
	}
}

func parseConfig(raw []byte) (config, error) {
	cfg := defaultConfig()
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("无法解析插件配置：%w", err)
	}
	cfg.Model, cfg.Prompt = strings.TrimSpace(cfg.Model), strings.TrimSpace(cfg.Prompt)
	if cfg.Model == "" {
		return cfg, fmt.Errorf("model 不能为空")
	}
	if !cfg.RandomArithmetic && cfg.Prompt == "" {
		return cfg, fmt.Errorf("固定发送内容的 prompt 不能为空")
	}
	if _, err := time.LoadLocation(cfg.Timezone); err != nil {
		return cfg, fmt.Errorf("timezone 必须是有效的 IANA 时区")
	}
	if cfg.MaxLogEntries < 1 || cfg.MaxLogEntries > 2000 {
		return cfg, fmt.Errorf("max_log_entries 必须在 1 到 2000 之间")
	}
	if cfg.HistoryRetentionDays < 1 || cfg.HistoryRetentionDays > 365 {
		return cfg, fmt.Errorf("history_retention_days 必须在 1 到 365 之间")
	}
	if err := validateTokenRefreshConfig(&cfg); err != nil {
		return cfg, err
	}
	if err := validateNotificationConfig(&cfg); err != nil {
		return cfg, err
	}
	if strings.TrimSpace(cfg.StateFile) == "" {
		return cfg, fmt.Errorf("state_file 不能为空")
	}
	var err error
	cfg.StateFile, err = filepath.Abs(cfg.StateFile)
	if err != nil {
		return cfg, fmt.Errorf("无法定位 state_file")
	}
	ids, times := map[string]bool{}, map[string]bool{}
	for i := range cfg.Schedules {
		s := &cfg.Schedules[i]
		s.ID, s.At = strings.TrimSpace(s.ID), strings.TrimSpace(s.At)
		at, err := time.Parse("15:04", s.At)
		if s.ID == "" || ids[s.ID] || err != nil || at.Format("15:04") != s.At {
			return cfg, fmt.Errorf("日程需要唯一的 id 和 HH:MM 格式的 at")
		}
		ids[s.ID] = true
		if s.Enabled == nil || *s.Enabled {
			if times[s.At] {
				return cfg, fmt.Errorf("启用的日程时间不能重复：%s", s.At)
			}
			times[s.At] = true
		}
	}
	return cfg, nil
}

func validateTokenRefreshConfig(cfg *config) error {
	if math.IsNaN(cfg.TokenRefreshHours) || math.IsInf(cfg.TokenRefreshHours, 0) || cfg.TokenRefreshHours < 1.0/60 || cfg.TokenRefreshHours > 365*24 {
		return fmt.Errorf("令牌刷新提前时长必须在 1 分钟到 365 天之间")
	}
	return nil
}

func validateNotificationConfig(cfg *config) error {
	for _, value := range []*string{&cfg.NotifyWebhookURL, &cfg.NotifyBarkURL} {
		*value = strings.TrimSpace(*value)
		if *value == "" {
			continue
		}
		u, err := url.Parse(*value)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("通知地址必须是有效的 HTTP(S) URL，不能包含用户名密码或片段")
		}
	}
	return prepareWebhookConfig(cfg)
}

func nextRuns(cfg config, now time.Time) []plannedRun {
	loc, _ := time.LoadLocation(cfg.Timezone)
	now = now.In(loc)
	runs := make([]plannedRun, 0, len(cfg.Schedules))
	for _, s := range cfg.Schedules {
		if s.Enabled != nil && !*s.Enabled {
			continue
		}
		at, _ := time.Parse("15:04", s.At)
		next := time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), 0, 0, loc)
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		runs = append(runs, plannedRun{ID: s.ID, At: s.At, Time: next})
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Time.Before(runs[j].Time) })
	return runs
}
