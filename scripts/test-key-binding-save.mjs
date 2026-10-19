import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import vm from 'node:vm';

const source = readFileSync(new URL('../internal/management/web/console.js', import.meta.url), 'utf8');
const functions = source.slice(source.indexOf('  function keyBindingDraftDirty()'), source.indexOf('  function setTab('));

function fixture(api, confirm = () => true) {
  const elements = new Map();
  const state = {
    keyBindingAccounts: [{ provider: 'codex', auth_id: 'A' }, { provider: 'codex', auth_id: 'B' }],
    keyBindingAccountID: 'A', keyBindingAdded: new Set(['key-a']), keyBindingRemoved: new Set(),
    keyBindingLeftChecked: new Set(['key-b']), keyBindingRightChecked: new Set(['key-a']), keyBindingSaving: false,
  };
  const context = vm.createContext({
    state, api, window: { confirm }, flash() {},
    bindingKey: (provider, id) => `${provider}:${id}`,
    $: id => {
      if (!elements.has(id)) elements.set(id, {});
      return elements.get(id);
    },
  });
  vm.runInContext(functions + '\nrenderKeyBinding = () => updateKeyBindingActions(); renderKeyBindingAccountOptions = () => {}; loadKeyBindingData = async () => { state.bindingReloads = (state.bindingReloads || 0) + 1; };', context);
  return { context, state, elements };
}

const freshKeys = { items: [
  { id: 'key-a', auth_bindings: [{ provider: 'codex', auth_id: 'other' }] },
  { id: 'key-b', auth_bindings: [] },
], total_pages: 1 };

test('save locks before awaiting, prevents account switching/double save, and snapshots its draft', async () => {
  let release;
  let reads = 0;
  const writes = [];
  const f = fixture(async (method, path, data) => {
    if (method === 'GET') { reads++; return await new Promise(resolve => { release = resolve; }); }
    writes.push(JSON.parse(JSON.stringify(data)));
  });
  const pending = f.context.keyBindingSave();
  assert.equal(f.state.keyBindingSaving, true);
  assert.equal(f.elements.get('keyBindingAccount').disabled, true);
  f.context.keyBindingChangeAccount('B');
  assert.equal(f.state.keyBindingAccountID, 'A');
  f.context.keyBindingMoveRight();
  f.context.keyBindingMoveLeft();
  assert.deepEqual([...f.state.keyBindingAdded], ['key-a']);
  await f.context.keyBindingSave();
  assert.equal(reads, 1);
  // Even if some other async refresh mutates shared state, this save owns its original draft.
  f.state.keyBindingAdded = new Set(['key-b']);
  release(freshKeys);
  await pending;
  assert.deepEqual(writes, [{ id: 'key-a', auth_bindings: [
    { provider: 'codex', auth_id: 'other' }, { provider: 'codex', auth_id: 'A' },
  ] }]);
  assert.equal(f.state.keyBindingSaving, false);
  assert.equal(f.elements.get('keyBindingAccount').disabled, false);
});

test('failed refresh unlocks and preserves the draft for retry', async () => {
  const f = fixture(async () => { throw new Error('refresh failed'); });
  await assert.rejects(f.context.keyBindingSave(), /refresh failed/);
  assert.equal(f.state.keyBindingSaving, false);
  assert.equal(f.elements.get('btnKeyBindingSave').disabled, false);
  assert.deepEqual([...f.state.keyBindingAdded], ['key-a']);
});

test('cancelled permission warning unlocks without writing', async () => {
  let writes = 0;
  const f = fixture(async method => {
    if (method === 'GET') return { items: [{ id: 'key-a', auth_bindings: [] }] };
    writes++;
  }, () => false);
  await f.context.keyBindingSave();
  assert.equal(writes, 0);
  assert.equal(f.state.keyBindingSaving, false);
  assert.equal(f.state.keyBindingAdded.size, 1);
});

test('save skips a key deleted after the draft was prepared', async () => {
  let writes = 0;
  const f = fixture(async (method, path) => {
    if (method === 'GET') {
      assert.equal(new URL(path, 'https://example.invalid/').searchParams.get('active_only'), '1');
      return { items: [{ id: 'key-a', revoked_at: '2026-10-19', auth_bindings: [] }], total_pages: 1 };
    }
    writes++;
  });
  await f.context.keyBindingSave();
  assert.equal(writes, 0);
  assert.equal(f.state.keyBindingAdded.size, 0);
  assert.equal(f.state.bindingReloads, 1);
  assert.equal(f.state.keyBindingSaving, false);
});

test('failed update releases the UI lock', async () => {
  const f = fixture(async method => {
    if (method === 'GET') return freshKeys;
    throw new Error('update failed');
  });
  await f.context.keyBindingSave();
  assert.equal(f.state.keyBindingSaving, false);
  assert.equal(f.elements.get('keyBindingAccount').disabled, false);
});
