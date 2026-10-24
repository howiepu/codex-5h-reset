import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const html = fs.readFileSync(new URL("../web/index.html", import.meta.url), "utf8");
const functions = html.slice(html.indexOf("function updatePromptMode()"), html.indexOf("function markDirty()"));
const submit = html.slice(html.indexOf('$("settings").addEventListener("submit"'), html.indexOf("async function startManual("));
const nodes = new Map(), saved = [];
let pending;
const context = vm.createContext({
  configuration: null, settingsDirty: false, base: "/plugin", URL,
  $: id => {
    if (!nodes.has(id)) nodes.set(id, {
      value: "", checked: false, children: [], listeners: {},
      addEventListener(type, callback) { this.listeners[type] = callback; },
      replaceChildren() { this.children = []; },
    });
    return nodes.get(id);
  },
  addSchedule() {}, notice() {}, refreshStatus: async () => {},
  action: (_button, _message, fn) => { pending = fn(); },
  request: async (path, options) => {
    assert.equal(path, "/plugin/config");
    assert.equal(options.method, "PUT");
    saved.push(JSON.parse(options.body));
  },
});
vm.runInContext(functions + submit, context);
const original = {
  enabled: true, priority: 7, store: {repository: "test-source"},
  model: "test-model", prompt: "keep this fixed message", timezone: "Asia/Singapore",
  max_log_entries: 200, state_file: "test-history.json", history_retention_days: 30,
  schedules: [], notify_webhook_url: "", notify_bark_url: "",
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
console.log("管理页设置验证通过：随机算术题开关读写、旧配置兼容、固定内容保留和按模式校验。");
