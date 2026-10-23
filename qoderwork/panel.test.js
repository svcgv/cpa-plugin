const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

// Execute the shipped panel script, including startAddLogin's popup call.
// Testing qwSafeURL alone misses callers that still open the raw response URL.
function loadPanel(region, response) {
  const html = fs.readFileSync(path.join(__dirname, "panel.html"), "utf8");
  const source = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].at(-1)[1];
  const elements = new Map();
  const opened = [];
  const requests = [];
  const document = {
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, {
        innerHTML: "", style: {}, focus() {}, addEventListener() {},
      });
      return elements.get(id);
    },
    querySelector() { return { value: region }; },
    addEventListener() {},
  };
  const context = {
    document, URLSearchParams,
    location: { search: "", pathname: "/panel" },
    sessionStorage: { getItem() { return null; } },
    setTimeout() { return 1; }, clearTimeout() {},
    open(...args) { opened.push(args); },
  };
  context.window = context;
  context.self = context;
  context.top = context;
  vm.createContext(context);
  vm.runInContext(source, context, { filename: "panel.html" });
  context.api = async (route, options) => {
    requests.push({ route, body: JSON.parse(options.body) });
    return response;
  };
  return { context, elements, opened, requests };
}

for (const region of ["cn", "intl"]) {
  for (const encoding of ["percent", "html", "plain"]) {
    test(`${region} ${encoding}: automatic and manual login preserve every OAuth parameter`, async () => {
      const host = region === "cn" ? "qoder.com.cn" : "qoder.com";
      const params = new URLSearchParams({
        challenge: "test-challenge", challenge_method: "S256",
        client_id: "test-client", machine_id: "test-machine", nonce: "test-nonce",
        redirect_uri: region === "cn" ? "qoder-work-cn://" : "qoder://aicoding.aicoding-agent/login-success",
      });
      const url = `https://${host}/device/selectAccounts?${params}`;
      const response = {
        success: true, session_id: "test-session",
        url: encoding === "plain" ? url : url.replaceAll("&", "&amp;"),
      };
      if (encoding === "percent") response.url_encoded = encodeURIComponent(url);
      const { context, elements, opened, requests } = loadPanel(region, response);
      const button = { innerHTML: "开始登录", dataset: {} };
      await context.startAddLogin(button);

      assert.deepEqual(requests, [{ route: "/login/start", body: { region } }]);
      assert.equal(opened.length, 1, "login must open its authorization page");
      const actual = new URL(opened[0][0]);
      assert.deepEqual([...actual.searchParams], [...params], "popup lost OAuth parameters (参数无效)");
      assert.deepEqual(opened[0], [url, "_blank", "noopener"]);
      assert.ok(elements.get("addBody").innerHTML.includes(context.esc(url)), "copy field must show the same URL");
      context.openAddURL();
      assert.deepEqual(opened[1], opened[0], "manual and automatic open must agree");
      assert.equal(button.disabled, false);
    });
  }
}
