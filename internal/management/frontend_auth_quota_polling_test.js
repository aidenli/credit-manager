'use strict';

// Run with node --test internal/management/frontend_auth_quota_polling_test.js.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const test = require('node:test');
const source = fs.readFileSync(path.join(__dirname, 'web', 'console.js'), 'utf8');

function harness() {
  const calls = [], timers = new Map();
  let timerID = 0;
  const items = [
    { provider: 'codex', auth_id: 'same', active_requests: 3, max_concurrent_requests: 8 },
    { provider: 'claude', auth_id: 'same', active_requests: 7, max_concurrent_requests: 9 },
  ];
  const badges = items.map(item => ({
    dataset: { provider: item.provider, itemKey: item.auth_id },
    textContent: String(item.active_requests),
    classList: { toggle(name, value) { this[name] = value; } },
  }));
  const input = { value: '123', focused: true };
  const list = {
    set innerHTML(_) { assert.fail('Polling must not re-render editing controls'); },
    querySelectorAll(selector) { assert.equal(selector, '.auth-quota-active'); return badges; },
  };
  const h = vm.createContext({
    URLSearchParams, AbortController,
    t: text => text, esc: text => String(text),
    state: { currentTab: 'auth-quotas', authQuotas: { items }, tabLoadSeq: 1, authQuotaLoading: 0, authQuotaCountsRequest: null },
    document: { hidden: false, activeElement: input },
    window: {
      setTimeout(fn) { timers.set(++timerID, fn); return timerID; },
      clearTimeout(id) { timers.delete(id); },
    },
    $: id => id === 'authQuotaList' ? list : null,
    sessionToken: 'management-token', base: 'https://example.test',
    token() { return h.sessionToken; }, apiBase() { return h.base; },
    api(...args) { return new Promise((resolve, reject) => calls.push({ args, resolve, reject })); },
    renderAuthQuotas(result) { h.state.authQuotas = result; },
  });
  const names = ['authQuotaValue', 'authQuotaItemKey', 'authQuotaQuery', 'authQuotaActiveCount', 'authQuotaActiveText', 'authQuotaActiveBadge', 'updateAuthQuotaCounts', 'cancelAuthQuotaCounts', 'pollAuthQuotaCounts', 'loadAuthQuotas'];
  const lines = source.split('\n');
  for (const name of names) {
    const start = lines.findIndex(line => new RegExp('^  (?:async )?function ' + name + '\\(').test(line));
    assert.notEqual(start, -1, name);
    let declaration = '', parsed;
    for (let i = start; i < lines.length; i++) {
      declaration += lines[i] + '\n';
      try { parsed = new vm.Script('(' + declaration + ')'); break; }
      catch (error) { if (!(error instanceof SyntaxError)) throw error; }
    }
    assert.ok(parsed, name);
    h[name] = parsed.runInContext(h);
  }
  return { h, calls, timers, items, badges, input };
}

const response = count => ({ items: [{ provider: 'codex', auth_id: 'same', active_requests: count }] });

test('poll updates badges and cached counts only; missing accounts stay stale, never zero', async () => {
  const { h, calls, items, badges, input } = harness();
  const pending = h.pollAuthQuotaCounts();
  assert.equal(calls[0].args[0], 'GET');
  assert.equal(calls[0].args[1], 'credit-manager/auth-quotas?page=1&page_size=12');
  calls[0].resolve(response(0));
  await pending;
  assert.equal(items[0].active_requests, 0);
  assert.equal(items[0].max_concurrent_requests, 8);
  assert.equal(badges[0].textContent, '0');
  assert.equal(badges[0].classList['is-active'], false);
  assert.equal(items[1].active_requests, 7);
  assert.equal(badges[1].textContent, '7 · 缓存过期');
  assert.equal(h.document.activeElement, input);
  assert.equal(input.value, '123');
  assert.equal(input.focused, true);
});

