/* Subnet dashboard.
 *
 * Two data sources behind one look:
 *
 * - demo: a synthetic inventory of a fictional organization, rendered the way the operator's
 *   own metrics would be. No network calls: the numbers are generated here and drift every
 *   few seconds so the page shows something alive. The panel on the project page and the
 *   public app at /dashboard/ use it.
 * - live: the VPC, Subnet, NetworkScope, SubnetClaim and ResourceImport objects of a real
 *   cluster, read from the Kubernetes API as the person looking at them — through
 *   kubectl proxy ("kubectl"), or through the dashboard container the Helm chart deploys
 *   ("cluster"), which forwards the viewer's own token and nothing else.
 *   Subnets and ResourceImports are watched, the rest listed every 15 seconds (see
 *   WATCHED_RESOURCES), and the header shows who the API server says the viewer is.
 *
 * window.SubnetDashboard.mount(container, { modal, onClose, onTheme, defaultTheme, source })
 *   -> controller. source comes from SubnetDashboard.sourceFromPage(); without it, demo.
 */
(function (global) {
  'use strict';

  var THEMES = [
    { id: 'midnight', label: 'Midnight' },
    { id: 'terminal', label: 'Terminal' },
    { id: 'cyberpunk', label: 'Cyber' },
    { id: 'daylight', label: 'Daylight' }
  ];
  var THEME_KEY = 'subnet-dashboard-theme';

  var ACCOUNTS = [
    { id: '111111111111', name: 'platform', slot: 1 },
    { id: '222222222222', name: 'payments', slot: 2 },
    { id: '333333333333', name: 'data', slot: 3 }
  ];
  var ENVS = [
    { env: 'prod', used: 0.78 },
    { env: 'staging', used: 0.46 },
    { env: 'dev', used: 0.31 },
    { env: 'sandbox', used: 0.12 }
  ];
  var SUBNETS = [
    { id: 'subnet-0e4f5a6b', owner: 'team-payments', total: 251, free: 31, missing: [] },
    { id: 'subnet-0b19c7d2', owner: 'team-web', total: 251, free: 44, missing: [] },
    { id: 'subnet-07c8d9e0', owner: '', total: 4091, free: 812, missing: ['hs/owner'] },
    { id: 'subnet-0a1b2c3d', owner: 'team-search', total: 507, free: 126, missing: [] },
    { id: 'subnet-0d5e6f70', owner: 'team-data', total: 1019, free: 301, missing: ['hs/tier'] },
    { id: 'subnet-0c3b2a19', owner: 'team-platform', total: 251, free: 96, missing: [] }
  ];
  var TARGETS = [
    { account: '111111111111', region: 'eu-central-1', ok: true, subnets: 96 },
    { account: '111111111111', region: 'eu-west-1', ok: true, subnets: 64 },
    { account: '222222222222', region: 'eu-central-1', ok: true, subnets: 88 },
    { account: '222222222222', region: 'eu-west-1', ok: true, subnets: 41 },
    { account: '333333333333', region: 'eu-central-1', ok: false, subnets: 29 }
  ];
  // Cost model: components priced from the public eu-central-1 list, attributed by tag.
  var COST_PARTS = [
    { key: 'nat', label: 'NAT gateways', slot: 1 },
    { key: 'ipv4', label: 'Public IPv4', slot: 2 },
    { key: 'endpoints', label: 'VPC endpoints', slot: 3 },
    { key: 'transfer', label: 'Cross-AZ transfer', slot: 4 }
  ];
  var ENV_COST = [
    { env: 'prod', nat: 456, ipv4: 219, endpoints: 264, transfer: 187 },
    { env: 'staging', nat: 152, ipv4: 47, endpoints: 88, transfer: 34 },
    { env: 'dev', nat: 114, ipv4: 29, endpoints: 44, transfer: 12 },
    { env: 'sandbox', nat: 38, ipv4: 11, endpoints: 0, transfer: 3 }
  ];
  var ACCOUNT_COST = [
    { account: '111111111111', name: 'platform', month: 812, delta: 6.2, driver: 'NAT gateways', idle: 84 },
    { account: '222222222222', name: 'payments', month: 521, delta: -3.1, driver: 'Public IPv4', idle: 38 },
    { account: '333333333333', name: 'data', month: 365, delta: 11.4, driver: 'Cross-AZ transfer', idle: 121 }
  ];
  // Teams are a tag, not an account, so a team can span accounts and an account can hold
  // several teams. That mismatch is exactly what an account-shaped bill cannot show.
  var TEAM_COST = [
    { team: 'team-platform', month: 604, delta: 4.1, accounts: 2, driver: 'NAT gateways', subnets: 96 },
    { team: 'team-payments', month: 448, delta: -2.6, accounts: 2, driver: 'Public IPv4', subnets: 64 },
    { team: 'team-data', month: 391, delta: 13.8, accounts: 2, driver: 'Cross-AZ transfer', subnets: 71 },
    { team: 'team-web', month: 168, delta: 1.2, accounts: 1, driver: 'NAT gateways', subnets: 42 },
    { team: 'team-search', month: 87, delta: -8.4, accounts: 1, driver: 'Interface endpoints', subnets: 29 },
    { team: null, month: 62, delta: 22.0, accounts: 2, driver: 'Public IPv4', subnets: 16 }
  ];
  var IDLE = [
    { what: '18 public IPv4 addresses on nothing', usd: 66 },
    { what: '3 NAT gateways under 1 GB/month', usd: 114 },
    { what: '9 interface endpoints with no traffic', usd: 63 }
  ];

  // Resources discovery can see but nobody has tagged for the operator yet. "tf" marks the
  // ones that look Terraform-managed, where tags belong in code rather than in a click.
  // People and pipelines that create things by hand. CloudTrail names the principal; the
  // directory turns it into a person. The auto-import policy matches on the principal.
  var CREATORS = [
    { name: 'A. Rivera', handle: 'a.rivera', principal: 'assumed-role/payments-deploy/a.rivera', via: 'console' },
    { name: 'J. Nakamura', handle: 'j.nakamura', principal: 'assumed-role/data-platform-admin/j.nakamura', via: 'aws cli' },
    { name: 'S. Okonkwo', handle: 's.okonkwo', principal: 'assumed-role/ops-admin/s.okonkwo', via: 'console' },
    { name: 'Terraform (CI)', handle: 'terraform-ci', principal: 'assumed-role/terraform-apply/ci-runner', via: 'terraform' },
    { name: 'M. Lindqvist', handle: 'm.lindqvist', principal: 'assumed-role/web-oncall/m.lindqvist', via: 'console' }
  ];
  var POLICY = {
    fromCreator: [
      { match: 'assumed-role/payments-', owner: 'team-payments' },
      { match: 'assumed-role/data-platform-', owner: 'team-data' },
      { match: 'assumed-role/web-', owner: 'team-web' }
    ],
    skip: [{ match: 'assumed-role/terraform-' }],
    inheritFromVPC: { 'vpc-0aa11bb2': { owner: 'team-platform', env: 'prod' }, 'vpc-0cc3d4e5': { owner: 'team-payments', env: 'prod' } }
  };
  var UNMANAGED = [
    { kind: 'vpc', id: 'vpc-0f3e2a1b7c9d4e56', name: 'legacy-shared', account: '333333333333', region: 'eu-central-1', cidr: '10.90.0.0/16', subnets: 6, tf: true, by: CREATORS[3], ago: 3100 },
    { kind: 'subnet', id: 'subnet-04d1c2b3a4e5f607', name: 'data-lake-a', account: '333333333333', region: 'eu-central-1', cidr: '10.30.4.0/24', vpc: 'vpc-0dd4e5f6', tf: false, by: CREATORS[1], ago: 240 },
    { kind: 'subnet', id: 'subnet-0b2c3d4e5f6a7b89', name: '', account: '222222222222', region: 'eu-west-1', cidr: '10.20.40.0/22', vpc: 'vpc-0cc3d4e5', tf: false, by: CREATORS[2], ago: 250 },
    { kind: 'vpc', id: 'vpc-09e8d7c6b5a43210', name: 'sandbox-ml', account: '111111111111', region: 'eu-west-1', cidr: '172.31.0.0/16', subnets: 3, tf: false, by: CREATORS[2], ago: 5400 },
    { kind: 'subnet', id: 'subnet-0c9b8a7f6e5d4c3b', name: 'ops-tools', account: '111111111111', region: 'eu-central-1', cidr: '10.20.9.0/24', vpc: 'vpc-0aa11bb2', tf: true, by: CREATORS[3], ago: 8600 }
  ];
  var OWNERS = ['team-platform', 'team-payments', 'team-data', 'team-web', 'team-search'];
  // SubnetClaims: what teams asked for, and how far the operator got.
  var CLAIMS = [
    { ns: 'payments', name: 'checkout-prod', vpc: 'vpc-0aa11bb2', prefix: 24, mode: 'Create', owner: 'team-payments',
      allocations: [
        { az: 'eu-central-1a', cidr: '10.20.12.0/24', subnet: 'subnet-0a7b6c5d4e3f2019', state: 'Created' },
        { az: 'eu-central-1b', cidr: '10.20.13.0/24', subnet: 'subnet-0b8c7d6e5f403122', state: 'Created' },
        { az: 'eu-central-1c', cidr: '10.20.14.0/24', subnet: 'subnet-0c9d8e7f60514233', state: 'Created' }
      ] },
    { ns: 'data', name: 'lakehouse', vpc: 'vpc-0cc3d4e5', prefix: 22, mode: 'Allocate', owner: 'team-data',
      allocations: [
        { az: 'eu-west-1a', cidr: '10.30.16.0/22', subnet: '', state: 'Pending' },
        { az: 'eu-west-1b', cidr: '10.30.20.0/22', subnet: '', state: 'Pending' }
      ] },
    { ns: 'web', name: 'edge-canary', vpc: 'vpc-0aa11bb2', prefix: 26, mode: 'Create', owner: 'team-web',
      allocations: [
        { az: 'eu-central-1a', cidr: '10.20.15.0/26', subnet: 'subnet-0d1e2f3a4b5c6d70', state: 'Created' },
        { az: 'eu-central-1b', cidr: '10.20.15.64/26', subnet: '', state: 'Failed', error: 'UnauthorizedOperation · write role missing in 222222222222' }
      ] }
  ];
  var AUTO_KEY = 'subnet-dashboard-autoimport';
  var autoMode = 'dryrun';   // off | dryrun | apply
  var autoLog = [];          // what the policy did, newest first
  var spawnCounter = 0;

  var EVENT_NAMES = ['CreateSubnet', 'CreateTags', 'AssociateRouteTable', 'DeleteSubnet', 'CreateVpc', 'CreateRoute'];

  var seed = 20260922;
  function rand() { seed = (seed * 1103515245 + 12345) % 2147483648; return seed / 2147483648; }

  function fmt(n) { return n.toLocaleString('en-US'); }
  function usedRatio(s) { return 1 - s.free / s.total; }
  function pct(r) { return Math.round(r * 100) + '%'; }

  /* Theme switch, shared by the demo and the live dashboard. ---------------- */

  // themeSwitch fills the theme buttons and returns apply(id), which also remembers the choice
  // and redraws whatever the caller draws.
  function themeSwitch(container, host, options, redraw) {
    function apply(id) {
      container.setAttribute('data-dash-theme', id);
      try { localStorage.setItem(THEME_KEY, id); } catch (e) { /* private mode */ }
      Array.prototype.forEach.call(host.children, function (b) {
        b.setAttribute('aria-pressed', String(b.dataset.theme === id));
      });
      if (options.onTheme) options.onTheme(id);
      redraw();
    }
    host.innerHTML = THEMES.map(function (t) {
      return '<button type="button" data-theme="' + t.id + '" aria-pressed="false">' + t.label + '</button>';
    }).join('');
    host.addEventListener('click', function (e) {
      var b = e.target.closest('button[data-theme]');
      if (b) apply(b.dataset.theme);
    });
    return { apply: apply };
  }

  function initialTheme(options) {
    // ?theme=<id> wins, so a specific look can be linked to or screenshotted.
    var q = new URLSearchParams(location.search).get('theme');
    if (q && THEMES.some(function (t) { return t.id === q; })) return q;
    var stored;
    try { stored = localStorage.getItem(THEME_KEY); } catch (e) { stored = null; }
    if (stored && THEMES.some(function (t) { return t.id === stored; })) return stored;
    if (options.defaultTheme) return options.defaultTheme;
    if (global.matchMedia && global.matchMedia('(prefers-color-scheme: light)').matches) return 'daylight';
    return 'midnight';
  }

  var TEMPLATE =
    '<div class="dash-head">' +
      '<h3>Subnet inventory</h3>' +
      '<span class="dash-badge"><i></i>Demo data</span>' +
      '<span class="dash-sync" data-el="sync">synced just now</span>' +
      '<div class="dash-tools">' +
        '<div class="dash-themes" role="group" aria-label="Colour theme" data-el="themes"></div>' +
        '<button class="dash-install" type="button" data-el="install" hidden>Install app</button>' +
        '<button class="dash-close" type="button" data-el="close" aria-label="Close the dashboard" hidden>' +
          '<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><path d="M6 6l12 12M18 6L6 18"/></svg>' +
        '</button>' +
      '</div>' +
    '</div>' +
    '<div class="dash-body">' +
      '<div class="dash-tiles" data-el="tiles"></div>' +
      '<div class="dash-grid">' +
        '<div class="dash-card">' +
          '<h4>IPv4 utilization by environment</h4>' +
          '<p class="sub">Share of usable addresses in use. Alert at 80%.</p>' +
          '<div class="dash-bars" data-el="bars"></div>' +
        '</div>' +
        '<div class="dash-card">' +
          '<h4>Free addresses by account</h4>' +
          '<p class="sub">Last 24 hours, one line per account.</p>' +
          '<div class="dash-legend" data-el="legend"></div>' +
          '<svg class="dash-spark" data-el="spark" viewBox="0 0 560 150" preserveAspectRatio="none" role="img" aria-label="Free IPv4 addresses per account over the last 24 hours"></svg>' +
          '<div class="dash-tip" data-el="tip"></div>' +
        '</div>' +
      '</div>' +
      '<div class="dash-grid">' +
        '<div class="dash-card">' +
          '<h4>Fullest subnets</h4>' +
          '<p class="sub">The subnets closest to running out of addresses.</p>' +
          '<table><thead><tr><th class="mono">Subnet</th><th class="hide-sm">Owner</th><th>Used</th><th>Free</th><th class="hide-sm">Tags</th></tr></thead>' +
          '<tbody data-el="subnets"></tbody></table>' +
        '</div>' +
        '<div class="dash-card">' +
          '<h4>Accounts and regions</h4>' +
          '<p class="sub">Discovery state per target, and the events that triggered the last syncs.</p>' +
          '<table><thead><tr><th class="mono">Account</th><th>Region</th><th>State</th><th class="hide-sm">Subnets</th></tr></thead>' +
          '<tbody data-el="targets"></tbody></table>' +
          '<p class="sub" style="margin:14px 0 8px">Change events from CloudTrail</p>' +
          '<ul class="dash-feed" data-el="feed"></ul>' +
        '</div>' +
      '</div>' +
      '<div class="dash-card dash-unmanaged">' +
        '<div class="dash-unmanaged-head">' +
          '<div><h4>Unmanaged resources <span class="dash-count" data-el="unmanagedCount"></span></h4>' +
          '<p class="sub">Discovered, but not tagged for the operator. CloudTrail says who created each one; the policy decides what happens next.</p></div>' +
          '<div class="dash-auto" role="group" aria-label="Auto-import mode" data-el="autoMode">' +
            '<span class="dash-auto-label">Auto-import</span>' +
            '<button type="button" data-mode="off">Off</button><button type="button" data-mode="dryrun">Dry run</button><button type="button" data-mode="apply">Apply</button>' +
          '</div>' +
        '</div>' +
        '<table><thead><tr><th></th><th class="mono">Resource</th><th>Created by</th><th class="hide-sm">CIDR</th><th>Policy</th><th></th></tr></thead>' +
        '<tbody data-el="unmanaged"></tbody></table>' +
        '<div data-el="drawer"></div>' +
        '<div class="dash-autolog" data-el="autoLog"></div>' +
      '</div>' +
      '<div class="dash-card">' +
        '<h4>Subnet claims <span class="dash-count" data-el="claimCount"></span></h4>' +
        '<p class="sub">Teams ask for capacity; the operator reserves free CIDRs and, with writes enabled, creates the subnets.</p>' +
        '<table><thead><tr><th class="mono">Claim</th><th class="hide-sm">VPC</th><th>Mode</th><th>Allocations</th><th>State</th></tr></thead>' +
        '<tbody data-el="claims"></tbody></table>' +
      '</div>' +
      '<div class="dash-costs" data-el="costTiles"></div>' +
      '<div class="dash-grid">' +
        '<div class="dash-card">' +
          '<h4>Network cost by environment</h4>' +
          '<p class="sub">Estimated monthly spend, split by what drives it.</p>' +
          '<div class="dash-legend" data-el="costLegend"></div>' +
          '<div class="dash-stack" data-el="costBars"></div>' +
        '</div>' +
        '<div class="dash-card">' +
          '<h4>Cost by team</h4>' +
          '<p class="sub">The same spend grouped by the <code>hs/owner</code> tag, which is who actually pays for it.</p>' +
          '<div class="dash-stack wide" data-el="costTeams"></div>' +
          '<p class="sub" style="margin-top:12px">A team is a tag, so it follows the resources across accounts.</p>' +
        '</div>' +
        '<div class="dash-card">' +
          '<h4>Cost by account</h4>' +
          '<p class="sub">Month to date against the previous month, and what is being paid for nothing.</p>' +
          '<table><thead><tr><th class="mono">Account</th><th class="hide-sm">Team</th><th>Monthly</th><th>Change</th><th class="hide-sm">Top driver</th></tr></thead>' +
          '<tbody data-el="costAccounts"></tbody></table>' +
          '<p class="sub" style="margin:14px 0 8px">Idle spend</p>' +
          '<table><tbody data-el="costIdle"></tbody></table>' +
        '</div>' +
      '</div>' +
    '</div>' +
    '<div class="dash-foot">Synthetic data from a fictional organization: nothing here comes from a real AWS account. ' +
      'In a real installation the same panels ship as a Grafana dashboard with the Helm chart. ' +
      'Costs are estimates: resources are attributed by tag and priced from the public list, not billed amounts. ' +
      'To see your own cluster here, open this app through <code>kubectl proxy</code> or enable it in the Helm chart: ' +
      '<a href="/docs/#app">how</a>.</div>';

  function mountDemo(container, options) {
    options = options || {};
    var reduced = global.matchMedia && global.matchMedia('(prefers-reduced-motion: reduce)').matches;

    container.classList.add('dash');
    container.innerHTML = TEMPLATE;
    var el = {};
    Array.prototype.forEach.call(container.querySelectorAll('[data-el]'), function (node) {
      el[node.getAttribute('data-el')] = node;
    });

    var series = ACCOUNTS.map(function (a, i) {
      var base = [9200, 6400, 4100][i], points = [];
      for (var h = 0; h < 24; h++) { base -= rand() * 90; points.push(Math.max(400, Math.round(base))); }
      return { account: a, points: points };
    });
    var feed = [];
    var syncedSecondsAgo = 4;
    var timer = null;

    function token(name) {
      return getComputedStyle(container).getPropertyValue('--d-' + name).trim();
    }
    function seriesColor(slot) { return token('s' + slot); }
    function statusColor(ratio) {
      return ratio >= 0.85 ? token('bad') : ratio >= 0.7 ? token('warn') : token('ok');
    }

    /* Theme ---------------------------------------------------------------- */

    var themes = themeSwitch(container, el.themes, options, function () { renderAll(); });
    var applyTheme = themes.apply;

    /* Rendering ------------------------------------------------------------ */

    function renderTiles() {
      var totalSubnets = TARGETS.reduce(function (n, t) { return n + t.subnets; }, 0);
      var free = series.reduce(function (n, s) { return n + s.points[s.points.length - 1]; }, 0);
      var hot = SUBNETS.filter(function (s) { return usedRatio(s) >= 0.8; }).length;
      var missing = SUBNETS.filter(function (s) { return s.missing.length; }).length + 9;
      var down = TARGETS.filter(function (t) { return !t.ok; }).length;
      var tiles = [
        { v: fmt(totalSubnets), l: 'subnets tracked', c: '' },
        { v: fmt(free), l: 'free IPv4 addresses', c: '' },
        { v: hot, l: 'at 80% or more', c: hot ? 'warn' : 'ok' },
        { v: missing, l: 'missing required tags', c: missing ? 'warn' : 'ok' },
        { v: 2, l: 'VPCs with overlapping CIDRs', c: 'bad' },
        { v: down, l: 'unreachable accounts', c: down ? 'bad' : 'ok' }
      ];
      el.tiles.innerHTML = tiles.map(function (t) {
        return '<div class="dash-tile ' + t.c + '"><b>' + t.v + '</b><span>' + t.l + '</span></div>';
      }).join('');
    }

    function renderBars() {
      el.bars.innerHTML = ENVS.map(function (e) {
        return '<div class="dash-bar"><span>' + e.env + '</span>' +
          '<span class="track"><span class="fill" style="width:' + (e.used * 100).toFixed(1) +
          '%;background:' + statusColor(e.used) + '"></span></span>' +
          '<span class="val">' + pct(e.used) + '</span></div>';
      }).join('');
    }

    function renderSpark() {
      var W = 560, H = 150, padL = 44, padR = 8, padT = 10, padB = 20;
      var all = series.reduce(function (acc, s) { return acc.concat(s.points); }, []);
      var max = Math.max.apply(null, all) * 1.08;
      var n = series[0].points.length;
      var x = function (i) { return padL + i * (W - padL - padR) / (n - 1); };
      var y = function (v) { return padT + (1 - v / max) * (H - padT - padB); };
      var parts = [];
      [0, 0.5, 1].forEach(function (f) {
        var v = f * max;
        parts.push('<line class="gridline" x1="' + padL + '" x2="' + (W - padR) + '" y1="' + y(v) + '" y2="' + y(v) + '"/>');
        parts.push('<text x="4" y="' + (y(v) + 3) + '">' + Math.round(v / 1000) + 'k</text>');
      });
      parts.push('<line class="axis" x1="' + padL + '" x2="' + (W - padR) + '" y1="' + y(0) + '" y2="' + y(0) + '"/>');
      parts.push('<text x="' + padL + '" y="' + (H - 4) + '">-24h</text>');
      parts.push('<text x="' + (W - padR - 18) + '" y="' + (H - 4) + '">now</text>');
      series.forEach(function (s) {
        var d = s.points.map(function (v, i) { return (i ? 'L' : 'M') + x(i).toFixed(1) + ' ' + y(v).toFixed(1); }).join(' ');
        parts.push('<path d="' + d + '" fill="none" stroke="' + seriesColor(s.account.slot) +
          '" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>');
      });
      parts.push('<line class="crosshair" data-el="cross" x1="0" x2="0" y1="' + padT + '" y2="' + y(0) + '" style="opacity:0"/>');
      el.spark.innerHTML = parts.join('');
      el.spark.__geom = { x: x, padL: padL, padR: padR, W: W, n: n };

      el.legend.innerHTML = series.map(function (s) {
        return '<span><i style="background:' + seriesColor(s.account.slot) + '"></i>' +
          s.account.name + ' · ' + s.account.id + '</span>';
      }).join('');
    }

    function bindSparkHover() {
      el.spark.addEventListener('mousemove', function (ev) {
        var g = el.spark.__geom, box = el.spark.getBoundingClientRect();
        var rel = (ev.clientX - box.left) / box.width * g.W;
        var i = Math.round((rel - g.padL) / ((g.W - g.padL - g.padR) / (g.n - 1)));
        i = Math.max(0, Math.min(g.n - 1, i));
        var cross = el.spark.querySelector('[data-el="cross"]');
        if (cross) { cross.setAttribute('x1', g.x(i)); cross.setAttribute('x2', g.x(i)); cross.style.opacity = 1; }
        var hoursAgo = g.n - 1 - i;
        el.tip.innerHTML = '<b>' + (hoursAgo === 0 ? 'now' : hoursAgo + 'h ago') + '</b>' +
          series.map(function (s) {
            return '<div class="row"><i style="background:' + seriesColor(s.account.slot) + '"></i>' +
              s.account.name + ' <b>' + fmt(s.points[i]) + '</b></div>';
          }).join('');
        var cardBox = el.spark.parentElement.getBoundingClientRect();
        var px = box.left - cardBox.left + (g.x(i) / g.W) * box.width;
        el.tip.style.left = Math.min(Math.max(px - 60, 4), cardBox.width - el.tip.offsetWidth - 4) + 'px';
        el.tip.style.top = (box.top - cardBox.top + 8) + 'px';
        el.tip.style.opacity = 1;
      });
      el.spark.addEventListener('mouseleave', function () {
        el.tip.style.opacity = 0;
        var cross = el.spark.querySelector('[data-el="cross"]');
        if (cross) cross.style.opacity = 0;
      });
    }

    function renderSubnets() {
      var rows = SUBNETS.slice().sort(function (a, b) { return usedRatio(b) - usedRatio(a); }).slice(0, 6);
      el.subnets.innerHTML = rows.map(function (s) {
        var r = usedRatio(s);
        return '<tr><td class="mono">' + s.id + '</td>' +
          '<td class="hide-sm">' + (s.owner || '<span class="dash-chip">no owner</span>') + '</td>' +
          '<td><span class="usage"><span class="track"><span class="fill" style="width:' + (r * 100).toFixed(0) +
          '%;background:' + statusColor(r) + '"></span></span>' + pct(r) + '</span></td>' +
          '<td>' + fmt(s.free) + '</td>' +
          '<td class="hide-sm">' + (s.missing.length
            ? '<span class="dash-chip">' + s.missing.join(', ') + '</span>'
            : '<span class="dash-state ok"><i></i>ok</span>') + '</td></tr>';
      }).join('');
    }

    function renderTargets() {
      el.targets.innerHTML = TARGETS.map(function (t) {
        return '<tr><td class="mono">' + t.account + '</td><td>' + t.region + '</td>' +
          '<td><span class="dash-state ' + (t.ok ? 'ok' : 'bad') + '"><i></i>' +
          (t.ok ? 'synced' : 'AccessDenied') + '</span></td>' +
          '<td class="hide-sm">' + t.subnets + '</td></tr>';
      }).join('');
    }

    function renderFeed() {
      el.feed.innerHTML = feed.map(function (e, i) {
        return '<li class="' + (i === 0 && e.fresh ? 'new' : '') + '"><span class="ev">' + e.name +
          ' · ' + e.account + '/' + e.region + '</span><span class="ago">' + e.ago + 's ago</span></li>';
      }).join('');
    }

    function usd(n) { return '$' + Math.round(n).toLocaleString('en-US'); }

    function envTotal(e) {
      return COST_PARTS.reduce(function (n, p) { return n + e[p.key]; }, 0);
    }

    function renderCosts() {
      var total = ENV_COST.reduce(function (n, e) { return n + envTotal(e); }, 0);
      var byPart = COST_PARTS.map(function (p) {
        return { part: p, sum: ENV_COST.reduce(function (n, e) { return n + e[p.key]; }, 0) };
      }).sort(function (a, b) { return b.sum - a.sum; });
      var idle = IDLE.reduce(function (n, i) { return n + i.usd; }, 0);

      el.costTiles.innerHTML = [
        '<div class="dash-cost-tile"><b>' + usd(total) + '</b><span>estimated monthly network spend</span></div>',
        '<div class="dash-cost-tile"><b>' + byPart[0].part.label + '</b><span>biggest driver · ' +
          Math.round(byPart[0].sum / total * 100) + '% of the bill</span></div>',
        '<div class="dash-cost-tile"><b><em>' + usd(idle) + '</em></b><span>idle spend you can reclaim</span></div>'
      ].join('');

      el.costLegend.innerHTML = COST_PARTS.map(function (p) {
        return '<span><i style="background:' + seriesColor(p.slot) + '"></i>' + p.label + '</span>';
      }).join('');

      var max = Math.max.apply(null, ENV_COST.map(envTotal));
      el.costBars.innerHTML = ENV_COST.map(function (e) {
        var t = envTotal(e);
        var segs = COST_PARTS.map(function (p) {
          return '<span class="seg" title="' + p.label + ' ' + usd(e[p.key]) + '" style="width:' +
            (e[p.key] / t * 100).toFixed(1) + '%;background:' + seriesColor(p.slot) + '"></span>';
        }).join('');
        return '<div class="dash-stack-row"><span>' + e.env + '</span>' +
          '<span class="track" style="width:' + (t / max * 100).toFixed(1) + '%">' + segs + '</span>' +
          '<span class="val">' + usd(t) + '</span></div>';
      }).join('');

      var teamMax = Math.max.apply(null, TEAM_COST.map(function (x) { return x.month; }));
      el.costTeams.innerHTML = TEAM_COST.map(function (x) {
        var untagged = !x.team;
        var up = x.delta >= 0;
        var label = untagged ? 'no owner tag' : x.team;
        var colour = untagged ? 'var(--d-bad)' : seriesColor(1);
        return '<div class="dash-stack-row" title="' + label + ' · ' + x.subnets + ' subnets across ' +
            x.accounts + ' account' + (x.accounts > 1 ? 's' : '') + ' · biggest driver: ' + x.driver + '">' +
          '<span' + (untagged ? ' class="mono" style="color:var(--d-bad)"' : '') + '>' + label + '</span>' +
          '<span class="track" style="width:' + (x.month / teamMax * 100).toFixed(1) + '%">' +
            '<span class="seg" style="width:100%;background:' + colour + '"></span></span>' +
          '<span class="val">' + usd(x.month) +
            ' <span class="dash-delta ' + (up ? 'up' : 'down') + '">' + (up ? '▲' : '▼') +
            ' ' + Math.abs(x.delta).toFixed(1) + '%</span></span>' +
        '</div>';
      }).join('');

      el.costAccounts.innerHTML = ACCOUNT_COST.map(function (a) {
        var up = a.delta >= 0;
        return '<tr><td class="mono">' + a.account + '</td><td class="hide-sm">' + a.name + '</td>' +
          '<td>' + usd(a.month) + '</td>' +
          '<td><span class="dash-delta ' + (up ? 'up' : 'down') + '">' + (up ? '▲' : '▼') + ' ' +
          Math.abs(a.delta).toFixed(1) + '%</span></td>' +
          '<td class="hide-sm">' + a.driver + '</td></tr>';
      }).join('');

      el.costIdle.innerHTML = IDLE.map(function (i) {
        return '<tr><td>' + i.what + '</td><td style="text-align:right"><span class="dash-delta up">' +
          usd(i.usd) + '/mo</span></td></tr>';
      }).join('');
    }

    var draft = null; // the resource currently in the import drawer

    function esc(v) {
      return String(v).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }

    // decide is the auto-import policy: what would the operator do with this resource?
    function decide(r) {
      if (r.tf || POLICY.skip.some(function (k) { return r.by.principal.indexOf(k.match) === 0; })) {
        return { kind: 'skip', text: 'skipped · Terraform-owned' };
      }
      var rule = POLICY.fromCreator.filter(function (k) { return r.by.principal.indexOf(k.match) === 0; })[0];
      if (rule) return { kind: 'auto', owner: rule.owner, text: 'owner from creator rule ' + rule.match + '*' };
      var inh = r.kind === 'subnet' && POLICY.inheritFromVPC[r.vpc];
      if (inh) return { kind: 'auto', owner: inh.owner, env: inh.env, text: 'owner inherited from ' + r.vpc };
      return { kind: 'alert', text: 'no owner rule · alert sent' };
    }

    function agoText(sec) {
      if (sec < 60) return sec + 's ago';
      if (sec < 3600) return Math.round(sec / 60) + 'm ago';
      return Math.round(sec / 3600) + 'h ago';
    }

    function renderUnmanaged() {
      el.unmanagedCount.textContent = UNMANAGED.length ? UNMANAGED.length + ' found' : 'all managed';
      el.unmanaged.innerHTML = UNMANAGED.map(function (r) {
        var d = decide(r);
        var decision;
        if (autoMode === 'off') decision = '<span class="dash-decision muted">manual</span>';
        else if (d.kind === 'skip') decision = '<span class="dash-decision warn">' + esc(d.text) + '</span>';
        else if (d.kind === 'alert') decision = '<span class="dash-decision bad">' + esc(d.text) + '</span>';
        else decision = '<span class="dash-decision ok">' + (autoMode === 'apply' ? 'importing as ' : 'would import as ') + esc(d.owner) + '</span>' +
          '<span class="dash-decision-why">' + esc(d.text) + '</span>';
        var action = (autoMode === 'dryrun' && d.kind === 'auto')
          ? '<button type="button" class="dash-import-btn" data-auto="' + r.id + '">Apply now</button>'
          : '<button type="button" class="dash-import-btn" data-import="' + r.id + '">Import</button>';
        return '<tr data-id="' + r.id + '"><td><span class="dash-kind">' + r.kind + '</span></td>' +
          '<td class="mono">' + r.id + (r.tf ? '<span class="dash-tf" title="carries managed-by=terraform">terraform</span>' : '') +
            '<div class="dash-sub2">' + esc(r.name || 'no name') + ' · ' + esc(r.account) + ' · ' + esc(r.region) + '</div></td>' +
          '<td><span class="dash-who">' + esc(r.by.name) + '</span><div class="dash-sub2 mono">' + esc(r.by.principal) + ' · ' + esc(r.by.via) + ' · ' + agoText(r.ago) + '</div></td>' +
          '<td class="hide-sm mono">' + r.cidr + '</td>' +
          '<td>' + decision + '</td>' +
          '<td style="text-align:right">' + action + '</td></tr>';
      }).join('') || '<tr><td colspan="6"><span class="dash-state ok"><i></i>everything discovery can see is tagged</span></td></tr>';
      renderAutoLog();
    }

    function renderAutoLog() {
      Array.prototype.forEach.call(el.autoMode.querySelectorAll('button'), function (b) {
        b.setAttribute('aria-pressed', String(b.dataset.mode === autoMode));
      });
      if (!autoLog.length) { el.autoLog.innerHTML = ''; return; }
      el.autoLog.innerHTML = '<p class="sub" style="margin:14px 0 6px">Policy log</p><ul class="dash-feed">' + autoLog.slice(0, 4).map(function (e) {
        return '<li class="' + (e.fresh ? 'new' : '') + '"><span class="ev"><span class="dash-decision ' + e.cls + '">' + esc(e.verdict) + '</span> ' +
          esc(e.id) + ' · created by ' + esc(e.who) + '</span><span class="ago">' + e.ago + 's ago</span></li>';
      }).join('') + '</ul>';
    }

    function setAutoMode(mode) {
      autoMode = mode;
      try { localStorage.setItem(AUTO_KEY, mode); } catch (e) { /* private mode */ }
      renderUnmanaged();
      if (mode === 'apply') UNMANAGED.slice().forEach(function (r) { var d = decide(r); if (d.kind === 'auto') autoImport(r, d); });
    }

    // autoImport is what Apply mode does: tag it, log it, let the inventory catch up.
    function autoImport(r, d) {
      UNMANAGED = UNMANAGED.filter(function (x) { return x.id !== r.id; });
      autoLog.unshift({ verdict: 'auto-imported as ' + d.owner, cls: 'ok', id: r.id, who: r.by.name, ago: 0, fresh: true });
      autoLog = autoLog.slice(0, 6);
      feed.unshift({ name: 'CreateTags', account: r.account, region: r.region, ago: 0, fresh: true });
      feed = feed.slice(0, 5);
      TARGETS.forEach(function (t) { if (t.account === r.account && t.region === r.region) t.subnets += (r.kind === 'vpc' ? r.subnets : 1); });
      syncedSecondsAgo = 0;
      el.sync.textContent = 'resynced from an event just now';
    }

    // spawn is the Friday-afternoon subnet: somebody just made one by hand.
    function spawn() {
      spawnCounter++;
      var who = CREATORS[Math.floor(rand() * CREATORS.length)];
      var t = TARGETS[Math.floor(rand() * TARGETS.length)];
      var vpcs = ['vpc-0aa11bb2', 'vpc-0cc3d4e5', 'vpc-0dd4e5f6'];
      var r = {
        kind: 'subnet', id: 'subnet-0' + (Math.floor(rand() * 0xfffffff)).toString(16).padStart(7, '0') + spawnCounter.toString(16).padStart(8, '0').slice(-8),
        name: '', account: t.account, region: t.region,
        cidr: '10.' + (20 + Math.floor(rand() * 40)) + '.' + Math.floor(rand() * 250) + '.0/24',
        vpc: vpcs[Math.floor(rand() * vpcs.length)], tf: who.handle === 'terraform-ci', by: who, ago: 0
      };
      feed.unshift({ name: 'CreateSubnet', account: r.account, region: r.region, ago: 0, fresh: true });
      feed = feed.slice(0, 5);
      var d = decide(r);
      if (autoMode === 'apply' && d.kind === 'auto') { autoImport(r, d); return; }
      UNMANAGED.unshift(r);
      if (d.kind === 'alert') {
        autoLog.unshift({ verdict: 'alert sent', cls: 'bad', id: r.id, who: r.by.name, ago: 0, fresh: true });
        toast('Unmanaged subnet by ' + r.by.name + ' · alert sent to #network-inventory');
      } else if (d.kind === 'skip') {
        autoLog.unshift({ verdict: 'skipped', cls: 'warn', id: r.id, who: r.by.name, ago: 0, fresh: true });
      } else if (autoMode === 'dryrun') {
        autoLog.unshift({ verdict: 'dry run: would import as ' + d.owner, cls: 'ok', id: r.id, who: r.by.name, ago: 0, fresh: true });
      }
      autoLog = autoLog.slice(0, 6);
    }

    function importYAML(r, form) {
      var name = r.id.slice(0, 15) + '-import';
      var lines = [
        ['apiVersion', 'aws.hypersurgery/v1alpha1'], ['kind', 'ResourceImport'],
        ['metadata', null], ['  name', name], ['  namespace', form.namespace],
        ['spec', null], ['  scopeRef', 'organization'], ['  account', '"' + r.account + '"'],
        ['  region', r.region], ['  resourceID', r.id], ['  tags', null],
        ['    hs/managed', '"true"'], ['    hs/owner', form.owner]
      ];
      if (form.env) lines.push(['    hs/env', form.env]);
      if (r.kind === 'subnet' && form.tier) lines.push(['    hs/tier', form.tier]);
      if (form.name) lines.push(['    Name', form.name]);
      lines.push(['  requestedBy', form.requestedBy]);
      return lines.map(function (l) {
        var k = esc(l[0]), v = l[1];
        return v === null ? '<span class="k">' + k + '</span>:' : '<span class="k">' + k + '</span>: <span class="v">' + esc(v) + '</span>';
      }).join('\n');
    }

    function readForm() {
      var g = function (n) { var f = el.drawer.querySelector('[name="' + n + '"]'); return f ? f.value.trim() : ''; };
      return { owner: g('owner'), env: g('env'), tier: g('tier'), name: g('name'), namespace: g('namespace') || 'platform', requestedBy: g('requestedBy') || 'you' };
    }

    function renderDrawer() {
      if (!draft) { el.drawer.innerHTML = ''; return; }
      var r = draft.resource, form = draft.form;
      var opts = function (list, cur) {
        return list.map(function (o) { return '<option' + (o === cur ? ' selected' : '') + '>' + esc(o) + '</option>'; }).join('');
      };
      el.drawer.innerHTML = '<div class="dash-drawer">' +
        '<div><h5>Import ' + esc(r.id) + '</h5><div class="dash-form">' +
          '<label>Owner <select name="owner">' + opts(OWNERS, form.owner) + '</select></label>' +
          '<label>Environment <select name="env">' + opts(['prod', 'staging', 'dev', 'sandbox'], form.env) + '</select></label>' +
          (r.kind === 'subnet' ? '<label>Tier <select name="tier">' + opts(['private', 'public', 'db'], form.tier) + '</select></label>' : '') +
          '<label>Name <input name="name" value="' + esc(form.name) + '" placeholder="Name tag"></label>' +
          '<label>Namespace <input name="namespace" value="' + esc(form.namespace) + '"></label>' +
          '<label>Requested by <input name="requestedBy" value="' + esc(form.requestedBy) + '"></label>' +
          '<div class="dash-actions"><button type="button" class="primary" data-act="apply">Apply import</button>' +
          '<button type="button" data-act="copy">Copy YAML</button><button type="button" data-act="cancel">Cancel</button></div>' +
        '</div></div>' +
        '<div><h5>What gets applied</h5><pre class="dash-yaml" data-el="yaml">' + importYAML(r, form) + '</pre>' +
          '<p class="sub" style="margin:8px 0 0">The operator applies these tags with <code>ec2:CreateTags</code> and nothing else; ' +
          'the resource shows up in the inventory on the next CloudTrail event.</p></div>' +
        (r.tf ? '<div class="dash-warn"><b>Looks Terraform-managed.</b> The next <code>terraform plan</code> will want to remove tags it does not know about. ' +
          'Add them in code, or ignore them: <code>lifecycle { ignore_changes = [tags["hs/managed"], tags["hs/owner"]] }</code></div>' : '') +
        '</div>';
    }

    function toast(msg) {
      var t = document.createElement('div');
      t.className = 'dash-toast'; t.textContent = msg;
      document.body.appendChild(t);
      requestAnimationFrame(function () { t.classList.add('show'); });
      setTimeout(function () { t.classList.remove('show'); setTimeout(function () { t.remove(); }, 300); }, 2600);
    }

    function applyImport() {
      var r = draft.resource, form = readForm();
      var row = el.unmanaged.querySelector('tr[data-id="' + r.id + '"]');
      if (row) row.classList.add('gone');
      UNMANAGED = UNMANAGED.filter(function (x) { return x.id !== r.id; });
      draft = null;
      // The import itself is a tag change, which is exactly what the event feed reports.
      feed.unshift({ name: 'CreateTags', account: r.account, region: r.region, ago: 0, fresh: true });
      feed = feed.slice(0, 5);
      syncedSecondsAgo = 0;
      if (r.kind === 'vpc') { TARGETS.forEach(function (t) { if (t.account === r.account && t.region === r.region) t.subnets += r.subnets; }); }
      else { TARGETS.forEach(function (t) { if (t.account === r.account && t.region === r.region) t.subnets += 1; }); }
      el.sync.textContent = 'resynced from an event just now';
      setTimeout(function () { renderAll(); }, 400);
      toast('Tags applied to ' + r.id + ' as ' + form.owner + ' · inventory resyncing');
    }

    function bindImport() {
      el.autoMode.addEventListener('click', function (e) {
        var b = e.target.closest('button[data-mode]');
        if (b) setAutoMode(b.dataset.mode);
      });
      el.unmanaged.addEventListener('click', function (e) {
        var a = e.target.closest('button[data-auto]');
        if (a) {
          var ar = UNMANAGED.filter(function (x) { return x.id === a.dataset.auto; })[0];
          if (ar) { autoImport(ar, decide(ar)); renderAll(); toast('Imported ' + ar.id + ' by policy'); }
          return;
        }
        var b = e.target.closest('button[data-import]');
        if (!b) return;
        var r = UNMANAGED.filter(function (x) { return x.id === b.dataset.import; })[0];
        if (!r) return;
        var d = decide(r);
        draft = { resource: r, form: { owner: d.owner || OWNERS[0], env: d.env || 'prod', tier: 'private', name: r.name, namespace: 'platform', requestedBy: 'you' } };
        renderDrawer();
        el.drawer.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
      });
      el.drawer.addEventListener('input', function () {
        if (!draft) return;
        draft.form = readForm();
        var y = el.drawer.querySelector('[data-el="yaml"]');
        if (y) y.innerHTML = importYAML(draft.resource, draft.form);
      });
      el.drawer.addEventListener('click', function (e) {
        var b = e.target.closest('button[data-act]');
        if (!b || !draft) return;
        if (b.dataset.act === 'cancel') { draft = null; renderDrawer(); }
        if (b.dataset.act === 'apply') applyImport();
        if (b.dataset.act === 'copy') {
          var text = el.drawer.querySelector('[data-el="yaml"]').textContent;
          if (navigator.clipboard) navigator.clipboard.writeText(text).then(function () { toast('YAML copied'); });
        }
      });
    }

    function renderClaims() {
      var pending = 0, failed = 0;
      CLAIMS.forEach(function (c) {
        c.allocations.forEach(function (a) {
          if (a.state === 'Pending') pending++;
          if (a.state === 'Failed') failed++;
        });
      });
      el.claimCount.textContent = CLAIMS.length + ' claims' + (failed ? ' · ' + failed + ' failed' : '');
      el.claims.innerHTML = CLAIMS.map(function (c) {
        var bad = c.allocations.filter(function (a) { return a.state === 'Failed'; })[0];
        var state = bad
          ? '<span class="dash-state bad"><i></i>Ready=False</span><div class="dash-sub2">' + esc(bad.error) + '</div>'
          : c.mode === 'Allocate'
            ? '<span class="dash-state ok"><i></i>allocated</span><div class="dash-sub2">Terraform creates them</div>'
            : '<span class="dash-state ok"><i></i>created</span>';
        var allocs = c.allocations.map(function (a) {
          var cls = a.state === 'Created' ? 'ok' : a.state === 'Failed' ? 'bad' : 'warn';
          return '<div class="dash-alloc"><span class="dash-decision ' + cls + '">' + esc(a.cidr) + '</span>' +
            '<span class="dash-sub2 mono">' + esc(a.az.slice(-1)) + ' · ' + esc(a.subnet || a.state.toLowerCase()) + '</span></div>';
        }).join('');
        return '<tr><td class="mono">' + esc(c.ns + '/' + c.name) + '<div class="dash-sub2">' + esc(c.owner) + ' · /' + c.prefix + '</div></td>' +
          '<td class="hide-sm mono">' + esc(c.vpc) + '</td>' +
          '<td><span class="dash-decision muted">' + esc(c.mode) + '</span></td>' +
          '<td><div class="dash-allocs">' + allocs + '</div></td>' +
          '<td>' + state + '</td></tr>';
      }).join('');
    }

    function renderAll() {
      renderTiles(); renderBars(); renderSpark(); renderSubnets(); renderTargets(); renderFeed(); renderCosts(); renderUnmanaged(); renderClaims();
    }

    function pushEvent(fresh) {
      var t = TARGETS[Math.floor(rand() * TARGETS.length)];
      feed.unshift({
        name: EVENT_NAMES[Math.floor(rand() * EVENT_NAMES.length)],
        account: t.account, region: t.region, ago: 0, fresh: !!fresh
      });
      feed = feed.slice(0, 5);
    }

    var ticks = 0;
    function tick() {
      ticks++;
      syncedSecondsAgo += 4;
      feed.forEach(function (e) { e.ago += 4; e.fresh = false; });
      autoLog.forEach(function (e) { e.ago += 4; e.fresh = false; });
      UNMANAGED.forEach(function (r) { r.ago += 4; });
      if (ticks % 5 === 0) spawn();
      SUBNETS.forEach(function (s) {
        s.free = Math.max(0, Math.min(s.total, s.free + Math.round((rand() - 0.55) * 4)));
      });
      ENVS.forEach(function (e) { e.used = Math.max(0.05, Math.min(0.97, e.used + (rand() - 0.5) * 0.012)); });
      series.forEach(function (s) {
        var last = s.points[s.points.length - 1];
        s.points.push(Math.max(400, Math.round(last + (rand() - 0.55) * 60)));
        s.points.shift();
      });
      if (rand() > 0.55) { pushEvent(true); syncedSecondsAgo = 0; }
      el.sync.textContent = syncedSecondsAgo === 0
        ? 'resynced from an event just now'
        : 'synced ' + syncedSecondsAgo + 's ago';
      renderAll();
    }

    function start() {
      if (reduced || timer) return;
      timer = setInterval(function () { if (!document.hidden) tick(); }, 4000);
    }
    function stop() { clearInterval(timer); timer = null; }

    for (var i = 0; i < 4; i++) { pushEvent(false); }
    feed.forEach(function (e, idx) { e.ago = 6 + idx * 17; });

    try { var savedMode = localStorage.getItem(AUTO_KEY); if (['off', 'dryrun', 'apply'].indexOf(savedMode) >= 0) autoMode = savedMode; } catch (e) { /* private mode */ }
    applyTheme(initialTheme(options));
    bindSparkHover();
    bindImport();
    // ?import=<resource id> opens the import drawer straight away, for links and screenshots.
    var want = new URLSearchParams(location.search).get('import');
    UNMANAGED.forEach(function (r) {
      if (r.id === want) {
        draft = { resource: r, form: { owner: OWNERS[0], env: 'prod', tier: 'private', name: r.name, namespace: 'platform', requestedBy: 'you' } };
        renderDrawer();
      }
    });

    if (options.modal) {
      el.close.hidden = false;
      el.close.addEventListener('click', function () { if (options.onClose) options.onClose(); });
    }

    return {
      start: start,
      stop: stop,
      element: container,
      installButton: el.install,
      setTheme: applyTheme
    };
  }

  /* Live data ------------------------------------------------------------------
   *
   * Everything below renders objects that came from the cluster. Their names and tags were
   * written in AWS by whoever can create a subnet there, so none of it is ever parsed as
   * HTML: the static frame is markup, every value is a text node built by h().
   */

  var API_GROUP_PATH = '/apis/aws.hypersurgery/v1alpha1/';
  // The only call outside the group: who the API server says the credentials belong to.
  var SELF_SUBJECT_REVIEW_PATH = '/apis/authentication.k8s.io/v1/selfsubjectreviews';
  var DEFAULT_PROXY = 'http://127.0.0.1:8001';
  var TOKEN_KEY = 'subnet-dashboard-token';
  // How often the kinds that are not watched are listed again.
  var REFRESH_MS = 15000;
  var LIVE_RESOURCES = ['networkscopes', 'vpcs', 'subnets', 'subnetclaims', 'resourceimports'];

  // The kinds kept current with a watch; the rest are listed every REFRESH_MS.
  //
  // Every watch holds an HTTP connection open for as long as it runs, and a browser opens at
  // most six HTTP/1.1 connections to one origin — shared by every tab on it. kubectl proxy and
  // a port-forward speak HTTP/1.1, so five watches per tab would leave one connection for
  // everything else, and a second tab would hang. Two per tab leave room for the polls, an
  // import, and a second tab; a hidden tab closes its watches (see mountLive).
  //
  // The two are the ones a watch does the most for. Subnets are by far the largest list, and
  // polling re-downloads all of them every time; a watch sends only what changed. Imports are
  // what this page creates, and the viewer is waiting to see Pending turn into Applied. Scopes
  // are few, VPCs change rarely, and claims are made elsewhere and progress at AWS's pace, so
  // a list every 15 seconds serves them as well.
  var WATCHED_RESOURCES = ['subnets', 'resourceimports'];
  // The API server ends a watch after this long, a little randomised so that tabs opened
  // together do not all reconnect at once. Ending it is routine: the next one resumes from the
  // last resourceVersion seen.
  var WATCH_TIMEOUT_S = 300;
  // A watch that ends or breaks sooner than this without an event counts as a failure, and so
  // does one that cannot be opened. One that lasted longer does not: a proxy in front that cuts
  // quiet connections after a minute is routine, not a reason to give up. Retries back off up
  // to WATCH_MAX_BACKOFF_MS; after WATCH_MAX_FAILURES in a row the kind is polled instead.
  var WATCH_HEALTHY_MS = 30000;
  var WATCH_MAX_BACKOFF_MS = 30000;
  var WATCH_MAX_FAILURES = 5;

  // sourceFromPage decides where the data comes from. The dashboard container marks its page
  // with a meta tag, and nothing in the URL can change that: its token must only ever go to
  // its own origin. Elsewhere the default is demo data, and ?source=kubectl (or ?api=<url>)
  // reads a cluster through kubectl proxy instead, which holds the credentials itself.
  function sourceFromPage(doc, loc) {
    var meta = doc.querySelector('meta[name="subnet-dashboard-source"]');
    if (meta && meta.getAttribute('content') === 'cluster') return { mode: 'cluster', base: '' };
    var q = new URLSearchParams(loc.search);
    var api = q.get('api');
    if (!api && q.get('source') !== 'kubectl') return { mode: 'demo' };
    var u;
    try { u = new URL(api || DEFAULT_PROXY); } catch (e) { return { mode: 'demo' }; }
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return { mode: 'demo' };
    var base = u.origin + u.pathname.replace(/\/+$/, '');
    return { mode: 'kubectl', base: u.origin === loc.origin && base === u.origin ? '' : base };
  }

  function readToken() {
    try { return sessionStorage.getItem(TOKEN_KEY) || ''; } catch (e) { return ''; }
  }
  function writeToken(t) {
    try {
      if (t) sessionStorage.setItem(TOKEN_KEY, t); else sessionStorage.removeItem(TOKEN_KEY);
    } catch (e) { /* private mode: the token then lives only as long as this call */ }
  }

  function apiError(status, data) {
    var err = new Error((data && typeof data.message === 'string' && data.message) || ('HTTP ' + status));
    err.code = status;
    return err;
  }

  // apiClient talks to the API server the source names. In the cluster the token comes from
  // this tab (pasted by the viewer) or from an SSO proxy in front of the dashboard, which adds
  // the header itself; kubectl proxy adds its own credentials, so the browser sends none.
  function apiClient(source, fetchImpl) {
    fetchImpl = fetchImpl || global.fetch.bind(global);
    function init(method, body) {
      var headers = { 'Accept': 'application/json' };
      var i = { method: method, headers: headers, cache: 'no-store', redirect: 'error' };
      if (source.mode === 'cluster') {
        i.credentials = 'same-origin';
        var t = readToken();
        if (t) headers.Authorization = 'Bearer ' + t;
      } else {
        i.credentials = 'omit';
      }
      if (body !== undefined) {
        headers['Content-Type'] = 'application/json';
        // Proves to the dashboard that the write comes from its own page; see proxy.go.
        headers['X-Subnet-Dashboard'] = '1';
        i.body = JSON.stringify(body);
      }
      return i;
    }
    function parse(text) {
      try { return text ? JSON.parse(text) : null; } catch (e) { return null; }
    }
    function request(method, path, body) {
      return fetchImpl(source.base + path, init(method, body)).then(function (res) {
        return res.text().then(function (text) {
          var data = parse(text);
          if (!res.ok) throw apiError(res.status, data);
          return data;
        });
      });
    }

    // watch streams the changes to one kind from resourceVersion on, calling onEvent with each
    // ADDED, MODIFIED, DELETED and BOOKMARK event. It resolves when the API server ends the
    // stream, and rejects with the HTTP status as err.code — or the code of an ERROR event, so
    // "410 Gone" arrives the same way whichever form the API server chose. err.noStream says
    // the browser cannot read a response as it arrives, and a watch is no use here.
    function watch(resource, resourceVersion, onEvent, signal) {
      var i = init('GET');
      if (signal) i.signal = signal;
      var timeout = WATCH_TIMEOUT_S + Math.floor(Math.random() * 60);
      var path = API_GROUP_PATH + resource + '?watch=1&allowWatchBookmarks=true' +
        '&resourceVersion=' + encodeURIComponent(resourceVersion) + '&timeoutSeconds=' + timeout;
      return fetchImpl(source.base + path, i).then(function (res) {
        if (!res.ok) {
          return res.text().then(function (text) { throw apiError(res.status, parse(text)); });
        }
        if (!res.body || typeof res.body.getReader !== 'function' || typeof global.TextDecoder !== 'function') {
          var e = new Error('this browser cannot stream a response');
          e.noStream = true;
          throw e;
        }
        var reader = res.body.getReader();
        var decoder = new global.TextDecoder();
        var buffered = '';
        function line(text) {
          if (!text.trim()) return;
          var ev = parse(text);
          if (!ev || typeof ev.type !== 'string') throw new Error('the watch sent something that is not an event');
          if (ev.type === 'ERROR') throw apiError(num(obj(ev.object).code) || 500, obj(ev.object));
          onEvent({ type: ev.type, object: obj(ev.object) });
        }
        function pump() {
          return reader.read().then(function (chunk) {
            if (chunk.done) {
              line(buffered + decoder.decode());
              return;
            }
            // One event per line; a line may arrive split over chunks.
            var lines = (buffered + decoder.decode(chunk.value, { stream: true })).split('\n');
            buffered = lines.pop();
            lines.forEach(line);
            return pump();
          });
        }
        return pump().catch(function (err) {
          try { reader.cancel(); } catch (e) { /* already closed */ }
          throw err;
        });
      });
    }

    return {
      list: function (resource) { return request('GET', API_GROUP_PATH + resource); },
      watch: watch,
      create: function (namespace, resource, obj) {
        return request('POST', API_GROUP_PATH + 'namespaces/' + encodeURIComponent(namespace) + '/' + resource, obj);
      },
      // whoami asks the API server who these credentials are. It is a POST that changes
      // nothing, sent with the same header as a write because the dashboard holds every POST
      // to the same rule.
      whoami: function () {
        return request('POST', SELF_SUBJECT_REVIEW_PATH, { apiVersion: 'authentication.k8s.io/v1', kind: 'SelfSubjectReview' })
          .then(function (d) { return str(obj(obj(obj(d).status).userInfo).username); });
      }
    };
  }

  function objectKey(o) {
    var md = obj(obj(o).metadata);
    return str(md.namespace) + '/' + str(md.name);
  }

  // liveSync keeps data — { <resource>: { items, error } } — current. load(kinds) lists
  // kinds, one request each, so a kind the viewer may not read leaves only its own panel empty
  // instead of the whole page; it resolves with the errors by kind, or with null when reset()
  // came first. A watched kind that listed fine then streams its changes from
  // the list's resourceVersion:
  //
  // - BOOKMARK moves the resourceVersion on without changing anything, so the next watch
  //   resumes from there rather than from an old version the API server may have forgotten.
  // - 410 Gone means it has forgotten it: the kind is listed again and watched from the new
  //   list.
  // - A stream that ends is opened again from the last version seen, at once after a healthy
  //   one, with a growing delay after failures.
  // - A watch that cannot work — the browser cannot stream, RBAC allows list but not watch,
  //   or it keeps failing — leaves the kind to the polls: needed() lists every kind that is
  //   not being watched right now, which is what the caller refreshes every REFRESH_MS.
  //
  // opts.onChange(resource) is called when a watch changed data. Timers and AbortController
  // can be passed in for the tests.
  function liveSync(api, opts) {
    opts = opts || {};
    var watched = opts.watched || WATCHED_RESOURCES;
    var later = opts.setTimeout || function (f, ms) { return setTimeout(f, ms); };
    var cancel = opts.clearTimeout || function (t) { clearTimeout(t); };
    var now = opts.now || function () { return Date.now(); };
    var Abort = opts.AbortController || global.AbortController;
    var onChange = opts.onChange || function () {};
    var data = {};
    var kinds = {};
    // generation changes on reset; answers to requests from before then are dropped.
    var generation = 0;

    function reset() {
      generation++;
      LIVE_RESOURCES.forEach(function (r) {
        var k = kinds[r];
        if (k) halt(k);
        kinds[r] = { store: {}, rv: '', running: false, paused: false, listing: false, failures: 0, pollOnly: false, controller: null, timer: null };
        data[r] = { items: [], error: null };
      });
    }

    function halt(k) {
      if (k.timer !== null) cancel(k.timer);
      k.timer = null;
      if (k.controller) k.controller.abort();
      k.controller = null;
      k.running = false;
    }

    function publish(r) {
      var store = kinds[r].store;
      data[r] = { items: Object.keys(store).map(function (key) { return store[key]; }), error: null };
    }

    function canWatch(r) {
      var k = kinds[r];
      return watched.indexOf(r) >= 0 && !k.pollOnly && !k.running && !k.paused && !!k.rv;
    }

    function load(list) {
      var g = generation;
      list.forEach(function (r) { kinds[r].listing = true; });
      return Promise.all(list.map(function (r) {
        return api.list(r).then(function (d) {
          if (g !== generation) return null;
          var k = kinds[r];
          k.listing = false;
          k.store = {};
          arr(obj(d).items).forEach(function (o) { k.store[objectKey(o)] = o; });
          k.rv = str(obj(obj(d).metadata).resourceVersion);
          publish(r);
          if (canWatch(r)) startWatch(r, 0);
          return null;
        }, function (e) {
          if (g !== generation) return null;
          kinds[r].listing = false;
          data[r] = { items: [], error: e };
          return e;
        });
      })).then(function (errors) {
        // Listed for a sign-in that has since been reset: nothing here applies any more.
        if (g !== generation) return null;
        var out = {};
        list.forEach(function (r, i) { if (errors[i]) out[r] = errors[i]; });
        return out;
      });
    }

    function startWatch(r, delay) {
      var k = kinds[r], g = generation;
      k.running = true;
      k.timer = later(function () {
        k.timer = null;
        if (g !== generation) return;
        var controller = Abort ? new Abort() : null;
        var started = now(), events = 0;
        k.controller = controller;
        api.watch(r, k.rv, function (ev) {
          if (g !== generation || k.controller !== controller) return;
          events++;
          apply(r, ev);
        }, controller ? controller.signal : undefined).then(function () {
          if (g !== generation || k.controller !== controller) return;
          k.controller = null;
          k.failures = events > 0 || now() - started >= WATCH_HEALTHY_MS ? 0 : k.failures + 1;
          retry(r);
        }, function (err) {
          // A watch this code aborted (hidden tab, sign-out) is no longer the current one.
          if (g !== generation || k.controller !== controller) return;
          k.controller = null;
          failed(r, err, events > 0 || now() - started >= WATCH_HEALTHY_MS);
        });
      }, delay);
    }

    function apply(r, ev) {
      var k = kinds[r], o = ev.object, rv = str(obj(o.metadata).resourceVersion);
      if (ev.type === 'ADDED' || ev.type === 'MODIFIED') k.store[objectKey(o)] = o;
      else if (ev.type === 'DELETED') delete k.store[objectKey(o)];
      else if (ev.type !== 'BOOKMARK') return;
      if (rv) k.rv = rv;
      if (ev.type === 'BOOKMARK') return;
      publish(r);
      onChange(r);
    }

    function retry(r) {
      var k = kinds[r];
      if (k.failures >= WATCH_MAX_FAILURES) { fallBack(r); return; }
      startWatch(r, k.failures ? Math.min(WATCH_MAX_BACKOFF_MS, 1000 * Math.pow(2, k.failures - 1)) : 0);
    }

    function fallBack(r) {
      halt(kinds[r]);
      kinds[r].pollOnly = true;
    }

    function failed(r, err, healthy) {
      var k = kinds[r];
      k.running = false;
      if (err.code === 410) {
        k.failures = 0;
        k.rv = '';
        load([r]).then(function (errors) { if (!errors[r]) onChange(r); });
        return;
      }
      if (err.noStream) { watched.forEach(fallBack); return; }
      // An expired token: the next poll lists the kind, reports the 401, and watches again
      // once a list works.
      if (err.code === 401) return;
      // The API server, or a proxy in between, will not watch this: polling still works.
      if (err.code === 400 || err.code === 403 || err.code === 404 || err.code === 405) { fallBack(r); return; }
      k.failures = healthy ? 1 : k.failures + 1;
      retry(r);
    }

    reset();
    return {
      data: data,
      load: load,
      // needed lists the kinds a poll has to fetch: all of them except those being watched.
      needed: function () {
        return LIVE_RESOURCES.filter(function (r) { return !kinds[r].running && !kinds[r].listing; });
      },
      watching: function () {
        return watched.filter(function (r) { return kinds[r].running || (kinds[r].paused && !kinds[r].pollOnly); });
      },
      // pause closes the watches, to give their connections back while nobody looks.
      pause: function () {
        watched.forEach(function (r) {
          var k = kinds[r];
          if (!k.running) return;
          halt(k);
          k.paused = true;
        });
      },
      // resume opens them again from where they were; a version the API server has forgotten
      // in the meantime comes back as 410 and a fresh list.
      resume: function () {
        watched.forEach(function (r) {
          var k = kinds[r];
          if (!k.paused) return;
          k.paused = false;
          if (canWatch(r)) startWatch(r, 0);
        });
      },
      reset: reset
    };
  }

  // renderWho shows who the API server says the viewer is. The user name comes from the
  // identity provider and is written as text like every other value from the cluster.
  function renderWho(node, source, username) {
    node.hidden = !username;
    node.textContent = username ? (source.mode === 'cluster' ? 'signed in as ' : 'kubectl proxy as ') + username : '';
    node.setAttribute('title', source.mode === 'cluster'
      ? 'The user the Kubernetes API server authenticated your token as. What you see and import is up to this user\'s RBAC.'
      : 'kubectl proxy sends every request with the credentials of its own kubeconfig, so this is the proxy\'s identity: ' +
        'anything that can reach the proxy reads the cluster as this user.');
  }

  function str(v) { return typeof v === 'string' ? v : (v === undefined || v === null ? '' : String(v)); }
  function num(v) { return typeof v === 'number' && isFinite(v) ? v : 0; }
  function obj(v) { return v && typeof v === 'object' && !Array.isArray(v) ? v : {}; }
  function arr(v) { return Array.isArray(v) ? v : []; }

  function readyCondition(o) {
    return arr(obj(o.status).conditions).filter(function (c) { return obj(c).type === 'Ready'; })[0] || null;
  }

  // summarize turns the API lists into what the panels show. It is pure, so it is tested on
  // its own (hack/dashboard/dashboard.test.js).
  function summarize(data) {
    var scopes = arr(obj(data.networkscopes).items);
    var vpcs = arr(obj(data.vpcs).items);
    var subnetItems = arr(obj(data.subnets).items);
    var imports = arr(obj(data.resourceimports).items);

    var subnets = subnetItems.map(function (s) {
      var spec = obj(s.spec), st = obj(s.status);
      var total = num(st.totalIPs), free = num(st.availableIPs);
      return {
        id: str(spec.subnetID) || str(obj(s.metadata).name), name: str(st.name),
        account: str(spec.account), region: str(spec.region), vpc: str(spec.vpcID),
        cidr: str(st.cidrBlock), owner: str(st.owner), env: str(st.env),
        total: total, free: free, used: total > 0 ? 1 - free / total : 0,
        missing: arr(st.missingTags).map(str)
      };
    });

    var byEnv = {};
    subnets.forEach(function (s) {
      var k = s.env || 'untagged';
      byEnv[k] = byEnv[k] || { env: k, total: 0, free: 0 };
      byEnv[k].total += s.total; byEnv[k].free += s.free;
    });
    var envs = Object.keys(byEnv).map(function (k) {
      var e = byEnv[k];
      return { env: e.env, used: e.total > 0 ? 1 - e.free / e.total : 0 };
    }).sort(function (a, b) { return b.used - a.used; });

    var targets = [];
    var unmanaged = [];
    // An import that is pending or applied stands for its resource until the next sync drops
    // it from the unmanaged list; one that failed leaves the resource importable again.
    var importing = {};
    imports.forEach(function (i) {
      var state = str(obj(i.status).state);
      if (state !== 'Failed') importing[str(obj(i.spec).resourceID)] = state || 'Pending';
    });
    scopes.forEach(function (sc) {
      var name = str(obj(sc.metadata).name);
      arr(obj(sc.status).targets).forEach(function (t) {
        t = obj(t);
        targets.push({
          scope: name, account: str(t.account), region: str(t.region),
          subnets: num(t.subnets), vpcs: num(t.vpcs), error: str(t.error), synced: !!t.lastSyncTime
        });
        arr(t.unmanagedIDs).forEach(function (id) {
          id = str(id);
          unmanaged.push({
            kind: id.indexOf('vpc-') === 0 ? 'vpc' : 'subnet', id: id, scope: name,
            account: str(t.account), region: str(t.region), importing: importing[id] || ''
          });
        });
      });
    });

    var claims = arr(obj(data.subnetclaims).items).map(function (c) {
      var spec = obj(c.spec), md = obj(c.metadata), ready = readyCondition(c);
      return {
        ns: str(md.namespace), name: str(md.name), vpc: str(spec.vpcID), prefix: num(spec.prefixLength),
        mode: str(spec.mode) || 'Create', owner: str(spec.owner),
        ready: ready ? str(ready.status) : '', message: ready ? str(ready.message) : '',
        allocations: arr(obj(c.status).allocations).map(function (a) {
          a = obj(a);
          return { az: str(a.availabilityZone), cidr: str(a.cidrBlock), subnet: str(a.subnetID), state: str(a.state) || 'Pending', error: str(a.error) };
        })
      };
    });

    var recentImports = imports.map(function (i) {
      var md = obj(i.metadata), spec = obj(i.spec), st = obj(i.status);
      return {
        ns: str(md.namespace), name: str(md.name), created: str(md.creationTimestamp),
        // createdBy is the user the API server authenticated, written by the operator's webhook;
        // requestedBy is free text anybody applying the import can fill in, so it is shown as
        // "for", never as the person who did it.
        createdBy: str(obj(md.annotations)['aws.hypersurgery/created-by']) || 'unknown',
        resource: str(spec.resourceID), requestedBy: str(spec.requestedBy), dryRun: !!spec.dryRun,
        state: str(st.state) || 'Pending', error: str(st.error)
      };
    }).sort(function (a, b) { return a.created < b.created ? 1 : a.created > b.created ? -1 : 0; }).slice(0, 8);

    return {
      tiles: {
        subnets: subnets.length,
        free: subnets.reduce(function (n, s) { return n + s.free; }, 0),
        hot: subnets.filter(function (s) { return s.used >= 0.8; }).length,
        missing: subnets.filter(function (s) { return s.missing.length; }).length,
        overlapping: vpcs.filter(function (v) { return arr(obj(v.status).overlapsWith).length; }).length,
        down: targets.filter(function (t) { return t.error; }).length
      },
      envs: envs,
      fullest: subnets.slice().sort(function (a, b) { return b.used - a.used; }).slice(0, 8),
      targets: targets,
      unmanaged: unmanaged,
      claims: claims,
      imports: recentImports,
      scopes: scopes
    };
  }

  // importObject is the ResourceImport the Import button creates. requestedBy is left out on
  // purpose: the operator's webhook fills it in with the user the API server authenticated,
  // which the viewer cannot type wrong or claim to be somebody else.
  function importObject(resource, form, scope) {
    var spec = obj(obj(scope).spec);
    var keys = obj(spec.tagKeys);
    var tags = {};
    if (resource.kind === 'vpc') {
      // Discovery picks a VPC up by the scope's tag selector, so those are the tags that make
      // it managed. An empty selector value matches any value; "true" is as good as any.
      var sel = obj(spec.vpcTagSelector);
      Object.keys(sel).forEach(function (k) { tags[k] = str(sel[k]) || 'true'; });
      if (!Object.keys(sel).length) tags['hs/managed'] = 'true';
    }
    if (form.owner) tags[str(keys.owner) || 'hs/owner'] = form.owner;
    if (form.env) tags[str(keys.env) || 'hs/env'] = form.env;
    if (resource.kind === 'subnet' && form.tier) tags[str(keys.tier) || 'hs/tier'] = form.tier;
    if (form.name) tags.Name = form.name;
    var o = {
      apiVersion: 'aws.hypersurgery/v1alpha1',
      kind: 'ResourceImport',
      metadata: { generateName: resource.id + '-', namespace: form.namespace },
      spec: {
        scopeRef: resource.scope, account: resource.account, region: resource.region,
        resourceID: resource.id, tags: tags
      }
    };
    if (form.dryRun) o.spec.dryRun = true;
    return o;
  }

  // toYAML prints importObject's output for the preview. Every scalar is JSON-quoted, which
  // is valid YAML and leaves no room for a value to change the structure around it.
  function toYAML(v, indent) {
    indent = indent || '';
    return Object.keys(v).map(function (k) {
      var x = v[k];
      if (x && typeof x === 'object') return indent + k + ':\n' + toYAML(x, indent + '  ');
      return indent + k + ': ' + JSON.stringify(x);
    }).join('\n');
  }

  // h builds an element. Strings among the children become text nodes, and attributes are
  // limited to ones that cannot carry script or a URL.
  var SAFE_ATTRS = /^(class|title|type|name|value|placeholder|role|colspan|for|id|autocomplete|spellcheck|rows|data-[a-z-]+|aria-[a-z-]+)$/;
  function h(tag, attrs, children) {
    var node = document.createElement(tag);
    Object.keys(attrs || {}).forEach(function (k) {
      if (!SAFE_ATTRS.test(k)) throw new Error('h: attribute ' + k + ' is not allowed');
      if (attrs[k] !== null && attrs[k] !== undefined && attrs[k] !== false) node.setAttribute(k, String(attrs[k]));
    });
    (children || []).forEach(function (c) {
      if (c === null || c === undefined || c === false) return;
      node.appendChild(typeof c === 'object' ? c : document.createTextNode(String(c)));
    });
    return node;
  }
  function fill(parent, nodes) {
    while (parent.firstChild) parent.removeChild(parent.firstChild);
    nodes.forEach(function (n) { parent.appendChild(n); });
  }
  // meter is a bar whose width and colour are set through the CSSOM, which the dashboard's
  // Content-Security-Policy allows where an inline style attribute would be blocked.
  function meter(cls, ratio, colour) {
    var f = h('span', { 'class': 'fill' });
    f.style.width = (Math.max(0, Math.min(1, ratio)) * 100).toFixed(1) + '%';
    f.style.background = colour;
    return h('span', { 'class': cls }, [f]);
  }
  function state(ok, text) { return h('span', { 'class': 'dash-state ' + (ok ? 'ok' : 'bad') }, [h('i'), text]); }
  function emptyRow(cols, text) { return h('tr', {}, [h('td', { colspan: cols, 'class': 'dash-empty' }, [text])]); }
  function deniedRow(cols, err) {
    var what = err.code === 403 ? 'Your account may not list these: ' : 'Could not load: ';
    return h('tr', {}, [h('td', { colspan: cols, 'class': 'dash-empty bad' }, [what + err.message])]);
  }

  var LIVE_TEMPLATE =
    '<div class="dash-head">' +
      '<h3>Subnet inventory</h3>' +
      '<span class="dash-badge live"><i></i><span data-el="badge">Live</span></span>' +
      '<span class="dash-sync" data-el="sync">loading…</span>' +
      '<span class="dash-sync dash-who" data-el="who" hidden></span>' +
      '<div class="dash-tools">' +
        '<button class="dash-install" type="button" data-el="signout" hidden>Forget token</button>' +
        '<div class="dash-themes" role="group" aria-label="Colour theme" data-el="themes"></div>' +
        '<button class="dash-install" type="button" data-el="install" hidden>Install app</button>' +
        '<button class="dash-close" type="button" data-el="close" aria-label="Close the dashboard" hidden>' +
          '<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><path d="M6 6l12 12M18 6L6 18"/></svg>' +
        '</button>' +
      '</div>' +
    '</div>' +
    '<div class="dash-body">' +
      '<form class="dash-card dash-auth" data-el="auth" hidden>' +
        '<h4>Sign in with your Kubernetes token</h4>' +
        '<p class="sub">The dashboard reads the cluster as you and has no access of its own. Paste a token the API server accepts — ' +
          'an OIDC ID token, or <code>kubectl create token &lt;service-account&gt;</code> — or open the dashboard through your single sign-on. ' +
          'The token stays in this tab only and is gone when you close it.</p>' +
        '<div class="dash-form"><label>Token <input name="token" type="password" autocomplete="off" spellcheck="false"></label>' +
        '<div class="dash-actions"><button type="submit" class="primary">Use token</button></div></div>' +
        '<p class="sub dash-error" data-el="authError"></p>' +
      '</form>' +
      '<p class="dash-banner" data-el="banner" hidden></p>' +
      '<div class="dash-tiles" data-el="tiles"></div>' +
      '<div class="dash-grid">' +
        '<div class="dash-card">' +
          '<h4>IPv4 utilization by environment</h4>' +
          '<p class="sub">Share of usable addresses in use, by the environment tag. Alert at 80%.</p>' +
          '<div class="dash-bars" data-el="bars"></div>' +
        '</div>' +
        '<div class="dash-card">' +
          '<h4>Accounts and regions</h4>' +
          '<p class="sub">Discovery state per NetworkScope target.</p>' +
          '<table><thead><tr><th class="mono">Account</th><th>Region</th><th>State</th><th class="hide-sm">Subnets</th></tr></thead>' +
          '<tbody data-el="targets"></tbody></table>' +
        '</div>' +
      '</div>' +
      '<div class="dash-card">' +
        '<h4>Fullest subnets</h4>' +
        '<p class="sub">The subnets closest to running out of addresses.</p>' +
        '<table><thead><tr><th class="mono">Subnet</th><th class="hide-sm">Owner</th><th>Used</th><th>Free</th><th class="hide-sm">Tags</th></tr></thead>' +
        '<tbody data-el="subnets"></tbody></table>' +
      '</div>' +
      '<div class="dash-card dash-unmanaged">' +
        '<h4>Unmanaged resources <span class="dash-count" data-el="unmanagedCount"></span></h4>' +
        '<p class="sub">Discovered, but outside every scope\'s tag selector. Import creates a ResourceImport in your name; the operator then tags the resource.</p>' +
        '<table><thead><tr><th></th><th class="mono">Resource</th><th class="hide-sm">Scope</th><th></th></tr></thead>' +
        '<tbody data-el="unmanaged"></tbody></table>' +
        '<div data-el="drawer"></div>' +
      '</div>' +
      '<div class="dash-grid">' +
        '<div class="dash-card">' +
          '<h4>Subnet claims <span class="dash-count" data-el="claimCount"></span></h4>' +
          '<p class="sub">Capacity teams asked for, and how far the operator got.</p>' +
          '<table><thead><tr><th class="mono">Claim</th><th>Allocations</th><th>State</th></tr></thead>' +
          '<tbody data-el="claims"></tbody></table>' +
        '</div>' +
        '<div class="dash-card">' +
          '<h4>Recent imports</h4>' +
          '<p class="sub">ResourceImports, newest first: who created them, and for whom.</p>' +
          '<table><thead><tr><th class="mono">Resource</th><th class="hide-sm">Created by</th><th>State</th></tr></thead>' +
          '<tbody data-el="imports"></tbody></table>' +
        '</div>' +
      '</div>' +
    '</div>' +
    '<div class="dash-foot" data-el="foot"></div>';

  // renderLive draws a summary into the panels. Kept apart from mountLive so it can be run
  // against a stand-in DOM in the tests.
  function renderLive(el, view, data, colours) {
    var t = view.tiles;
    fill(el.tiles, [
      { v: fmt(t.subnets), l: 'subnets tracked', c: '' },
      { v: fmt(t.free), l: 'free IPv4 addresses', c: '' },
      { v: fmt(t.hot), l: 'at 80% or more', c: t.hot ? 'warn' : 'ok' },
      { v: fmt(t.missing), l: 'missing required tags', c: t.missing ? 'warn' : 'ok' },
      { v: fmt(t.overlapping), l: 'VPCs with overlapping CIDRs', c: t.overlapping ? 'bad' : 'ok' },
      { v: fmt(t.down), l: 'unreachable targets', c: t.down ? 'bad' : 'ok' }
    ].map(function (x) { return h('div', { 'class': 'dash-tile ' + x.c }, [h('b', {}, [x.v]), h('span', {}, [x.l])]); }));

    fill(el.bars, data.subnets.error
      ? [h('p', { 'class': 'sub bad' }, [data.subnets.error.message])]
      : view.envs.length ? view.envs.map(function (e) {
        return h('div', { 'class': 'dash-bar' }, [
          h('span', {}, [e.env]), meter('track', e.used, colours.status(e.used)), h('span', { 'class': 'val' }, [pct(e.used)])
        ]);
      }) : [h('p', { 'class': 'sub' }, ['No subnets to show yet.'])]);

    fill(el.targets, data.networkscopes.error ? [deniedRow(4, data.networkscopes.error)]
      : view.targets.length ? view.targets.map(function (x) {
        return h('tr', {}, [
          h('td', { 'class': 'mono' }, [x.account, h('div', { 'class': 'dash-sub2' }, ['scope ' + x.scope])]),
          h('td', {}, [x.region]),
          h('td', { title: x.error || null }, [x.error ? state(false, 'error') : state(true, x.synced ? 'synced' : 'waiting')]),
          h('td', { 'class': 'hide-sm' }, [fmt(x.subnets)])
        ]);
      }) : [emptyRow(4, 'No NetworkScope has synced a target yet.')]);

    fill(el.subnets, data.subnets.error ? [deniedRow(5, data.subnets.error)]
      : view.fullest.length ? view.fullest.map(function (s) {
        return h('tr', {}, [
          h('td', { 'class': 'mono' }, [s.id, h('div', { 'class': 'dash-sub2' }, [(s.name || 'no name') + ' · ' + s.account + ' · ' + s.region])]),
          h('td', { 'class': 'hide-sm' }, [s.owner || h('span', { 'class': 'dash-chip' }, ['no owner'])]),
          h('td', {}, [h('span', { 'class': 'usage' }, [meter('track', s.used, colours.status(s.used)), pct(s.used)])]),
          h('td', {}, [fmt(s.free)]),
          h('td', { 'class': 'hide-sm' }, [s.missing.length
            ? h('span', { 'class': 'dash-chip' }, [s.missing.join(', ')])
            : state(true, 'ok')])
        ]);
      }) : [emptyRow(5, 'No subnets to show yet.')]);

    el.unmanagedCount.textContent = data.networkscopes.error ? '' : view.unmanaged.length ? view.unmanaged.length + ' found' : 'all managed';
    fill(el.unmanaged, data.networkscopes.error ? [deniedRow(4, data.networkscopes.error)]
      : view.unmanaged.length ? view.unmanaged.map(function (r) {
        return h('tr', { 'data-id': r.id }, [
          h('td', {}, [h('span', { 'class': 'dash-kind' }, [r.kind])]),
          h('td', { 'class': 'mono' }, [r.id, h('div', { 'class': 'dash-sub2' }, [r.account + ' · ' + r.region])]),
          h('td', { 'class': 'hide-sm' }, [r.scope]),
          h('td', { 'class': 'dash-right' }, [r.importing
            ? h('span', { 'class': 'dash-decision muted' }, ['import ' + r.importing.toLowerCase()])
            : h('button', { type: 'button', 'class': 'dash-import-btn', 'data-import': r.id }, ['Import'])])
        ]);
      }) : [emptyRow(4, 'Everything discovery can see is managed.')]);

    el.claimCount.textContent = data.subnetclaims.error ? '' : view.claims.length + ' claims';
    fill(el.claims, data.subnetclaims.error ? [deniedRow(3, data.subnetclaims.error)]
      : view.claims.length ? view.claims.map(function (c) {
        var bad = c.allocations.filter(function (a) { return a.state === 'Failed'; })[0];
        return h('tr', {}, [
          h('td', { 'class': 'mono' }, [c.ns + '/' + c.name, h('div', { 'class': 'dash-sub2' }, [c.owner + ' · ' + c.vpc + ' · /' + c.prefix + ' · ' + c.mode])]),
          h('td', {}, [h('div', { 'class': 'dash-allocs' }, c.allocations.map(function (a) {
            var cls = a.state === 'Created' ? 'ok' : a.state === 'Failed' ? 'bad' : 'warn';
            return h('div', { 'class': 'dash-alloc' }, [
              h('span', { 'class': 'dash-decision ' + cls }, [a.cidr]),
              h('span', { 'class': 'dash-sub2 mono' }, [a.az.slice(-1) + ' · ' + (a.subnet || a.state.toLowerCase())])
            ]);
          }))]),
          h('td', {}, [c.ready === 'True' ? state(true, 'ready') : state(false, c.ready ? 'Ready=' + c.ready : 'pending'),
            (bad || c.message) ? h('div', { 'class': 'dash-sub2' }, [bad ? bad.error : c.message]) : null])
        ]);
      }) : [emptyRow(3, 'No SubnetClaims.')]);

    fill(el.imports, data.resourceimports.error ? [deniedRow(3, data.resourceimports.error)]
      : view.imports.length ? view.imports.map(function (i) {
        var cls = i.state === 'Applied' ? 'ok' : i.state === 'Failed' ? 'bad' : i.state === 'Skipped' ? 'muted' : 'warn';
        return h('tr', {}, [
          h('td', { 'class': 'mono' }, [i.resource, h('div', { 'class': 'dash-sub2' }, [i.ns + '/' + i.name])]),
          h('td', { 'class': 'hide-sm' }, [i.createdBy,
            i.requestedBy && i.requestedBy !== i.createdBy ? h('div', { 'class': 'dash-sub2' }, ['for ' + i.requestedBy]) : null]),
          h('td', { title: i.error || null }, [h('span', { 'class': 'dash-decision ' + cls }, [i.dryRun ? i.state + ' · dry run' : i.state])])
        ]);
      }) : [emptyRow(3, 'No ResourceImports.')]);
  }

  // renderDrawer draws the import form for one unmanaged resource.
  function renderLiveDrawer(host, draft) {
    if (!draft) { fill(host, []); return; }
    var r = draft.resource, f = draft.form;
    function field(label, name, value, placeholder) {
      return h('label', {}, [label, h('input', { name: name, value: value, placeholder: placeholder || null, autocomplete: 'off', spellcheck: 'false' })]);
    }
    var dry = h('input', { name: 'dryRun', type: 'checkbox' });
    dry.checked = !!f.dryRun;
    fill(host, [h('form', { 'class': 'dash-drawer', 'data-el': 'importForm' }, [
      h('div', {}, [
        h('h5', {}, ['Import ' + r.id]),
        h('div', { 'class': 'dash-form' }, [
          field('Owner', 'owner', f.owner, 'team-…'),
          field('Environment', 'env', f.env, 'prod'),
          r.kind === 'subnet' ? field('Tier', 'tier', f.tier, 'private') : null,
          field('Name', 'name', f.name, 'Name tag, optional'),
          field('Namespace', 'namespace', f.namespace),
          h('label', {}, ['Dry run', dry]),
          h('div', { 'class': 'dash-actions' }, [
            h('button', { type: 'submit', 'class': 'primary', 'data-act': 'apply' }, ['Create import']),
            h('button', { type: 'button', 'data-act': 'copy' }, ['Copy YAML']),
            h('button', { type: 'button', 'data-act': 'cancel' }, ['Cancel'])
          ]),
          h('p', { 'class': 'sub dash-error', 'data-el': 'importError' }, [draft.error || ''])
        ])
      ]),
      h('div', {}, [
        h('h5', {}, ['What gets created']),
        h('pre', { 'class': 'dash-yaml', 'data-el': 'yaml' }, [toYAML(draft.object)]),
        h('p', { 'class': 'sub dash-gap-top' }, ['The operator records the user the API server signed you in as, and fills in requestedBy with it. ' +
          'It applies these tags with ec2:CreateTags and nothing else.'])
      ])
    ])]);
  }

  function mountLive(container, options) {
    var source = options.source;
    var api = apiClient(source);
    container.classList.add('dash');
    container.innerHTML = LIVE_TEMPLATE;
    var el = {};
    Array.prototype.forEach.call(container.querySelectorAll('[data-el]'), function (node) {
      el[node.getAttribute('data-el')] = node;
    });
    el.badge.textContent = source.mode === 'cluster' ? 'Live · cluster' : 'Live · kubectl proxy';
    el.foot.textContent = 'Read from the Kubernetes API as you' +
      (source.mode === 'cluster' ? ', through the dashboard, which has no access of its own' : ', through kubectl proxy at ' + (source.base || location.origin)) +
      '. What you see is what your RBAC lets you read.';

    function token(name) { return getComputedStyle(container).getPropertyValue('--d-' + name).trim(); }
    var colours = {
      status: function (ratio) { return ratio >= 0.85 ? token('bad') : ratio >= 0.7 ? token('warn') : token('ok'); }
    };

    var view = null, loadedAt = 0, polledAt = 0, timer = null, draft = null, loading = false;
    var redrawTimer = null, askedWho = false;
    var sync = liveSync(api, { onChange: changed });
    var data = sync.data;

    function showBanner(text) {
      el.banner.hidden = !text;
      el.banner.textContent = text || '';
    }

    function redraw() { if (view) renderLive(el, view, data, colours); }

    // changed runs when a watch changed data. A burst of events — a resync touching every
    // subnet — becomes one redraw.
    function changed() {
      loadedAt = Date.now();
      if (!view || redrawTimer) return;
      redrawTimer = setTimeout(function () {
        redrawTimer = null;
        view = summarize(data);
        redraw();
        tickSync();
      }, 250);
    }

    // refresh lists every kind that is not being watched: at first all of them, then the
    // ones left to polling, and any whose watch has stopped.
    function refresh() {
      if (loading) return Promise.resolve();
      var kinds = sync.needed();
      if (!kinds.length) { polledAt = Date.now(); return Promise.resolve(); }
      loading = true;
      return sync.load(kinds).then(function (failed) {
        loading = false;
        // The token changed while this was loading: list again for the new one.
        if (!failed) return refresh();
        polledAt = Date.now();
        var errors = kinds.map(function (r) { return failed[r]; }).filter(Boolean);
        var unauthorized = errors.length === kinds.length && errors.every(function (e) { return e.code === 401; });
        el.signout.hidden = !(source.mode === 'cluster' && readToken());
        if (unauthorized) {
          el.auth.hidden = source.mode !== 'cluster';
          showBanner(source.mode === 'cluster' ? '' : 'kubectl proxy answered 401: its kubeconfig has no valid credentials.');
          return;
        }
        el.auth.hidden = true;
        if (errors.length === kinds.length) {
          showBanner('Could not read the cluster: ' + errors[0].message +
            (source.mode === 'kubectl' ? '. Is kubectl proxy running, and does this page come from it?' : ''));
          return;
        }
        showBanner('');
        view = summarize(data); loadedAt = Date.now();
        redraw();
        tickSync();
        whoami();
      });
    }

    // whoami asks once per sign-in. An API server older than Kubernetes 1.28 has no
    // SelfSubjectReview; the name is then simply not shown.
    function whoami() {
      if (askedWho) return;
      askedWho = true;
      api.whoami().then(function (name) { renderWho(el.who, source, name); }, function () { renderWho(el.who, source, ''); });
    }

    function tickSync() {
      if (!loadedAt) return;
      var s = Math.round((Date.now() - loadedAt) / 1000);
      var watching = sync.watching();
      el.sync.textContent = s < 2 ? 'updated just now' : 'updated ' + s + 's ago';
      el.sync.setAttribute('title', watching.length
        ? 'Watching ' + watching.join(' and ') + ' for changes; the rest is listed again every ' + REFRESH_MS / 1000 + ' seconds.'
        : 'Everything is listed again every ' + REFRESH_MS / 1000 + ' seconds.');
    }

    function toast(msg, bad) {
      var t = document.createElement('div');
      t.className = 'dash-toast' + (bad ? ' bad' : ''); t.textContent = msg;
      document.body.appendChild(t);
      requestAnimationFrame(function () { t.classList.add('show'); });
      setTimeout(function () { t.classList.remove('show'); setTimeout(function () { t.remove(); }, 300); }, 3200);
    }

    function scopeNamed(name) {
      return view.scopes.filter(function (s) { return str(obj(s.metadata).name) === name; })[0] || {};
    }

    function readForm() {
      var form = el.drawer.querySelector('form');
      function g(n) { var f = form && form.elements.namedItem(n); return f ? String(f.value).trim() : ''; }
      var dry = form && form.elements.namedItem('dryRun');
      return { owner: g('owner'), env: g('env'), tier: g('tier'), name: g('name'), namespace: g('namespace') || 'default', dryRun: !!(dry && dry.checked) };
    }

    function openDrawer(id) {
      var r = view.unmanaged.filter(function (x) { return x.id === id; })[0];
      if (!r) return;
      var scope = scopeNamed(r.scope);
      var ns = str(obj(obj(scope.spec).autoImport).namespace) || 'default';
      var form = { owner: '', env: '', tier: r.kind === 'subnet' ? 'private' : '', name: '', namespace: ns, dryRun: false };
      draft = { resource: r, form: form, object: importObject(r, form, scope), error: '' };
      renderLiveDrawer(el.drawer, draft);
      el.drawer.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    }

    function createImport() {
      draft.form = readForm();
      draft.object = importObject(draft.resource, draft.form, scopeNamed(draft.resource.scope));
      if (!draft.form.owner) {
        draft.error = 'An owner is required: a resource nobody owns stays unmanaged.';
        renderLiveDrawer(el.drawer, draft);
        return;
      }
      var r = draft.resource;
      api.create(draft.form.namespace, 'resourceimports', draft.object).then(function (created) {
        var md = obj(obj(created).metadata);
        draft = null;
        renderLiveDrawer(el.drawer, null);
        toast('Created ResourceImport ' + str(md.namespace) + '/' + str(md.name) + ' for ' + r.id);
        refresh();
      }, function (err) {
        draft.error = (err.code === 403 ? 'Not allowed: ' : 'Failed: ') + err.message;
        renderLiveDrawer(el.drawer, draft);
      });
    }

    el.unmanaged.addEventListener('click', function (e) {
      var b = e.target.closest('button[data-import]');
      if (b) openDrawer(b.getAttribute('data-import'));
    });
    el.drawer.addEventListener('input', function () {
      if (!draft) return;
      draft.form = readForm();
      draft.object = importObject(draft.resource, draft.form, scopeNamed(draft.resource.scope));
      var y = el.drawer.querySelector('[data-el="yaml"]');
      if (y) y.textContent = toYAML(draft.object);
    });
    el.drawer.addEventListener('submit', function (e) {
      e.preventDefault();
      if (draft) createImport();
    });
    el.drawer.addEventListener('click', function (e) {
      var b = e.target.closest('button[data-act]');
      if (!b || !draft) return;
      var act = b.getAttribute('data-act');
      if (act === 'cancel') { draft = null; renderLiveDrawer(el.drawer, null); }
      if (act === 'copy' && navigator.clipboard) {
        navigator.clipboard.writeText(toYAML(draft.object) + '\n').then(function () { toast('YAML copied'); });
      }
    });
    el.auth.addEventListener('submit', function (e) {
      e.preventDefault();
      var input = el.auth.elements.namedItem('token');
      var t = String(input.value).trim().replace(/^Bearer\s+/i, '');
      input.value = '';
      if (!t) return;
      writeToken(t);
      // Whoever the new token belongs to, the watches opened with the old one are not theirs.
      sync.reset();
      askedWho = false;
      refresh().then(function () {
        el.authError.textContent = el.auth.hidden ? '' : 'The API server did not accept that token.';
      });
    });
    el.signout.addEventListener('click', function () {
      writeToken('');
      sync.reset();
      view = null; loadedAt = 0; askedWho = false;
      renderWho(el.who, source, '');
      refresh();
    });

    var themes = themeSwitch(container, el.themes, options, redraw);
    themes.apply(initialTheme(options));
    refresh();

    if (options.modal) {
      el.close.hidden = false;
      el.close.addEventListener('click', function () { if (options.onClose) options.onClose(); });
    }

    function start() {
      if (timer) return;
      timer = setInterval(function () {
        // A hidden tab gives its watch connections back, for the tabs someone is looking at.
        if (document.hidden) { sync.pause(); return; }
        sync.resume();
        tickSync();
        if (Date.now() - polledAt >= REFRESH_MS && !draft) refresh();
      }, 1000);
    }
    function stop() { clearInterval(timer); timer = null; sync.pause(); }

    return { start: start, stop: stop, element: container, installButton: el.install, setTheme: themes.apply, source: source, refresh: refresh };
  }

  function mount(container, options) {
    options = options || {};
    var source = options.source || { mode: 'demo' };
    if (source.mode === 'demo') return mountDemo(container, options);
    return mountLive(container, options);
  }

  global.SubnetDashboard = {
    mount: mount,
    themes: THEMES,
    sourceFromPage: function () { return sourceFromPage(document, location); },
    // Exposed for the tests in hack/dashboard; not an API.
    _live: {
      sourceFromPage: sourceFromPage, apiClient: apiClient, summarize: summarize, importObject: importObject,
      toYAML: toYAML, renderLive: renderLive, renderLiveDrawer: renderLiveDrawer, h: h, TOKEN_KEY: TOKEN_KEY,
      liveSync: liveSync, renderWho: renderWho, WATCHED_RESOURCES: WATCHED_RESOURCES
    }
  };
})(window);
