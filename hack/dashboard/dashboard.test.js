// Tests for the live data source of site/assets/dashboard.js. Run with `make test-dashboard`
// (node --test); they need nothing but Node.
//
// The dashboard renders objects whose names and tags come from AWS, where anybody who can
// create a subnet can write them. The tests below feed it such values and check that they end
// up as text and never as markup, against a stand-in DOM that records every innerHTML write.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const SCRIPT = fs.readFileSync(path.join(__dirname, '../../site/assets/dashboard.js'), 'utf8');

class FakeText {
  constructor(text) { this.nodeType = 3; this.data = String(text); }
  get textContent() { return this.data; }
}

class FakeElement {
  constructor(doc, tag) {
    this.ownerDocument = doc;
    this.nodeType = 1;
    this.tagName = tag.toUpperCase();
    this.childNodes = [];
    this.attributes = {};
    this.style = {};
  }
  appendChild(c) { this.childNodes.push(c); return c; }
  removeChild(c) { this.childNodes = this.childNodes.filter((x) => x !== c); return c; }
  get firstChild() { return this.childNodes[0] || null; }
  setAttribute(k, v) { this.attributes[k] = String(v); }
  getAttribute(k) { return k in this.attributes ? this.attributes[k] : null; }
  get textContent() { return this.childNodes.map((c) => c.textContent).join(''); }
  set textContent(v) { this.childNodes = [new FakeText(v)]; }
  set innerHTML(v) { this.ownerDocument.innerHTMLWrites.push(String(v)); }
}

function load(extra) {
  const doc = {
    innerHTMLWrites: [],
    metas: {},
    createElement(tag) { return new FakeElement(doc, tag); },
    createTextNode(t) { return new FakeText(t); },
    querySelector(sel) {
      const m = /name="([^"]+)"/.exec(sel);
      const content = m && doc.metas[m[1]];
      return content ? { getAttribute: () => content } : null;
    },
  };
  const store = {};
  const ctx = {
    document: doc,
    location: new URL('https://hypersurgery.dev/dashboard/'),
    URL, URLSearchParams,
    sessionStorage: {
      getItem: (k) => (k in store ? store[k] : null),
      setItem: (k, v) => { store[k] = String(v); },
      removeItem: (k) => { delete store[k]; },
    },
    ...extra,
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  vm.runInContext(SCRIPT, ctx);
  return { live: ctx.SubnetDashboard._live, doc, store, ctx };
}

function walk(node, visit) {
  visit(node);
  (node.childNodes || []).forEach((c) => walk(c, visit));
}

const EVIL = '<img src=x onerror="alert(document.cookie)">';

function hostileData() {
  return {
    networkscopes: { items: [{
      metadata: { name: 'org' + EVIL },
      spec: { vpcTagSelector: { 'hs/managed': 'true' }, tagKeys: { owner: 'team', env: 'stage', tier: 'layer' } },
      status: { targets: [
        { account: '111111111111', region: 'eu-central-1' + EVIL, subnets: 3, vpcs: 1, lastSyncTime: '2026-09-24T10:00:00Z',
          unmanagedIDs: ['vpc-0aaa' + EVIL, 'subnet-0bbb'] },
        { account: '222222222222', region: 'eu-west-1', error: 'AccessDenied' + EVIL },
      ] },
    }] },
    vpcs: { items: [{ status: { overlapsWith: ['x'] } }, { status: {} }] },
    subnets: { items: [
      { spec: { subnetID: 'subnet-01' + EVIL, account: '111111111111', region: 'eu-central-1', vpcID: 'vpc-1' },
        status: { name: EVIL, owner: EVIL, env: 'prod', totalIPs: 100, availableIPs: 10, missingTags: [EVIL] } },
      { spec: { subnetID: 'subnet-02' }, status: { env: 'prod', totalIPs: 100, availableIPs: 90 } },
      { spec: { subnetID: 'subnet-03' }, status: { totalIPs: 0, availableIPs: 0 } },
      { spec: { subnetID: 'subnet-04' }, status: { env: EVIL, totalIPs: 0, availableIPs: 0 } },
    ] },
    subnetclaims: { items: [{
      metadata: { namespace: 'payments', name: 'checkout' + EVIL },
      spec: { vpcID: 'vpc-1', prefixLength: 24, owner: EVIL },
      status: { allocations: [{ availabilityZone: 'eu-central-1a', cidrBlock: EVIL, state: 'Failed', error: EVIL }],
        conditions: [{ type: 'Ready', status: 'False', message: EVIL }] },
    }] },
    resourceimports: { items: [
      { metadata: { namespace: 'default', name: 'a', creationTimestamp: '2026-09-24T09:00:00Z' },
        spec: { resourceID: 'subnet-0bbb', requestedBy: EVIL }, status: { state: 'Pending', error: EVIL } },
      { metadata: { namespace: 'default', name: 'b', creationTimestamp: '2026-09-24T10:00:00Z' },
        spec: { resourceID: 'subnet-0ccc' }, status: { state: 'Failed' } },
    ] },
  };
}

