const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');

function loadWizard(options = {}) {
  const html = fs.readFileSync(require.resolve('./wizard.html'), 'utf8');
  const source = html.slice(html.indexOf('<script>') + 8, html.indexOf('</script>')).replace(/\nloadData\(\);\s*$/, '\n');
  const values = new Map();
  const elements = new Map();
  function element(id) {
    if (!elements.has(id)) elements.set(id, {id, value:'', textContent:'', innerHTML:'', style:{display:''}, className:'', classList:{add(){}, remove(){}}, disabled:false, dataset:{}, querySelector(){ return null; }});
    return elements.get(id);
  }
  const localStorage = options.throwStorage ? {
    getItem(){ throw new Error('storage disabled'); }, setItem(){ throw new Error('storage disabled'); }, removeItem(){ throw new Error('storage disabled'); }
  } : {
    getItem(k){ return values.has(k) ? values.get(k) : null; }, setItem(k,v){ values.set(k,String(v)); }, removeItem(k){ values.delete(k); }
  };
  const document = {
    getElementById: element,
    querySelectorAll(){ return []; },
    createElement(){ return element('created'); }
  };
  const context = {console, document, window:{localStorage}, localStorage, setTimeout, clearTimeout, Promise, URL, encodeURIComponent, decodeURIComponent, isFinite, Number, String, Math, Date, JSON};
  context.fetch = options.fetch || (() => Promise.reject(new Error('network disabled')));
  vm.runInNewContext(source, context, {filename:'wizard.html'});
  return {context, values, elements, element};
}

test('snapshot is one first/last record per account, day, and currency', () => {
  const {context, values} = loadWizard();
  context.recordSnapshot('deepseek@a', {ok:true, balance:10, used:100, limit:100, has_limit:true, currency:'USD'});
  context.recordSnapshot('deepseek@a', {ok:true, balance:8, used:125, limit:90, has_limit:true, currency:'USD'});
  context.recordSnapshot('deepseek@a', {ok:true, balance:8, used:125, currency:'CNY'});
  const records = JSON.parse(values.get('api-balance-daily-v2'));
  assert.equal(records.length, 2);
  const usd = records.find(x => x.currency === 'USD');
  assert.equal(usd.firstBalance, 10);
  assert.equal(usd.lastBalance, 8);
  assert.equal(usd.firstLimit, 100);
  assert.equal(usd.lastLimit, 90);
  assert.equal(context.snapshotUsedDelta(usd), 25);
  assert.equal(context.snapshotUsedDelta({...usd, lastUsed: 2}), null);
});

test('GLM quota snapshots retain first and last values per window', () => {
  const {context, element} = loadWizard();
  context.DATA = {providers:[], credentials:[{provider:'openai-compatible-glm',profile_key:'glm-a',label:'GLM A'}], config:{}};
  const quota = (fiveHour, weekly) => ({ok:true,has_balance:false,quota_windows:[
    {window:'5 小时窗口',remaining:fiveHour,remaining_fraction:fiveHour/100,reset_time:'2026-09-28T00:00:00Z'},
    {window:'周配额',remaining:weekly,remaining_fraction:weekly/1000}
  ]});
  context.recordSnapshot('glm-a', quota(50, 700));
  context.recordSnapshot('glm-a', quota(40, 650));
  const records = context.snapshotItems();
  assert.equal(records.length, 2);
  const five = records.find(x => x.window === '5 小时窗口');
  assert.equal(five.kind, 'quota');
  assert.equal(five.firstRemaining, 50);
  assert.equal(five.lastRemaining, 40);
  assert.equal(context.snapshotRemainingDelta(five), -10);
  assert.equal(five.lastResetTime, '2026-09-28T00:00:00Z');
  context.renderDailySnapshots();
  assert.match(element('dailyRows').innerHTML, /GLM 配额/);
  assert.match(element('dailyRows').innerHTML, /5 小时窗口/);
  assert.match(element('dailyRows').innerHTML, /周配额/);
  assert.doesNotMatch(element('dailyRows').innerHTML, /余额 0/);
});

