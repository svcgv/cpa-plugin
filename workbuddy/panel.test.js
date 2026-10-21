const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

function fakeElement() {
  const queries = new Map();
  const element = {
    hidden: false,
    className: "",
    textContent: "",
    innerHTML: "",
    value: "",
    files: [],
    style: {},
    dataset: {},
    children: [],
    parentNode: null,
    classList: {
      values: new Set(),
      add(value) { this.values.add(value); },
      remove(value) { this.values.delete(value); },
      contains(value) { return this.values.has(value); },
    },
    appendChild(child) {
      child.parentNode = this;
      this.children.push(child);
      return child;
    },
    querySelector(selector) {
      if (!queries.has(selector)) queries.set(selector, fakeElement());
      return queries.get(selector);
    },
    querySelectorAll() { return []; },
    addEventListener() {},
    focus() {},
    remove() {
      if (!this.parentNode) return;
      const index = this.parentNode.children.indexOf(this);
      if (index >= 0) this.parentNode.children.splice(index, 1);
      this.parentNode = null;
    },
  };
  Object.defineProperty(element, "firstChild", {
    get() { return element.children[0] || null; },
  });
  return element;
}

function loadPanel(overrides = {}) {
  const html = fs.readFileSync(path.join(__dirname, "panel.html"), "utf8");
  const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)];
  const source = scripts.at(-1)[1]
    .replace("__WB_MANAGEMENT_BASE_PATH_JSON__", JSON.stringify("/v0/management"))
    .replace(/\nloadInitial\(\);\s*$/, "");
  const elements = new Map();
  const storage = new Map(overrides.sessionEntries || []);
  const document = {
    documentElement: fakeElement(),
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, fakeElement());
      return elements.get(id);
    },
    createElement() { return fakeElement(); },
    querySelectorAll() { return []; },
    addEventListener() {},
  };
  const sessionStorage = overrides.sessionStorage || {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, String(value)); },
    removeItem(key) { storage.delete(key); },
  };
  const context = {
    console,
    document,
    sessionStorage,
    localStorage: overrides.localStorage || { getItem() { return null; }, setItem() { throw new Error("localStorage write"); } },
    location: overrides.location || { href: "http://localhost/panel", search: "", pathname: "/panel", hash: "", host: "localhost" },
    history: overrides.history || { replaceState() {} },
    navigator: { userAgent: "node-test" },
    URL,
    URLSearchParams,
    TextEncoder,
    TextDecoder,
    Uint8Array,
    btoa(value) { return Buffer.from(value, "binary").toString("base64"); },
    atob(value) { return Buffer.from(value, "base64").toString("binary"); },
    requestAnimationFrame(fn) { fn(); },
    setTimeout(fn) { fn(); return 1; },
    clearTimeout() {},
    fetch: overrides.fetch || (async () => { throw new Error("unexpected fetch"); }),
  };
  context.window = context;
  context.self = context;
  context.top = context;
  vm.createContext(context);
  vm.runInContext(source, context, { filename: "panel.html" });
  return { context, document, elements, storage };
}

function fakeResponse(status, contentType, body) {
  return {
    status,
    ok: status >= 200 && status < 300,
    headers: { get(name) { return name.toLowerCase() === "content-type" ? contentType : null; } },
    async text() { return body; },
  };
}

test("query key replaces session key once and is removed from URL", () => {
  let replaced = "";
  let localWrites = 0;
  const location = {
    href: "http://localhost/panel?key=secret-value&view=all#accounts",
    search: "?key=secret-value&view=all",
    pathname: "/panel",
    hash: "#accounts",
    host: "localhost",
  };
  const { storage } = loadPanel({
    sessionEntries: [["workbuddy-mgmt-key", "old-value"]],
    location,
    history: { replaceState(state, title, url) { replaced = url; } },
    localStorage: {
      getItem() { return null; },
      setItem() { localWrites += 1; },
    },
  });
  assert.equal(storage.get("workbuddy-mgmt-key"), "secret-value");
  assert.equal(replaced, "/panel?view=all#accounts");
  assert.equal(localWrites, 0);
});