function panels(doc) {
  const el = {};
  ['tiles', 'bars', 'targets', 'subnets', 'unmanaged', 'unmanagedCount', 'claims', 'claimCount', 'imports', 'drawer']
    .forEach((k) => { el[k] = doc.createElement('div'); });
  return el;
}

test('values from the cluster are rendered as text, never as markup', () => {
  const { live, doc } = load();
  const data = hostileData();
  const el = panels(doc);
  live.renderLive(el, live.summarize(data), data, { status: () => '#0f0' });
  const view = live.summarize(data);
  live.renderLiveDrawer(el.drawer, {
    resource: view.unmanaged[0], form: { owner: EVIL, env: '', tier: '', name: EVIL, namespace: 'default' },
    object: live.importObject(view.unmanaged[0], { owner: EVIL, name: EVIL, namespace: 'default' }, data.networkscopes.items[0]),
    error: EVIL,
  });

  assert.deepEqual(doc.innerHTMLWrites, [], 'live rendering must not write innerHTML');

  const allowed = new Set(['DIV', 'SPAN', 'B', 'I', 'TR', 'TD', 'P', 'BUTTON', 'FORM', 'LABEL', 'INPUT', 'H5', 'PRE']);
  let texts = '';
  Object.values(el).forEach((root) => walk(root, (n) => {
    if (n.nodeType === 3) { texts += n.data + '\n'; return; }
    assert.ok(allowed.has(n.tagName), 'unexpected element ' + n.tagName);
    Object.keys(n.attributes).forEach((k) => {
      assert.ok(!/^on/i.test(k) && k !== 'style' && k !== 'href' && k !== 'src', 'unsafe attribute ' + k);
    });
  }));
  // The hostile value reached the page, as the literal text it is.
  assert.ok(texts.includes(EVIL));
});

test('h refuses attributes that can carry script or a URL', () => {
  const { live } = load();
  for (const attr of ['onclick', 'onerror', 'href', 'src', 'style', 'srcdoc', 'formaction']) {
    assert.throws(() => live.h('a', { [attr]: 'javascript:alert(1)' }), new RegExp(attr));
  }
});

test('summarize counts what the tiles show', () => {
  const { live } = load();
  const v = live.summarize(hostileData());
  assert.equal(v.tiles.subnets, 4);
  assert.equal(v.tiles.free, 100);
  assert.equal(v.tiles.hot, 1, 'one subnet at 90%');
  assert.equal(v.tiles.missing, 1);
  assert.equal(v.tiles.overlapping, 1);
  assert.equal(v.tiles.down, 1, 'the target with an error');
  assert.equal(v.fullest[0].id, 'subnet-01' + EVIL);
  // Two prod subnets pool their addresses: 200 total, 100 free.
  const prod = v.envs.find((e) => e.env === 'prod');
  assert.equal(prod.used, 0.5);
  assert.ok(v.envs.find((e) => e.env === 'untagged'), 'a subnet without an env tag is still counted');
});

test('an unmanaged resource with an import on the way is not offered again', () => {
  const { live } = load();
  const v = live.summarize(hostileData());
  const bbb = v.unmanaged.find((u) => u.id === 'subnet-0bbb');
  assert.equal(bbb.importing, 'Pending');
  const vpc = v.unmanaged.find((u) => u.kind === 'vpc');
  assert.equal(vpc.importing, '');
  assert.equal(vpc.scope, 'org' + EVIL);
});

