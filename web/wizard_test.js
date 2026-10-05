const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');

function loadWizard(options = {}) {
  const html = fs.readFileSync(require.resolve('./wizard.html'), 'utf8');
  const source = html.slice(html.indexOf('<script>') + 8, html.indexOf('</script>')).replace(/\nloadData\(\)\.then\(loadServerBalances\);\s*$/, '\n');
  const elements = new Map();
  function element(id) {
    if (!elements.has(id)) elements.set(id, {id, value:'', textContent:'', innerHTML:'', style:{display:''}, hidden:false, className:'', classList:{add(){}, remove(){}, toggle(){}}, disabled:false, tabIndex:0, dataset:{}, setAttribute(){}, getAttribute(){return null;}, querySelector(){ return null; } });
    return elements.get(id);
  }
  const document = {
    getElementById: element,
    querySelectorAll(){ return []; },
    querySelector(){ return null; },
    createElement(){ return element('created'); }
  };
  const context = {console, document, window:{}, setTimeout, clearTimeout, Promise, URL, encodeURIComponent, decodeURIComponent, isFinite, Number, String, Math, Date, JSON};
  context.fetch = options.fetch || (() => Promise.reject(new Error('network disabled')));
  vm.runInNewContext(source, context, {filename:'wizard.html'});
  return {context, elements, element};
}

test('history chart aggregates filtered server samples by day and hour', () => {
  const {context, element} = loadWizard();
  context.DATA = {providers:[], credentials:[{provider:'deepseek',profile_key:'a',label:'A'}], config:{}};
  context.serverHistory = [
    {observed_at:'2026-09-28T08:10:00Z',day:'2026-09-28',account_key:'a',provider:'deepseek',kind:'balance',currency:'USD',balance:10,used:2,has_balance:true,has_used:true},
    {observed_at:'2026-09-28T09:10:00Z',day:'2026-09-28',account_key:'a',provider:'deepseek',kind:'balance',currency:'USD',balance:9,used:3,has_balance:true,has_used:true},
    {observed_at:'2026-09-29T09:10:00Z',day:'2026-09-29',account_key:'a',provider:'deepseek',kind:'balance',currency:'USD',balance:8,used:4,has_balance:true,has_used:true}
  ];
  element('historyMetric').value = 'balance';
  element('historyGranularity').value = 'day';
  context.renderHistoryChart();
  assert.match(element('historyChart').innerHTML, /2026-09/);
  assert.match(element('historyChart').innerHTML, /<svg/);
  element('historyGranularity').value = 'hour';
  element('historyMetric').value = 'used';
  context.renderHistoryChart();
  assert.match(element('historyChartLegend').textContent, /用量/);
  assert.match(element('historyChartNote').textContent, /小时/);
});

test('history chart shows a clear empty state without numeric samples', () => {
  const {context, element} = loadWizard();
  context.serverHistory = [{observed_at:'2026-09-28T08:00:00Z',day:'2026-09-28',account_key:'a',kind:'quota',remaining:50}];
  element('historyMetric').value = 'balance';
  context.renderHistoryChart();
  assert.match(element('historyChart').innerHTML, /没有可绘制的余额数据/);
});
test('server history failure does not render browser snapshots', async () => {
  const {context, element} = loadWizard({fetch: async () => { throw new Error('offline'); }});
  const result = await context.loadServerHistory();
  assert.equal(result.length, 0);
  assert.equal(context.serverHistory.length, 0);
  assert.match(element('dailyRows').innerHTML, /服务器历史读取失败/);
  assert.doesNotMatch(element('dailyRows').innerHTML, /本机兼容快照/);
});

test('server history hydrates the homepage without querying suppliers', async () => {
  let calls = [];
  const {context, element} = loadWizard({fetch: async url => {
    calls.push(url);
    return {ok:true, json:async () => ({items:[{observed_at:'2026-09-28T12:00:00Z',day:'2026-09-28',account_key:'deepseek@a',provider:'deepseek',currency:'USD',kind:'balance',balance:8,used:125,limit:90,has_balance:true,has_used:true,has_limit:true,source:'manual'}]})};
  }});
  context.DATA = {providers:[{provider:'deepseek',status:'ok'}], credentials:[{provider:'deepseek',profile_key:'deepseek@a',status:'ok'}], config:{}};
  context.displaySel = {'deepseek@a':true};
  context.renderSelectedBalances();
  await context.loadServerBalances();
  assert.equal(calls.length, 1);
  assert.match(calls[0], /usage-latest/);
  assert.doesNotMatch(calls[0], /config-wizard\?balance/);
  assert.match(element('selected-bal-deepseek%40a').innerHTML, /USD 8/);
});

