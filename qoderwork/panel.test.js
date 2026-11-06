const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function loadPanel(region) {
  const html = fs.readFileSync(path.join(__dirname, 'panel.html'), 'utf8');
  const source = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].at(-1)[1];
  const elements = new Map();
  const document = {
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, { value: '', innerHTML: '', style: {}, dataset: {},
        classList: { add() {}, remove() {}, contains() { return false; } }, focus() {}, addEventListener() {} });
      return elements.get(id);
    },
    querySelector() { return { value: region }; }, addEventListener() {},
  };
  const context = { document, URLSearchParams, location: { search: '', pathname: '/panel' },
    sessionStorage: { getItem() { return null; } }, setTimeout() { return 1; }, clearTimeout() {} };
  context.window = context; context.self = context; context.top = context;
  vm.createContext(context); vm.runInContext(source, context, { filename: 'panel.html' });
  context.toast = () => {}; context.load = () => {};
  return { context, elements };
}

for (const region of ['cn', 'intl']) {
  test(`${region} PAT import sends selected region and clears the input on success`, async () => {
    const { context, elements } = loadPanel(region);
    const input = context.document.getElementById('importPAT');
    input.value = 'pt-example-token';
    const calls = [];
    context.api = async (route, options) => { calls.push({ route, body: JSON.parse(options.body) }); return { success: true }; };
    await context.importAuth({ innerHTML: '导入', dataset: {} });
    assert.deepEqual(calls, [{ route: '/import', body: { pat: 'pt-example-token', region } }]);
    assert.equal(input.value, '');
  });
}

test('invalid PAT is not submitted', async () => {
  const { context } = loadPanel('cn');
  context.document.getElementById('importPAT').value = 'invalid';
  let called = false; context.api = async () => { called = true; };
  await context.importAuth({ innerHTML: '导入', dataset: {} });
  assert.equal(called, false);
});
