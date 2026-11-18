/* Team Network dashboard: a demo of what the Team Network Operator is meant to write into the
   cluster, drawn from made-up TeamNetwork objects (data.js). It makes no requests of its own.

   window.TeamNetworkDashboard.mount(container, { modal, onClose, onTheme }) renders it into
   container; the standalone page (index.html) and the modal on /team-network/ both use it.
   Links and screenshots: #team=<name> (or ?team=<name>) opens a team's details,
   ?state=ready|waiting|problem|progress, ?route=external|connector, ?q=<text>, ?theme=dark. */
(function (global) {
  'use strict';

  var THEME_KEY = 'team-network-dashboard-theme';
  var STEP_TYPES = ['SubnetsReady', 'IsolationReady', 'NodesReady', 'RouteReady', 'Access'];
  var STEP_SHORT = { SubnetsReady: 'SUB', IsolationReady: 'ISO', NodesReady: 'NOD', RouteReady: 'RTE', AccessGranted: 'ACC', AccessProposed: 'ACC' };
  var STATE_LABEL = { ready: 'Ready', waiting: 'Proposal waiting', problem: 'Problem', progress: 'In progress' };

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }
  function ago(iso, now) {
    if (!iso) return '';
    var m = Math.max(0, Math.round((now - Date.parse(iso)) / 60000));
    if (m < 1) return 'just now';
    if (m < 60) return m + 'm ago';
    if (m < 60 * 48) return Math.round(m / 60) + 'h ago';
    return Math.round(m / 1440) + 'd ago';
  }
  function params() {
    try { return new URLSearchParams(location.search); } catch (e) { return { get: function () { return null; } }; }
  }
  function hashTeam() {
    var m = /(?:^#|&)team=([a-z0-9-]+)/.exec(location.hash || '');
    return m ? m[1] : null;
  }

  var ICON_CLOSE = '<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><path d="M6 6l12 12M18 6L6 18"/></svg>';
  var ICON_NET = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="8.5" y="14" width="7" height="7" rx="1.5"/><path d="M6.5 10v2h11v-2M12 12v2"/></svg>';
  var ICON_SEARCH = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="M20 20l-3.5-3.5"/></svg>';

  var TEMPLATE =
    '<div class="tn-head">' +
      '<div class="tn-title"><span class="tn-logo">' + ICON_NET + '</span><h3>Team networks</h3></div>' +
      '<span class="tn-badge"><i></i>Demo data</span>' +
      '<span class="tn-ctx" data-el="ctx"></span>' +
      '<div class="tn-tools">' +
        '<div class="tn-seg" role="group" aria-label="Colour theme" data-el="themes">' +
          '<button type="button" data-theme="light">Light</button><button type="button" data-theme="dark">Dark</button></div>' +
        '<button class="tn-install" type="button" data-el="install" hidden>Install app</button>' +
        '<button class="tn-close" type="button" data-el="close" aria-label="Close the dashboard" hidden>' + ICON_CLOSE + '</button>' +
      '</div>' +
    '</div>' +
    '<div class="tn-body">' +
      '<div class="tn-class" data-el="cls"></div>' +
      '<div class="tn-tiles" data-el="tiles"></div>' +
      '<section class="tn-card">' +
        '<h4>Teams <span class="tn-count" data-el="count"></span></h4>' +
        '<p class="sub">One cluster-scoped <code>TeamNetwork</code> per team. The five steps run in order; access is granted last and revoked first. Pick a team for its details.</p>' +
        '<div class="tn-filters">' +
          '<label class="tn-search">' + ICON_SEARCH + '<input type="search" data-el="q" placeholder="Search team, group, CIDR, subnet, security group" aria-label="Search teams"></label>' +
          '<select data-el="state" aria-label="State"><option value="">All states</option><option value="ready">Ready</option><option value="waiting">Proposal waiting</option><option value="problem">Problem</option><option value="progress">In progress</option></select>' +
          '<select data-el="route" aria-label="Route mode"><option value="">All route modes</option><option value="external">external</option><option value="connector">connector</option></select>' +
          '<select data-el="approval" aria-label="Access approval"><option value="">All approvals</option><option value="propose">propose</option><option value="auto">auto</option></select>' +
          '<button type="button" class="tn-clear" data-el="clear">Clear</button>' +
        '</div>' +
        '<div class="tn-legend"><span><span class="tn-chip True">SUB</span>Subnets</span><span><span class="tn-chip True">ISO</span>Isolation</span>' +
          '<span><span class="tn-chip True">NOD</span>Nodes</span><span><span class="tn-chip True">RTE</span>Route</span><span><span class="tn-chip True">ACC</span>Access</span>' +
          '<span class="tn-dim">green True · red False · amber proposed · dashed waiting</span></div>' +
        '<div class="tn-list" role="table" aria-label="Team networks">' +
          '<div class="tn-row tn-row-head" role="row"><span role="columnheader" class="c-team">Team</span><span role="columnheader" class="c-subnets">Subnets</span>' +
            '<span role="columnheader" class="c-nodes">Nodes</span><span role="columnheader" class="c-route">Route</span>' +
            '<span role="columnheader" class="c-access">Access</span><span role="columnheader" class="c-steps">Steps · Ready</span></div>' +
          '<div data-el="rows"></div>' +
        '</div>' +
      '</section>' +
    '</div>' +
    '<div class="tn-foot">Made-up data from a fictional cluster in eu-central-1 with one VPC and a Tailscale tailnet. The objects, ' +
      'conditions and messages follow the design of the Team Network Operator, a commercial product in development: nothing is ' +
      'released yet. In a cluster, <code>kubectl get teamnetworks</code> would show the same list.</div>' +
    '<div class="tn-drawer-back" data-el="back" hidden></div>' +
    '<aside class="tn-drawer" data-el="drawer" role="dialog" aria-modal="true" aria-label="Team details" hidden></aside>';

  function cond(t, type) {
    var cs = t.status.conditions;
    for (var i = 0; i < cs.length; i++) {
      if (cs[i].type === type || (type === 'Access' && (cs[i].type === 'AccessGranted' || cs[i].type === 'AccessProposed'))) return cs[i];
    }
    return null;
  }
  function chipClass(c) {
    if (c.status === 'True' && c.type === 'AccessProposed' && c.reason === 'Proposed') return 'Proposed';
    return c.status;
  }
  function teamState(t) {
    if (cond(t, 'Ready').status === 'True') return 'ready';
    var a = cond(t, 'Access');
    if (a.type === 'AccessProposed' && a.status === 'True' && a.reason === 'Proposed') return 'waiting';
    if (cond(t, 'SubnetsReady').reason === 'Allocating') return 'progress';
    return 'problem';
  }
  function zoneShort(z) { return z.replace(/^.*-(\d[a-z])$/, '$1'); }

  /* A small YAML writer for the objects, with keys marked for colour. */
  function toYaml(v, indent) {
    indent = indent || '';
    function scalar(x) {
      if (x === null || x === undefined) return 'null';
      if (typeof x === 'number' || typeof x === 'boolean') return String(x);
      var s = String(x);
      if (/^[A-Za-z0-9_./@-]+$/.test(s) && !/^(true|false|null|yes|no|\d+)$/i.test(s)) return esc(s);
      return '<span class="s">' + esc(JSON.stringify(s)) + '</span>';
    }
    var out = [];
    Object.keys(v).forEach(function (k) {
      var x = v[k];
      var key = indent + '<span class="k">' + esc(k) + '</span>:';
      if (Array.isArray(x)) {
        if (!x.length) { out.push(key + ' []'); return; }
        if (x.every(function (e) { return typeof e !== 'object' || e === null; })) {
          out.push(key + ' [' + x.map(scalar).join(', ') + ']'); return;
        }
        out.push(key);
        x.forEach(function (e) {
          var inner = toYaml(e, indent + '    ').split('\n');
          inner[0] = indent + '  - ' + inner[0].slice((indent + '    ').length);
          out.push(inner.join('\n'));
        });
      } else if (x && typeof x === 'object') {
        if (!Object.keys(x).length) { out.push(key + ' {}'); return; }
        out.push(key);
        out.push(toYaml(x, indent + '  '));
      } else {
        out.push(key + ' ' + scalar(x));
      }
    });
    return out.join('\n');
  }
  function clean(o) {
    if (Array.isArray(o)) return o.map(clean);
    if (o && typeof o === 'object') {
      var r = {};
      Object.keys(o).forEach(function (k) { if (o[k] !== null && o[k] !== undefined) r[k] = clean(o[k]); });
      return r;
    }
    return o;
  }

  function mount(container, options) {
    options = options || {};
    var data = global.TeamNetworkDemoData.build();
    var now = data.now, cls = data.cls;
    container.classList.add('tn');
    container.innerHTML = TEMPLATE;
    var el = {};
    Array.prototype.forEach.call(container.querySelectorAll('[data-el]'), function (n) { el[n.getAttribute('data-el')] = n; });

    var q = params();
    var state = { q: q.get('q') || '', st: q.get('state') || '', route: q.get('route') || '', approval: q.get('approval') || '', open: null };

    // theme
    var theme = q.get('theme');
    if (theme !== 'dark' && theme !== 'light') { try { theme = localStorage.getItem(THEME_KEY); } catch (e) { theme = null; } }
    if (theme !== 'dark' && theme !== 'light') theme = 'light';
    function applyTheme(t) {
      theme = t;
      container.setAttribute('data-tn-theme', t);
      el.drawer.setAttribute('data-tn-theme', t);
      Array.prototype.forEach.call(el.themes.querySelectorAll('button'), function (b) {
        b.setAttribute('aria-pressed', String(b.getAttribute('data-theme') === t));
      });
      try { localStorage.setItem(THEME_KEY, t); } catch (e) { /* private mode */ }
      if (options.onTheme) options.onTheme(t);
    }
    el.themes.addEventListener('click', function (e) { var t = e.target.getAttribute('data-theme'); if (t) applyTheme(t); });
    applyTheme(theme);

    if (options.modal) { el.close.hidden = false; el.close.addEventListener('click', function () { if (options.onClose) options.onClose(); }); }

    var deferred = null;
    global.addEventListener('beforeinstallprompt', function (e) { e.preventDefault(); deferred = e; if (!options.modal) el.install.hidden = false; });
    el.install.addEventListener('click', function () { if (deferred) { deferred.prompt(); deferred = null; el.install.hidden = true; } });

    el.ctx.textContent = 'TeamNetworkClass ' + cls.name + ' · ' + cls.region + ' · tailnet ' + cls.tailnet;

    function matches(t) {
      if (state.st && teamState(t) !== state.st && !(state.st === 'route' && cond(t, 'RouteReady').status === 'False')) return false;
      if (state.route && t.status.route.mode !== state.route) return false;
      if (state.approval && t.spec.vpn.approval !== state.approval) return false;
      if (state.q) {
        var hay = [t.metadata.name, t.spec.vpn.group, t.status.securityGroupID, t.status.route.mode, t.spec.vpn.approval]
          .concat(t.status.subnets.map(function (s) { return [s.cidr, s.subnetID, s.zone].join(' '); }))
          .concat(t.status.conditions.map(function (c) { return c.reason; })).join(' ').toLowerCase();
        var words = state.q.toLowerCase().split(/\s+/).filter(Boolean);
        for (var i = 0; i < words.length; i++) if (hay.indexOf(words[i]) < 0) return false;
      }
      return true;
    }

    function render() { renderClass(); renderTiles(); renderFilters(); renderRows(); }

    function renderClass() {
      el.cls.innerHTML =
        '<div class="tn-classbox">' +
          '<div class="tn-classtop"><b>TeamNetworkClass ' + esc(cls.name) + '</b><span class="tn-pill mode">route: ' + esc(cls.route.mode) + ' · approval: propose</span></div>' +
          '<div class="tn-classmeta mono">' + esc(cls.networkID) + ' · account ' + esc(cls.account) + ' · NetworkScope ' + esc(cls.scopeRef) + '</div>' +
          '<dl>' +
            '<div><dt>Region</dt><dd>' + esc(cls.region) + '</dd></div>' +
            '<div><dt>Claims in</dt><dd class="mono">' + esc(cls.claimNamespace) + '</dd></div>' +
            '<div><dt>Cluster SG</dt><dd class="mono">' + esc(cls.clusterSecurityGroup.slice(0, 10)) + '…</dd></div>' +
            '<div><dt>Allowed groups</dt><dd>' + cls.allowedGroups.length + '</dd></div>' +
          '</dl>' +
          '<div class="tn-groups" aria-label="allowedGroups">' + cls.allowedGroups.map(function (g) { return '<span>' + esc(g) + '</span>'; }).join('') + '</div>' +
          '<p class="tn-classnote">Nodes: EC2NodeClass and NodePool per team from the class templates (role <span class="mono">' + esc(cls.nodeRole) + '</span>), or <span class="mono">TagOnly</span>. ' +
            'Subnets never come from <span class="mono">100.64.0.0/10</span>.</p>' +
        '</div>' +
        '<div class="tn-classbox">' +
          '<div class="tn-classtop"><b>Subnet routers <span class="tn-dim mono" style="font-weight:500">' + esc(cls.route.deviceTag) + '</span></b><span class="tn-pill ok">2 online</span></div>' +
          '<div class="tn-classmeta">Existing EC2 routers, outside the cluster. The operator changes nothing on them; it only checks their routes via the Tailscale API.</div>' +
          '<div class="tn-routers">' + data.routers.map(function (r) {
            return '<div class="tn-router"><b>' + esc(r.name) + '</b><span class="tn-pill ok">online</span>' +
              '<small>' + esc(r.zone) + ' · ' + esc(r.ip) + ' · advertises ' + esc(r.advertised.join(', ')) + ' · approved ' + esc(r.approved.join(', ')) + '</small></div>';
          }).join('') + '</div>' +
        '</div>';
    }

    function renderTiles() {
      var ts = data.teams;
      var ready = ts.filter(function (t) { return teamState(t) === 'ready'; }).length;
      var waiting = ts.filter(function (t) { return teamState(t) === 'waiting'; }).length;
      var routeBad = ts.filter(function (t) { return cond(t, 'RouteReady').status === 'False'; }).length;
      var subnets = 0, pending = 0, nodes = 0, pools = 0, tagOnly = 0;
      ts.forEach(function (t) {
        t.status.subnets.forEach(function (s) { if (s.cidr) subnets++; else pending++; });
        nodes += t.status.nodes || 0;
        if (t.status.nodePool) pools++;
        if (t.ui.nodesMode === 'TagOnly') tagOnly++;
      });
      var tiles = [
        { v: ts.length, l: 'Teams', s: 'TeamNetwork objects', f: '' },
        { v: ready, l: 'Ready', s: 'subnets, nodes, route and access', cls: 'ok', f: 'ready' },
        { v: waiting, l: 'Proposals waiting', s: 'a grant to paste into the policy file', cls: waiting ? 'warn' : '', f: 'waiting' },
        { v: routeBad, l: 'Route problems', s: 'RouteReady=False, so no access', cls: routeBad ? 'bad' : '', f: 'route' },
        { v: subnets, l: 'Subnets', s: pending ? pending + ' more being allocated' : 'one per team and zone' },
        { v: nodes, l: 'Nodes', s: 'in ' + pools + ' NodePools · ' + tagOnly + ' team TagOnly' }
      ];
      el.tiles.innerHTML = tiles.map(function (t) {
        var btn = t.f !== undefined;
        var open = btn ? '<button type="button" class="tn-tile ' + (t.cls || '') + '" data-st="' + t.f + '" aria-pressed="' + (state.st === t.f && t.f !== '') + '">' : '<div class="tn-tile">';
        return open + '<b>' + t.v + '</b><span>' + esc(t.l) + '</span><small>' + esc(t.s) + '</small>' + (btn ? '</button>' : '</div>');
      }).join('');
    }

    function renderFilters() {
      el.state.value = state.st === 'route' ? '' : state.st;
      el.route.value = state.route;
      el.approval.value = state.approval;
      if (el.q.value !== state.q) el.q.value = state.q;
      el.clear.hidden = !(state.q || state.st || state.route || state.approval);
    }

    function chips(t) {
      return STEP_TYPES.map(function (type) {
        var c = cond(t, type);
        return '<span class="tn-chip ' + chipClass(c) + '" title="' + esc(c.type + '=' + c.status + ' · ' + c.reason) + '">' + STEP_SHORT[c.type] + '</span>';
      }).join('') + readyPill(t, 'tn-ready');
    }
    function readyPill(t, extra) {
      var r = cond(t, 'Ready'), st = teamState(t);
      var k = r.status === 'True' ? 'ok' : st === 'waiting' ? 'warn' : st === 'progress' ? 'wait' : 'bad';
      return '<span class="tn-pill ' + k + ' ' + (extra || '') + '" title="' + esc(r.reason + ': ' + r.message) + '">Ready ' + r.status + '</span>';
    }
    function routeCell(t) {
      var r = t.status.route, c = cond(t, 'RouteReady');
      var mode = '<span class="tn-pill mode">' + r.mode + '</span>';
      if (c.status === 'Unknown') return mode + '<small>waits for the steps before it</small>';
      if (r.mode === 'connector') return mode + '<span class="tn-pill ok">' + r.connector.replicas + ' replicas</span><small>Connector ' + esc(r.connector.name) + ' advertises the team CIDRs</small>';
      if (c.status === 'False') return mode + '<span class="tn-pill bad">' + esc(c.reason) + '</span><small>outside 10.20.0.0/16 of ' + esc(r.deviceTag) + '</small>';
      return mode + '<span class="tn-pill ok">Covered</span><small>by ' + esc(r.coveredBy) + ' via ' + esc(r.deviceTag) + '</small>';
    }
    function accessCell(t) {
      var a = t.status.access, c = cond(t, 'Access');
      var mode = '<span class="tn-pill mode">' + esc(t.spec.vpn.approval) + '</span>';
      if (a.state === 'Proposed' && !a.inPolicy) return mode + '<span class="tn-pill warn">Proposed</span><small>grant ready to paste</small>';
      if (a.state === 'Proposed') return mode + '<span class="tn-pill ok">In policy</span><small>pasted by a person</small>';
      if (a.state === 'Granted') return mode + '<span class="tn-pill ok">Granted</span><small>ETag ' + esc(a.etag.slice(0, 4)) + '… · ' + ago(a.grantedAt, now) + '</small>';
      if (c.status === 'False' && c.reason === 'MarkersMissing') return mode + '<span class="tn-pill bad">MarkersMissing</span><small>no marked section in the policy file</small>';
      if (c.reason === 'WaitingForRoute') return mode + '<span class="tn-pill wait">No access</span><small>waits for RouteReady</small>';
      return mode + '<span class="tn-pill wait">Waiting</span><small>granted last</small>';
    }
    function subnetsCell(t) {
      return t.status.subnets.map(function (s) {
        return s.cidr ? '<span><i>' + zoneShort(s.zone) + '</i> ' + esc(s.cidr) + '</span>'
          : '<span class="pending"><i>' + zoneShort(s.zone) + '</i> allocating…</span>';
      }).join('');
    }
    function nodesCell(t) {
      if (t.ui.nodesMode === 'TagOnly') return '<span class="tn-pill mode">TagOnly</span><small>subnets tagged hs/team=' + esc(t.metadata.name) + '</small>';
      if (!t.status.nodePool) return '<b class="tn-dim">–</b><small>NodePool after isolation</small>';
      return '<b>' + t.status.nodes + ' nodes</b><small>NodePool ' + esc(t.status.nodePool) + ' · cpu ' + esc(t.ui.cpu) + '</small>';
    }

    function renderRows() {
      var order = { problem: 0, waiting: 1, progress: 2, ready: 3 };
      var rows = data.teams.filter(matches).sort(function (a, b) {
        return order[teamState(a)] - order[teamState(b)] || a.metadata.name.localeCompare(b.metadata.name);
      });
      el.count.textContent = rows.length === data.teams.length ? String(rows.length) : rows.length + ' of ' + data.teams.length;
      if (!rows.length) { el.rows.innerHTML = '<p class="tn-empty">No team matches these filters.</p>'; return; }
      el.rows.innerHTML = rows.map(function (t) {
        var n = t.metadata.name;
        return '<button type="button" class="tn-row' + (state.open === n ? ' on' : '') + '" role="row" data-team="' + esc(n) + '">' +
          '<span class="c-team" role="cell"><b>' + esc(n) + '</b><small>' + esc(t.spec.vpn.group) + '</small></span>' +
          '<span class="c-subnets" role="cell">' + subnetsCell(t) + '</span>' +
          '<span class="c-nodes" role="cell">' + nodesCell(t) + '</span>' +
          '<span class="c-route" role="cell">' + routeCell(t) + '</span>' +
          '<span class="c-access" role="cell">' + accessCell(t) + '</span>' +
          '<span class="c-steps" role="cell">' + chips(t) + '</span>' +
        '</button>';
      }).join('');
    }

    /* Detail panel */
    function find(name) { for (var i = 0; i < data.teams.length; i++) if (data.teams[i].metadata.name === name) return data.teams[i]; return null; }
    function codeBlock(html, copyText) {
      return '<div class="tn-code' + (copyText ? ' has-copy' : '') + '">' +
        (copyText ? '<button type="button" class="tn-copy" data-copy="' + esc(copyText) + '">Copy</button>' : '') +
        '<pre>' + html + '</pre></div>';
    }
    function kv(rows) {
      return '<div class="tn-kvs">' + rows.map(function (r) {
        return '<div class="tn-kv"><span>' + esc(r[0]) + '</span><span' + (r[2] ? ' class="mono"' : '') + '>' + r[1] + '</span></div>';
      }).join('') + '</div>';
    }
    function markerBlock(rule, withGrant) {
      return '<span class="k">"grants"</span>: [\n' +
        '  <span class="c">// … your other grants stay as they are …</span>\n' +
        '  <span class="hl">// hs:team-network begin</span>\n' +
        (withGrant ? '  ' + esc(rule) + ',\n' : '') +
        '  <span class="hl">// hs:team-network end</span>\n' +
        ']';
    }

    function sectionConditions(t) {
      var order = ['SubnetsReady', 'IsolationReady', 'NodesReady', 'RouteReady', 'Access', 'Ready'];
      return '<section><h5>Conditions</h5>' + order.map(function (type, i) {
        var c = cond(t, type);
        var k = chipClass(c);
        var pk = k === 'True' ? 'ok' : k === 'False' ? 'bad' : k === 'Proposed' ? 'warn' : 'wait';
        return '<div class="tn-cond ' + k + '"><span class="n">' + (i < 5 ? '0' + (i + 1) : '✓') + '</span><div>' +
          '<div class="tn-chead"><b>' + esc(c.type) + '</b><span class="tn-pill ' + pk + '">' + esc(c.status) + ' · ' + esc(c.reason) + '</span>' +
          '<span class="when">' + ago(c.lastTransitionTime, now) + '</span></div>' +
          '<p class="tn-cmsg mono">' + esc(c.message) + '</p></div></div>';
      }).join('') +
      '<p class="sub">Created in this order; on deletion the operator runs them backwards, so access is revoked first and the subnets go last.</p></section>';
    }
    function sectionSubnets(t) {
      return '<section><h5>Subnets <span class="tn-count">' + t.spec.subnets.prefixLength + '</span></h5>' +
        '<div class="tn-tablewrap"><table class="tn-table"><thead><tr><th>Zone</th><th>CIDR</th><th>Subnet</th><th>State</th></tr></thead><tbody>' +
        t.status.subnets.map(function (s) {
          return '<tr><td>' + esc(s.zone) + '</td><td class="mono">' + (s.cidr ? esc(s.cidr) : '<span class="tn-warnline">allocating</span>') + '</td>' +
            '<td class="mono">' + (s.subnetID ? esc(s.subnetID) : '–') + '</td><td>' + (s.cidr ? '<span class="tn-pill ok">' + s.state + '</span>' : '<span class="tn-pill wait">pending</span>') + '</td></tr>';
        }).join('') + '</tbody></table></div>' +
        kv([['SubnetClaim', esc(cls.claimNamespace + '/' + t.metadata.name), true], ['VPC', esc(cls.networkID), true],
          ['tag on each subnet', esc('hs/team=' + t.metadata.name), true], ['never from', '100.64.0.0/10 <span class="tn-dim">(the Tailscale range)</span>', true]]) +
        '</section>';
    }
    function sectionIsolation(t) {
      if (t.ui.allocating) return '<section><h5>Isolation</h5><p class="tn-dim">Waits for the subnets: the security group allows exactly their CIDRs, so it is created after them.</p></section>';
      var n = t.metadata.name;
      return '<section><h5>Isolation</h5>' +
        kv([['security group', esc(t.status.securityGroupID), true], ['node taint', esc('team=' + n + ':NoSchedule'), true], ['node label', esc('team=' + n), true],
          ['NetworkPolicy', '<span class="mono">hs-team-' + esc(n) + '</span> in namespaces labelled <span class="mono">hs/team=' + esc(n) + '</span>', false]]) +
        '<h6>Security group rules</h6><div class="tn-tablewrap"><table class="tn-table"><thead><tr><th>Direction</th><th>From / to</th><th>Ports</th><th>Why</th></tr></thead><tbody>' +
        t.ui.sgRules.map(function (r) {
          return '<tr><td>' + r.dir + '</td><td class="mono">' + esc(r.from) + '</td><td>' + r.ports + '</td><td class="why">' + esc(r.why) + '</td></tr>';
        }).join('') + '</tbody></table></div></section>';
    }
    function sectionNodes(t) {
      var n = t.metadata.name, d = t.ui.nodeDetail;
      if (t.ui.nodesMode === 'TagOnly') {
        return '<section><h5>Nodes <span class="tn-pill mode">TagOnly</span></h5>' +
          '<p class="tn-note">This team runs its own Karpenter, so the operator creates no EC2NodeClass or NodePool. It only tags the subnets ' +
          '<code>hs/team=' + esc(n) + '</code>; the team\'s EC2NodeClass selects them with that tag.</p>' +
          codeBlock('<span class="k">subnetSelectorTerms</span>:\n  - <span class="k">tags</span>:\n      <span class="k">hs/team</span>: ' + esc(n) + '\n<span class="k">securityGroupSelectorTerms</span>:\n  - <span class="k">id</span>: ' + esc(t.status.securityGroupID)) +
          '</section>';
      }
      if (t.ui.allocating) return '<section><h5>Nodes</h5><p class="tn-dim">EC2NodeClass and NodePool <span class="mono">' + esc(n) + '</span> are created once the subnets and the security group exist.</p></section>';
      return '<section><h5>Nodes <span class="tn-pill mode">Managed</span></h5>' +
        '<h6>EC2NodeClass ' + esc(d.nodeClass.name) + '</h6>' +
        kv([['role', esc(d.nodeClass.role), true], ['AMI', esc(d.nodeClass.ami), true], ['subnets', 'tag ' + esc(d.nodeClass.subnetSelector), true], ['security group', esc(d.nodeClass.securityGroup), true]]) +
        '<h6>NodePool ' + esc(d.nodePool.name) + '</h6>' +
        kv([['nodes', '<b>' + d.nodePool.nodes + '</b>', false], ['limits', esc(d.nodePool.limits), true], ['taint', esc(d.nodePool.taint), true], ['label', esc(d.nodePool.label), true]]) +
        '</section>';
    }
    function devicesTable(devs) {
      return '<div class="tn-tablewrap"><table class="tn-table"><thead><tr><th>Device</th><th>Zone</th><th>Advertised</th><th>Approved</th><th></th></tr></thead><tbody>' +
        devs.map(function (d) {
          return '<tr><td class="mono">' + esc(d.name) + '<br><span class="tn-dim">' + esc(d.ip) + '</span></td><td>' + esc(d.zone) + '</td><td class="mono">' + esc(d.advertised.join(', ')) +
            '</td><td class="mono">' + esc(d.approved.join(', ')) + '</td><td><span class="tn-pill ok">online</span></td></tr>';
        }).join('') + '</tbody></table></div>';
    }
    function sectionRoute(t) {
      var r = t.status.route, c = cond(t, 'RouteReady');
      var cidrs = t.status.subnets.filter(function (s) { return s.cidr; }).map(function (s) { return s.cidr; });
      if (r.mode === 'connector') {
        var cn = r.connector;
        return '<section><h5>Route <span class="tn-pill mode">connector</span></h5>' +
          '<p class="tn-note">A Tailscale operator <code>Connector</code> in the cluster, only for this team, advertises exactly the team\'s CIDRs.</p>' +
          kv([['Connector', esc(cn.namespace + '/' + cn.name), true], ['replicas', String(cn.replicas), false], ['device tag', esc(cn.tag), true], ['routes', esc(cidrs.join(', ')), true]]) +
          '<h6>Devices</h6>' + devicesTable(cn.devices) + '</section>';
      }
      var coverage = '<div class="tn-cover">' + (cidrs.length ? cidrs.map(function (x) {
        return c.status === 'False'
          ? '<div><span>' + esc(x) + '</span><span class="arrow">⊄</span><span>10.20.0.0/16</span><span class="tn-pill bad">not covered</span></div>'
          : '<div><span>' + esc(x) + '</span><span class="arrow">⊂</span><span>' + esc(r.coveredBy) + '</span><span class="tn-pill ok">covered</span></div>';
      }).join('') : '<div class="tn-dim">No CIDRs yet.</div>') + '</div>';
      return '<section><h5>Route <span class="tn-pill mode">external</span></h5>' +
        '<p class="tn-note">Existing subnet routers advertise one broad route. The operator changes nothing on them: it checks via the Tailscale API that a device tagged ' +
        '<code>' + esc(r.deviceTag) + '</code> advertises an approved route covering the team\'s CIDRs. Teams are kept apart by the access grant, not by the route.</p>' +
        coverage +
        (c.status === 'False' ? '<p class="tn-badline">' + esc(c.message) + '</p>' +
          '<ol class="tn-steps"><li>Advertise and approve <span class="mono">10.31.0.0/16</span> on the routers (a change people make; the operator never touches them), or</li>' +
          '<li>set <span class="mono">spec.vpn.route.mode: connector</span> for this team, so a Connector advertises exactly its CIDRs.</li></ol>' : '') +
        '<h6>Devices with ' + esc(r.deviceTag) + '</h6>' + devicesTable(r.devices) + '</section>';
    }
    function sectionAccess(t) {
      var a = t.status.access, c = cond(t, 'Access'), approval = t.spec.vpn.approval;
      var head = '<section><h5>Access <span class="tn-pill mode">' + esc(approval) + '</span></h5>';
      var group = '<p class="sub">Group <code>' + esc(t.spec.vpn.group) + '</code> is in <code>allowedGroups</code> of class ' + esc(cls.name) + '. The grant names only this team\'s CIDRs.</p>';
      if (!a.rule) return head + group + '<p class="tn-dim">Nothing yet: the grant is computed from the subnets once they exist, and access is always the last step.</p></section>';
      if (a.state === 'Proposed' && !a.inPolicy) {
        return head + group + '<h6>The grant to add</h6>' + codeBlock(esc(a.rule), a.rule) +
          '<h6>Where to paste it</h6>' +
          '<ol class="tn-steps"><li>Open the tailnet policy file of <span class="mono">' + esc(cls.tailnet) + '</span> (Access controls in the Tailscale admin console, or your GitOps repository for it).</li>' +
          '<li>Inside <span class="mono">"grants"</span>, put it between the two marker lines. Add the markers once if they are not there yet.</li>' +
          '<li>Save. The operator reads the file, finds the grant and turns <span class="mono">Ready</span> True.</li></ol>' +
          codeBlock(markerBlock(a.rule, true)) +
          '<p class="sub">In propose mode the operator never writes the policy file; a person does. Proposed ' + ago(a.proposedAt, now) + '.</p></section>';
      }
      if (a.state === 'Proposed') {
        return head + group + codeBlock(esc(a.rule), a.rule) +
          '<p class="tn-okline">Found in the policy file between <span class="mono">// hs:team-network begin</span> and <span class="mono">end</span>, ' + ago(a.foundAt, now) + '. Added by a person; the operator only read it.</p></section>';
      }
      if (a.state === 'Granted') {
        return head + group + codeBlock(esc(a.rule), a.rule) +
          kv([['written', ago(a.grantedAt, now) + ' <span class="tn-dim">(' + esc(a.grantedAt) + ')</span>', false], ['ETag', esc(a.etag), true],
            ['how', 'validated by Tailscale first, then written with <span class="mono">If-Match</span> on that ETag, only between the markers', false]]) +
          '</section>';
      }
      if (c.reason === 'MarkersMissing') {
        return head + group + '<p class="tn-badline">' + esc(c.message) + '. In auto mode the operator writes only its own marked section and refuses to edit a file without it.</p>' +
          '<h6>Add the markers once, inside "grants"</h6>' + codeBlock(markerBlock(a.rule, false), '// hs:team-network begin\n// hs:team-network end') +
          '<h6>The grant it will write there</h6>' + codeBlock(esc(a.rule)) + '</section>';
      }
      return head + group + '<p class="tn-note">No grant is proposed while <span class="mono">RouteReady</span> is False: access always comes last, so the team never gets a rule for a route that does not work. Once the route is fixed it proposes:</p>' +
        codeBlock(esc(a.rule)) + '</section>';
    }
    function sectionObject(t) {
      var o = clean({ apiVersion: t.apiVersion, kind: t.kind, metadata: t.metadata, spec: t.spec, status: t.status });
      return '<section><h5>In the cluster</h5>' +
        '<pre class="tn-cmd"><span class="p">$</span> kubectl get teamnetwork ' + esc(t.metadata.name) + ' -o yaml</pre>' +
        '<details class="tn-yaml" open><summary>The TeamNetwork object</summary>' + codeBlock(toYaml(o)) + '</details></section>';
    }

    function openTeam(name, push) {
      var t = find(name);
      if (!t) return;
      state.open = name;
      var st = teamState(t);
      el.drawer.innerHTML =
        '<div class="tn-dhead">' +
          '<div><div class="tn-dkicker mono">TeamNetwork ' + esc(name) + ' · class ' + esc(t.spec.classRef) + ' · created ' + ago(t.metadata.creationTimestamp, now) + '</div>' +
          '<h3>' + esc(name) + '</h3>' +
          '<div class="tn-dtags">' + readyPill(t) + '<span class="tn-tag">' + esc(STATE_LABEL[st]) + '</span>' +
            '<span class="tn-tag">' + esc(t.spec.vpn.group) + '</span><span class="tn-tag">route: ' + esc(t.status.route.mode) + '</span>' +
            '<span class="tn-tag">approval: ' + esc(t.spec.vpn.approval) + '</span><span class="tn-tag">nodes: ' + esc(t.ui.nodesMode) + '</span></div></div>' +
          '<button type="button" class="tn-close" data-act="close" aria-label="Close the details">' + ICON_CLOSE + '</button>' +
        '</div>' +
        '<div class="tn-dbody">' + sectionConditions(t) + sectionAccess(t) + sectionRoute(t) + sectionSubnets(t) + sectionIsolation(t) + sectionNodes(t) + sectionObject(t) + '</div>';
      el.drawer.hidden = false; el.back.hidden = false;
      renderRows();
      if (push !== false && !options.modal) {
        try { history.replaceState(null, '', location.pathname + location.search + '#team=' + encodeURIComponent(name)); } catch (e) { /* file: */ }
      }
      var btn = el.drawer.querySelector('[data-act="close"]');
      if (btn && push !== false) btn.focus({ preventScroll: true });
      el.drawer.scrollTop = 0;
    }
    function closeTeam() {
      if (!state.open) return;
      var name = state.open;
      state.open = null;
      el.drawer.hidden = true; el.back.hidden = true;
      renderRows();
      if (!options.modal && hashTeam()) { try { history.replaceState(null, '', location.pathname + location.search); } catch (e) { /* file: */ } }
      var row = el.rows.querySelector('[data-team="' + name + '"]');
      if (row) row.focus({ preventScroll: true });
    }
    function copy(btn) {
      var text = btn.getAttribute('data-copy');
      function done(ok) { btn.textContent = ok ? 'Copied' : 'Select and copy'; setTimeout(function () { btn.textContent = 'Copy'; }, 1600); }
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(function () { done(true); }, function () { done(fallback(text)); });
      } else done(fallback(text));
    }
    function fallback(text) {
      var ta = document.createElement('textarea');
      ta.value = text; ta.setAttribute('readonly', ''); ta.style.position = 'fixed'; ta.style.opacity = '0';
      document.body.appendChild(ta); ta.select();
      var ok = false; try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
      document.body.removeChild(ta);
      return ok;
    }
    el.drawer.addEventListener('click', function (e) {
      if (e.target.closest('[data-act="close"]')) { closeTeam(); return; }
      var cp = e.target.closest('[data-copy]'); if (cp) copy(cp);
    });
    el.back.addEventListener('click', closeTeam);

    container.addEventListener('click', function (e) {
      var r = e.target.closest('[data-team]');
      if (r && !el.drawer.contains(r)) { openTeam(r.getAttribute('data-team')); return; }
      var s = e.target.closest('[data-st]');
      if (s) { var v = s.getAttribute('data-st'); state.st = state.st === v ? '' : v; render(); }
    });
    el.q.addEventListener('input', function () { state.q = el.q.value; renderRows(); el.clear.hidden = !(state.q || state.st || state.route || state.approval); });
    el.state.addEventListener('change', function () { state.st = el.state.value; render(); });
    el.route.addEventListener('change', function () { state.route = el.route.value; render(); });
    el.approval.addEventListener('change', function () { state.approval = el.approval.value; render(); });
    el.clear.addEventListener('click', function () { state.q = state.st = state.route = state.approval = ''; render(); });
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && state.open) { e.stopPropagation(); closeTeam(); } }, true);
    if (!options.modal) {
      global.addEventListener('hashchange', function () { var h = hashTeam(); if (h && h !== state.open) openTeam(h, false); else if (!h && state.open) closeTeam(); });
    }

    render();
    var first = (!options.modal && hashTeam()) || q.get('team');
    if (first) openTeam(first, false);

    return { isDetailOpen: function () { return !!state.open; }, closeDetail: closeTeam, openTeam: openTeam };
  }

  global.TeamNetworkDashboard = { mount: mount };
})(window);