test('an import uses the scope\'s tag keys, and leaves requestedBy to the API server', () => {
  const { live } = load();
  const scope = hostileData().networkscopes.items[0];
  const subnet = { kind: 'subnet', id: 'subnet-0bbb', scope: 'org', account: '111111111111', region: 'eu-central-1' };
  const o = live.importObject(subnet, { owner: 'team-a', env: 'prod', tier: 'private', name: '', namespace: 'platform' }, scope);
  assert.equal(o.kind, 'ResourceImport');
  assert.equal(o.metadata.namespace, 'platform');
  assert.equal(o.metadata.generateName, 'subnet-0bbb-');
  assert.deepEqual({ ...o.spec.tags }, { team: 'team-a', stage: 'prod', layer: 'private' });
  assert.equal(o.spec.scopeRef, 'org');
  assert.ok(!('requestedBy' in o.spec), 'requestedBy is filled in by the webhook from the authenticated user');
  assert.ok(!('dryRun' in o.spec));

  const vpc = { kind: 'vpc', id: 'vpc-0aaa', scope: 'org', account: '111111111111', region: 'eu-central-1' };
  const v = live.importObject(vpc, { owner: 'team-a', tier: 'private', dryRun: true, namespace: 'default' }, scope);
  assert.deepEqual({ ...v.spec.tags }, { 'hs/managed': 'true', team: 'team-a' }, 'a VPC gets the selector tags and no tier');
  assert.equal(v.spec.dryRun, true);
});

test('the YAML preview quotes every value', () => {
  const { live } = load();
  const y = live.toYAML({ spec: { tags: { Name: 'x\n  evil: "1"' } } });
  assert.equal(y, 'spec:\n  tags:\n    Name: "x\\n  evil: \\"1\\""');
});

test('the data source is demo unless the page or the URL says otherwise', () => {
  const { live, doc } = load();
  const at = (u) => live.sourceFromPage(doc, new URL(u));
  assert.equal(at('https://hypersurgery.dev/dashboard/').mode, 'demo');
  assert.deepEqual({ ...at('http://127.0.0.1:8001/ui/dashboard/?source=kubectl') }, { mode: 'kubectl', base: '' });
  assert.deepEqual({ ...at('http://localhost:3000/dashboard/?source=kubectl') }, { mode: 'kubectl', base: 'http://127.0.0.1:8001' });
  assert.deepEqual({ ...at('http://localhost:3000/?api=http://127.0.0.1:9000/') }, { mode: 'kubectl', base: 'http://127.0.0.1:9000' });
  assert.equal(at('https://hypersurgery.dev/dashboard/?api=javascript:alert(1)').mode, 'demo');

  // The chart's page is always the cluster, whatever the URL asks for: its token must only
  // ever go to its own origin.
  doc.metas['subnet-dashboard-source'] = 'cluster';
  assert.deepEqual({ ...at('https://dash.example/dashboard/?source=kubectl&api=https://attacker.example') }, { mode: 'cluster', base: '' });
});

function recordingFetch() {
  const calls = [];
  const fetch = (url, init) => {
    calls.push({ url, init });
    return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('{"items":[]}') });
  };
  return { calls, fetch };
}

test('in the cluster the pasted token is sent; through kubectl proxy no credentials are', async () => {
  const { live, store } = load();
  store[live.TOKEN_KEY] = 'tok-123';

  const cluster = recordingFetch();
  await live.apiClient({ mode: 'cluster', base: '' }, cluster.fetch).list('subnets');
  assert.equal(cluster.calls[0].url, '/apis/aws.hypersurgery/v1alpha1/subnets');
  assert.equal(cluster.calls[0].init.headers.Authorization, 'Bearer tok-123');
  assert.equal(cluster.calls[0].init.credentials, 'same-origin');

  const kubectl = recordingFetch();
  await live.apiClient({ mode: 'kubectl', base: 'http://127.0.0.1:8001' }, kubectl.fetch).list('subnets');
  assert.equal(kubectl.calls[0].url, 'http://127.0.0.1:8001/apis/aws.hypersurgery/v1alpha1/subnets');
  assert.ok(!('Authorization' in kubectl.calls[0].init.headers), 'the token must not leave for another origin');
  assert.equal(kubectl.calls[0].init.credentials, 'omit');
});