test('browser storage APIs are not referenced by the wizard', () => {
  const html = fs.readFileSync(require.resolve('./wizard.html'), 'utf8');
  assert.doesNotMatch(html, /localStorage|sessionStorage|storageGet|storageSet|storageRemove/);
});

test('in-flight requests are deduplicated and failures are cached', async () => {
  let calls = 0;
  let resolveFetch;
  const h = loadWizard({fetch: () => { calls++; return new Promise(resolve => { resolveFetch = resolve; }); }});
  const {context} = h;
  context.DATA = {providers:[{provider:'deepseek',status:'ok'}], credentials:[{provider:'deepseek',profile_key:'deepseek@a',status:'ok'}], config:{}};
  const first = context.fetchBalance('deepseek@a');
  const second = context.fetchBalance('deepseek@a');
  assert.equal(first, second);
  assert.equal(calls, 1);
  resolveFetch({ok:false, json:async () => ({message:'mock failure'})});
  const result = await first;
  assert.equal(result.ok, false);
  assert.equal(context.balanceCache['deepseek@a'].message, 'mock failure');
});

test('account configuration does not auto-refresh and only refreshes manually', async () => {
  let calls = 0;
  const {context, element} = loadWizard({fetch: async url => {
    calls++;
    assert.match(url, /config-data/);
    return {ok:true, json:async () => ({providers:[], credentials:[], config:{}})};
  }});
  context.showView('accounts', element('accountsTab'));
  assert.equal(calls, 0);
  const button = element('refreshBtn');
  button.textContent = '手动刷新账号配置';
  await context.refreshAccountConfig(button);
  assert.equal(calls, 1);
  assert.equal(button.disabled, false);
});

test('home view shows selected balances and hides account configuration', () => {
  const {context, element} = loadWizard();
  context.selected = {a:true};
  context.showView('home', element('homeTab'));
  assert.equal(element('view-home').style.display, 'block');
  assert.equal(element('accountPanel').style.display, 'none');
  assert.equal(element('cfgCard').style.display, 'none');
  context.showView('accounts', element('accountsTab'));
  assert.equal(element('view-home').style.display, 'none');
  assert.equal(element('accountPanel').style.display, 'block');
  assert.equal(element('cfgCard').style.display, 'block');
});
test('selected account table ignores unselected accounts and escapes labels', () => {
  const {context, element} = loadWizard();
  context.DATA = {providers:[], credentials:[
    {provider:'deepseek', profile_key:'a', label:'<account>', base_url:'https://a.test'},
    {provider:'moonshot', profile_key:'b', label:'hidden'}
  ], config:{}};
  context.displaySel = {a:true};
  context.balanceCache.a = {ok:true, balance:12.5, currency:'CNY'};
  context.renderSelectedBalances();
  assert.match(element('selectedRows').innerHTML, /&lt;account&gt;/);
  assert.match(element('selectedRows').innerHTML, /deepseek/);
  assert.match(element('selectedRows').innerHTML, /https:\/\/a\.test/);
  assert.doesNotMatch(element('selectedRows').innerHTML, /hidden/);
  assert.match(element('selected-bal-a').innerHTML, /CNY 12.5/);
  assert.equal(element('selected-bal-a').innerHTML, element('bal-a').innerHTML);
  context.toggleDisplay('a', false);
  assert.match(element('selectedRows').innerHTML, /尚未选择账号/);
});

test('selected balance page does not auto-refresh on entry', () => {
  let calls = 0;
  loadWizard({fetch: async () => { calls++; throw new Error('unexpected automatic query'); }});
  assert.equal(calls, 0);
});

