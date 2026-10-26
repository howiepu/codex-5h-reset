package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"strings"
)

type managementConnection struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

type connectionState struct {
	Cached bool   `json:"cached"`
	Error  string `json:"error,omitempty"`
}

func connectionCachePath(stateFile string) string { return stateFile + ".connection.json" }

func (c *managementConnection) validate() error {
	c.URL, c.Key = strings.TrimRight(strings.TrimSpace(c.URL), "/"), strings.TrimSpace(c.Key)
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(c.URL, "#") || u.Path != "" {
		return errors.New("CPA 后台连接地址无效")
	}
	if c.Key == "" || strings.IndexFunc(c.Key, func(r rune) bool { return r < 32 || r == 127 }) >= 0 || len(c.Key) > 8192 {
		return errors.New("CPA 管理密钥格式无效，请重新连接")
	}
	return nil
}

func readConnection(stateFile string) (managementConnection, error) {
	var connection managementConnection
	path := connectionCachePath(stateFile)
	info, err := os.Lstat(path)
	if err != nil {
		return connection, errors.New("请在顶部连接设置中连接，后台连接将自动缓存")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 32<<10 {
		return connection, errors.New("后台连接缓存格式或权限无效，请在顶部重新连接")
	}
	f, err := os.Open(path)
	if err != nil {
		return connection, errors.New("后台连接缓存无法读取，请在顶部重新连接")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 32<<10+1))
	if err != nil || len(raw) > 32<<10 || json.Unmarshal(raw, &connection) != nil || connection.validate() != nil {
		return managementConnection{}, errors.New("后台连接缓存无法读取，请在顶部重新连接")
	}
	return connection, nil
}

func cachedConnectionState(stateFile string) connectionState {
	_, err := readConnection(stateFile)
	if err != nil {
		return connectionState{Error: err.Error()}
	}
	return connectionState{Cached: true}
}

func refreshConnectionConfig(cfg config) (config, connectionState) {
	connection, err := readConnection(cfg.StateFile)
	if err != nil {
		cfg.TokenRefreshURL, cfg.TokenRefreshKey = "", ""
		return cfg, connectionState{Error: err.Error()}
	}
	cfg.TokenRefreshURL, cfg.TokenRefreshKey = connection.URL, connection.Key
	return cfg, connectionState{Cached: true}
}

func (w *warmer) cacheConnection(req managementRequest) error {
	var body struct {
		URL string `json:"url"`
	}
	decoder := json.NewDecoder(bytes.NewReader(req.Body))
	decoder.DisallowUnknownFields()
	if len(req.Body) > 8192 || decoder.Decode(&body) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("后台连接请求格式无效")
	}
	key := strings.TrimSpace(req.Headers.Get("X-Management-Key"))
	parts := strings.SplitN(req.Headers.Get("Authorization"), " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		key = parts[1]
	}
	connection := managementConnection{URL: body.URL, Key: key}
	if err := connection.validate(); err != nil {
		return err
	}
	w.lifecycle.Lock()
	defer w.lifecycle.Unlock()
	w.mu.Lock()
	stateFile := w.cfg.StateFile
	w.mu.Unlock()
	if err := writePrivateJSON(connectionCachePath(stateFile), connection); err != nil {
		return errors.New("后台连接缓存写入失败，请检查持久目录的权限和空间")
	}
	w.mu.Lock()
	if journal := w.history.TokenRefresh; journal != nil && journal.Hold != "" {
		journal.Hold = ""
		w.persistLocked()
	}
	w.mu.Unlock()
	select {
	case w.refreshWake <- struct{}{}:
	default:
	}
	return nil
}