test('an import is posted as JSON with the header the dashboard checks for writes', async () => {
  const { live } = load();
  const rec = recordingFetch();
  await live.apiClient({ mode: 'cluster', base: '' }, rec.fetch).create('team a', 'resourceimports', { kind: 'ResourceImport' });
  const { url, init } = rec.calls[0];
  assert.equal(url, '/apis/aws.hypersurgery/v1alpha1/namespaces/team%20a/resourceimports');
  assert.equal(init.method, 'POST');
  assert.equal(init.headers['Content-Type'], 'application/json');
  assert.equal(init.headers['X-Subnet-Dashboard'], '1');
  assert.equal(init.body, '{"kind":"ResourceImport"}');
});

test('an API refusal carries its status and message', async () => {
  const { live } = load();
  const fetch = () => Promise.resolve({
    ok: false, status: 403,
    text: () => Promise.resolve('{"kind":"Status","message":"subnets is forbidden: User \\"bob\\" cannot list"}'),
  });
  await assert.rejects(live.apiClient({ mode: 'cluster', base: '' }, fetch).list('subnets'),
    (e) => e.code === 403 && /cannot list/.test(e.message));
});

// --- Watching ------------------------------------------------------------------------------

// streamed is a fetch response whose body arrives in the given chunks, the way a watch does.
function streamed(chunks) {
  const enc = new TextEncoder();
  let i = 0;
  return {
    ok: true, status: 200,
    body: { getReader: () => ({
      read: () => Promise.resolve(i < chunks.length ? { done: false, value: enc.encode(chunks[i++]) } : { done: true }),
      cancel: () => Promise.resolve(),
    }) },
    text: () => Promise.resolve(''),
  };
}

const flush = async () => { for (let i = 0; i < 10; i++) await new Promise((r) => setImmediate(r)); };

test('a watch asks for bookmarks from a resourceVersion and delivers events split across chunks', async () => {
  const { live } = load({ TextDecoder });
  const calls = [];
  const fetch = (url, init) => {
    calls.push({ url, init });
    return Promise.resolve(streamed([
      '{"type":"ADDED","object":{"metadata":{"name":"a","resourceVersion":"42"}}}\n{"type":"MODI',
      'FIED","object":{"metadata":{"name":"a","resourceVersion":"43"}}}\n',
      '{"type":"BOOKMARK","object":{"metadata":{"resourceVersion":"50"}}}',
    ]));
  };
  const seen = [];
  await live.apiClient({ mode: 'cluster', base: '' }, fetch).watch('subnets', '41', (e) => seen.push(e));
  const u = new URL(calls[0].url, 'https://dash.example');
  assert.equal(u.pathname, '/apis/aws.hypersurgery/v1alpha1/subnets');
  assert.equal(u.searchParams.get('watch'), '1');
  assert.equal(u.searchParams.get('allowWatchBookmarks'), 'true');
  assert.equal(u.searchParams.get('resourceVersion'), '41');
  assert.ok(Number(u.searchParams.get('timeoutSeconds')) >= 300);
  assert.equal(calls[0].init.method, 'GET');
  assert.deepEqual(seen.map((e) => e.type + '@' + e.object.metadata.resourceVersion), ['ADDED@42', 'MODIFIED@43', 'BOOKMARK@50']);
});

test('410 Gone reaches the caller as code 410, whether as an ERROR event or as the response', async () => {
  const { live } = load({ TextDecoder });
  const inStream = () => Promise.resolve(streamed([
    '{"type":"ERROR","object":{"kind":"Status","code":410,"reason":"Expired","message":"too old resource version"}}\n',
  ]));
  await assert.rejects(live.apiClient({ mode: 'cluster', base: '' }, inStream).watch('subnets', '1', () => {}),
    (e) => e.code === 410 && /too old/.test(e.message));
  const asResponse = () => Promise.resolve({ ok: false, status: 410, text: () => Promise.resolve('{"message":"gone"}') });
  await assert.rejects(live.apiClient({ mode: 'cluster', base: '' }, asResponse).watch('subnets', '1', () => {}),
    (e) => e.code === 410);
});