test('selected account refresh queries only the clicked account', async () => {
  const calls = [];
  const {context, element} = loadWizard({fetch: async url => {
    calls.push(url);
    return {ok:true, json:async () => ({ok:true, balance:9, currency:'USD'})};
  }});
  context.DATA = {providers:[], credentials:[
    {provider:'deepseek', profile_key:'a', label:'Account A'},
    {provider:'moonshot', profile_key:'b', label:'Account B'}
  ], config:{}};
  context.displaySel = {a:true, b:true};
  context.renderSelectedBalances();
  assert.match(element('selectedRows').innerHTML, /fetchSingleBalance/);
  const button = element('selectedRefreshButton');
  button.textContent = '刷新';
  const result = await context.fetchSingleBalance('a', button);
  assert.equal(result.ok, true);
  assert.equal(calls.length, 1);
  assert.match(calls[0], /credential_key=a/);
  assert.equal(context.balanceCache.a.balance, 9);
  assert.equal(context.balanceCache.b, undefined);
  assert.equal(button.disabled, false);
});


test('history summary calculates usage, possible recharge, balance change, and quota consumption', () => {
  const {context, element} = loadWizard();
  context.serverHistory = [
    {observed_at:'2026-09-28T09:00:00Z',day:'2026-09-28',account_key:'cash',provider:'relay',currency:'CNY',kind:'balance',used:100,limit:100,balance:10},
    {observed_at:'2026-09-28T12:00:00Z',day:'2026-09-28',account_key:'cash',provider:'relay',currency:'CNY',kind:'balance',used:130,limit:120,balance:7},
    {observed_at:'2026-09-28T18:00:00Z',day:'2026-09-28',account_key:'cash',provider:'relay',currency:'CNY',kind:'balance',used:150,limit:120,balance:5},
    {observed_at:'2026-09-28T09:00:00Z',day:'2026-09-28',account_key:'quota',provider:'glm',currency:'积分',kind:'quota',window:'5 小时',remaining:100},
    {observed_at:'2026-09-28T12:00:00Z',day:'2026-09-28',account_key:'quota',provider:'glm',currency:'积分',kind:'quota',window:'5 小时',remaining:70}
  ];
  context.historyRange = 'today';
  element('dailyFrom').value = '2026-09-28';
  element('dailyTo').value = '2026-09-28';
  context.renderHistorySummary();
  assert.equal(element('historyUsage').textContent, 'CNY 50');
  assert.equal(element('historyRecharge').textContent, 'CNY 20');
  assert.equal(element('historyBalanceChange').textContent, 'CNY -5');
  assert.equal(element('historyQuotaUsed').textContent, '积分 30');
  assert.match(element('historySummaryBreakdown').innerHTML, /总额度增加 CNY 20/);
  assert.match(element('historySummaryBreakdown').innerHTML, /配额消耗 积分 30/);
});


