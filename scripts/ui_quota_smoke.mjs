import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const html = fs.readFileSync(new URL("../web/index.html", import.meta.url), "utf8");
const code = html.slice(html.indexOf("function renderQuotaStatus("), html.indexOf("async function startManual("));
class Element {
  constructor(tag = "div") { this.tagName = tag; this.children = []; this.dataset = {}; this.listeners = {}; this.checked = false; this.value = ""; }
  append(...nodes) { this.children.push(...nodes); }
  replaceChildren(...nodes) { this.children = nodes; }
  addEventListener(event, fn) { this.listeners[event] = fn; }
  setAttribute(name, value) { this[name] = value; }
  querySelectorAll(selector) {
    const field = selector.match(/data-field="([^"]+)"/)?.[1];
    return this.children.flatMap(child => child instanceof Element ? [...(field ? child.dataset.field === field : child.tagName === selector) ? [child] : [], ...child.querySelectorAll(selector)] : []);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0]; }
}
const nodes = new Map(), saved = [], tables = [];
let pending;
const original = {enabled: true, priority: 7, model: "existing-model", warmup_quota_check: false, token_refresh_enabled: true, quota_guard_enabled: true, quota_guard_accounts: {a: {five_hour_min_remaining: 20, weekly_min_remaining: 10}, disconnected: {five_hour_min_remaining: 0, weekly_min_remaining: 15}}};
const context = vm.createContext({
  accounts: [{id: "a", label: "账号 A"}, {id: "b", label: "账号 B"}], connected: true, quotaSettingsDirty: false,
  currentStatus: {configuration: {timezone: "Asia/Singapore"}}, configuration: original, base: "/plugin",
  $: id => { if (!nodes.has(id)) nodes.set(id, new Element()); return nodes.get(id); },
  document: {createElement: tag => new Element(tag), createTextNode: text => text},
  table: (...args) => tables.push(args), formatTime: value => value,
  action: (_button, _message, fn) => { pending = fn(); }, notice() {}, refreshStatus: async () => {},
  request: async (path, options) => { assert.equal(path, "/plugin/config"); saved.push(JSON.parse(options.body)); },
});
vm.runInContext(code, context);
context.fillQuotaConfig(original);
const rows = nodes.get("quotaRules").children;
assert.deepEqual(rows.map(row => row.dataset.id), ["a", "b", "disconnected"], "暂未连接的已有规则必须保留");
for (const row of rows) for (const input of row.querySelectorAll("input")) if (input.type === "number") input.value = String(input.value);
assert.equal(rows[0].querySelector('[data-field="enabled"]').checked, true);
assert.equal(rows[1].querySelectorAll("input").filter(input => input.type === "number").every(input => input.disabled), true);
nodes.get("quotaSettings").listeners.submit({preventDefault() {}}); await pending;
assert.deepEqual(saved.at(-1), original, "额度保护保存不得修改预热开关或令牌刷新设置");
const input = rows[0].querySelector('[data-field="five_hour_min_remaining"]');
input.value = "25"; nodes.get("quotaSettings").listeners.input();
context.fillQuotaConfig(original); assert.equal(input.value, "25", "刷新时保留未保存的额度草稿");
nodes.get("quotaSettings").listeners.submit({preventDefault() {}}); await pending;
assert.equal(saved.at(-1).quota_guard_accounts.a.five_hour_min_remaining, 25);
input.value = ""; assert.throws(() => context.readQuotaRules(), /额度下限/);
input.value = "NaN"; assert.throws(() => context.readQuotaRules(), /额度下限/);
input.value = "0"; rows[0].querySelector('[data-field="weekly_min_remaining"]').value = "0";
assert.throws(() => context.readQuotaRules(), /非零下限/);
context.renderQuotaStatus({enabled: true, accounts: [{id: "a", paused: true, five_hour: {remaining: 15}, weekly: {remaining: 50}, reason: "5 小时到达下限", checked_at: "2026-11-26T00:00:00Z"}]});
assert.equal(tables.at(-1)[2][0][0], "账号 A"); assert.equal(tables.at(-1)[2][0][3], "保护中");
assert.match(tables.at(-1)[2][0][5], /5 小时/);
console.log("额度页验证通过：账号规则、断开保留、输入校验、草稿与独立保存。");