test("Web Storage exceptions clean the query key and use the fixed prompt fallback", async t => {
  for (const errorName of ["SecurityError", "QuotaExceededError"]) {
    await t.test(errorName, async () => {
      const nativeError = new Error(errorName + " native storage detail");
      nativeError.name = errorName;
      const sessionStorage = {
        getItem() { throw nativeError; },
        setItem() { throw nativeError; },
        removeItem() { throw nativeError; },
      };
      let replaced = "";
      let fetchCalls = 0;
      const { context, elements } = loadPanel({
        sessionStorage,
        location: {
          href: "http://localhost/custom/panel?view=all&key=secret-value#accounts",
          search: "?view=all&key=secret-value",
          pathname: "/custom/panel",
          hash: "#accounts",
          host: "localhost",
        },
        history: { replaceState(state, title, url) { replaced = url; } },
        fetch: async () => { fetchCalls += 1; throw new Error("unexpected fetch"); },
      });

      assert.equal(replaced, "/custom/panel?view=all#accounts");
      assert.equal(context.getKey(), null);
      assert.equal(await context.load(false), false);
      assert.equal(elements.get("authBox").style.display, "block");
      assert.match(elements.get("grid").innerHTML, /请先填写 management key/);
      assert.doesNotMatch(elements.get("grid").innerHTML, /native storage detail|SecurityError|QuotaExceededError/);
      await assert.rejects(
        context.api("/accounts"),
        error => error.message === "需要管理密钥" && !error.message.includes("native storage detail"),
      );

      context.document.getElementById("keyInput").value = "manual-key";
      await context.saveKey();
      assert.equal(elements.get("authBox").style.display, "block");
      assert.throws(
        () => context.rejectPanelAuth(fakeResponse(401, "application/json", "")),
        error => error.message === "management key 无效或缺失" && !error.message.includes("native storage detail"),
      );
      assert.equal(fetchCalls, 0);
    });
  }
});

test("query key storage write failure never reuses an older session key", async () => {
  const nativeError = new Error("QuotaExceededError native storage detail");
  nativeError.name = "QuotaExceededError";
  let replaced = "";
  let fetchCalls = 0;
  const sessionStorage = {
    getItem() { return "old-value"; },
    setItem() { throw nativeError; },
    removeItem() { throw nativeError; },
  };
  const { context, elements } = loadPanel({
    sessionStorage,
    location: {
      href: "http://localhost/custom/panel?key=new-value&view=all#accounts",
      search: "?key=new-value&view=all",
      pathname: "/custom/panel",
      hash: "#accounts",
      host: "localhost",
    },
    history: { replaceState(state, title, url) { replaced = url; } },
    fetch: async () => { fetchCalls += 1; throw new Error("unexpected fetch"); },
  });

  assert.equal(replaced, "/custom/panel?view=all#accounts");
  assert.equal(context.getKey(), null);
  context.readPanelKey = () => "old-panel-value";
  assert.equal(context.getKey(), null);
  assert.equal(await context.load(false), false);
  assert.equal(elements.get("authBox").style.display, "block");
  assert.match(elements.get("grid").innerHTML, /请先填写 management key/);
  assert.doesNotMatch(elements.get("grid").innerHTML, /old-value|new-value|native storage detail/);
  assert.equal(fetchCalls, 0);
});

test("model status banner hides ready and persists non-ready states", () => {
  const { context, elements } = loadPanel();
  const statuses = [
    { state: "stale", message: "模型目录刷新失败，正在使用上次有效缓存" },
    { state: "failed", message: "模型目录不可用" },
    { state: "loading", message: "模型目录正在初始化" },
    { state: "not_started", message: "模型目录尚未初始化" },
  ];
  for (const status of statuses) {
    context.updateModelStatus(status);
    const banner = elements.get("modelStatus");
    assert.equal(banner.hidden, false);
    assert.equal(banner.className, "model-status " + status.state);
    assert.equal(banner.textContent, status.message);
  }
  context.updateModelStatus({ state: "ready", message: "ignored" });
  const banner = elements.get("modelStatus");
  assert.equal(banner.hidden, true);
  assert.equal(banner.className, "model-status");
  assert.equal(banner.textContent, "");
});

