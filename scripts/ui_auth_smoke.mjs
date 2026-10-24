import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const html = fs.readFileSync(new URL("../web/index.html", import.meta.url), "utf8");
const load = html.slice(html.indexOf("async function loadAll()"), html.indexOf("async function action("));
for (const code of [401, 403, 200]) {
  const paths = [];
  const nodes = new Map();
  const context = vm.createContext({
    base: "/plugin",
    connected: false,
    $: id => {
      if (!nodes.has(id)) nodes.set(id, {value: ""});
      return nodes.get(id);
    },
    request: async path => {
      paths.push(path);
      if (code !== 200) throw Object.assign(new Error("denied"), {status: code});
      return {configuration: {}, accounts: [], logs: []};
    },
    renderStatus() {}, fillConfig() {}, renderAccounts() {}, renderLogs() {}, schedulePoll() {},
    document: {querySelectorAll: () => []},
  });
  vm.runInContext(load, context);
  if (code === 200) {
    await context.loadAll();
    assert.deepEqual(paths, ["/plugin/status", "/plugin/config", "/plugin/accounts", "/plugin/logs"]);
    assert.equal(context.connected, true);
  } else {
    await assert.rejects(context.loadAll(), error => error.status === code);
    assert.deepEqual(paths, ["/plugin/status"], "鉴权失败后不得并发读取其他管理接口");
  }
}
const request = html.slice(html.indexOf("async function request("), html.indexOf("function table("));
for (const code of [401, 403]) {
  const removed = [], nodes = new Map();
  const context = vm.createContext({
    connected: true, pollTimer: undefined, storageKey: "test-key",
    $: id => {
      if (!nodes.has(id)) nodes.set(id, {value: id === "key" ? "fake-key" : ""});
      return nodes.get(id);
    },
    setTimeout, clearTimeout, Headers, AbortController,
    rememberKey() {}, renderControls() {},
    localStorage: {removeItem: key => removed.push(key)},
    sessionStorage: {removeItem: key => removed.push(key)},
    fetch: async () => ({ok: false, status: code, text: async () => JSON.stringify({error: "IP banned"})}),
  });
  vm.runInContext(request, context);
  await assert.rejects(context.request("/plugin/status"), error => error.status === code && (code !== 403 || error.message.includes("IP banned")));
  assert.equal(context.connected, false);
  assert.equal(nodes.get("connection").open, true);
  assert.equal(nodes.get("settingsFields").disabled, true);
  assert.equal(removed.length, code === 401 ? 2 : 0, "401 清除无效密钥，403 保留密钥并展示限制原因");
}
console.log("管理页鉴权验证通过：错误密钥只请求一次，正确密钥继续加载。");
