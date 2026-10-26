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
    activeView: "logs",
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
  assert.equal(nodes.get("tokenRefreshFields").disabled, true);
  assert.equal(removed.length, code === 401 ? 2 : 0, "401 清除无效密钥，403 保留密钥并展示限制原因");
}
const connect = html.slice(html.indexOf("async function cacheCurrentConnection()"), html.indexOf('$("connectionForm").addEventListener'));
for (const [server, expected] of [
  [{host: "", port: 8318}, "http://127.0.0.1:8318"],
  [{host: "0.0.0.0", port: 8317}, "http://127.0.0.1:8317"],
  [{host: "::", port: 8317}, "http://[::1]:8317"],
  [{host: "192.0.2.1", port: 8317}, "http://192.0.2.1:8317"],
  [{host: "", port: 8317, tls: {enable: true}}, "https://public.example"],
  [{port: 0}, null], [{port: 99999}, null], [{}, null],
]) {
  const paths = [], nodes = new Map();
  const context = vm.createContext({
    base: "/plugin", activeView: "logs", window: {location: {origin: "https://public.example"}},
    $: id => { if (!nodes.has(id)) nodes.set(id, {}); return nodes.get(id); },
    loadAll: async () => {}, notice() {},
    request: async (path, options) => { paths.push([path, options]); return path.endsWith("/server") ? server : {cached: true}; },
  });
  vm.runInContext(connect, context);
  if (expected) {
    await context.connect();
    assert.deepEqual(paths.map(([path]) => path), ["/v8/management/config/server", "/plugin/connection"]);
    assert.deepEqual(JSON.parse(paths[1][1].body), {url: expected}, "只传自动识别的地址，密钥由现有认证头复用");
    assert.equal(nodes.get("connection").open, false);
  } else {
    await assert.rejects(context.cacheCurrentConnection(), /端口/);
    assert.equal(paths.length, 1, "无法识别原生端口时不能猜测并缓存地址");
  }
  paths.length = 0;
  context.loadAll = async () => { throw Object.assign(new Error("denied"), {status: 401}); };
  await assert.rejects(context.connect(), error => error.status === 401);
  assert.equal(paths.length, 0, "顶部认证失败后不得继续读取地址或缓存密钥");
}
console.log("管理页鉴权与缓存验证通过：错误密钥只请求一次，自动识别原生地址，复用现有认证头。");
