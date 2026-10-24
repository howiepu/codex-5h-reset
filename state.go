package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type history struct {
	SchemaVersion int          `json:"schema_version"`
	LastRun       *time.Time   `json:"last_run,omitempty"`
	LastSource    string       `json:"last_source,omitempty"`
	LastResults   []result     `json:"last_results"`
	LastProgress  *runProgress `json:"last_progress,omitempty"`
	Logs          []logEntry   `json:"logs"`
}

func loadHistory(path string) (history, error) {
	h := history{SchemaVersion: 1, LastResults: []result{}, Logs: []logEntry{}}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 8<<20+1))
	if err != nil || len(raw) > 8<<20 {
		return h, fmt.Errorf("历史文件无法读取或超过 8 MiB")
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return history{SchemaVersion: 1}, err
	}
	if h.SchemaVersion != 1 {
		return history{SchemaVersion: 1}, fmt.Errorf("不支持的历史格式")
	}
	if p := h.LastProgress; p != nil && p.FinishedAt == nil {
		now := time.Now().UTC()
		p.FinishedAt, p.Error = &now, "上轮在插件退出前未完成，结果可能未确认；插件不会补发，请检查 CPA 日志"
		for i := range h.LastResults {
			r := &h.LastResults[i]
			switch r.Status {
			case "queued":
				r.Status = "stopped"
				p.Skipped++
			case "running":
				r.Status, r.ErrorKind, r.Error = "failed", "uncertain", "退出前未收到 CPA 结果，可能已消耗额度"
				p.Completed++
				p.Failed++
			}
		}
	}
	return h, nil
}

func saveHistory(path string, h history) error {
	raw, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".history-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(raw)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
