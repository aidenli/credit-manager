'use strict';

// Run with node --test internal/management/frontend_usage_cost_test.js.
// Exercise the actual browser helpers without a DOM or new dependencies.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const test = require('node:test');

const source = name => fs.readFileSync(path.join(__dirname, 'web', name), 'utf8');
function helpers(script, names, globals = {}) {
  const context = vm.createContext({
    t: text => text,
    formatMoney: value => '$' + (Number(value || 0) / 1e6).toFixed(6),
    usd: value => '$' + (Number(value || 0) / 1e6).toFixed(6),
    getCurrency: () => 'USD', getUsdCnyRate: () => 7,
    currency: 'USD', modelMetric: 'cost',
    ...globals,
  });
  const lines = script.split('\n');
  for (const name of names) {
    const start = lines.findIndex(line => line.startsWith('  function ' + name + '('));
    assert.notEqual(start, -1, 'Missing helper ' + name);
    let declaration = '';
    let parsed;
    for (let i = start; i < lines.length; i++) {
      declaration += lines[i] + '\n';
      try {
        parsed = new vm.Script('(' + declaration + ')');
        break;
      } catch (error) {
        if (!(error instanceof SyntaxError)) throw error;
      }
    }
    assert.ok(parsed, 'Invalid helper ' + name);
    context[name] = parsed.runInContext(context);
  }
  return context;
}

const records = [
  { model: 'paid', total_tokens: 100, usage_cost_micro_usd: 1250000, usage_cost_status: 'priced', cost_micro_usd: 0, estimated_cost_micro_usd: 9000000, statistics_only: true, price_known: true },
  { model: 'Magpie', total_tokens: 200, usage_cost_micro_usd: 0, usage_cost_status: 'free', cost_micro_usd: 7000000 },
  { model: 'unknown', total_tokens: 300, usage_cost_micro_usd: 0, usage_cost_status: 'unpriced', cost_micro_usd: 8000000 },
  { model: 'historical', total_tokens: 400, usage_cost_micro_usd: 500000, usage_cost_status: 'settled', cost_micro_usd: 500000 },
].map(item => ({ ...item, created_at: '2026-11-08T12:00:00Z' }));

for (const filename of ['console.js', 'lookup.js']) {
  test(filename + ': detail usage, free, unpriced and historical costs stay distinct', () => {
    const h = helpers(source(filename), ['usageCostText', 'usageCostTitle', 'usageUnpricedText', ...(filename === 'console.js' ? ['usageCost'] : [])]);
    assert.equal(h.usageCostText(records[0]), '$1.250000');
    assert.equal(h.usageCostText(records[1]), '免费 · $0.000000');
    assert.equal(h.usageCostText(records[2]), '未配置价格');
    assert.equal(h.usageCostText(records[3]), '$0.500000');
    assert.equal(h.usageCostText({ cost_micro_usd: 99000000 }), '未配置价格');
    assert.match(h.usageCostTitle(records[0]), /实际扣费 \$0\.000000/);
    assert.match(h.usageCostTitle(records[0]), /参考费用（不扣费） \$9\.000000/);
    assert.match(h.usageUnpricedText(2), /未配置价格: 2/);
    assert.equal(h.usageUnpricedText(0), '');
  });

  test(filename + ': unpriced models cannot rank as free', () => {
    const h = helpers(source(filename), ['displaySpend', 'modelRankSpec']);
    const unknown = { requests: 1, tokens: 100, cost: 0, unpriced: 1 };
    const free = { ...unknown, unpriced: 0 };
    for (const metric of ['value', 'unit']) {
      assert.equal(h.modelRankSpec(metric).ready(unknown), false);
      assert.equal(h.modelRankSpec(metric).ready(free), true);
    }
    assert.equal(h.modelRankSpec('value').score(free), Infinity);
    assert.equal(h.modelRankSpec('value').format(Infinity), '免费');
  });
}