test("load updates model status before dashboard error", async () => {
  const { context, elements, storage } = loadPanel();
  storage.set("workbuddy-mgmt-key", "test-key");
  let toastCalls = 0;
  context.toast = () => { toastCalls += 1; };
  context.api = async () => ({
    model_status: { state: "failed", message: "模型目录不可用" },
    error: "dashboard unavailable",
  });
  await context.load(false);
  assert.equal(elements.get("modelStatus").textContent, "模型目录不可用");
  assert.equal(toastCalls, 0);
});

test("model status message is rendered as text", () => {
  const { context, elements } = loadPanel();
  const message = "<img src=x onerror=1>";
  context.updateModelStatus({ state: "failed", message });
  const banner = elements.get("modelStatus");
  assert.equal(banner.textContent, message);
  assert.equal(banner.innerHTML, "");
});

test("panel response parser never exposes response bodies", async () => {
  const { context } = loadPanel();
  const cases = [
    [fakeResponse(200, "application/json", ""), "响应为空"],
    [fakeResponse(200, "text/html; charset=utf-8", "<html>secret-token</html>"), "响应格式无效"],
    [fakeResponse(200, "application/json", "{secret-token"), "响应 JSON 无效"],
    [fakeResponse(503, "application/json", `{"error":"secret-token"}`), "请求失败"],
  ];
  for (const [response, category] of cases) {
    await assert.rejects(
      context.readPanelResponse(response),
      error => error.message.includes(category) &&
        error.message.includes("HTTP ") &&
        !error.message.includes("secret-token"),
    );
  }
  const sanitized = await context.readPanelResponse(
    fakeResponse(200, "application/json", `{"error":"secret-token","nested":{"error":"credential-value"}}`),
  );
  assert.equal(sanitized.error, "请求失败");
  assert.equal(sanitized.nested.error, "请求失败");
  assert.doesNotMatch(JSON.stringify(sanitized), /secret-token|credential-value/);
});

test("panel transport errors use a fixed message", async () => {
  const nativeMessage = "https://host.invalid/?key=secret-token";
  const { context, elements, storage } = loadPanel({
    fetch: async () => { throw new Error(nativeMessage); },
  });
  storage.set("workbuddy-mgmt-key", "test-key");
  await assert.rejects(context.api("/accounts"), error => error.message === "网络请求失败");
  await assert.rejects(context.managementAPI("/plugins/workbuddy/config"), error => error.message === "网络请求失败");

  const toasts = [];
  context.toast = (...args) => { toasts.push(args.join(" ")); };
  await context.load(false);
  await context.load(true, fakeElement());
  const visible = elements.get("grid").innerHTML + toasts.join(" ");
  assert.match(visible, /网络请求失败/);
  assert.doesNotMatch(visible, /secret-token|host\.invalid/);
});

test("panel response auth failures clear the key without reading bodies", async () => {
  for (const [status, message] of [[401, "management key 无效或缺失"], [403, "禁止访问 (403)"]]) {
    let reads = 0;
    const response = fakeResponse(status, "application/json", `{"error":"secret-token"}`);
    response.text = async () => { reads += 1; return `{"error":"secret-token"}`; };
    const { context, storage } = loadPanel({ fetch: async () => response });

    storage.set("workbuddy-mgmt-key", "test-key");
    await assert.rejects(context.api("/accounts"), error => error.message === message);
    assert.equal(storage.has("workbuddy-mgmt-key"), false);

    assert.equal(context.storeSessionKey("test-key"), true);
    await assert.rejects(context.managementAPI("/plugins/workbuddy/config"), error => error.message === message);
    assert.equal(storage.has("workbuddy-mgmt-key"), false);
    assert.equal(reads, 0);
  }
});

