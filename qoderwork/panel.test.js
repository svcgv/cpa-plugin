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

test('model status renders numeric context lengths and escapes model names', async () => {
  const { context, elements } = loadPanel('cn');
  context.api = async route => route === '/models/context' ? {} : {
    status: { source: 'dynamic' },
    accounts: [{ name: 'account', region: 'cn', source: 'dynamic', models: [
      { name: '<model>', id: 'model-id', default_context: 128000, max_context: 256000 },
    ] }],
  };
  await context.loadContextSettings();
  assert.equal(elements.get('modelSource').textContent, '模型目录：dynamic');
  assert.match(elements.get('modelCatalog').innerHTML, /&lt;model&gt;<\/td><td class="model-id">model-id<\/td><td>128000<\/td><td>256000/);
  assert.equal(context.esc(0), '0');
});

test('model prices show upstream promotion and escape text', () => {
  const { context, elements } = loadPanel('cn');
  context.renderModelCatalog([{name:'account',region:'cn',source:'saved',fetched_at:'2026-01-01',models:[
    {id:'one',name:'<model>',context:100,price_factor:0.05,original_price_factor:0.5,promotion:{active:false,description:'<discount>',window_start:'22:00',window_end:'08:00',timezone:'Asia/Shanghai'}},
    {id:'two',name:'unknown',context:100},
  ]}]);
  const html=elements.get('modelCatalog').innerHTML;
  assert.match(html,/<table class="model-table qoder-model-table">/);
  assert.match(html,/<th scope="col">原始倍率<\/th>/);
  assert.match(html,/0.05×/); assert.match(html,/<td>0.5×<\/td>/); assert.match(html,/当前未生效/);
  assert.match(html,/22:00–08:00 Asia\/Shanghai/); assert.match(html,/&lt;discount&gt;/); assert.match(html,/未提供/);
});

test('context compaction settings load and save', async () => {
  const { context, elements } = loadPanel('cn');
  const runtime = {
    cn_max_context: true, intl_max_context: false, queue_wait: true, queue_wait_seconds: 540,
    context_compaction: {
      enabled: true, percent_override: 75, max_tokens: 4096, summary_model: 'qmodel',
      successes: 2, cache_hits: 3, failures: 1, tokens_saved: 9000,
    },
  };
  context.api = async route => route === '/models/context' ? runtime : { status: { source: 'dynamic' }, accounts: [] };
  await context.loadContextSettings();
  assert.equal(elements.get('contextCompaction').checked, true);
  assert.equal(elements.get('contextCompactionPct').value, '75');
  assert.equal(elements.get('contextCompactionMaxTokens').value, '4096');
  assert.equal(elements.get('contextCompactionModel').value, 'qmodel');
  assert.match(elements.get('compactionStatus').textContent, /缓存命中 3/);

  let saved = null;
  context.fetch = async (_url, options) => {
    saved = JSON.parse(options.body);
    runtime.context_compaction = {
      enabled: saved.context_compaction, percent_override: saved.context_compaction_pct,
      max_tokens: saved.context_compaction_max_tokens, summary_model: saved.context_compaction_model,
    };
    return { ok: true, status: 200 };
  };
  elements.get('contextCompaction').checked = false;
  elements.get('contextCompactionPct').value = '60';
  elements.get('contextCompactionMaxTokens').value = '2048';
  elements.get('contextCompactionModel').value = 'dfmodel';
  await context.saveContextSettings();
  assert.equal(saved.context_compaction, false);
  assert.equal(saved.context_compaction_pct, 60);
  assert.equal(saved.context_compaction_max_tokens, 2048);
  assert.equal(saved.context_compaction_model, 'dfmodel');
});
