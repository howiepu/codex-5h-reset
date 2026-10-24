# Codex 5H Reset

由本仓库独立构建、发布的 CPA 原生插件，插件 ID 为 `codex-5h-reset`。

第一版与 [Codex Five-Hour Quota Warmer](https://github.com/Windyskr/codex-5h-quota-warmer) 保持相同的消息发送逻辑：读取 CPA 中全部启用的 Codex 凭证，按 ID 排序，逐账号发送一次普通 Chat 消息；指定 `AuthID` 和 `ForcedProvider=codex`，入口和出口协议均为 `openai`，`stream=false`。

```json
{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"Reply with exactly: quota window activated"}],"stream":false}
```

仅以 HTTP 2xx 判定成功，不读取生成内容，也不额外查询额度。插件不会强制重置额度；它发送的正常模型请求用于触发账号的额度窗口，实际窗口由上游决定。插件自身不重试、不切换模型；CPA 自身的重试与执行规则仍然生效。`unavailable` 或冷却中的账号仍会交给 CPA 判断，与 Warmer 一致。

支持按时区设置多个每日时刻、手动预热、中文设置页面、逐账号结果和持久化日志。定时运行全部启用账号；手动可运行全部或勾选账号。启动时不发送请求，也不补发已错过的时刻；一轮正在运行时，其他触发返回冲突。

在“预热设置 → 消息内容”中选择“随机算术题”（`random_arithmetic: true`），手动和定时预热都会在每次账号请求前重新出题：两个数字为 1～9，随机使用 `+ - * /`，随机带空格，除法保证整除，例如 `3+5`、`8 / 2`。题目可能重复。启用后忽略但保留 `prompt`，固定内容可以留空；关闭后恢复发送固定内容，且不能为空。默认使用“固定消息”。随机模式显示示例题目并隐藏固定内容编辑区，原固定内容仍会保留，切换回固定模式即可查看和编辑。

随机算术模式使用 `openai-response` 入口和出口、`stream=false`、`store=false`，设置 `reasoning.effort=low`、Lite 所需的 `reasoning.context=all_turns`、`text.verbosity=low`、空工具列表及 `tool_choice=none`，并携带 `X-OpenAI-Internal-Codex-Responses-Lite: true` 和 `X-CLIProxyAPI-Account-Warmup: true`，避免 CPA 自动附加图片工具。Codex 不支持该链路的硬输出 Token 上限；CPA 的模型后缀和全局请求规则仍会生效。固定消息继续使用原 Chat 请求；两种模式都固定原账号和模型，仅按 HTTP 2xx 判断成功，不核对算术答案或额度窗口。

管理功能沿用单文件页面与 Go 标准库，没有增加前端框架或模型 SDK：

- 运行时显示每个账号的等待、执行、成功、失败、结果未确认或未发送状态，以及整轮进度和请求耗时。页面运行中每秒读取状态，空闲时每 15 秒读取；隐藏页面暂停轮询，刷新不覆盖未保存的设置。
- “停止本轮”等待 CPA 完成当前请求，然后跳过剩余账号，不取消 CPA 的在途执行。不会修改后续定时计划。
- 日志按事件、账号和说明分列，支持级别、关键词、轮次 ID 筛选，以及按 UTC 日期读取归档和导出筛选结果。最多同时渲染 500 条，导出包含全部筛选结果。
- 已在本插件页面保存的管理密钥可用于打开页面自动连接；连接先单次验证鉴权，401 时清除保存的无效密钥并停止轮询，403 显示 CPA 的拒绝原因；主题跟随同源 CPA 页面，独立打开时使用 CPA 保存的主题或系统主题。
- HTTP 错误分为认证、限流、请求拒绝、服务异常等。超时或缺少 CPA 结果时提示“结果未确认，可能已消耗额度”，仍记为未成功，不触发模型重试。
- 模型输入框提供已启用账号的注册模型建议，并显示当前模型在各账号的注册情况。连接或手动刷新时读取 CPA 的 `host.auth.models` 注册表，不发送模型请求或查询额度。未注册的模型仍可能是别名；提示不会阻止保存、过滤账号或替换模型，实际执行交给 CPA。旧宿主不支持该接口时显示列表未知，预热照常运行。

## 本地验证

需要 Go 1.26、CGO、C 编译器和 Python 3；唯一的 Go 依赖是 `gopkg.in/yaml.v3`。代码实现 CPA 的原生 C ABI 1，通过 JSON 调用宿主，不依赖整套 CPA SDK。协议对照基于 CPA v8.0.41 的官方接口与示例；真实 CPA 加载和上游请求仍需在安装后验证。

```sh
go test -race ./...
go vet ./...
python3 scripts/check_ui.py
node scripts/ui_auth_smoke.mjs
node scripts/ui_settings_smoke.mjs
mkdir -p dist
go build -buildmode=c-shared -o dist/codex-5h-reset.dylib .
python3 scripts/abi_smoke.py dist/codex-5h-reset.dylib 0.1.3
python3 scripts/package.py dist/codex-5h-reset.dylib 0.1.3 darwin arm64
```

以上构建命令适用于 Apple Silicon macOS。Linux amd64 上把输出后缀改为 `.so`，打包参数改为 `linux amd64`。ABI 自检使用模拟宿主与临时历史文件，不访问网络或真实账号。页面脚本检查需要 Node.js，不需要安装 npm 依赖；本项目没有 `package.json` 或 TypeScript/lint 脚本，Go 静态检查使用 `go vet`。

## 发布与接入 CPA

仓库包含 `registry.json` 和 GitHub Actions。推送代码后，推送 `v0.1.3` 这样的版本 tag，工作流会执行检查、构建 Linux amd64 共享库、运行 ABI 自检，然后发布 CPA 要求的文件：

- `codex-5h-reset_0.1.3_linux_amd64.zip`，共享库位于 ZIP 根目录。
- `checksums.txt`，包含 ZIP 的 SHA-256。

版本由 tag 写入共享库元信息。首版自动发布目标为 Linux amd64；本地 macOS 构建用于开发验证，不能安装到 Linux CPA。

在 CPA 的插件商店设置中添加自定义源，或在完整 CPA 配置的 `plugins` 节点合并：

```yaml
plugins:
  store-sources:
    - https://raw.githubusercontent.com/howiepu/codex-5h-reset/main/registry.json
```

确保 `plugins.enabled` 已开启，保留已有的 `dir`、`configs` 及其他商店源。`store-sources` 是 URL 字符串数组；CPA 自动生成源名称和 ID，并始终保留官方源。源 URL 需要在本仓库公开且代码已经推送后才能访问；插件需要先有对应平台的 GitHub Release 才能安装。插件有独立 ID，安装后 CPA 会从本仓库查找更新。

切换时先停用原 `codex-5h-quota-warmer`，安装并启用 `codex-5h-reset`，然后在新插件配置中使用 [config.example.yaml](config.example.yaml)。示例保留 `Asia/Singapore` 下的 `05:00`、`10:05`、`15:10` 三个时刻及原有提示词。不要把整个 CPA 配置文件替换成这个示例，也不要复制旧插件的 `store` 来源字段。

插件菜单页面为 `/v0/resource/plugins/codex-5h-reset/status`。页面使用顶部菜单切换运行日志、运行状态、账号结果和预热设置，默认打开日志。参考 shadcn/ui 的中性色、边框与表单风格，保留单文件页面。长文本可以展开阅读全文，JSON 详情在单元格内换行；窄屏下表格改为卡片排列。

输入 CPA 管理密钥后可读取配置、保存设置、查看日志、手动预热。管理接口由 CPA 鉴权，公开资源只包含静态页面。保存配置时保留 CPA 管理的 `enabled`、`priority`、`store` 等字段；默认只在当前标签页保存密钥，勾选“记住密钥”才写入浏览器持久存储。

管理页本地验证可在构建共享库后运行：

```sh
python3 scripts/ui_test_server.py dist/codex-5h-reset.dylib
# 需要观察执行进度与失败展示时：
python3 scripts/ui_test_server.py dist/codex-5h-reset.dylib --model-delay 3 --fail-second
```

打开终端给出的本地地址，使用固定模拟密钥 `test-management-key`。测试宿主提供超长日志、模拟账号和 `/narrow` 的 375px 视口；所有数据均为测试数据，不会发送真实模型请求。默认通知地址指向本服务的 `/mock/webhook`，可直接验证自定义请求和测试按钮，不会向真实通知渠道投递。`scripts/ui_assertions.mjs` 导出的检查函数可在浏览器 DOM 检查中对页面的 `html` 元素执行，验证菜单与可见页面一致、页面和单元格无水平溢出。

`python3 scripts/ui_server_smoke.py dist/codex-5h-reset.dylib` 验证浏览器保留空闲连接时，同步运行、异步进度、柔性停止、请求去重和日期归档仍能响应。该检查和页面脚本检查已加入发布工作流。

## 历史文件

默认路径为 `plugins/data/codex-5h-reset/history.json`，相对于 CPA 工作目录。Bero 容器示例明确设置为：

```yaml
state_file: /CLIProxyAPI/plugins/data/codex-5h-reset/history.json
```

该路径应落在宿主机持久化挂载的 `plugins` 目录内。新建历史目录权限为 `0700`，文件为 `0600`，使用临时文件、同步和原子重命名写入。默认保留 200 条日志，最多 2000 条；保存账号 ID、显示名称和执行结果，不保存凭证、管理密钥、模型返回正文或上游原始错误。

旧版 `schema_version: 1` 的历史文件继续可读。新版从启用时起把每条新事件追加到同目录的 `logs/YYYY-MM-DD.jsonl`（UTC），独立于最近日志条数限制。`history_retention_days` 默认 30，可配置 1–365，按 UTC 日保留含当天的 N 天；写入新事件时清理更早的日期文件。新建归档目录和文件的权限分别为 `0700`、`0600`。旧历史快照不会自动回填为归档，已被旧版淘汰的记录无法恢复。

CPA 或容器更新后能否保留这些文件，仍取决于历史目录是否在持久化挂载内。改变 `state_file` 后，会使用新文件所在目录的 `logs`，不会迁移原归档。归档读取上限为每天 16 MiB，超过后可直接从磁盘读取；文件损坏会报告错误并保留原件。

如果历史文件损坏，插件保留原文件并在管理页显示错误，在修复文件并重新加载插件前不覆盖它。执行请求仍可继续。更改历史路径会读取新路径的记录，不会搬迁或删除旧文件；需要保留历史时先停用插件，再手动复制历史文件。

每个账号交给 CPA 执行前保存“执行中”状态；重启发现上轮未结束时，将在途账号标记为结果未确认，将等待账号标记为未发送，不补发请求。

## 可选异常通知

`notify_webhook_url` 与 `notify_bark_url` 默认留空，均为关闭。配置后，仅在整轮存在失败或账号列表读取失败等异常时，每个渠道发送一条摘要；定时任务没有可执行 Codex 账号时同样记为整轮异常并通知，手动检查空账号仅记录警告。正常轮次保持安静。Bark 地址使用含设备 key 的推送 URL，Webhook 默认使用 JSON POST，也可自定义方法、格式、请求头和请求体。摘要包含 `title`、`body`、`group`、`run_id` 和 `source`，不附账号 ID、凭证或原始错误。

每个渠道超时 5 秒，不跟随重定向，不自动重试。通知失败单独记录，不改变模型请求结果。通知地址可能含密钥，只保存在 CPA 插件配置中，不出现在状态、历史或日志中；请保护 CPA 配置文件，跨公网使用 HTTPS。当前配置页需 CPA 管理鉴权，读取的是宿主的配置接口。

“自定义 Webhook”提供 `notify_webhook_method`（GET/POST/PUT/PATCH/DELETE）、`notify_webhook_format`（json/form/text）、`notify_webhook_headers`（字符串键值对）和 `notify_webhook_body`（请求体模板）。认证直接填入 `Authorization` 等请求头，不再提供单独的账号密码输入。GET 仅发送地址和请求头，保留但不发送请求体草稿；其他方法中，JSON 与 Form 模板填写合法 JSON，Form 要求对象的值均为字符串，发送时自动编码为表单；Text 直接填写文本。Content-Type 自动设置，可通过自定义请求头覆盖。

例如 Apprise 的请求体可填写：

```json
{"body": "{{notification}}", "format": "text"}
```

支持 `{{notification}}`（通知正文）、`{{body}}`（同正文）、`{{title}}`、`{{group}}`、`{{run_id}}`、`{{source}}`。JSON 中变量应位于字符串引号内，替换后自动转义换行、引号等内容；模板中的未知占位符保留原文。请求体留空时使用默认摘要，Text 格式则使用通知正文。自定义请求头和模板只作用于 Webhook，Bark 始终使用原 JSON POST；请求头、模板和通知地址不会出现在状态、历史或日志中。

旧配置中的 `notify_webhook_username` / `notify_webhook_password` 会自动转换为 Authorization 请求头，保留密码原值；已有自定义 Authorization 时优先使用它。在新页面保存后移除旧字段。请求头名称不区分大小写，不能重复或包含换行；Host、Content-Length、Transfer-Encoding 由客户端管理。通知 URL 仍不能嵌入账号密码。

填写后点击“测试通知”，按表单当前内容向每个已填写渠道发送一条测试消息，无需先保存。结果显示在按钮下方；失败时只显示安全的提示或 HTTP 状态码，不返回地址、认证信息或接收端正文。测试不会保存配置、执行模型预热或改变运行记录，仍使用每渠道 5 秒超时、不跟随重定向、不重试的规则。

## 管理接口

下列接口使用 CPA 的管理鉴权：

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/v0/management/plugins/codex-5h-reset/status` | 状态、计划和最近结果 |
| POST | `/v0/management/plugins/codex-5h-reset/run` | 手动执行一轮，支持所选账号、请求 ID 和异步响应 |
| POST | `/v0/management/plugins/codex-5h-reset/stop` | 停止指定轮次的剩余队列 |
| POST | `/v0/management/plugins/codex-5h-reset/test-notification` | 使用请求中的通知配置发送测试消息，返回逐渠道结果，不保存配置 |
| GET | `/v0/management/plugins/codex-5h-reset/accounts` | Codex 账号列表和可执行状态 |
| GET | `/v0/management/plugins/codex-5h-reset/logs` | 最近日志；`?day=YYYY-MM-DD` 读取 UTC 日期归档 |
| GET / PUT | `/v0/management/plugins/codex-5h-reset/config` | CPA 提供的配置接口 |

`POST /run` 的空请求或 `{}` 保持原先的同步行为，执行全部启用账号。页面使用如下请求立即返回，再只读轮询 `/status`：

```json
{"auth_ids":["account-id"],"request_id":"manual-request-0001","async":true}
```

省略 `auth_ids` 或传 `null` 表示全部账号，显式空数组会拒绝。混入停用、已删除或非 Codex 账号时整轮拒绝，不部分发送。`request_id` 可选，由 8–128 个字母、数字、点、下划线或连字符组成；相同 ID 和相同账号选择复用同一轮结果，不再次调用模型。去重仅保留本进程最近 64 次带 ID 的请求，重启会清空，不能据此自动补发。页面连接结果未确认时不会自动重试；用户再次执行同一选择会复用尚未确认的请求 ID。

`POST /stop` 请求体为 `{"run_id":"状态中 last_progress.id 的值"}`，旧轮次的停止操作不会影响新轮次。`status.history.last_progress` 提供整轮计数与时间，`last_results` 提供逐账号状态。配置重载和卸载同样跳过尚未发送的账号、等待在途请求完成。模型请求的超时由 CPA 控制，因此正在执行时停用或重配也可能需要等待。
