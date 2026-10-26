import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const html = fs.readFileSync(new URL("../web/index.html", import.meta.url), "utf8");
const models = html.slice(html.indexOf("function renderModelHint()"), html.indexOf("function renderAccounts()"));
const functions = html.slice(html.indexOf("function updatePromptMode()"), html.indexOf("function markDirty()"));
const submit = html.slice(html.indexOf('$("settings").addEventListener("submit"'), html.indexOf("async function startManual("));
const nodes = new Map(), saved = [], notificationRequests = [], refreshTables = [];
let pending, waitNotification = Promise.resolve();
const notificationResults = [{channel: "Webhook", success: true}, {channel: "Bark", success: false, error: "接收端返回 HTTP 401"}];
const context = vm.createContext({
  configuration: null, settingsDirty: false, tokenSettingsDirty: false, tokenStatus: null, tokenRequest: undefined, currentStatus: null, accounts: [], base: "/plugin", URL, TextEncoder, btoa,
  document: {createElement: tag => ({tagName: tag, value: ""})},
  $: id => {
    if (!nodes.has(id)) nodes.set(id, {
      value: "", checked: false, type: id.endsWith("URL") ? "password" : "text", children: [], listeners: {}, attributes: {},
      setAttribute(name, value) { this.attributes[name] = value; },
      addEventListener(type, callback) { this.listeners[type] = callback; },
      replaceChildren(...children) { this.children = children; },
    });
    return nodes.get(id);
  },
  addSchedule() {}, notice() {}, refreshStatus: async () => {},
  table: (...args) => refreshTables.push(args),
  formatTime: value => value || "—",
  action: (_button, _message, fn) => { pending = fn(); },
  request: async (path, options) => {
    if (path === "/plugin/token-refresh") return {enabled: true, lead_hours: 144, scanned_at: "2026-11-01T00:00:00Z", next_at: "2030-11-01T00:00:00Z", next_account: "模拟账号", accounts: [{id: "a", label: "模拟账号", state: "scheduled", expires_at: "2030-11-07T00:00:00Z", next_at: "2030-11-01T00:00:00Z", last_result: "updated"}]};
    if (path === "/plugin/test-notification") {
      assert.equal(options.method, "POST");
      notificationRequests.push(JSON.parse(options.body));
      await waitNotification;
      return {results: notificationResults};
    }
    assert.equal(path, "/plugin/config"); assert.equal(options.method, "PUT");
    saved.push(JSON.parse(options.body));
  },
});
vm.runInContext(models + functions + submit, context);
const original = {
  enabled: true, priority: 7, store: {repository: "test-source"},
  model: "test-model", prompt: "keep this fixed message", timezone: "Asia/Singapore",
  max_log_entries: 200, state_file: "test-history.json", history_retention_days: 30,
  schedules: [], notify_webhook_url: "", notify_bark_url: "",
  notify_webhook_method: "POST", notify_webhook_format: "json",
  notify_webhook_headers: {Authorization: "Bearer fake-saved-header"},
  notify_webhook_body: '{"body":"{{notification}}","format":"text"}',
};
context.fillConfig(original, {});
const mode = nodes.get("randomArithmetic"), prompt = nodes.get("prompt");
for (const random of [true, false]) {
  mode.checked = random; mode.listeners.change();
  assert.equal(nodes.get("fixedPrompt").checked, !random);
  assert.equal(nodes.get("fixedPromptFields").hidden, random, "随机模式隐藏未参与发送的固定内容");
  assert.equal(nodes.get("randomPromptPreview").hidden, !random, "随机模式显示当前发送的题目说明");
  assert.equal(prompt.required, !random); assert.equal(prompt.disabled, random);
  assert.equal(prompt.value, original.prompt, "切换模式保留固定消息草稿");
  nodes.get("settings").listeners.submit({preventDefault() {}}); await pending;
  assert.deepEqual(saved.at(-1), {...original, random_arithmetic: random}, "保存必须保留 CPA 管理字段和通知模板");
}
const legacy = {...original, notify_webhook_headers: {}, notify_webhook_username: " 旧账号 ", notify_webhook_password: " fake-password with spaces "};
context.fillConfig(legacy, {});
const expectedAuth = "Basic " + Buffer.from("旧账号: fake-password with spaces ").toString("base64");
assert.equal(JSON.parse(nodes.get("webhookHeaders").value).Authorization, expectedAuth, "旧认证转换为 UTF-8 Basic 请求头");
nodes.get("settings").listeners.submit({preventDefault() {}}); await pending;
assert.equal(saved.at(-1).notify_webhook_headers.Authorization, expectedAuth);
assert.equal("notify_webhook_username" in saved.at(-1), false); assert.equal("notify_webhook_password" in saved.at(-1), false);
context.fillConfig({...original, random_arithmetic: true, prompt: ""}, {});
assert.equal(mode.checked, true); assert.equal(nodes.get("fixedPromptFields").hidden, true);
assert.doesNotThrow(() => context.validateConfig({...original, random_arithmetic: true, prompt: ""}));
assert.throws(() => context.validateConfig({...original, random_arithmetic: false, prompt: ""}), /固定发送内容不能为空/);
assert.throws(() => context.validateConfig({...original, model: ""}), /模型和历史路径不能为空/);
for (const headers of [[], {Authorization: 123}, {"bad name": "x"}, {Authorization: "one", authorization: "two"}, {Authorization: "line\nbreak"}]) {
  assert.throws(() => context.validateConfig({...original, notify_webhook_headers: headers}), /请求头/);
}
assert.throws(() => context.validateConfig({...original, notify_webhook_body: '{"body":{{notification}}}'}), /请求体必须是有效 JSON/);
assert.throws(() => context.validateConfig({...original, notify_webhook_format: "form", notify_webhook_body: '{"body":123}'}), /表单请求体/);
nodes.get("webhookMethod").value = "GET"; nodes.get("webhookMethod").listeners.change();
assert.equal(nodes.get("webhookBody").disabled, true); assert.equal(nodes.get("webhookFormat").disabled, true);
assert.match(nodes.get("webhookBodyHint").textContent, /GET 不发送请求体/);
nodes.get("webhookMethod").value = "POST"; nodes.get("webhookMethod").listeners.change();
assert.equal(nodes.get("webhookBody").disabled, false);
assert.equal(nodes.get("webhookBody").value, original.notify_webhook_body, "切换请求方法不能删除模板草稿");
nodes.get("webhookURLToggle").listeners.click();
assert.equal(nodes.get("webhookURL").type, "text"); assert.equal(nodes.get("webhookURLToggle").attributes["aria-pressed"], "true");
nodes.get("webhookURLToggle").listeners.click(); assert.equal(nodes.get("webhookURL").type, "password");
context.accounts = [{id: "a", label: "账号 A", models: ["test-model", "shared"]}, {id: "b", label: "账号 B", models: []}, {id: "c", models: null}, {id: "disabled", disabled: true, models: ["disabled-model"]}];
context.renderModelOptions();
assert.deepEqual(nodes.get("modelOptions").children.map(option => option.value), ["shared", "test-model"]);
assert.match(nodes.get("modelHint").textContent, /1 \/ 3.*1 个账号暂无模型列表.*未注册：账号 B/);
nodes.get("model").value = "possible-alias"; nodes.get("model").listeners.input();
assert.match(nodes.get("modelHint").textContent, /0 \/ 3.*未注册：账号 A、账号 B.*支持模型别名/);
assert.equal(nodes.get("model").value, "possible-alias");
context.accounts = [{id: "legacy"}]; context.renderModelOptions();
assert.match(nodes.get("modelHint").textContent, /CPA 暂未提供模型列表/);
context.accounts = []; context.renderModelOptions(); assert.match(nodes.get("modelHint").textContent, /暂无可执行的 Codex 账号/);
nodes.get("webhookURL").value = " http://127.0.0.1/draft-webhook "; nodes.get("barkURL").value = " http://127.0.0.1/draft-bark ";
nodes.get("webhookHeaders").value = '{"Authorization":"Bearer fake-draft-header"}'; nodes.get("model").value = "";
context.settingsDirty = true;
const beforeTestConfig = JSON.stringify(context.configuration), savedBeforeTest = saved.length;
let release; waitNotification = new Promise(resolve => { release = resolve; });
const testing = nodes.get("testNotification").listeners.click();
assert.equal(nodes.get("testNotification").disabled, true); release(); await testing;
assert.equal(nodes.get("testNotification").disabled, false); assert.equal(nodes.get("notificationTestResult").hidden, false);
assert.deepEqual(notificationRequests, [{notify_webhook_url: "http://127.0.0.1/draft-webhook", notify_webhook_method: "POST", notify_webhook_format: "json", notify_webhook_headers: {Authorization: "Bearer fake-draft-header"}, notify_webhook_body: original.notify_webhook_body, notify_bark_url: "http://127.0.0.1/draft-bark"}]);
assert.match(nodes.get("notificationTestResult").textContent, /Webhook：测试通知已发送.*Bark：发送失败，接收端返回 HTTP 401/);
assert.equal(saved.length, savedBeforeTest); assert.equal(JSON.stringify(context.configuration), beforeTestConfig); assert.equal(context.settingsDirty, true);
nodes.get("webhookURL").value = nodes.get("barkURL").value = ""; await nodes.get("testNotification").listeners.click();
assert.match(nodes.get("notificationTestResult").textContent, /请先填写 Webhook 或 Bark 地址/);
nodes.get("webhookHeaders").value = '{"Authorization":'; await nodes.get("testNotification").listeners.click();
assert.match(nodes.get("notificationTestResult").textContent, /请求头必须是有效的 JSON 对象/);
assert.equal(nodes.get("testNotification").disabled, false); assert.equal(notificationRequests.length, 1);
context.settingsDirty = false; context.tokenSettingsDirty = false; context.fillConfig(original, {});
assert.equal(nodes.get("tokenRefreshLead").value, 5); assert.equal(nodes.get("tokenRefreshUnit").value, "24");
assert.equal(nodes.get("tokenRefreshEnabled").checked, false, "令牌刷新默认关闭");
nodes.get("tokenRefreshLead").value = "6";
assert.equal(context.readTokenRefreshConfig().token_refresh_before_hours, 144);
nodes.get("tokenRefreshUnit").value = "1"; nodes.get("tokenRefreshLead").value = "36";
assert.equal(context.readTokenRefreshConfig().token_refresh_before_hours, 36, "小时单位正确换算");
nodes.get("tokenRefreshLead").value = "0"; assert.throws(() => context.readTokenRefreshConfig(), /提前时长/);
nodes.get("tokenRefreshLead").value = "36"; nodes.get("tokenRefreshEnabled").checked = true;
nodes.get("tokenRefreshSettings").listeners.input();
assert.equal(context.tokenSettingsDirty, true);
nodes.get("prompt").value = "unsaved warmup draft"; context.settingsDirty = true;
context.fillConfig({...original, token_refresh_before_hours: 120, token_refresh_management_key: "obsolete-fake-key", token_refresh_management_url: "http://127.0.0.1:8317"}, {});
assert.equal(nodes.get("tokenRefreshLead").value, "36", "读取配置保留未保存的刷新时长");
assert.equal(nodes.get("prompt").value, "unsaved warmup draft", "刷新设置与预热草稿独立");
const beforeTokenSave = saved.length;
nodes.get("tokenRefreshSettings").listeners.submit({preventDefault() {}}); await pending;
assert.equal(saved.length, beforeTokenSave + 1);
assert.deepEqual(saved.at(-1), {...original, token_refresh_enabled: true, token_refresh_before_hours: 36}, "刷新设置保留预热配置和 CPA 管理字段，清理旧连接字段");
assert.equal(nodes.get("prompt").value, "unsaved warmup draft"); assert.equal(context.settingsDirty, true); assert.equal(context.tokenSettingsDirty, false);
assert.match(nodes.get("nextTokenRefresh").textContent, /2030/); assert.equal(nodes.get("nextTokenAccount").textContent, "模拟账号");
assert.equal(refreshTables.at(-1)[0], "tokenRefreshAccounts"); assert.equal(refreshTables.at(-1)[2][0][3], "已排期");
assert.equal(refreshTables.at(-1)[2][0][5], "凭据已更新");
assert.ok(html.includes('id="tab-tokens"') && html.includes('id="view-tokens"') && html.includes('id="nextTokenRefresh"'), "顶部菜单与刷新计划摘要齐备");
assert.ok(html.indexOf('id="dailyPlan"') < html.indexOf('id="add"') && html.indexOf('id="add"') < html.indexOf('id="historyHeading"'), "添加时刻属于每日计划");
assert.equal(/id="webhook(?:Username|Password)"/.test(html), false, "不再显示独立账号密码输入");
assert.equal(/id="(?:tokenRefreshURL|tokenRefreshKey|useConnectionKey)"/.test(html), false, "只保留顶部连接设置");
console.log("设置页验证通过：预热与通知、令牌刷新独立保存、天/小时换算、草稿保留和下次刷新时间。");
