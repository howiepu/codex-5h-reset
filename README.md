# Codex 5H Reset

用于 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的 **ChatGPT / Codex 模型额度窗口预热插件**，支持每日计划、手动执行、日志归档和异常通知。

**原始参考项目：[Codex Five-Hour Quota Warmer](https://github.com/Windyskr/codex-5h-quota-warmer)，作者 [Windyskr](https://github.com/Windyskr)。** 本项目基于原项目的预热方案扩展实现，以独立插件 ID `codex-5h-reset` 维护和发布。

> 仅处理 CPA 中 `provider=codex` 的账号，面向 ChatGPT / Codex 模型，不支持 Claude、Gemini 等其他供应商。预热通过正常模型请求尝试触发五小时额度窗口，**不会强制重置或增加额度**；窗口是否开启由上游决定，请求可能消耗额度。

## 保留与新增

沿用原项目的每日定时、手动全量预热、按启用账号逐个发送和 HTTP 2xx 成功判定。固定消息模式继续使用普通非流式 Chat 请求。

| 新增或优化 | 内容 |
| --- | --- |
| 消息模式 | 固定消息、随机算术题二选一；随机模式使用 Responses Lite 请求参数，保留固定消息草稿。 |
| 执行控制 | 勾选账号预热、异步进度、请求耗时、柔性停止；本进程最近 64 个请求 ID 去重。 |
| 日志增强 | 按 UTC 日期归档、配置保留天数、关键词与级别筛选、导出、长文本展开。 |
| 异常通知 | Webhook / Bark 失败汇总、定时任务无可用账号告警、测试通知。Webhook 支持自定义方法、请求头和模板。 |
| 模型提示 | 展示已启用账号的注册模型及当前模型注册情况，不额外发送模型探测请求。 |
| 管理界面 | 中文顶部菜单、默认查看日志、深浅主题和窄屏适配；刷新保留未保存的设置，鉴权失败停止轮询。 |

执行规则：启动不补发错过的计划；同一时间只运行一轮；每个目标账号只提交一次请求，插件不自动重试或替换模型，CPA 自身执行规则仍然生效。“停止本轮”会等待当前请求结束，再跳过剩余账号。HTTP 2xx 只表示请求成功，不验证算术答案或额度是否重置。

## 安装与使用

已在 **CPA v8.0.41** 验证；当前 [Release](https://github.com/howiepu/codex-5h-reset/releases) 提供 **Linux amd64** 安装包。

1. 在 CPA 插件商店添加自定义源：

   ```text
   https://raw.githubusercontent.com/howiepu/codex-5h-reset/main/registry.json
   ```

   也可将以下设置合并到现有 CPA 配置，保留原有插件目录、配置和其他商店源：

   ```yaml
   plugins:
     enabled: true
     store-sources:
       - https://raw.githubusercontent.com/howiepu/codex-5h-reset/main/registry.json
   ```

2. 从该源安装并启用 `Codex 5H Reset`。若已使用原 Warmer，先停用原插件，避免重复预热。
3. 打开 `/v0/resource/plugins/codex-5h-reset/status`，输入 CPA 管理密钥。
4. 在“预热设置”中选择可用的 ChatGPT / Codex 模型、消息模式、时区和每日时刻，点击“保存配置”。完整示例见 [config.example.yaml](config.example.yaml)。

从原 Warmer 迁移时可沿用模型、固定消息、时区和日程；不要复制原插件的 `store` 来源字段，旧插件历史不会自动导入。管理密钥默认只保存在当前标签页，勾选“记住密钥”后才存入本机浏览器。

## 历史与通知

### 历史持久化

将 `state_file` 放在 CPA 的持久化挂载目录内，例如 Docker 中：

```yaml
state_file: /CLIProxyAPI/plugins/data/codex-5h-reset/history.json
```

最近日志默认保留 200 条，完整事件按 UTC 日期保存在同目录 `logs/` 下，默认保留 30 天。升级时保留挂载和文件路径；修改路径不会自动搬迁历史。历史不保存管理密钥、账号凭据和模型返回正文。

### 异常通知

Webhook 和 Bark 地址留空即关闭。仅在整轮失败或定时任务无可用账号时汇总通知，正常轮次保持安静。

- **Webhook**：支持 GET / POST / PUT / PATCH / DELETE，以及 JSON / Form / Text。认证填写在 `Authorization` 等请求头中，旧 Basic Auth 字段会自动转换。
- **请求体模板**：支持 `{{notification}}`、`{{title}}`、`{{run_id}}`、`{{source}}`、`{{group}}`。JSON 变量放在字符串引号内；Form 使用值均为字符串的 JSON 对象；GET 不发送请求体。
- **测试通知**：使用当前表单，无需先保存；会实际向已填写渠道发送消息，不执行模型预热或保存配置。

例如 Apprise 可使用 JSON 请求体：

```json
{"body": "{{notification}}", "format": "text"}
```

通知请求每渠道超时 5 秒，不跟随重定向、不重试；失败不改变预热结果。通知地址、认证信息和请求模板不写入运行日志。

## 开发与发布

<details>
<summary>本地验证与构建</summary>

需要 Go 1.26、CGO / C 编译器、Python 3 和 Node.js。仅依赖 `gopkg.in/yaml.v3`，无前端框架或 npm 依赖。项目没有 TypeScript / lint 脚本，Go 静态检查使用 `go vet`。

以下为 Apple Silicon macOS 的本地验证命令；Linux 构建将 `.dylib` 换为 `.so`：

```sh
go test -race ./...
go vet ./...
python3 scripts/check_ui.py
node scripts/ui_auth_smoke.mjs
node scripts/ui_settings_smoke.mjs
mkdir -p dist
go build -buildmode=c-shared -o dist/codex-5h-reset.dylib .
python3 scripts/abi_smoke.py dist/codex-5h-reset.dylib 0.1.4
python3 scripts/ui_server_smoke.py dist/codex-5h-reset.dylib
```

启动模拟页面：

```sh
python3 scripts/ui_test_server.py dist/codex-5h-reset.dylib
```

使用模拟密钥 `test-management-key`；账号、模型请求和默认 Webhook 均为本地模拟数据。

</details>

推送 `vX.Y.Z` 标签后，[发布工作流](.github/workflows/release.yml) 自动检查、构建并发布 Linux amd64 ZIP 和 `checksums.txt`；动态库位于 ZIP 根目录。管理接口实现见 [plugin.go](plugin.go)，打包脚本见 [scripts/package.py](scripts/package.py)。