test('errors preserve counts with visible stale status; next successful poll recovers', async () => {
  const { h, calls, items, badges } = harness();
  let pending = h.pollAuthQuotaCounts();
  calls[0].reject(new Error('offline'));
  await pending;
  assert.equal(items[0].active_requests, 3);
  assert.equal(badges[0].textContent, '3 · 缓存过期');
  assert.match(h.authQuotaActiveBadge(items[0]), /3 · 缓存过期/);
  pending = h.pollAuthQuotaCounts();
  calls[1].resolve(response(2));
  await pending;
  assert.equal(badges[0].textContent, '2');
  assert.equal(badges[0].classList['is-active'], true);
  for (const value of [undefined, null, '', -1, 1.5, '3', Infinity]) {
    assert.equal(h.authQuotaActiveCount({ active_requests: value }), null);
    assert.equal(h.authQuotaActiveText({ active_requests: value }), '— · 缓存过期');
  }
});

test('poll skips hidden, other tabs, logged-out, unloaded and loading pages', async () => {
  for (const change of [
    h => { h.document.hidden = true; },
    h => { h.state.currentTab = 'keys'; },
    h => { h.sessionToken = ''; },
    h => { h.state.authQuotas = null; },
    h => { h.state.authQuotaLoading = 1; },
  ]) {
    const { h, calls } = harness();
    change(h);
    await h.pollAuthQuotaCounts();
    assert.equal(calls.length, 0);
  }
});

test('only one poll is in flight; timeout aborts and keeps stale counts', async () => {
  const { h, calls, timers, badges } = harness();
  const pending = h.pollAuthQuotaCounts();
  await h.pollAuthQuotaCounts();
  assert.equal(calls.length, 1);
  [...timers.values()][0]();
  assert.equal(calls[0].args[3].aborted, true);
  assert.equal(badges[0].textContent, '3 · 缓存过期');
  calls[0].resolve(response(99));
  await pending;
  assert.equal(badges[0].textContent, '3 · 缓存过期');
  assert.equal(timers.size, 0);
  assert.equal(h.state.authQuotaCountsRequest, null);
});

test('responses from old pages, queries, sessions, tabs and hidden periods are discarded', async () => {
  for (const change of [
    h => { h.state.tabLoadSeq++; },
    h => { h.state.authQuotaPage = 2; },
    h => { h.state.authQuotaProvider = 'other'; },
    h => { h.state.authQuotaName = 'search'; },
    h => { h.state.authQuotas = { items: [] }; },
    h => { h.state.currentTab = 'overview'; },
    h => { h.document.hidden = true; },
    h => { h.sessionToken = 'replacement'; },
    h => { h.base = 'https://other.test'; },
    h => { h.cancelAuthQuotaCounts(); },
  ]) {
    const { h, calls, badges } = harness();
    const pending = h.pollAuthQuotaCounts();
    change(h);
    calls[0].resolve(response(99));
    await pending;
    assert.equal(badges[0].textContent, '3');
  }
});

test('full page loads cancel polling and prevent new polls until settled', async () => {
  const { h, calls, badges } = harness();
  const poll = h.pollAuthQuotaCounts();
  const load = h.loadAuthQuotas();
  assert.equal(calls[0].args[3].aborted, true);
  await h.pollAuthQuotaCounts();
  assert.equal(calls.length, 2);
  calls[0].resolve(response(99));
  await poll;
  assert.equal(badges[0].textContent, '3');
  calls[1].resolve(response(5));
  await load;
  assert.equal(h.state.authQuotaLoading, 0);
  assert.equal(h.state.authQuotas.items[0].active_requests, 5);
});

test('visibility, navigation, session teardown, signal and five-second interval are wired', () => {
  assert.match(source, /document\.addEventListener\('visibilitychange', \(\) => \{\s*cancelAuthQuotaCounts\(\);\s*if \(!document\.hidden\) pollAuthQuotaCounts\(\);/);
  assert.match(source, /window\.setInterval\(pollAuthQuotaCounts, 5000\)/);
  assert.match(source, /cancelAuthQuotaCounts\(\);\s*state\.currentTab = tab;/);
  assert.match(source, /function clearLoadedSession\(\) \{\s*cancelAuthQuotaCounts\(\);/);
  assert.match(source, /async function api\(method, path, body, signal\)/);
  assert.match(source, /const opts = \{\s*method,\s*signal,/);
  assert.match(source, /authQuotaActiveBadge\(item\)\+'<\/span><input/);
});
