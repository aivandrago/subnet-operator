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
