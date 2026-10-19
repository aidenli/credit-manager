import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import vm from 'node:vm';

const source = readFileSync(new URL('../internal/management/web/console.js', import.meta.url), 'utf8');
const bindingLoad = source.slice(source.indexOf('  async function loadKeyBindingData()'), source.indexOf('  function renderKeyBindingAccountOptions()'));
const searchSource = source.slice(source.indexOf('  function setKeySearchSource('), source.indexOf('  function renderKeys('));
const keys = [
  { id: 'live', revoked_at: null, enabled: true, created_at: '2026-10-18' },
  { id: 'deleted', revoked_at: '2026-10-19', enabled: false, created_at: '2026-10-19' },
  { id: 'disabled', revoked_at: null, enabled: false, created_at: '2026-10-17' },
];

test('overview and usage key dropdowns hide deleted keys without mutating history', () => {
  const state = {};
  const rendered = [];
  const context = vm.createContext({ state, $: () => null, renderKeySearchOptions: kind => rendered.push(kind) });
  vm.runInContext(searchSource, context);
  const snapshot = JSON.stringify(keys);
  context.setKeySearchSource(keys);
  assert.deepEqual(Array.from(state.allKeys, key => key.id), ['live', 'disabled']);
  assert.deepEqual(rendered, ['usage', 'overview']);
  assert.equal(JSON.stringify(keys), snapshot);
  context.setKeySearchSource(null);
  assert.equal(state.allKeys.length, 0);
});

test('both key filters clear deleted selections but preserve free-text and valid selections', () => {
  const elements = {
    usageKeyFilter: { value: 'deleted label', dataset: { selectedKeyId: 'deleted' } },
    overviewKeyFilter: { value: 'deleted label', dataset: { selectedKeyId: 'deleted' } },
  };
  const context = vm.createContext({ state: {}, $: id => elements[id], renderKeySearchOptions() {} });
  vm.runInContext(searchSource, context);
  context.setKeySearchSource(keys);
  for (const input of Object.values(elements)) {
    assert.equal(input.value, '');
    assert.equal(input.dataset.selectedKeyId, undefined);
  }
  elements.usageKeyFilter.value = 'unfinished search';
  elements.overviewKeyFilter = { value: 'live label', dataset: { selectedKeyId: 'live' } };
  context.setKeySearchSource(keys);
  assert.equal(elements.usageKeyFilter.value, 'unfinished search');
  assert.equal(elements.overviewKeyFilter.value, 'live label');
  assert.equal(elements.overviewKeyFilter.dataset.selectedKeyId, 'live');
});

test('overview refetches after clearing a deleted filter and never renders stale scoped totals', async () => {
  const state = { tabLoadSeq: 0 };
  const input = { value: 'deleted label', dataset: { selectedKeyId: 'deleted' } };
  const requests = [];
  const rendered = [];
  const context = vm.createContext({
    state, $: id => id === 'overviewKeyFilter' ? input : null,
    resolveKeyFilter: control => control.dataset.selectedKeyId || '',
    overviewQuery: () => input.dataset.selectedKeyId ? '?plugin_key_id=' + input.dataset.selectedKeyId : '',
    api: async (method, path) => { requests.push(path); return { keys, scope: path.includes('?') ? 'deleted' : 'all' }; },
    renderKeySearchOptions() {}, renderOverview: data => rendered.push(data.scope), renderPricing() {},
  });
  const overviewLoad = source.slice(source.indexOf('  async function loadOverviewBundle()'), source.indexOf('  // OAuth intelligence probe.'));
  vm.runInContext(searchSource + overviewLoad + '\nrenderKeys = keys => setKeySearchSource(keys);', context);
  await context.loadOverviewBundle();
  assert.deepEqual(requests, ['credit-manager/overview?plugin_key_id=deleted', 'credit-manager/overview']);
  assert.deepEqual(rendered, ['all']);
  assert.equal(state.overview.scope, 'all');
});

test('moving overview time boundaries do not trigger repeated refreshes', async () => {
  const state = { tabLoadSeq: 0 };
  const input = { value: 'live label', dataset: { selectedKeyId: 'live' } };
  let queries = 0;
  let requests = 0;
  const context = vm.createContext({
    state, $: id => id === 'overviewKeyFilter' ? input : null,
    resolveKeyFilter: control => control.dataset.selectedKeyId || '',
    overviewQuery: () => '?plugin_key_id=live&to=' + (++queries),
    api: async () => { assert.equal(++requests, 1); return { keys }; },
    renderKeySearchOptions() {}, renderOverview() {}, renderPricing() {},
  });
  const overviewLoad = source.slice(source.indexOf('  async function loadOverviewBundle()'), source.indexOf('  // OAuth intelligence probe.'));
  vm.runInContext(searchSource + overviewLoad + '\nrenderKeys = keys => setKeySearchSource(keys);', context);
  await context.loadOverviewBundle();
  assert.equal(requests, 1);
});

test('account binding list filters every page, keeping disabled but not deleted keys', async () => {
  const state = { tabLoadSeq: 0, keyBindingAccountID: '' };
  const calls = [];
  const hint = {};
  const context = vm.createContext({
    state, $: () => hint,
    loadKeyAuthAccounts: async () => [{ provider: 'codex', auth_id: 'account' }],
    api: async (method, path) => {
      assert.equal(method, 'GET');
      const params = new URL(path, 'https://example.invalid/').searchParams;
      assert.equal(params.get('active_only'), '1');
      calls.push(Number(params.get('page')));
      return { items: params.get('page') === '1' ? keys.slice(0, 2) : keys.slice(2), total_pages: 2 };
    },
    keyBindingResetDraft() {}, renderKeyBindingAccountOptions() {}, renderKeyBinding() {},
  });
  vm.runInContext(bindingLoad, context);
  await context.loadKeyBindingData();
  assert.deepEqual(calls, [1, 2]);
  assert.deepEqual(Array.from(state.keyBindingKeys, key => key.id), ['live', 'disabled']);
  assert.equal(hint.textContent, '共 2 个 Key');
  assert.equal(state.keyBindingLoaded, true);
});
