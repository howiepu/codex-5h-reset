package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDailyArchiveBeyondRecentLimit(t *testing.T) {
	w, cfg := testWarmer(t, func(string, any, any) error { return nil })
	cfg.MaxLogEntries = 2
	cfg.HistoryRetentionDays = 1
	dir := archiveDir(cfg)
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02") + ".jsonl"
	for _, name := range []string{"2000-01-01.jsonl", yesterday, "keep.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep-original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "2000-01-02.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	w.Configure(cfg)
	_, _ = w.Run("manual")
	day := time.Now().UTC().Format("2006-01-02")
	entries, err := w.Logs(day)
	if err != nil || len(entries) <= 2 || len(w.Status().History.Logs) != 2 {
		t.Fatalf("归档应独立于最近日志条数：%d %v", len(entries), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2000-01-01.jsonl")); !os.IsNotExist(err) {
		t.Fatal("过期日期归档应按保留期清理")
	}
	if _, err := os.Stat(filepath.Join(dir, yesterday)); !os.IsNotExist(err) {
		t.Fatal("保留 1 天应只保留今天")
	}
	for _, name := range []string{"keep.txt", "2000-01-02.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("不得清理其他文件或目录：%s", name)
		}
	}
	for path, mode := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, day+".jsonl"): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("归档权限错误：%s %v", path, err)
		}
	}
	for _, day := range []string{"../history", "2026-02-31", "2026-1-2"} {
		if _, err := w.Logs(day); err == nil {
			t.Fatalf("不应接受无效日期：%s", day)
		}
	}
	broken := filepath.Join(dir, "2001-01-01.jsonl")
	if err := os.WriteFile(broken, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Logs("2001-01-01"); err == nil {
		t.Fatal("应报告损坏归档")
	}
	if raw, _ := os.ReadFile(broken); string(raw) != "{broken" {
		t.Fatal("不得覆盖损坏的原文件")
	}
}

func TestArchiveFailureDoesNotBlockCPA(t *testing.T) {
	w, cfg := testWarmer(t, func(method string, _, response any) error {
		if method == "host.auth.list" {
			return json.Unmarshal([]byte(`{"files":[{"id":"a","provider":"codex"}]}`), response)
		}
		if method == "host.model.execute" {
			response.(*modelResponse).StatusCode = 200
		}
		return nil
	})
	if err := os.Rename(archiveDir(cfg), archiveDir(cfg)+".backup"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archiveDir(cfg), []byte("blocking-file"), 0600); err != nil {
		t.Fatal(err)
	}
	results, err := w.Run("manual")
	if err != nil || len(results) != 1 || !results[0].Success || w.Status().ArchiveError == "" {
		t.Fatal("归档失败应可见，并保留 CPA 执行能力")
	}
	if err := os.Remove(archiveDir(cfg)); err != nil {
		t.Fatal(err)
	}
	_, _ = w.Run("manual")
	if w.Status().ArchiveError != "" {
		t.Fatal("归档恢复后应清除错误")
	}
}

func TestInterruptedHistoryDoesNotReplay(t *testing.T) {
	cfg := testConfig(t)
	h := history{SchemaVersion: 1, LastProgress: &runProgress{ID: "old-run", StartedAt: time.Now().UTC(), Total: 3, Completed: 1, Succeeded: 1}, LastResults: []result{{AuthID: "a", Status: "success", Success: true}, {AuthID: "b", Status: "running"}, {AuthID: "c", Status: "queued"}}}
	if err := saveHistory(cfg.StateFile, h); err != nil {
		t.Fatal(err)
	}
	w := newWarmer(func(method string, _, _ any) error {
		if method == "host.model.execute" {
			t.Error("重载不应补发")
		}
		return nil
	})
	w.Configure(cfg)
	defer w.Stop()
	s := w.Status()
	p := s.History.LastProgress
	if s.Running || p.Completed != 2 || p.Failed != 1 || p.Skipped != 1 || !strings.Contains(p.Error, "未确认") || s.History.LastResults[1].ErrorKind != "uncertain" || s.History.LastResults[2].Status != "stopped" {
		t.Fatalf("上次中断的状态应明确显示：%+v", s.History)
	}
}