test("panel response auth failures use a fixed local cooldown", async () => {
  let calls = 0;
  const { context, storage } = loadPanel({
    fetch: async () => {
      calls += 1;
      return fakeResponse(403, "application/json", `{"error":"secret-token"}`);
    },
  });
  for (let attempt = 0; attempt < 2; attempt += 1) {
    assert.equal(context.storeSessionKey("test-key"), true);
    await assert.rejects(context.api("/accounts"), error => error.message === "禁止访问 (403)");
  }
  assert.equal(context.storeSessionKey("test-key"), true);
  await assert.rejects(
    context.api("/accounts"),
    error => error.message === "认证多次失败，请检查管理密钥后 60s 再试",
  );
  assert.equal(context.storeSessionKey("test-key"), true);
  await assert.rejects(
    context.api("/accounts"),
    error => error.message === "认证多次失败，请稍后重试（防 IP 封禁）",
  );
  assert.equal(calls, 3);
});

test("panel response raw dashboard errors never reach the grid or toast", async () => {
  const originalError = "credential-value secret-token";
  const { context, elements, storage } = loadPanel();
  storage.set("workbuddy-mgmt-key", "test-key");
  context.api = async () => ({
    model_status: { state: "failed", message: "模型目录不可用" },
    error: originalError,
  });
  const toasts = [];
  context.toast = (...args) => { toasts.push(args.join(" ")); };

  await context.load(false);
  await context.load(true, fakeElement());
  const visible = elements.get("grid").innerHTML + toasts.join(" ");
  assert.match(visible, /请求失败/);
  assert.doesNotMatch(visible, /credential-value|secret-token/);
});

test("credits sort keeps real zero above unknown without mutation", () => {
  const { context } = loadPanel();
  const accounts = [
    { auth_index: "unknown" },
    { auth_index: "zero", credits: { total_remain: 0, total_used: 0, total_size: 0 } },
    { auth_index: "positive", credits: { total_remain: 10, total_used: 0, total_size: 10 } },
  ];
  const original = structuredClone(accounts);
  const ids = list => Array.from(list, account => account.auth_index);
  assert.deepEqual(ids(context.accountsForView(accounts)), ["unknown", "zero", "positive"]);
  const button = { textContent: "" };
  context.cycleRemainSort(button);
  assert.deepEqual(ids(context.accountsForView(accounts)), ["positive", "zero", "unknown"]);
  context.cycleRemainSort(button);
  assert.deepEqual(ids(context.accountsForView(accounts)), ["zero", "positive", "unknown"]);
  context.cycleRemainSort(button);
  assert.deepEqual(ids(context.accountsForView(accounts)), ["unknown", "zero", "positive"]);
  assert.deepEqual(accounts, original);
});

test("available balance excludes disabled and exhausted positive credits", () => {
  const { context, elements } = loadPanel();
  context.renderSummary([
    { region: "cn", credits: { total_remain: 100, total_used: 1, total_size: 101 } },
    { region: "cn", disabled: true, credits: { total_remain: 50, total_used: 2, total_size: 52 } },
    { region: "cn", exhausted: true, credits: { total_remain: 25, total_used: 3, total_size: 28 } },
  ]);
  const html = elements.get("summaryBox").innerHTML;
  assert.match(html, /剩余\(可用\)<\/div><div class="v ok">100<\/div>/);
  assert.match(html, /已用\(消耗\)<\/div><div class="v [^"]*">6<\/div>/);
  assert.match(html, /CN 可用 100 \/ 已用 6/);
  assert.doesNotMatch(html, /剩余\(可用\)<\/div><div class="v ok">175<\/div>/);
});

