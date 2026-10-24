import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const html = fs.readFileSync(new URL("../web/index.html", import.meta.url), "utf8");
const models = html.slice(html.indexOf("function renderModelHint()"), html.indexOf("function renderAccounts()"));
const functions = html.slice(html.indexOf("function updatePromptMode()"), html.indexOf("function markDirty()"));
const submit = html.slice(html.indexOf('$("settings").addEventListener("submit"'), html.indexOf("async function startManual("));
const nodes = new Map(), saved = [], notificationRequests = [];
let pending, waitNotification = Promise.resolve();
const notificationResults = [{channel: "Webhook", success: true}, {channel: "Bark", success: false, error: "接收端返回 HTTP 401"}];
const context = vm.createContext({
  configuration: null, settingsDirty: false, accounts: [], base: "/plugin", URL,
  document: {createElement: tag => ({tagName: tag, value: ""})},
  $: id => {
    if (!nodes.has(id)) nodes.set(id, {
      value: "", checked: false, children: [], listeners: {},
      addEventListener(type, callback) { this.listeners[type] = callback; },
      replaceChildren(...children) { this.children = children; },
    });
    return nodes.get(id);
  },
  addSchedule() {}, notice() {}, refreshStatus: async () => {},
  action: (_button, _message, fn) => { pending = fn(); },
  request: async (path, options) => {
    if (path === "/plugin/test-notification") {
      assert.equal(options.method, "POST");
      notificationRequests.push(JSON.parse(options.body));
      await waitNotification;
      return {results: notificationResults};
    }
    assert.equal(path, "/plugin/config");
    assert.equal(options.method, "PUT");
    saved.push(JSON.parse(options.body));
  },
});
vm.runInContext(models + functions + submit, context);
const original = {
  enabled: true, priority: 7, store: {repository: "test-source"},
  model: "test-model", prompt: "keep this fixed message", timezone: "Asia/Singapore",
  max_log_entries: 200, state_file: "test-history.json", history_retention_days: 30,
  schedules: [], notify_webhook_url: "", notify_bark_url: "",
  notify_webhook_username: "fake-user", notify_webhook_password: " fake-password with spaces ",
};
context.fillConfig(original, {});
const mode = nodes.get("randomArithmetic"), prompt = nodes.get("prompt");
assert.equal(mode.checked, false, "旧配置默认发送固定内容");
assert.equal(prompt.required, true);
assert.equal(prompt.disabled, false);
for (const random of [true, false]) {
  mode.checked = random;
  mode.listeners.change();
  assert.equal(prompt.required, !random);
  assert.equal(prompt.disabled, random);
  assert.equal(prompt.value, original.prompt, "切换模式必须保留原固定内容");
  nodes.get("settings").listeners.submit({preventDefault() {}});
  await pending;
  assert.deepEqual(saved.at(-1), {...original, random_arithmetic: random}, "保存开关时必须保留固定内容和 CPA 管理字段");
}
context.fillConfig({...original, random_arithmetic: true, prompt: ""}, {});
assert.equal(mode.checked, true, "重新加载后恢复已保存的模式");
assert.equal(prompt.disabled, true);
assert.doesNotThrow(() => context.validateConfig({...original, random_arithmetic: true, prompt: ""}));
assert.throws(() => context.validateConfig({...original, random_arithmetic: false, prompt: ""}), /固定发送内容不能为空/);
assert.throws(() => context.validateConfig({...original, random_arithmetic: true, model: ""}), /模型和历史路径不能为空/);
assert.equal(nodes.get("webhookPassword").value, original.notify_webhook_password, "读取密码时必须保留首尾空格");
assert.throws(() => context.validateConfig({...original, notify_webhook_username: "user:name"}), /用户名不能包含冒号或换行/);
assert.throws(() => context.validateConfig({...original, notify_webhook_username: ""}), /必须填写用户名/);
assert.doesNotThrow(() => context.validateConfig({...original, notify_webhook_username: "", notify_webhook_password: ""}));
context.accounts = [
  {id: "a", label: "账号 A", models: ["test-model", "shared"]},
  {id: "b", label: "账号 B", models: []},
  {id: "c", models: null},
  {id: "disabled", disabled: true, models: ["disabled-model"]},
];
context.renderModelOptions();
assert.deepEqual(nodes.get("modelOptions").children.map(option => option.value), ["shared", "test-model"]);
assert.match(nodes.get("modelHint").textContent, /1 \/ 3.*1 个账号暂无模型列表.*未注册：账号 B/);
nodes.get("model").value = "possible-alias";
nodes.get("model").listeners.input();
assert.match(nodes.get("modelHint").textContent, /0 \/ 3.*未注册：账号 A、账号 B.*模型别名及实际执行仍交给 CPA 判断/);
assert.equal(nodes.get("model").value, "possible-alias", "提示和建议不能替换用户选择的模型");
assert.doesNotThrow(() => context.validateConfig({...original, model: "possible-alias"}));
context.accounts = [{id: "legacy"}];
context.renderModelOptions();
assert.match(nodes.get("modelHint").textContent, /CPA 暂未提供账号模型列表/);
assert.equal(nodes.get("modelOptions").children.length, 0);
context.accounts = [];
context.renderModelOptions();
assert.match(nodes.get("modelHint").textContent, /暂无可执行的 Codex 账号/);
nodes.get("webhookURL").value = " http://127.0.0.1/draft-webhook ";
nodes.get("barkURL").value = " http://127.0.0.1/draft-bark ";
nodes.get("webhookUsername").value = " fake-draft-user ";
nodes.get("webhookPassword").value = " fake-draft-password ";
nodes.get("model").value = "";
context.settingsDirty = true;
const beforeTestConfig = JSON.stringify(context.configuration), savedBeforeTest = saved.length;
let release;
waitNotification = new Promise(resolve => { release = resolve; });
const testing = nodes.get("testNotification").listeners.click();
assert.equal(nodes.get("testNotification").disabled, true, "发送中禁用按钮，避免重复点击");
release(); await testing;
assert.equal(nodes.get("testNotification").disabled, false);
assert.deepEqual(notificationRequests, [{notify_webhook_url: "http://127.0.0.1/draft-webhook", notify_webhook_username: "fake-draft-user", notify_webhook_password: " fake-draft-password ", notify_bark_url: "http://127.0.0.1/draft-bark"}]);
assert.match(nodes.get("notificationTestResult").textContent, /Webhook：测试通知已发送.*Bark：发送失败，接收端返回 HTTP 401/);
assert.equal(saved.length, savedBeforeTest, "测试不能保存配置");
assert.equal(JSON.stringify(context.configuration), beforeTestConfig);
assert.equal(context.settingsDirty, true, "测试不能清除未保存状态");
nodes.get("webhookURL").value = nodes.get("barkURL").value = "";
await nodes.get("testNotification").listeners.click();
assert.match(nodes.get("notificationTestResult").textContent, /请先填写 Webhook 或 Bark 地址/);
nodes.get("webhookUsername").value = "invalid:name";
await nodes.get("testNotification").listeners.click();
assert.match(nodes.get("notificationTestResult").textContent, /用户名不能包含冒号或换行/);
assert.equal(nodes.get("testNotification").disabled, false, "失败后允许修正配置重新测试");
assert.equal(notificationRequests.length, 1, "校验失败不能发送请求");
console.log("管理页设置验证通过：消息模式、Webhook 认证、模型提示、测试通知使用未保存输入并独立显示渠道结果。");