test('console: all model, trend and summary aggregation uses usage cost, not deductions', () => {
  const h = helpers(source('console.js'), ['usageCost', 'usageTokens', 'usageCacheRelated', 'overviewModelUsage', 'overviewTrendBucket', 'overviewTrendSeries', 'normKeySum', 'normModelSum']);
  const models = h.overviewModelUsage(records);
  assert.equal(models.reduce((sum, item) => sum + item.cost, 0), 1750000);
  assert.equal(models.find(item => item.model === 'unknown').unpriced, 1);
  assert.equal(models.find(item => item.model === 'Magpie').cost, 0);
  for (const grain of ['hour', 'day', 'month']) {
    assert.equal(h.overviewTrendSeries(records, grain).reduce((sum, item) => sum + item.cost, 0), 1750000);
  }
  for (const name of ['normKeySum', 'normModelSum']) {
    assert.equal(h[name]({ usage_cost_micro_usd: 0, cost_micro_usd: 999, unpriced_count: 2 }).cost, 0);
    assert.equal(h[name]({ UsageCostMicroUSD: 12, CostMicroUSD: 999, UnpricedCount: 2 }).cost, 12);
    assert.equal(h[name]({ usage_cost_micro_usd: 12, unpriced_count: 2 }).unpriced, 2);
  }
});

test('lookup: model shares, model ranking and trend buckets use usage cost', () => {
  const h = helpers(source('lookup.js'), ['lookupModelUsage', 'shareMetricValue', 'bucketTrend'], { trendLabel: label => label });
  const summaries = records.map(item => ({ ...item, request_count: 1, unpriced_count: item.usage_cost_status === 'unpriced' ? 1 : 0, date: item.created_at }));
  assert.equal(h.lookupModelUsage(summaries).reduce((sum, item) => sum + item.cost, 0), 1750000);
  assert.equal(summaries.reduce((sum, item) => sum + h.shareMetricValue(item), 0), 1750000);
  for (const grain of ['hour', 'day', 'month']) {
    assert.equal(h.bucketTrend(summaries, grain).reduce((sum, item) => sum + item.usage_cost_micro_usd, 0), 1750000);
  }
});

test('report wiring changes do not replace financial quota and key spend fields', () => {
  const consoleJS = source('console.js'), lookupJS = source('lookup.js');
  assert.match(consoleJS, /items\.reduce\(\(sum, item\) => sum \+ usageCost\(item\), 0\)/);
  assert.match(consoleJS, /min_cost_micro_usd: toMicro\('usageMinCost'\)/);
  assert.match(consoleJS, /max_cost_micro_usd: toMicro\('usageMaxCost'\)/);
  assert.match(lookupJS, /usd\(summary\.usage_cost_micro_usd\)/);
  assert.match(lookupJS, /key:'usage_cost_micro_usd',name:t\('用量费用'\)/);
  assert.match(lookupJS, /Number\(key\.settled_spend_micro_usd\|\|0\)\+Number\(key\.held_amount_micro_usd\|\|0\)/);
  assert.match(lookupJS, /quota\('日额度',overview\.daily_micro_usd/);
  assert.match(lookupJS, /quota\('周额度',overview\.weekly_micro_usd/);
  assert.match(lookupJS, /quota\('月额度',overview\.monthly_micro_usd/);
  assert.doesNotMatch(lookupJS, /usd\(summary\.cost_micro_usd\)/);
  for (const filename of ['console.html', 'lookup.html']) {
    const html = source(filename);
    assert.match(html, /用量费用按实际 Token 与已配置价格计算/);
    assert.match(html, /不扣减余额或更新密钥已用额度/);
    assert.match(html, /Magpie 免费（0）/);
    assert.match(html, /历史已结算账本保持不变/);
    assert.match(html, /用量费用趋势/);
  }
  assert.match(source('lookup.html'), /id="usagePriceStatus"/);
});