test('a browser that cannot stream a response says so instead of watching', async () => {
  const { live } = load({ TextDecoder });
  const fetch = () => Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('') });
  await assert.rejects(live.apiClient({ mode: 'cluster', base: '' }, fetch).watch('subnets', '1', () => {}),
    (e) => e.noStream === true);
});

// fakeAPI answers lists from `lists` and hands every watch to the test to play out.
function fakeAPI(lists) {
  const api = {
    listed: [], watches: [],
    list(r) {
      api.listed.push(r);
      const next = lists[r] && lists[r].shift();
      return next instanceof Error ? Promise.reject(next) : Promise.resolve(next || { metadata: { resourceVersion: '1' }, items: [] });
    },
    watch(r, rv, onEvent, signal) {
      return new Promise((resolve, reject) => { api.watches.push({ r, rv, onEvent, resolve, reject, signal }); });
    },
  };
  return api;
}

function fakeTimers() {
  const t = { queue: [], delays: [] };
  t.setTimeout = (f, ms) => { t.queue.push(f); t.delays.push(ms); return t.queue.length; };
  t.clearTimeout = () => {};
  t.run = async () => { while (t.queue.length) { t.queue.shift()(); await flush(); } };
  return t;
}

const sub = (name, rv, extra) => ({ metadata: { name, resourceVersion: rv }, ...extra });
const gone = () => Object.assign(new Error('too old resource version'), { code: 410 });

test('a watched kind is listed once, then kept current by its watch', async () => {
  const { live } = load();
  const api = fakeAPI({ subnets: [{ metadata: { resourceVersion: '10' }, items: [sub('a', '9'), sub('b', '10')] }] });
  const timers = fakeTimers();
  const changes = [];
  const sync = live.liveSync(api, { watched: ['subnets'], onChange: (r) => changes.push(r), ...timers });

  assert.deepEqual([...sync.needed()], ['networkscopes', 'vpcs', 'subnets', 'subnetclaims', 'resourceimports']);
  await sync.load(sync.needed());
  await timers.run();
  assert.equal(api.watches.length, 1);
  assert.equal(api.watches[0].rv, '10', 'the watch starts from the list');
  assert.deepEqual([...sync.needed()], ['networkscopes', 'vpcs', 'subnetclaims', 'resourceimports'], 'a watched kind is not polled');

  const w = api.watches[0];
  w.onEvent({ type: 'ADDED', object: sub('c', '11') });
  w.onEvent({ type: 'MODIFIED', object: sub('a', '12', { status: { availableIPs: 3 } }) });
  w.onEvent({ type: 'DELETED', object: sub('b', '13') });
  assert.deepEqual([...sync.data.subnets.items].map((o) => o.metadata.name), ['a', 'c']);
  assert.equal(sync.data.subnets.items[0].status.availableIPs, 3);
  assert.equal(changes.length, 3);

  // A bookmark changes nothing on the page, only where the next watch starts.
  w.onEvent({ type: 'BOOKMARK', object: { metadata: { resourceVersion: '20' } } });
  assert.equal(changes.length, 3, 'a bookmark is not a change');
  assert.equal(sync.data.subnets.items.length, 2);

  // The API server ends the stream at its timeout: watch again from the bookmark, at once.
  w.resolve();
  await flush();
  assert.equal(timers.delays[timers.delays.length - 1], 0);
  await timers.run();
  assert.equal(api.watches.length, 2);
  assert.equal(api.watches[1].rv, '20');
  assert.deepEqual(api.listed.filter((r) => r === 'subnets'), ['subnets'], 'no relist on a routine stream end');
});

test('410 Gone lists the kind again and watches from the new list', async () => {
  const { live } = load();
  const api = fakeAPI({ subnets: [
    { metadata: { resourceVersion: '10' }, items: [sub('a', '10')] },
    { metadata: { resourceVersion: '90' }, items: [sub('a', '80'), sub('z', '90')] },
  ] });
  const timers = fakeTimers();
  const changes = [];
  const sync = live.liveSync(api, { watched: ['subnets'], onChange: (r) => changes.push(r), ...timers });
  await sync.load(['subnets']);
  await timers.run();
  api.watches[0].reject(gone());
  await flush();
  await timers.run();
  assert.deepEqual(api.listed, ['subnets', 'subnets'], 'relisted');
  assert.deepEqual([...sync.data.subnets.items].map((o) => o.metadata.name), ['a', 'z']);
  assert.deepEqual(changes, ['subnets'], 'the page redraws with the new list');
  assert.equal(api.watches.length, 2);
  assert.equal(api.watches[1].rv, '90');
});

