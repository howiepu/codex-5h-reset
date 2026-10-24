package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestManagementRoutesAndPublicResource(t *testing.T) {
	w, cfg := testWarmer(t, func(string, any, any) error { return nil })
	cfg.Prompt = "private-prompt-canary"
	w.Configure(cfg)
	response := w.handleManagement(managementRequest{Method: "GET", Path: resourcePath})
	if response.StatusCode != 200 || !strings.Contains(response.Headers.Get("Content-Type"), "text/html") || strings.Contains(string(response.Body), cfg.Prompt) {
		t.Fatal("公开资源应仅返回静态页面")
	}
	for _, req := range []managementRequest{
		{Method: "POST", Path: resourcePath},
		{Method: "POST", Path: "/v0/resource/plugins/" + pluginID + "/run"},
		{Method: "GET", Path: managementBase + "/run"},
		{Method: "GET", Path: "/unregistered/status"},
	} {
		if got := w.handleManagement(req); got.StatusCode != 404 {
			t.Fatalf("接口不应被其他方法或资源路径触发：%+v", req)
		}
	}
	if got := w.handleManagement(managementRequest{Method: "GET", Path: managementBase + "/status"}); got.StatusCode != http.StatusOK {
		t.Fatal(got)
	}
	w.Stop()
	if got := w.handleManagement(managementRequest{Method: "POST", Path: managementBase + "/run"}); got.StatusCode != http.StatusConflict {
		t.Fatal("停用后手动运行应被拒绝")
	}
}

func TestManagementInputAndSelection(t *testing.T) {
	w, _ := testWarmer(t, func(method string, _, response any) error {
		if method == "host.auth.list" {
			return json.Unmarshal([]byte(`{"files":[{"id":"a","provider":"codex"},{"id":"b","provider":"codex","disabled":true},{"id":"other","provider":"claude"}]}`), response)
		}
		if method == "host.model.execute" {
			response.(*modelResponse).StatusCode = 204
		}
		return nil
	})
	for _, body := range []string{`null`, `[]`, `{`, `{"auth_id":"a"}`, `{"auth_ids":[]}`, `{"auth_ids":"a"}`, `{"request_id":"short"}`, `{} {}`} {
		if response := w.handleManagement(managementRequest{Method: "POST", Path: managementBase + "/run", Body: []byte(body)}); response.StatusCode != 400 {
			t.Fatalf("应拒绝错误请求 %s：%d", body, response.StatusCode)
		}
	}
	response := w.handleManagement(managementRequest{Method: "GET", Path: managementBase + "/accounts"})
	if response.StatusCode != 200 || strings.Contains(string(response.Body), "claude") || !strings.Contains(string(response.Body), `"disabled":true`) {
		t.Fatal("账号列表应只包含 Codex，停用账号仍应显示原因")
	}
	response = w.handleManagement(managementRequest{Method: "POST", Path: managementBase + "/run", Body: []byte(`{"auth_ids":["a"],"request_id":"api-manual-once"}`)})
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), `"status_code":204`) {
		t.Fatal("Body 应按 CPA 的字节字段解码并执行所选账号")
	}
	response = w.handleManagement(managementRequest{Method: "POST", Path: managementBase + "/run", Body: []byte(`{"auth_ids":["b"]}`)})
	if response.StatusCode != 400 {
		t.Fatal("停用账号不应被执行")
	}
	for _, body := range []string{`{}`, `{"run_id":"previous-run"}`} {
		response = w.handleManagement(managementRequest{Method: "POST", Path: managementBase + "/stop", Body: []byte(body)})
		if response.StatusCode != 400 && response.StatusCode != 409 {
			t.Fatal("停止请求必须验证轮次")
		}
	}
	response = w.handleManagement(managementRequest{Method: "GET", Path: managementBase + "/logs", Query: url.Values{"day": {"../../history"}}})
	if response.StatusCode != 400 {
		t.Fatal("不能用归档日期读取任意路径")
	}
}

func TestLifecycleAndPublishedConfiguration(t *testing.T) {
	w := newWarmer(func(string, any, any) error { return nil })
	defer w.Stop()
	cfg := testConfig(t)
	raw, _ := json.Marshal(map[string][]byte{"config_yaml": []byte("enabled: true\nstate_file: " + cfg.StateFile + "\n")})
	for _, method := range []string{"plugin.register", "plugin.quiesce", "plugin.reconfigure", "management.register", "plugin.shutdown"} {
		if _, err := dispatch(w, method, raw); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if method == "plugin.reconfigure" {
			if _, err := w.Run("manual"); err != nil {
				t.Fatalf("quiesce 后应能用相同配置重新启用：%v", err)
			}
		}
	}
	if _, err := dispatch(w, "plugin.register", []byte("bad json")); err == nil {
		t.Fatal("应拒绝错误格式")
	}
	if _, err := dispatch(w, "unknown", nil); err == nil {
		t.Fatal("应拒绝未知方法")
	}
	example, err := os.ReadFile("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseConfig(example); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile("registry.json")
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		SchemaVersion int `json:"schema_version"`
		Plugins       []struct{ ID, Repository string }
	}
	if err := json.Unmarshal(raw, &registry); err != nil || registry.SchemaVersion != 1 || len(registry.Plugins) != 1 || registry.Plugins[0].ID != pluginID || registry.Plugins[0].Repository != repository {
		t.Fatalf("插件源信息不一致：%s %v", raw, err)
	}
}