test("partial import clears credentials and keeps modal open", async () => {
  const { context, elements } = loadPanel();
  const modal = elements.get("importModal") || context.document.getElementById("importModal");
  modal.classList.add("show");
  const raw = context.document.getElementById("importRaw");
  raw.value = "raw-secret-credential";
  const files = context.document.getElementById("importFiles");
  files.value = "selected";
  files.files = [
    { name: "ok.json", size: 10, async text() { return "credential-one"; } },
    { name: "bad.json", size: 10, async text() { return "credential-two"; } },
  ];
  context.importOneCredential = async value => ({ success: value !== "credential-two" });
  let toastDetail = "";
  context.toast = (title, kind, detail) => { toastDetail = title + " " + kind + " " + detail; };
  let reloads = 0;
  context.load = async () => { reloads += 1; return true; };
  await context.importAuth({ dataset: {}, innerHTML: "导入", disabled: false });
  assert.equal(modal.classList.contains("show"), true);
  assert.equal(raw.value, "");
  assert.equal(files.value, "");
  assert.equal(reloads, 1);
  assert.match(toastDetail, /ok\.json：成功/);
  assert.match(toastDetail, /bad\.json：导入失败/);
  assert.doesNotMatch(toastDetail, /raw-secret|credential-one|credential-two/);
});

test("partial import all success closes modal", async () => {
  const { context, elements } = loadPanel();
  const modal = elements.get("importModal") || context.document.getElementById("importModal");
  modal.classList.add("show");
  const files = context.document.getElementById("importFiles");
  files.value = "selected";
  files.files = [
    { name: "ok.json", size: 10, async text() { return "credential-one"; } },
  ];
  context.importOneCredential = async () => ({ success: true });
  context.toast = () => {};
  context.load = async () => true;
  await context.importAuth({ dataset: {}, innerHTML: "导入", disabled: false });
  assert.equal(modal.classList.contains("show"), false);
});

test("partial import all failure keeps modal open and sanitizes long names", async () => {
  const { context, elements } = loadPanel();
  const modal = elements.get("importModal") || context.document.getElementById("importModal");
  modal.classList.add("show");
  const files = context.document.getElementById("importFiles");
  files.value = "selected";
  files.files = [
    { name: "\u0000\u001f\u007f" + "x".repeat(125) + ".json", size: 10, async text() { return "credential-one"; } },
  ];
  context.importOneCredential = async () => ({ success: false });
  let toastDetail = "";
  context.toast = (_title, _kind, detail) => { toastDetail = detail; };
  context.load = async () => { throw new Error("unexpected reload"); };
  await context.importAuth({ dataset: {}, innerHTML: "导入", disabled: false });
  assert.equal(modal.classList.contains("show"), true);
  assert.equal(toastDetail, "0 成功 / 1 失败 · " + "x".repeat(120) + "：导入失败");
});

// ---------------------------------------------------------------------------
// Add-account dialog and capability-aware cards.
// ---------------------------------------------------------------------------

test("cards never offer check-in to international accounts", () => {
  const { context } = loadPanel();
  for (const service of ["codebuddy-global", "workbuddy-global"]) {
    const html = context.card({
      auth_index: "idx-1", nickname: "Nick", region: "global", service,
      checkin_capability: "supported", checkin: { today_checked_in: false },
    });
    assert.doesNotMatch(html, /data-action="checkin"/);
    assert.doesNotMatch(html, /data-action="rewards"/);
  }
});

test("only the legacy workbuddy-global service offers the expert trial pack", () => {
  const { context } = loadPanel();
  const base = { auth_index: "idx-1", nickname: "Nick", region: "global" };

  const codebuddy = context.card(Object.assign({}, base, {
    service: "codebuddy-global",
    checkin_capability: "supported",
  }));
  assert.doesNotMatch(codebuddy, /data-action="claim"/);

  const workbuddy = context.card(Object.assign({}, base, {
    service: "workbuddy-global",
    checkin_capability: "supported",
  }));
  assert.match(workbuddy, /data-action="claim"/);

  const claimed = context.card(Object.assign({}, base, {
    service: "workbuddy-global",
    trial_claimed: true,
    checkin_capability: "supported",
  }));
  assert.doesNotMatch(claimed, /data-action="claim"/);
  assert.match(claimed, /已领取/);
});