test('history summary ignores implausible limit jumps as recharge', () => {
  const {context, element} = loadWizard();
  context.serverHistory = [
    {observed_at:'2026-09-28T09:00:00Z',day:'2026-09-28',account_key:'cash',provider:'relay',currency:'CNY',kind:'balance',used:10,limit:100,balance:90},
    {observed_at:'2026-09-28T12:00:00Z',day:'2026-09-28',account_key:'cash',provider:'relay',currency:'CNY',kind:'balance',used:20,limit:100000000,balance:80}
  ];
  context.historyRange = 'today';
  element('dailyFrom').value = '2026-09-28';
  element('dailyTo').value = '2026-09-28';
  context.renderHistorySummary();
  assert.equal(element('historyRecharge').textContent, 'CNY 0');
  assert.match(element('historySummaryBreakdown').innerHTML, /异常总额度跳变/);
});
test('history summary uses filtered first and last values only', () => {
  const {context, element} = loadWizard();
  context.serverHistory = [
    {observed_at:'2026-09-28T09:00:00Z',day:'2026-09-28',account_key:'cash-a',provider:'relay',currency:'CNY',kind:'balance',used:100,limit:100,balance:10},
    {observed_at:'2026-09-28T12:00:00Z',day:'2026-09-28',account_key:'cash-a',provider:'relay',currency:'CNY',kind:'balance',used:110,limit:99999835,balance:9},
    {observed_at:'2026-09-28T18:00:00Z',day:'2026-09-28',account_key:'cash-a',provider:'relay',currency:'CNY',kind:'balance',used:120,limit:100.0002,balance:8},
    {observed_at:'2026-09-28T09:00:00Z',day:'2026-09-28',account_key:'cash-b',provider:'other',currency:'CNY',kind:'balance',used:0,limit:50,balance:20},
    {observed_at:'2026-09-28T18:00:00Z',day:'2026-09-28',account_key:'cash-b',provider:'other',currency:'CNY',kind:'balance',used:5,limit:70,balance:15}
  ];
  context.historyRange = 'today';
  element('dailyFrom').value = '2026-09-28';
  element('dailyTo').value = '2026-09-28';
  element('dailyAccount').value = 'cash-a';
  context.renderHistorySummary();
  assert.equal(element('historySummaryRange').textContent, 'today · 2026-09-28 至 2026-09-28 · 3 条采样 / 1 个账号');
  assert.equal(element('historyUsage').textContent, 'CNY 20');
  assert.equal(element('historyRecharge').textContent, 'CNY 0.0002');
  assert.equal(element('historyBalanceChange').textContent, 'CNY -2');
  assert.doesNotMatch(element('historySummaryBreakdown').innerHTML, /99,998/);

  element('dailyAccount').value = 'cash-b';
  context.renderHistorySummary();
  assert.equal(element('historySummaryRange').textContent, 'today · 2026-09-28 至 2026-09-28 · 2 条采样 / 1 个账号');
  assert.equal(element('historyUsage').textContent, 'CNY 5');
  assert.equal(element('historyRecharge').textContent, 'CNY 20');
});
test('configuration opens above account list and focuses the selected form', () => {
  const html = fs.readFileSync(require.resolve('./wizard.html'), 'utf8');
  assert.ok(html.indexOf('id="cfgCard"') < html.indexOf('id="accountPanel"'));
  const {context, element} = loadWizard();
  let scrolled = false;
  element('f-a').scrollIntoView = () => { scrolled = true; };
  context.renderForms = () => {};
  context.togglePick('a', true);
  assert.equal(context.selected.a, true);
  assert.equal(scrolled, true);
});

test('GLM quota is described without showing a zero cash balance', () => {
  const {context, element} = loadWizard();
  context.DATA = {providers:[], credentials:[{provider:'openai-compatible-glm',profile_key:'glm-a'}], config:{}};
  context.displaySel = {'glm-a':true};
  context.balanceCache['glm-a'] = {ok:true,has_balance:false,balance:0,description:'5 小时额度 剩余 75% · 每周额度 剩余 90%'};
  context.renderSelectedBalances();
  assert.match(element('selected-bal-glm-a').innerHTML, /5 小时额度 剩余 75%/);
  assert.doesNotMatch(element('selected-bal-glm-a').innerHTML, /余额 0/);
});

test('manual refresh queries only existing selected accounts and bypasses cached balances', async () => {
  const calls = [];
  const {context, element} = loadWizard({fetch: async url => {
    calls.push(url);
    return {ok:true, json:async () => ({ok:true, balance:9, currency:'USD'})};
  }});
  context.DATA = {providers:[], credentials:[
    {provider:'deepseek', profile_key:'a'}, {provider:'moonshot', profile_key:'b'}
  ], config:{}};
  context.displaySel = {a:true, deleted:true};
  context.balanceCache.a = {ok:true, balance:1, currency:'USD'};
  const button = element('selectedBalancesBtn');
  button.textContent = '刷新余额';
  const pending = context.fetchSelectedBalances(button);
  assert.equal(button.disabled, true);
  assert.match(element('selected-bal-a').innerHTML, /查询中/);
  await pending;
  assert.equal(calls.length, 1);
  assert.match(calls[0], /credential_key=a/);
  assert.equal(context.balanceCache.a.balance, 9);
  assert.equal(button.disabled, false);
  assert.equal(button.textContent, '刷新余额');
  assert.match(element('selected-bal-a').innerHTML, /USD 9/);
});
test('manual refresh reports per-account failure and handles empty selection', async () => {
  const {context, element} = loadWizard({fetch: async () => {
    throw new Error('offline');
  }});
  context.DATA = {providers:[], credentials:[{provider:'deepseek', profile_key:'a'}], config:{}};
  const button = element('selectedBalancesBtn');
  button.textContent = '刷新余额';
  await context.fetchSelectedBalances(button);
  assert.match(element('globalMsg').textContent, /尚未选择账号/);
  context.displaySel = {a:true};
  await context.fetchSelectedBalances(button);
  assert.equal(button.disabled, false);
  assert.equal(element('globalMsg').className, 'err');
  assert.match(element('selected-bal-a').innerHTML, /offline/);
});

