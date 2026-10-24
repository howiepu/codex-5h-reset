package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func archiveDir(cfg config) string { return filepath.Join(filepath.Dir(cfg.StateFile), "logs") }

func appendArchive(cfg config, entry logEntry, prune bool) error {
	dir := archiveDir(cfg)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, entry.Time.UTC().Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(entry)
	if syncErr := f.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil || !prune {
		return err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	cutoff := entry.Time.UTC().AddDate(0, 0, 1-cfg.HistoryRetentionDays).Format("2006-01-02")
	for _, file := range files {
		day := strings.TrimSuffix(file.Name(), ".jsonl")
		if file.IsDir() || file.Name() != day+".jsonl" {
			continue
		}
		if _, err := time.Parse("2006-01-02", day); err != nil {
			continue
		}
		if day < cutoff {
			if err := os.Remove(filepath.Join(dir, file.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *warmer) Logs(day string) ([]logEntry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if day == "" {
		return append([]logEntry{}, w.history.Logs...), nil
	}
	date, err := time.Parse("2006-01-02", day)
	if err != nil || date.Format("2006-01-02") != day {
		return nil, errors.New("归档日期必须是 YYYY-MM-DD（UTC）")
	}
	f, err := os.Open(filepath.Join(archiveDir(w.cfg), day+".jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return []logEntry{}, nil
	}
	if err != nil {
		return nil, errors.New("无法读取日志归档")
	}
	defer f.Close()
	// 限制响应大小，保留完整归档供管理员从磁盘读取。
	reader := io.LimitReader(f, (16<<20)+1)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	logs := []logEntry{}
	var total int
	for scanner.Scan() {
		total += len(scanner.Bytes()) + 1
		if total > 16<<20 {
			return nil, errors.New("当天归档超过 16 MiB，请从历史目录读取文件")
		}
		var entry logEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nil, fmt.Errorf("日志归档格式错误，原文件已保留")
		}
		logs = append(logs, entry)
	}
	if scanner.Err() != nil {
		return nil, errors.New("读取日志归档失败或单条日志过大，原文件已保留")
	}
	return logs, nil
}
