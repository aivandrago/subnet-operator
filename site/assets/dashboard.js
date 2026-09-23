/* Live demo dashboard.
 *
 * Synthetic inventory of a fictional organization, rendered the way the operator's own
 * metrics would be. No network calls: the numbers are generated here and drift every few
 * seconds so the page shows something alive. Used by the panel on the project page and by
 * the installable app at /dashboard/.
 *
 * window.SubnetDashboard.mount(container, { modal, onClose, onTheme, defaultTheme }) -> controller
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
      'Costs are estimates: resources are attributed by tag and priced from the public list, not billed amounts.</div>';

  function mount(container, options) {
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

    function applyTheme(id) {
      container.setAttribute('data-dash-theme', id);
      try { localStorage.setItem(THEME_KEY, id); } catch (e) { /* private mode */ }
      Array.prototype.forEach.call(el.themes.children, function (b) {
        b.setAttribute('aria-pressed', String(b.dataset.theme === id));
      });
      if (options.onTheme) options.onTheme(id);
      renderAll();
    }

    function buildThemeSwitch() {
      el.themes.innerHTML = THEMES.map(function (t) {
        return '<button type="button" data-theme="' + t.id + '" aria-pressed="false">' + t.label + '</button>';
      }).join('');
      el.themes.addEventListener('click', function (e) {
        var b = e.target.closest('button[data-theme]');
        if (b) applyTheme(b.dataset.theme);
      });
    }

    function initialTheme() {
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
    buildThemeSwitch();
    applyTheme(initialTheme());
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

  global.SubnetDashboard = { mount: mount, themes: THEMES };
})(window);