test('legacy balance snapshots remain separate from quota windows', () => {
  const {context, values, element} = loadWizard();
  context.DATA = {providers:[], credentials:[{provider:'openai-compatible-glm',profile_key:'a',label:'A'}], config:{}};
  context.recordSnapshot('a', {ok:true,balance:12,currency:'CNY',used:1});
  const legacy = JSON.parse(values.get('api-balance-daily-v2'));
  delete legacy[0].kind;
  values.set('api-balance-daily-v2', JSON.stringify(legacy));
  context.recordSnapshot('a', {ok:true,has_balance:false,quota_windows:[{window:'5 小时窗口',remaining:200,remaining_fraction:0.5}]});
  const rows = context.snapshotItems();
  assert.equal(rows.length, 2);
  assert.equal(rows[0].firstBalance, 12);
  assert.equal(rows[1].firstRemaining, 200);
  context.renderDailySnapshots();
  assert.match(element('dailyRows').innerHTML, /现金余额/);
  assert.match(element('dailyRows').innerHTML, /GLM 配额/);
  assert.equal(context.snapshotRemainingDelta({...rows[1], lastRemaining:null}), null);
});
test('missing currency defaults to CNY across balance rendering and snapshots', () => {
  const {context, element} = loadWizard();
  context.DATA = {providers:[], credentials:[{provider:'relay',profile_key:'relay',label:'Relay'}], config:{}};
  context.displaySel = {relay:true};
  context.balanceCache.relay = {ok:true,balance:16.5336,limit:62.4968,used:45.9632,has_limit:true,has_used:true};
  context.renderSelectedBalances();
  assert.match(element('selected-bal-relay').innerHTML, /CNY 16\.5336/);
  assert.match(element('selected-bal-relay').innerHTML, /CNY 62\.4968/);
  assert.match(element('selected-bal-relay').innerHTML, /CNY 45\.9632/);
  context.recordSnapshot('relay', {ok:true,balance:16.5336,currency:''});
  context.renderDailySnapshots();
  assert.match(element('dailyRows').innerHTML, /CNY/);
});

test('daily cash snapshot separates current balance from change and labels missing currency', () => {
  const {context, element} = loadWizard();
  context.DATA = {providers:[], credentials:[
    {provider:'deepseek',profile_key:'usd',label:'USD account',base_url:'https://usd.test'},
    {provider:'relay',profile_key:'unknown',label:'Relay account',base_url:'https://relay.test'}
  ], config:{}};
  context.recordSnapshot('usd', {ok:true,balance:10.1,used:100,currency:'USD'});
  context.recordSnapshot('usd', {ok:true,balance:10,used:100.1,currency:'USD'});
  context.recordSnapshot('unknown', {ok:true,balance:8.1968,used:2});
  context.renderDailySnapshots();
  const html = element('dailyRows').innerHTML;
  assert.match(html, /当前余额/);
  assert.match(html, /USD 10/);
  assert.match(html, /CNY/);
  assert.match(html, /CNY 8\.1968/);
  assert.match(html, /仅一次采样，无法计算 used 增量/);
  assert.match(html, /查看账号明细/);
});

test('storage failures do not throw or break query state', () => {
  const {context} = loadWizard({throwStorage:true});
  assert.doesNotThrow(() => context.recordSnapshot('x', {ok:true, balance:1, currency:'USD'}));
  assert.equal(context.snapshotItems().length, 0);
});

test('summary keeps currencies separate and does not coerce unknown values to zero', () => {
  const h = loadWizard();
  const {context, element} = h;
  context.DATA = {providers:[], credentials:[
    {provider:'one', name:'a', profile_key:'one@a', status:'ok'},
    {provider:'two', name:'b', profile_key:'two@b', status:'ok'},
    {provider:'three', name:'c', profile_key:'three@c', status:'ok'}
  ], config:{}};
  context.displaySel = {'one@a':true, 'two@b':true, 'three@c':true};
  context.balanceCache = {
    'one@a': {ok:true, balance:10, has_used:true, used:4, currency:'USD'},
    'two@b': {ok:true, balance:20, has_used:true, used:8, currency:'CNY'},
    'three@c': {ok:true, has_used:false, currency:'USD'}
  };
  context.renderSummary();
  assert.equal(element('statBalance').textContent, '—');
  assert.equal(element('statUsed').textContent, '—');
  assert.match(element('summaryByCurrency').innerHTML, /USD/);
  assert.match(element('summaryByCurrency').innerHTML, /CNY/);
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
  assert.doesNotMatch(element('selectedRows').innerHTML, /hidden/);
  assert.match(element('selected-bal-a').innerHTML, /CNY 12.5/);
  assert.equal(element('selected-bal-a').innerHTML, element('bal-a').innerHTML);
  context.toggleDisplay('a', false);
  assert.match(element('selectedRows').innerHTML, /尚未选择账号/);
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
  context.renderSummary();
  assert.equal(element('statBalance').textContent, '—');
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
