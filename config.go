package main

import (
	"fmt"
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

var version = "0.1.0"

type config struct {
	Enabled              bool       `yaml:"enabled" json:"enabled"`
	Model                string     `yaml:"model" json:"model"`
	Prompt               string     `yaml:"prompt" json:"prompt"`
	Timezone             string     `yaml:"timezone" json:"timezone"`
	Schedules            []schedule `yaml:"schedules" json:"schedules"`
	MaxLogEntries        int        `yaml:"max_log_entries" json:"max_log_entries"`
	StateFile            string     `yaml:"state_file" json:"state_file"`
	HistoryRetentionDays int        `yaml:"history_retention_days" json:"history_retention_days"`
	NotifyWebhookURL     string     `yaml:"notify_webhook_url" json:"notify_webhook_url"`
	NotifyBarkURL        string     `yaml:"notify_bark_url" json:"notify_bark_url"`
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
		StateFile:            filepath.Join("plugins", "data", pluginID, "history.json"),
	}
}

func parseConfig(raw []byte) (config, error) {
	cfg := defaultConfig()
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("无法解析插件配置：%w", err)
	}
	cfg.Model, cfg.Prompt = strings.TrimSpace(cfg.Model), strings.TrimSpace(cfg.Prompt)
	if cfg.Model == "" || cfg.Prompt == "" {
		return cfg, fmt.Errorf("model 和 prompt 不能为空")
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
	for _, value := range []*string{&cfg.NotifyWebhookURL, &cfg.NotifyBarkURL} {
		*value = strings.TrimSpace(*value)
		if *value == "" {
			continue
		}
		u, err := url.Parse(*value)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return cfg, fmt.Errorf("通知地址必须是有效的 HTTP(S) URL，不能包含用户名密码或片段")
		}
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