test("cards label the three services distinctly", () => {
  const { context } = loadPanel();
  for (const [service, region, want] of [
    ["cn", "cn", "CN"],
    ["codebuddy-global", "global", "Global(CB)"],
    ["workbuddy-global", "global", "Global(WB)"],
  ]) {
    const html = context.card({ auth_index: "i", region, service, checkin_capability: "supported" });
    assert.ok(html.indexOf(want) >= 0, "expected " + want + " in card for " + service);
  }
});

test("starting an add-account login posts the selected region", async () => {
  const calls = [];
  const { context, elements } = loadPanel({
    sessionEntries: [["workbuddy-mgmt-key", "k"]],
    fetch: async (url, opts) => {
      calls.push({ url, body: opts && opts.body });
      return fakeResponse(200, "application/json", JSON.stringify({
        success: true, session_id: "s-1", url: "https://example.test/login", region: "global",
      }));
    },
  });
  context.openAddModal();
  const radio = { value: "workbuddy-global", checked: true };
  context.document.querySelector = (sel) => (sel.indexOf("addRegion") >= 0 ? radio : null);

  await context.startAddLogin(elements.get("addStartBtn") || { disabled: false, dataset: {}, classList: { add() {}, remove() {} } });

  const start = calls.find(c => String(c.url).indexOf("/login/start") >= 0);
  assert.ok(start, "expected a /login/start call");
  assert.equal(JSON.parse(start.body).region, "workbuddy-global");
  // The authorization URL is shown so the user can copy it if the popup is blocked.
  assert.ok((elements.get("addBody").innerHTML || "").indexOf("https://example.test/login") >= 0);
});

test("closing the dialog cancels the in-flight login session", async () => {
  const calls = [];
  const { context } = loadPanel({
    sessionEntries: [["workbuddy-mgmt-key", "k"]],
    fetch: async (url, opts) => {
      calls.push({ url, body: opts && opts.body });
      if (String(url).indexOf("/login/start") >= 0) {
        return fakeResponse(200, "application/json", JSON.stringify({
          success: true, session_id: "s-42", url: "https://example.test/login",
        }));
      }
      return fakeResponse(200, "application/json", JSON.stringify({ success: true, cancelled: true }));
    },
  });
  context.openAddModal();
  context.document.querySelector = () => ({ value: "cn", checked: true });
  await context.startAddLogin({ disabled: false, dataset: {}, classList: { add() {}, remove() {} } });

  context.closeAddModal();
  // The cancel request is dispatched asynchronously.
  await new Promise(resolve => setImmediate(resolve));
  const cancel = calls.find(c => String(c.url).indexOf("/login/cancel") >= 0);
  assert.ok(cancel, "expected a /login/cancel call");
  assert.equal(JSON.parse(cancel.body).session_id, "s-42");
});

test("a late poll result cannot revive a superseded attempt", async () => {
  let released = null;
  const { context } = loadPanel({
    sessionEntries: [["workbuddy-mgmt-key", "k"]],
    fetch: async (url) => {
      if (String(url).indexOf("/login/status") >= 0) {
        // Hold the response until the test releases it.
        await new Promise(resolve => { released = resolve; });
        return fakeResponse(200, "application/json", JSON.stringify({
          status: "success", nickname: "Stale",
        }));
      }
      return fakeResponse(200, "application/json", JSON.stringify({
        success: true, session_id: "s-old", url: "https://example.test/login",
      }));
    },
  });
  context.openAddModal();
  context.document.querySelector = () => ({ value: "cn", checked: true });
  await context.startAddLogin({ disabled: false, dataset: {}, classList: { add() {}, remove() {} } });

  // Supersede the flow before the held poll resolves.
  assert.ok(released, "expected the poll to be in flight");
  context.resetAddState();
  released();
  await new Promise(resolve => setImmediate(resolve));

  // The superseded session must not be treated as the active one: no success
  // state may be rendered for it.
  const body = context.document.getElementById("addBody");
  assert.ok(!/登录成功/.test(body.innerHTML || ""), "stale poll rendered a success state");
});