test('without a way to stream, every watched kind falls back to polling', async () => {
  const { live } = load();
  const api = fakeAPI({});
  const timers = fakeTimers();
  const sync = live.liveSync(api, { watched: ['subnets', 'resourceimports'], ...timers });
  await sync.load(sync.needed());
  await timers.run();
  assert.deepEqual([...sync.needed()], ['networkscopes', 'vpcs', 'subnetclaims']);
  api.watches[0].reject(Object.assign(new Error('no stream'), { noStream: true }));
  await flush();
  assert.deepEqual([...sync.needed()], ['networkscopes', 'vpcs', 'subnets', 'subnetclaims', 'resourceimports']);
  assert.deepEqual([...sync.watching()], []);
  // And it stays that way: a later poll does not try to watch again.
  await sync.load(['subnets', 'resourceimports']);
  await timers.run();
  assert.equal(api.watches.length, 2);
});

test('a kind RBAC may list but not watch is polled', async () => {
  const { live } = load();
  const api = fakeAPI({});
  const timers = fakeTimers();
  const sync = live.liveSync(api, { watched: ['subnets', 'resourceimports'], ...timers });
  await sync.load(sync.needed());
  await timers.run();
  const w = api.watches.find((x) => x.r === 'resourceimports');
  w.reject(Object.assign(new Error('cannot watch'), { code: 403 }));
  await flush();
  assert.ok(sync.needed().includes('resourceimports'));
  assert.ok(!sync.needed().includes('subnets'), 'the other watch carries on');
});

test('a watch that keeps failing backs off, then leaves the kind to polling', async () => {
  const { live } = load();
  const api = fakeAPI({});
  const timers = fakeTimers();
  const sync = live.liveSync(api, { watched: ['subnets'], ...timers });
  await sync.load(['subnets']);
  await timers.run();
  for (let i = 0; i < 5; i++) {
    api.watches[api.watches.length - 1].reject(Object.assign(new Error('bad gateway'), { code: 502 }));
    await flush();
    await timers.run();
  }
  assert.deepEqual([...timers.delays], [0, 1000, 2000, 4000, 8000], 'retries back off');
  assert.equal(api.watches.length, 5);
  assert.ok(sync.needed().includes('subnets'), 'polled after five failures in a row');
});

test('a hidden page closes its watches and resumes them from where they were', async () => {
  const { live } = load();
  const api = fakeAPI({ subnets: [{ metadata: { resourceVersion: '10' }, items: [] }] });
  const timers = fakeTimers();
  const sync = live.liveSync(api, { watched: ['subnets'], AbortController, ...timers });
  await sync.load(['subnets']);
  await timers.run();
  api.watches[0].onEvent({ type: 'BOOKMARK', object: { metadata: { resourceVersion: '15' } } });
  sync.pause();
  assert.ok(api.watches[0].signal.aborted, 'the connection is given back');
  api.watches[0].reject(Object.assign(new Error('aborted'), { name: 'AbortError' }));
  await flush();
  assert.deepEqual([...sync.watching()], ['subnets'], 'paused, not given up');
  sync.resume();
  await timers.run();
  assert.equal(api.watches.length, 2);
  assert.equal(api.watches[1].rv, '15');
});

// --- Who is signed in ----------------------------------------------------------------------

test('whoami posts a SelfSubjectReview with the write header and returns the user name', async () => {
  const { live } = load();
  const calls = [];
  const fetch = (url, init) => {
    calls.push({ url, init });
    return Promise.resolve({ ok: true, status: 201, text: () => Promise.resolve(
      '{"kind":"SelfSubjectReview","status":{"userInfo":{"username":"alice@example.com","groups":["dev"]}}}') });
  };
  const name = await live.apiClient({ mode: 'cluster', base: '' }, fetch).whoami();
  assert.equal(name, 'alice@example.com');
  assert.equal(calls[0].url, '/apis/authentication.k8s.io/v1/selfsubjectreviews');
  assert.equal(calls[0].init.method, 'POST');
  assert.equal(calls[0].init.headers['X-Subnet-Dashboard'], '1');
  assert.equal(calls[0].init.headers['Content-Type'], 'application/json');
  assert.deepEqual(JSON.parse(calls[0].init.body), { apiVersion: 'authentication.k8s.io/v1', kind: 'SelfSubjectReview' });
});

