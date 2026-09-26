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
    if (!elements.has(id)) elements.set(id, {id, value:'', textContent:'', innerHTML:'', style:{display:''}, className:'', disabled:false, dataset:{}, querySelector(){ return null; }});
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
  context.recordSnapshot('deepseek@a', {ok:true, balance:10, used:100, currency:'USD'});
  context.recordSnapshot('deepseek@a', {ok:true, balance:8, used:125, currency:'USD'});
  context.recordSnapshot('deepseek@a', {ok:true, balance:8, used:125, currency:'CNY'});
  const records = JSON.parse(values.get('api-balance-daily-v2'));
  assert.equal(records.length, 2);
  const usd = records.find(x => x.currency === 'USD');
  assert.equal(usd.firstBalance, 10);
  assert.equal(usd.lastBalance, 8);
  assert.equal(context.snapshotUsedDelta(usd), 25);
  assert.equal(context.snapshotUsedDelta({...usd, lastUsed: 2}), null);
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