// CPA HTML-escapes every string in a plugin management response, so the "&"
// separators of an authorization URL arrive as "&amp;". The panel must use the
// percent-encoded field so the state/nonce parameters survive.
test("authorization URL survives the host's HTML escaping", () => {
  const { context } = loadPanel();
  const real = "https://www.codebuddy.ai/login?loginSessionId=abc&platform=ide&state=S1&version=5.3.14";
  const hostEscaped = real.replace(/&/g, "&amp;");

  // The raw field alone would be corrupted.
  assert.notEqual(hostEscaped, real);
  assert.ok(hostEscaped.indexOf("&amp;") >= 0);

  // With url_encoded present the panel recovers the exact URL.
  const encoded = encodeURIComponent(real);
  const resolved = context.resolveAuthURL({ url: hostEscaped, url_encoded: encoded });
  assert.equal(resolved, real);
  const u = new URL(resolved);
  assert.equal(u.searchParams.get("state"), "S1");
  assert.equal(u.searchParams.get("platform"), "ide");
});

// Even without the encoded field, a host-escaped URL must be repaired rather
// than opened with a broken query.
test("host-escaped URL is unescaped when no encoded form is present", () => {
  const { context } = loadPanel();
  const real = "https://www.workbuddy.ai/login?platform=ide&state=S2";
  const hostEscaped = real.replace(/&/g, "&amp;");
  const resolved = context.resolveAuthURL({ url: hostEscaped });
  assert.equal(resolved, real);
  assert.equal(new URL(resolved).searchParams.get("state"), "S2");
});

test("resolveAuthURL tolerates a missing or malformed payload", () => {
  const { context } = loadPanel();
  assert.equal(context.resolveAuthURL({}), "");
  assert.equal(context.resolveAuthURL(null), "");
  assert.equal(context.resolveAuthURL({ url_encoded: "%%%not-hex" }), "%%%not-hex");
});

test("automatic and manual login links use the decoded authorization URL and polling keeps them", async () => {
  const calls = [];
  const scheduled = [];
  const expected = "https://www.codebuddy.ai/login?loginSessionId=abc&platform=ide&state=S1&version=5.3.14";
  const { context, document, elements } = loadPanel({
    sessionEntries: [["workbuddy-mgmt-key", "k"]],
    fetch: async (url, opts) => {
      calls.push({ url, body: opts && opts.body });
      if (String(url).includes("/login/start")) {
        return fakeResponse(200, "application/json", JSON.stringify({
          success: true,
          session_id: "s-url",
          service: "codebuddy-global",
          region: "global",
          // This is what CPA 7.2.x returns after htmlsanitize.JSONBody.
          url: expected.replace(/&/g, "&amp;"),
          url_encoded: encodeURIComponent(expected),
        }));
      }
      return fakeResponse(200, "application/json", JSON.stringify({
        status: "pending",
        message: "等待浏览器完成授权",
      }));
    },
  });

  context.document.querySelector = () => ({ value: "codebuddy-global", checked: true });
  const opened = [];
  context.open = url => { opened.push(url); };
  context.setTimeout = fn => { scheduled.push(fn); return scheduled.length; };
  context.clearTimeout = () => {};

  await context.startAddLogin(document.getElementById("addStartBtn"));

  assert.equal(opened[0], expected, "automatic popup must use the decoded URL");
  const body = document.getElementById("addBody").innerHTML;
  assert.ok(body.includes(expected.replace(/&/g, "&amp;")), "manual link must render the decoded URL safely");
  assert.ok(body.includes("id=\"addUrl\""), "manual URL input must remain visible");
  assert.ok(body.includes("复制") && body.includes("打开"), "copy/open controls must remain visible");

  await scheduled.shift()();
  const afterPending = document.getElementById("addBody").innerHTML;
  assert.ok(afterPending.includes("id=\"addUrl\""), "pending poll must not remove the authorization URL");
  assert.ok(afterPending.includes("复制") && afterPending.includes("打开"), "pending poll must retain URL actions");
  assert.equal(elements.get("addStatus").textContent, "等待浏览器完成授权", "pending message should be updated");
  context.openAddURL();
  assert.equal(opened[1], expected, "manual open must use the same decoded URL");
});