test('the user name is shown as text, and through kubectl proxy as the proxy\'s', () => {
  const { live, doc } = load();
  const node = doc.createElement('span');
  live.renderWho(node, { mode: 'cluster', base: '' }, EVIL);
  assert.deepEqual(doc.innerHTMLWrites, [], 'the user name must not be written as markup');
  assert.equal(node.childNodes.length, 1);
  assert.equal(node.childNodes[0].nodeType, 3);
  assert.equal(node.textContent, 'signed in as ' + EVIL);
  assert.equal(node.hidden, false);

  live.renderWho(node, { mode: 'kubectl', base: '' }, 'kubernetes-admin');
  assert.equal(node.textContent, 'kubectl proxy as kubernetes-admin');
  assert.match(node.getAttribute('title'), /proxy's identity/);

  live.renderWho(node, { mode: 'cluster', base: '' }, '');
  assert.equal(node.hidden, true, 'nothing to show on an API server without SelfSubjectReview');
});

test('a watch a proxy cuts after a quiet minute is opened again, not given up', async () => {
  const { live } = load();
  const api = fakeAPI({});
  const timers = fakeTimers();
  let clock = 0;
  const sync = live.liveSync(api, { watched: ['subnets'], now: () => clock, ...timers });
  await sync.load(['subnets']);
  await timers.run();
  for (let i = 0; i < 8; i++) {
    clock += 60000;
    api.watches[api.watches.length - 1].reject(new TypeError('network error'));
    await flush();
    await timers.run();
  }
  assert.equal(api.watches.length, 9);
  assert.ok(!sync.needed().includes('subnets'), 'still watched');
  assert.ok(timers.delays.slice(1).every((d) => d === 1000), 'no growing back-off after a long-lived stream');
});

test('a list that finishes after a sign-out changes nothing', async () => {
  const { live } = load();
  let release;
  const api = fakeAPI({});
  api.list = () => new Promise((r) => { release = r; });
  const sync = live.liveSync(api, { watched: ['subnets'], ...fakeTimers() });
  const pending = sync.load(['subnets']);
  sync.reset();
  release({ metadata: { resourceVersion: '5' }, items: [sub('old-viewer', '5')] });
  assert.equal(await pending, null, 'the caller is told the answer is stale');
  assert.equal(sync.data.subnets.items.length, 0);
  assert.equal(api.watches.length, 0);
});

test('an import shows the authenticated creator, and requestedBy only as who it was for', () => {
  const { live, doc } = load();
  const data = hostileData();
  data.resourceimports.items = [
    { metadata: { namespace: 'default', name: 'forged', creationTimestamp: '2026-09-24T09:00:00Z',
      annotations: { 'aws.hypersurgery/created-by': 'jane@example.com' } },
      spec: { resourceID: 'subnet-0bbb', requestedBy: 'the CTO' }, status: { state: 'Applied' } },
    { metadata: { namespace: 'default', name: 'old', creationTimestamp: '2026-09-24T08:00:00Z' },
      spec: { resourceID: 'subnet-0ccc', requestedBy: 'the CTO' }, status: { state: 'Applied' } },
  ];
  const view = live.summarize(data);
  assert.equal(view.imports[0].createdBy, 'jane@example.com');
  assert.equal(view.imports[1].createdBy, 'unknown', 'no annotation: nobody vouched for a creator');

  const el = panels(doc);
  live.renderLive(el, view, data, { status: () => '#0f0' });
  let rows = '';
  walk(el.imports, (n) => { if (n.nodeType === 3) rows += n.data + '|'; });
  assert.match(rows, /jane@example\.com\|for the CTO\|/);
  assert.match(rows, /unknown\|for the CTO\|/);
});