test('server history filters build query parameters and expose scoped deletion', () => {
  const {context, element} = loadWizard();
  context.historyRange = 'custom';
  element('dailyFrom').value = '2026-09-01';
  element('dailyTo').value = '2026-09-07';
  element('dailyAccountSelect').value = 'account-a';
  element('dailyProvider').value = 'relay';
  element('dailySource').value = 'scheduled';
  element('dailyCurrency').value = 'CNY';
  const query = context.historyQuery(true);
  assert.match(query, /from=2026-09-01/);
  assert.match(query, /to=2026-09-07/);
  assert.match(query, /key=account-a/);
  assert.match(query, /provider=relay/);
  assert.match(query, /source=scheduled/);
  assert.match(query, /currency=CNY/);
  const html = fs.readFileSync(require.resolve('./wizard.html'), 'utf8');
  assert.match(html, /删除当前筛选历史/);
  assert.match(html, /删除全部服务器历史/);
  assert.match(html, /clearServerHistory\(true\)/);
  assert.match(html, /clearServerHistory\(false\)/);
  assert.match(html, /action=clear/);
  assert.match(html, /method:"DELETE",cache:"no-store"/);
  assert.ok(html.indexOf('<caption class="sr-only">已选择账号的服务器余额快照</caption>') >= 0);
  assert.ok(html.indexOf('<th scope="col">站点地址</th><th scope="col">余额 / 用量</th>') >= 0);
  assert.ok(html.indexOf('<th>日期</th><th>站点地址</th><th>类型</th>') >= 0);
  assert.ok(html.indexOf('id="dailyProvider"') < html.indexOf('id="dailyAccountSelect"'));
});

test('server history rows lead with provider and separate account and site', () => {
  const {context, element} = loadWizard();
  context.DATA = {providers:[], credentials:[{
    provider:'codex', profile_key:'codex-a', label:'codex API-Key', base_url:'https://kuaipao.ai/v1'
  }], config:{}};
  context.serverHistory = [{
    observed_at:'2026-10-01T09:00:00Z', day:'2026-10-01', account_key:'codex-a',
    provider:'codex', currency:'CNY', kind:'balance', balance:10, source:'manual'
  }];
  context.renderServerHistory();
  const html = element('dailyRows').innerHTML;
  assert.match(html, /<th>日期<\/th><th>站点地址<\/th><th>类型<\/th>/);
  assert.match(html, /<b>https:\/\/kuaipao\.ai\/v1<\/b><div class='tip'>codex \| codex API-Key<\/div>/);
  assert.doesNotMatch(html, /<th>供应商<\/th><th>账号<\/th>/);
});

test('usage history range buttons set the expected date bounds', () => {
  const {context, element} = loadWizard();
  const today = context.localDay(new Date());
  context.setHistoryRange('today');
  assert.equal(element('dailyFrom').value, today);
  assert.equal(element('dailyTo').value, today);

  context.setHistoryRange('yesterday');
  const yesterday = context.localDay(new Date(Date.now() - 86400000));
  assert.equal(element('dailyFrom').value, yesterday);
  assert.equal(element('dailyTo').value, yesterday);

  context.setHistoryRange('all');
  assert.equal(element('dailyFrom').value, '');
  assert.equal(element('dailyTo').value, '');

  context.setHistoryRange('custom');
  assert.notEqual(element('dailyFrom').value, '');
  assert.notEqual(element('dailyTo').value, '');
});