test("CN cards expose the reward action and international cards do not", () => {
  const { context } = loadPanel();
  const cn = context.card({ auth_index: "cn-1", region: "cn", service: "cn", checkin_capability: "supported" });
  assert.match(cn, /data-action="rewards"/);
  assert.match(cn, /签到与奖励/);
  for (const service of ["codebuddy-global", "workbuddy-global"]) {
    const global = context.card({ auth_index: "g-1", region: "global", service, checkin_capability: "supported" });
    assert.doesNotMatch(global, /data-action="rewards"/);
    assert.doesNotMatch(global, /签到与奖励/);
  }
});

test("reward run sends only the selected CN account and reports skipped international accounts", async () => {
  const calls = [];
  const { context } = loadPanel({
    sessionEntries: [["workbuddy-mgmt-key", "k"]],
    fetch: async (url, opts) => {
      calls.push({ url: String(url), body: opts && opts.body });
      if (String(url).includes("/rewards/run")) {
        return fakeResponse(200, "application/json", JSON.stringify({ started: 1, skipped: 2 }));
      }
      return fakeResponse(200, "application/json", JSON.stringify({ results: [] }));
    },
  });
  let toastText = "";
  context.toast = (title, _kind, detail) => { toastText = title + " " + detail; };
  context.loadRewardStatus = async () => {};
  await context.runRewards("cn-1", { dataset: {}, innerHTML: "成长奖励", disabled: false });
  const run = calls.find(call => call.url.includes("/rewards/run"));
  assert.ok(run, "expected /rewards/run");
  assert.equal(JSON.parse(run.body).auth_index, "cn-1");
  assert.match(toastText, /国际版已跳过（2）/);
});

test("saving reward switches patches the host config and verifies runtime state", async () => {
  const calls = [];
  const { context } = loadPanel({ sessionEntries: [["workbuddy-mgmt-key", "k"]] });
  context.managementAPI = async (url, opts) => {
    calls.push({ url, body: opts && opts.body, method: opts && opts.method });
    return {};
  };
  context.api = async () => ({
    auto: false,
    actions: { checkin: true, travel: false, tasks: true, makeup: true, redeem: true, lottery: false, energy: true },
    results: [],
  });
  await context.saveRewardSettings(
    { auto: false, actions: { checkin: true, travel: false, tasks: true, makeup: true, redeem: true, lottery: false, energy: true } },
    d => d.auto === false,
  );
  const patch = calls.find(call => call.method === "PATCH");
  assert.ok(patch, "expected a config PATCH");
  assert.match(patch.url, /\/plugins\/workbuddy\/config$/);
  const body = JSON.parse(patch.body);
  assert.equal(body.rewards.auto, false);
  assert.equal(body.rewards.actions.travel, false);
});

test("a per-action reward button sends exactly that action", async () => {
  const calls = [];
  const { context } = loadPanel({
    sessionEntries: [["workbuddy-mgmt-key", "k"]],
    fetch: async (url, opts) => {
      calls.push({ url: String(url), body: opts && opts.body });
      return fakeResponse(200, "application/json", JSON.stringify({ started: 1, skipped: 0 }));
    },
  });
  context.loadRewardStatus = async () => {};
  const html = context.card({ auth_index: "cn-1", region: "cn", service: "cn", checkin: { today_checked_in: false } });
  for (const action of ["travel", "makeup", "lottery", "energy"]) {
    assert.match(html, new RegExp(`data-reward-action="${action}"`));
  }
  context.api = async (path, opts) => {
    calls.push({ url: String(path), body: opts && opts.body });
    return { started: 1, skipped: 0 };
  };
  await context.runRewards("cn-1", { dataset: {}, innerHTML: "x", disabled: false }, ["makeup"]);
  const run = calls.find(call => call.url.includes("/rewards/run"));
  assert.deepEqual(JSON.parse(run.body).actions, ["makeup"]);
});
