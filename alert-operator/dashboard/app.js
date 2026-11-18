/* Alert rule inventory: a demo of what Alert Operator writes into the cluster, drawn from
   made-up AlertRule and AlertScope objects (data.js). It makes no requests of its own.

   window.AlertDashboard.mount(container, { modal, onClose }) renders it into container; the
   standalone page (index.html) and the modal on /alert-operator/ both use it. URL parameters,
   for links and screenshots: ?rule=<AlertRule name>, ?finding=<type>, ?team=<team>, ?q=<text>,
   ?scope=<scope>, ?theme=dark. */
(function (global) {
  'use strict';

  var THEME_KEY = 'alert-dashboard-theme';
  var SEVERITY_LABEL = { critical: 'Critical', warning: 'Warning', info: 'Info' };

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }
  function ago(iso, now) {
    if (!iso) return '';
    var m = Math.max(0, Math.round((now - Date.parse(iso)) / 60000));
    if (m < 60) return m + 'm';
    if (m < 60 * 48) return Math.round(m / 60) + 'h';
    return Math.round(m / 1440) + 'd';
  }
  function until(iso, now) {
    var m = Math.round((Date.parse(iso) - now) / 60000);
    if (m <= 0) return 'expired ' + ago(iso, now) + ' ago';
    if (m < 60 * 48) return 'ends in ' + Math.round(m / 60) + 'h';
    return 'ends in ' + Math.round(m / 1440) + 'd';
  }
  function pct(x) {
    if (!x) return '0%';
    if (x >= 0.995) return '100%';
    if (x < 0.001) return '<0.1%';
    if (x < 0.1) return (Math.round(x * 1000) / 10) + '%';
    return Math.round(x * 100) + '%';
  }
  function params() {
    try { return new URLSearchParams(location.search); } catch (e) { return { get: function () { return null; } }; }
  }

  var ICON_CLOSE = '<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><path d="M6 6l12 12M18 6L6 18"/></svg>';
  var ICON_BELL = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 8a6 6 0 0 0-12 0c0 7-3 9-3 9h18s-3-2-3-9"/><path d="M13.7 21a2 2 0 0 1-3.4 0"/></svg>';
  var ICON_SEARCH = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="M20 20l-3.5-3.5"/></svg>';

  var TEMPLATE =
    '<div class="ad-head">' +
      '<div class="ad-title"><span class="ad-logo">' + ICON_BELL + '</span><h3>Alert rule inventory</h3></div>' +
      '<span class="ad-badge"><i></i>Demo data</span>' +
      '<span class="ad-sync" data-el="sync"></span>' +
      '<div class="ad-tools">' +
        '<div class="ad-seg" role="group" aria-label="AlertScope" data-el="scopes"></div>' +
        '<div class="ad-seg" role="group" aria-label="Colour theme" data-el="themes">' +
          '<button type="button" data-theme="light">Light</button><button type="button" data-theme="dark">Dark</button></div>' +
        '<button class="ad-install" type="button" data-el="install" hidden>Install app</button>' +
        '<button class="ad-close" type="button" data-el="close" aria-label="Close the dashboard" hidden>' + ICON_CLOSE + '</button>' +
      '</div>' +
    '</div>' +
    '<div class="ad-body">' +
      '<div class="ad-scopes" data-el="scopeCards"></div>' +
      '<div class="ad-tiles" data-el="tiles"></div>' +
      '<div class="ad-grid">' +
        '<section class="ad-card">' +
          '<h4>Findings</h4>' +
          '<p class="sub">Rules with each finding, from <code>status.findings</code>. Pick one to filter the list.</p>' +
          '<div class="ad-findings" data-el="findings"></div>' +
        '</section>' +
        '<section class="ad-card">' +
          '<h4>Teams</h4>' +
          '<p class="sub">Rules and findings per owner, from <code>status.teams</code> of the scope.</p>' +
          '<div class="ad-teams" data-el="teams"></div>' +
          '<h4 class="ad-h4b">Noisiest rules</h4>' +
          '<p class="sub">Activations in the window (<code>hs_alert_rule_activations</code>).</p>' +
          '<ol class="ad-noisy" data-el="noisy"></ol>' +
        '</section>' +
      '</div>' +
      '<section class="ad-card ad-rules">' +
        '<div class="ad-rules-head"><h4>Alerting rules <span class="ad-count" data-el="count"></span></h4>' +
          '<p class="sub">One <code>AlertRule</code> per rule, in the namespace of its PrometheusRule. Pick a rule for its details.</p></div>' +
        '<div class="ad-filters">' +
          '<label class="ad-search">' + ICON_SEARCH + '<input type="search" data-el="q" placeholder="Search alert, namespace, expression" aria-label="Search rules"></label>' +
          '<select data-el="team" aria-label="Team"></select>' +
          '<select data-el="ns" aria-label="Namespace"></select>' +
          '<select data-el="state" aria-label="Loaded"><option value="">Loaded or not</option><option value="loaded">Loaded</option><option value="notloaded">Not loaded</option></select>' +
          '<select data-el="finding" aria-label="Finding"></select>' +
          '<button type="button" class="ad-clear" data-el="clear">Clear</button>' +
        '</div>' +
        '<div class="ad-list" role="table" aria-label="Alerting rules">' +
          '<div class="ad-row ad-row-head" role="row"><span role="columnheader">Alert</span><span role="columnheader" class="c-ns">Namespace</span>' +
            '<span role="columnheader">Team</span><span role="columnheader" class="c-num">Fired</span><span role="columnheader" class="c-last">Last fired</span>' +
            '<span role="columnheader">Findings</span></div>' +
          '<div data-el="rows"></div>' +
        '</div>' +
      '</section>' +
    '</div>' +
    '<div class="ad-foot">Made-up data from a fictional cluster with two Prometheus servers. The objects, findings and messages ' +
      'follow what Alert Operator 0.1 writes: <code>AlertScope</code> and <code>AlertRule</code> objects, the <code>hs_alert_*</code> metrics ' +
      'and the "Alert quality" Grafana dashboard that ships with the chart. This app itself is a demo and is not part of the release; ' +
      'in a cluster, <code>kubectl get alertrules -A</code> shows the same list.</div>' +
    '<div class="ad-drawer-back" data-el="back" hidden></div>' +
    '<aside class="ad-drawer" data-el="drawer" role="dialog" aria-modal="true" aria-label="Rule details" hidden></aside>';

  function mount(container, options) {
    options = options || {};
    var data = global.AlertDemoData.build();
    var now = data.now;
    container.classList.add('ad');
    container.innerHTML = TEMPLATE;
    var el = {};
    Array.prototype.forEach.call(container.querySelectorAll('[data-el]'), function (n) { el[n.getAttribute('data-el')] = n; });

    var q = params();
    var state = {
      scope: q.get('scope') || '', q: q.get('q') || '', team: q.get('team') || '', ns: q.get('ns') || '',
      loaded: q.get('loaded') || '', finding: q.get('finding') || '', open: null
    };

    // theme
    var theme = q.get('theme');
    if (theme !== 'dark' && theme !== 'light') { try { theme = localStorage.getItem(THEME_KEY); } catch (e) { theme = null; } }
    if (theme !== 'dark' && theme !== 'light') theme = 'light';
    function applyTheme(t) {
      theme = t;
      container.setAttribute('data-ad-theme', t);
      Array.prototype.forEach.call(el.themes.querySelectorAll('button'), function (b) {
        b.setAttribute('aria-pressed', String(b.getAttribute('data-theme') === t));
      });
      try { localStorage.setItem(THEME_KEY, t); } catch (e) { /* private mode */ }
      if (options.onTheme) options.onTheme(t);
    }
    el.themes.addEventListener('click', function (e) { var t = e.target.getAttribute('data-theme'); if (t) applyTheme(t); });
    applyTheme(theme);

    if (options.modal) { el.close.hidden = false; el.close.addEventListener('click', function () { if (options.onClose) options.onClose(); }); }

    // install prompt (only where the browser offers one)
    var deferred = null;
    global.addEventListener('beforeinstallprompt', function (e) { e.preventDefault(); deferred = e; if (!options.modal) el.install.hidden = false; });
    el.install.addEventListener('click', function () { if (deferred) { deferred.prompt(); deferred = null; el.install.hidden = true; } });

    // scope switch
    el.scopes.innerHTML = '<button type="button" data-scope="">All scopes</button>' + data.scopes.map(function (s) {
      return '<button type="button" data-scope="' + esc(s.metadata.name) + '">' + esc(s.metadata.name) + '</button>';
    }).join('');
    el.scopes.addEventListener('click', function (e) {
      var b = e.target.closest('button'); if (!b) return;
      state.scope = b.getAttribute('data-scope'); state.ns = ''; render();
    });

    function inScope(r) { return !state.scope || r.spec.scopeRef === state.scope; }
    function scopesShown() { return data.scopes.filter(function (s) { return !state.scope || s.metadata.name === state.scope; }); }
    function matches(r) {
      if (!inScope(r)) return false;
      if (state.team && r.status.team !== state.team) return false;
      if (state.ns && r.metadata.namespace !== state.ns) return false;
      if (state.loaded === 'loaded' && !r.status.loaded) return false;
      if (state.loaded === 'notloaded' && r.status.loaded) return false;
      if (state.finding === 'any' && !r.status.findings.length) return false;
      if (state.finding === 'none' && r.status.findings.length) return false;
      if (state.finding && state.finding !== 'any' && state.finding !== 'none' &&
          !r.status.findings.some(function (f) { return f.type === state.finding; })) return false;
      if (state.q) {
        var hay = [r.spec.source.alert, r.metadata.namespace, r.metadata.name, r.spec.expr, r.status.team,
          r.spec.source.name || r.spec.source.file, r.spec.source.group].join(' ').toLowerCase();
        var words = state.q.toLowerCase().split(/\s+/).filter(Boolean);
        for (var i = 0; i < words.length; i++) if (hay.indexOf(words[i]) < 0) return false;
      }
      return true;
    }

    function render() {
      Array.prototype.forEach.call(el.scopes.querySelectorAll('button'), function (b) {
        b.setAttribute('aria-pressed', String(b.getAttribute('data-scope') === state.scope));
      });
      var shown = scopesShown();
      var newest = Math.min.apply(null, shown.map(function (s) { return now - Date.parse(s.status.lastSync); }));
      el.sync.textContent = 'synced ' + Math.round(newest / 60000) + 'm ago';
      renderScopes(shown); renderTiles(shown); renderFindings(); renderTeams(shown); renderNoisy(); renderFilters(); renderRows();
    }

    function renderScopes(shown) {
      el.scopeCards.innerHTML = shown.map(function (s) {
        var st = s.status, ready = st.conditions[0];
        var capped = s.spec.quality.window !== st.effectiveWindow;
        return '<div class="ad-scope">' +
          '<div class="ad-scope-top"><b>AlertScope ' + esc(s.metadata.name) + '</b>' +
            '<span class="ad-pill ok" title="' + esc(ready.message) + '">Ready · ' + esc(ready.reason) + '</span></div>' +
          '<div class="ad-scope-meta mono">' + esc(s.spec.prometheus.resourceRef) + '</div>' +
          '<dl>' +
            '<div><dt>Rules</dt><dd>' + st.rules.total + '</dd></div>' +
            '<div><dt>Loaded</dt><dd>' + st.rules.loaded + '</dd></div>' +
            '<div><dt>Findings</dt><dd>' + st.findingsTotal + '</dd></div>' +
            '<div><dt>Window</dt><dd>' + esc(st.effectiveWindow) + (capped ? '<small title="spec.quality.window ' + esc(s.spec.quality.window) + ', capped to the retention Prometheus reports"> of ' + esc(s.spec.quality.window) + '</small>' : '') + '</dd></div>' +
            '<div><dt>Last sync</dt><dd>' + ago(st.lastSync, now) + '</dd></div>' +
          '</dl>' +
          (s.spec.alertmanager ? '' : '<p class="ad-scope-note">No Alertmanager in this scope, so AlwaysSilenced is not evaluated.</p>') +
        '</div>';
      }).join('');
    }

    function renderTiles(shown) {
      var rules = data.rules.filter(inScope);
      var total = 0, loaded = 0, notLoaded = 0, findings = 0, crit = 0, clean = 0;
      shown.forEach(function (s) { total += s.status.rules.total; loaded += s.status.rules.loaded; notLoaded += s.status.rules.notLoaded; findings += s.status.findingsTotal; });
      rules.forEach(function (r) {
        if (r.status.findings.some(function (f) { return f.severity === 'critical'; })) crit++;
        if (!r.status.findings.length) clean++;
      });
      var tiles = [
        { v: total, l: 'Alerting rules', s: loaded + ' loaded by Prometheus' },
        { v: notLoaded, l: 'Not loaded', s: 'defined, but Prometheus never runs them', cls: notLoaded ? 'crit' : '', f: 'NotLoaded' },
        { v: crit, l: 'Rules with a critical finding', s: 'NotLoaded or LoadError', cls: crit ? 'crit' : '' },
        { v: findings, l: 'Findings', s: 'on ' + (rules.length - clean) + ' rules', f: 'any' },
        { v: clean, l: 'Rules without findings', s: Math.round(clean * 100 / Math.max(1, rules.length)) + '% of the rules', cls: 'ok', f: 'none' }
      ];
      el.tiles.innerHTML = tiles.map(function (t) {
        var tag = t.f ? 'button type="button" data-finding="' + t.f + '"' : 'div';
        return '<' + tag + ' class="ad-tile ' + (t.cls || '') + '"><b>' + t.v + '</b><span>' + esc(t.l) + '</span><small>' + esc(t.s) + '</small></' + (t.f ? 'button' : 'div') + '>';
      }).join('');
    }

    function renderFindings() {
      var rules = data.rules.filter(inScope);
      var max = 1, counts = {};
      data.findingTypes.forEach(function (f) {
        counts[f.type] = rules.filter(function (r) { return r.status.findings.some(function (x) { return x.type === f.type; }); }).length;
        max = Math.max(max, counts[f.type]);
      });
      el.findings.innerHTML = data.findingTypes.map(function (f) {
        var n = counts[f.type];
        return '<button type="button" class="ad-frow' + (state.finding === f.type ? ' on' : '') + (n ? '' : ' zero') + '" data-finding="' + f.type + '" aria-pressed="' + (state.finding === f.type) + '">' +
          '<span class="ad-sev ' + f.severity + '">' + SEVERITY_LABEL[f.severity] + '</span>' +
          '<span class="ad-fname">' + f.type + '</span>' +
          '<span class="ad-fbar"><i class="' + f.severity + '" style="width:' + Math.round(n * 100 / max) + '%"></i></span>' +
          '<span class="ad-fnum">' + n + '</span></button>';
      }).join('');
    }

    function renderTeams(shown) {
      var teams = {};
      shown.forEach(function (s) {
        s.status.teams.forEach(function (t) {
          var x = teams[t.team] || (teams[t.team] = { team: t.team, rules: 0, findings: 0 });
          x.rules += t.rules; x.findings += t.findings;
        });
      });
      var list = Object.keys(teams).map(function (k) { return teams[k]; })
        .sort(function (a, b) { return (a.team === '<none>') - (b.team === '<none>') || b.rules - a.rules; });
      var max = Math.max.apply(null, list.map(function (t) { return Math.max(t.rules, t.findings); }).concat([1]));
      el.teams.innerHTML = '<div class="ad-legend"><span><i class="r"></i>Rules</span><span><i class="f"></i>Findings</span></div>' + list.map(function (t) {
        return '<button type="button" class="ad-team' + (state.team === t.team ? ' on' : '') + '" data-team="' + esc(t.team) + '" aria-pressed="' + (state.team === t.team) + '">' +
          '<span class="ad-tname' + (t.team === '<none>' ? ' none' : '') + '">' + esc(t.team === '<none>' ? 'no team' : t.team) + '</span>' +
          '<span class="ad-tbars"><i class="r" style="width:' + Math.round(t.rules * 100 / max) + '%"></i><i class="f" style="width:' + Math.round(t.findings * 100 / max) + '%"></i></span>' +
          '<span class="ad-tnum">' + t.rules + ' · ' + t.findings + '</span></button>';
      }).join('');
    }

    function renderNoisy() {
      var top = data.rules.filter(function (r) { return inScope(r) && r.status.quality && r.status.quality.activations; })
        .sort(function (a, b) { return b.status.quality.activations - a.status.quality.activations; }).slice(0, 5);
      el.noisy.innerHTML = top.map(function (r) {
        return '<li><button type="button" data-rule="' + esc(r.metadata.name) + '"><span class="mono">' + esc(r.spec.source.alert) + '</span>' +
          '<span class="ad-nmeta">' + esc(r.status.team) + ' · ' + r.status.quality.activationsPerDay + '/day</span>' +
          '<b>' + r.status.quality.activations + '</b></button></li>';
      }).join('');
    }

    function optionTags(list, value, all) {
      return '<option value="">' + esc(all) + '</option>' + list.map(function (o) {
        var v = typeof o === 'string' ? o : o.v, l = typeof o === 'string' ? o : o.l;
        return '<option value="' + esc(v) + '"' + (v === value ? ' selected' : '') + '>' + esc(l) + '</option>';
      }).join('');
    }
    function renderFilters() {
      var rules = data.rules.filter(inScope);
      var teams = [], nss = [];
      rules.forEach(function (r) {
        if (teams.indexOf(r.status.team) < 0) teams.push(r.status.team);
        if (nss.indexOf(r.metadata.namespace) < 0) nss.push(r.metadata.namespace);
      });
      teams.sort(); nss.sort();
      el.team.innerHTML = optionTags(teams.map(function (t) { return { v: t, l: t === '<none>' ? 'no team' : t }; }), state.team, 'All teams');
      el.ns.innerHTML = optionTags(nss, state.ns, 'All namespaces');
      el.finding.innerHTML = optionTags([{ v: 'any', l: 'Any finding' }, { v: 'none', l: 'No findings' }].concat(data.findingTypes.map(function (f) { return f.type; })), state.finding, 'Findings: all');
      el.state.value = state.loaded;
      if (el.q.value !== state.q) el.q.value = state.q;
      el.clear.hidden = !(state.q || state.team || state.ns || state.loaded || state.finding);
    }

    function badges(r, limit) {
      var fs = r.status.findings;
      if (!fs.length) return '<span class="ad-none">none</span>';
      var out = fs.slice(0, limit || 9).map(function (f) { return '<span class="ad-fb ' + f.severity + '">' + f.type + '</span>'; });
      if (fs.length > (limit || 9)) out.push('<span class="ad-fb more">+' + (fs.length - limit) + '</span>');
      return out.join('');
    }

    function renderRows() {
      var rows = data.rules.filter(matches).sort(function (a, b) {
        var ra = a.status.findings[0] ? { critical: 0, warning: 1, info: 2 }[a.status.findings[0].severity] : 3;
        var rb = b.status.findings[0] ? { critical: 0, warning: 1, info: 2 }[b.status.findings[0].severity] : 3;
        return ra - rb || b.status.findings.length - a.status.findings.length || a.spec.source.alert.localeCompare(b.spec.source.alert);
      });
      var total = data.rules.filter(inScope).length;
      el.count.textContent = rows.length === total ? String(total) : rows.length + ' of ' + total;
      if (!rows.length) { el.rows.innerHTML = '<p class="ad-empty">No rule matches these filters.</p>'; return; }
      el.rows.innerHTML = rows.map(function (r) {
        var qy = r.status.quality;
        return '<button type="button" class="ad-row' + (state.open === r.metadata.name ? ' on' : '') + '" role="row" data-rule="' + esc(r.metadata.name) + '">' +
          '<span class="c-alert" role="cell"><b>' + esc(r.spec.source.alert) + '</b><small class="mono">' + esc(r.metadata.name) + '</small></span>' +
          '<span class="c-ns mono" role="cell">' + esc(r.metadata.namespace) + '</span>' +
          '<span class="c-team' + (r.status.team === '<none>' ? ' none' : '') + '" role="cell">' + esc(r.status.team) + '</span>' +
          '<span class="c-num" role="cell">' + (r.status.loaded ? (qy ? qy.activations : 0) : '<span class="ad-nl">not loaded</span>') + '</span>' +
          '<span class="c-last" role="cell">' + (qy && qy.lastFired ? ago(qy.lastFired, now) + ' ago' : '<span class="ad-dim">never</span>') + '</span>' +
          '<span class="c-find" role="cell">' + badges(r) + '</span>' +
        '</button>';
      }).join('');
    }

    // detail panel
    function find(name) { for (var i = 0; i < data.rules.length; i++) if (data.rules[i].metadata.name === name) return data.rules[i]; return null; }
    function spark(r) {
      var d = r.daily, max = Math.max.apply(null, d.concat([1])), w = 100 / d.length;
      return '<svg class="ad-spark" viewBox="0 0 100 32" preserveAspectRatio="none" role="img" aria-label="Activations per day over the window">' +
        d.map(function (v, i) {
          var h = v ? Math.max(2, v * 30 / max) : 0.6;
          return '<rect x="' + (i * w + w * 0.15).toFixed(2) + '" y="' + (32 - h).toFixed(2) + '" width="' + (w * 0.7).toFixed(2) + '" height="' + h.toFixed(2) + '" class="' + (v ? 'on' : 'off') + '"/>';
        }).join('') + '</svg>';
    }
    function kv(obj) {
      var keys = Object.keys(obj || {});
      if (!keys.length) return '<span class="ad-dim">none</span>';
      return keys.map(function (k) {
        var v = obj[k];
        if (k === 'runbook_url') v = '<a href="' + esc(v) + '" target="_blank" rel="noopener noreferrer">' + esc(v) + '</a>';
        else v = esc(v);
        return '<div class="ad-kv"><span class="mono">' + esc(k) + '</span><span>' + v + '</span></div>';
      }).join('');
    }
    function yaml(r) {
      var s = r.status, sp = r.spec, lines = [];
      function y(v) { return /^[\w./<>-]+$/.test(String(v)) ? String(v) : JSON.stringify(String(v)); }
      lines.push('<span class="k">apiVersion</span>: alerts.hypersurgery.dev/v1alpha1', '<span class="k">kind</span>: AlertRule',
        '<span class="k">metadata</span>:', '  <span class="k">name</span>: ' + esc(r.metadata.name), '  <span class="k">namespace</span>: ' + esc(r.metadata.namespace),
        '<span class="k">spec</span>:', '  <span class="k">scopeRef</span>: ' + esc(sp.scopeRef), '  <span class="k">source</span>:');
      Object.keys(sp.source).forEach(function (k) { lines.push('    <span class="k">' + k + '</span>: ' + esc(y(sp.source[k]))); });
      lines.push('  <span class="k">expr</span>: ' + esc(y(sp.expr)));
      if (sp.for) lines.push('  <span class="k">for</span>: ' + esc(sp.for));
      lines.push('<span class="k">status</span>:', '  <span class="k">identity</span>: ' + esc(s.identity), '  <span class="k">loaded</span>: ' + s.loaded);
      if (s.notLoadedReason) lines.push('  <span class="k">notLoadedReason</span>: ' + esc(y(s.notLoadedReason)));
      lines.push('  <span class="k">team</span>: ' + esc(y(s.team)), '  <span class="k">window</span>: ' + esc(s.window));
      if (s.quality) {
        lines.push('  <span class="k">quality</span>:', '    <span class="k">activations</span>: ' + s.quality.activations,
          '    <span class="k">activationsPerDay</span>: ' + s.quality.activationsPerDay);
        if (s.quality.medianFiringDuration) lines.push('    <span class="k">medianFiringDuration</span>: ' + s.quality.medianFiringDuration);
        lines.push('    <span class="k">firingFraction</span>: ' + s.quality.firingFraction, '    <span class="k">silencedFraction</span>: ' + s.quality.silencedFraction);
        if (s.quality.lastFired) lines.push('    <span class="k">lastFired</span>: "' + s.quality.lastFired.replace(/\.\d+Z$/, 'Z') + '"');
      }
      if (s.findings.length) {
        lines.push('  <span class="k">findings</span>:');
        s.findings.forEach(function (f) {
          lines.push('    - <span class="k">type</span>: ' + f.type, '      <span class="k">severity</span>: ' + f.severity, '      <span class="k">message</span>: ' + esc(JSON.stringify(f.message)));
        });
      }
      var c = s.conditions[0];
      lines.push('  <span class="k">conditions</span>:', '    - { <span class="k">type</span>: Healthy, <span class="k">status</span>: "' + c.status + '", <span class="k">reason</span>: ' + c.reason + ' }');
      return lines.join('\n');
    }

    function openRule(name, push) {
      var r = find(name);
      if (!r) return;
      state.open = name;
      var s = r.status, sp = r.spec, qy = s.quality, c = s.conditions[0];
      var info = {};
      data.findingTypes.forEach(function (f) { info[f.type] = f; });
      var sev = sp.labels.severity;
      var scope = data.scopes.filter(function (x) { return x.metadata.name === sp.scopeRef; })[0];
      var html =
        '<div class="ad-dhead">' +
          '<div><div class="ad-dkicker mono">' + esc(r.metadata.namespace) + ' / ' + esc(r.metadata.name) + '</div>' +
          '<h3>' + esc(sp.source.alert) + '</h3>' +
          '<div class="ad-dtags">' +
            (sev ? '<span class="ad-tag">severity: ' + esc(sev) + '</span>' : '<span class="ad-tag warn">no severity</span>') +
            '<span class="ad-tag' + (s.team === '<none>' ? ' warn' : '') + '">team: ' + esc(s.team) + '</span>' +
            '<span class="ad-pill ' + (c.status === 'True' ? 'ok' : 'bad') + '">Healthy ' + c.status + (c.status === 'True' ? '' : ' · ' + esc(c.reason)) + '</span>' +
            (s.loaded ? '<span class="ad-pill ok">Loaded</span>' : '<span class="ad-pill bad">Not loaded</span>') +
          '</div></div>' +
          '<button type="button" class="ad-close" data-act="close" aria-label="Close the details">' + ICON_CLOSE + '</button>' +
        '</div>' +
        '<div class="ad-dbody">' +
          '<section><h5>Findings' + (s.findings.length ? ' <span class="ad-count">' + s.findings.length + '</span>' : '') + '</h5>' +
          (s.findings.length ? s.findings.map(function (f) {
            var i = info[f.type];
            return '<div class="ad-finding ' + f.severity + '"><div class="ad-fhead"><span class="ad-fb ' + f.severity + '">' + f.type + '</span>' +
              '<span class="ad-sev ' + f.severity + '">' + SEVERITY_LABEL[f.severity] + '</span><span class="ad-from">from ' + esc(i.from) + '</span></div>' +
              '<p class="ad-msg mono">' + esc(f.message) + '</p>' +
              '<p><b>Why:</b> ' + esc(i.means) + '</p><p><b>Fix:</b> ' + esc(i.fix) + '</p></div>';
          }).join('') : '<p class="ad-good">No findings: the rule is loaded, has fired in the window, has an owner, a runbook and a severity.</p>') +
          '</section>' +
          '<section><h5>Behaviour over ' + esc(s.window) + '</h5>' +
          (qy ? '<div class="ad-quality">' +
              '<div><b>' + qy.activations + '</b><span>activations</span></div>' +
              '<div><b>' + qy.activationsPerDay + '</b><span>per day</span></div>' +
              '<div><b>' + (qy.medianFiringDuration || '–') + '</b><span>median firing</span></div>' +
              '<div><b>' + pct(qy.firingFraction) + '</b><span>of the window firing</span></div>' +
              '<div><b>' + (scope.spec.alertmanager ? pct(qy.silencedFraction) : '–') + '</b><span>of firing silenced</span></div>' +
              '<div><b>' + (qy.lastFired ? ago(qy.lastFired, now) : 'never') + '</b><span>last fired' + (qy.lastFired ? ' ago' : '') + '</span></div>' +
            '</div>' + spark(r) + '<p class="sub">Activations per day, oldest on the left. From <code>ALERTS_FOR_STATE</code> and <code>ALERTS</code> in Prometheus.</p>'
            : '<p class="ad-dim">Not measured: Prometheus does not run this rule, so it has no history.</p>') +
          '</section>' +
          '<section><h5>Silences</h5>' +
          (r.silences === null ? '<p class="ad-dim">This scope reads no Alertmanager, so silences are not known and AlwaysSilenced is not evaluated.</p>'
            : r.silences.length ? r.silences.map(function (x) {
              return '<div class="ad-silence"><div><span class="ad-pill ' + (x.state === 'active' ? 'warn' : 'muted') + '">' + x.state + '</span> <span class="ad-dim">' + until(x.endsAt, now) + ' · by ' + esc(x.createdBy) + '</span></div>' +
                '<code class="mono">' + esc(x.matchers) + '</code><p>' + esc(x.comment) + '</p></div>';
            }).join('') : '<p class="ad-dim">No matching silence in Alertmanager (it keeps expired silences for five days).</p>') +
          '</section>' +
          '<section><h5>Rule</h5>' +
            '<pre class="ad-expr">' + esc(sp.expr) + '</pre>' +
            '<div class="ad-kvs">' +
              '<div class="ad-kv"><span>for</span><span class="mono">' + esc(sp.for || '0s') + '</span></div>' +
              (sp.keepFiringFor ? '<div class="ad-kv"><span>keep_firing_for</span><span class="mono">' + esc(sp.keepFiringFor) + '</span></div>' : '') +
            '</div>' +
            '<h6>Labels</h6><div class="ad-kvs">' + kv(sp.labels) + '</div>' +
            '<h6>Annotations</h6><div class="ad-kvs">' + kv(sp.annotations) + '</div>' +
            (sp.annotations.runbook_url ? '' : '<p class="ad-warnline">No runbook: whoever is paged has nothing to start from.</p>') +
          '</section>' +
          '<section><h5>Source and ownership</h5><div class="ad-kvs">' +
            '<div class="ad-kv"><span>kind</span><span>' + esc(sp.source.kind) + '</span></div>' +
            (sp.source.kind === 'File' ? '<div class="ad-kv"><span>file</span><span class="mono">' + esc(sp.source.file) + '</span></div>'
              : '<div class="ad-kv"><span>PrometheusRule</span><span class="mono">' + esc(sp.source.namespace + '/' + sp.source.name) + '</span></div>') +
            '<div class="ad-kv"><span>group</span><span class="mono">' + esc(sp.source.group) + '</span></div>' +
            '<div class="ad-kv"><span>scope</span><span class="mono">' + esc(sp.scopeRef) + ' · ' + esc(scope.spec.prometheus.resourceRef) + '</span></div>' +
            '<div class="ad-kv"><span>team</span><span>' + (s.team === '<none>' ? '<span class="ad-warnline">none found</span>' : esc(s.team) + ' <span class="ad-dim">from the ' + esc(r.teamFrom) + '</span>') + '</span></div>' +
            (s.notLoadedReason ? '<div class="ad-kv"><span>not loaded</span><span>' + esc(s.notLoadedReason) + '</span></div>' : '') +
            '<div class="ad-kv"><span>first seen</span><span>' + ago(s.firstSeen, now) + ' ago</span></div>' +
            (s.lastEvaluated ? '<div class="ad-kv"><span>last evaluated</span><span>' + ago(s.lastEvaluated, now) + ' ago</span></div>' : '') +
            '<div class="ad-kv"><span>identity</span><span class="mono">' + esc(s.identity) + '</span></div>' +
          '</div></section>' +
          '<section><h5>In the cluster</h5>' +
            '<pre class="ad-cmd"><span class="p">$</span> kubectl get hsar -n ' + esc(r.metadata.namespace) + ' ' + esc(r.metadata.name) + ' -o yaml</pre>' +
            '<details class="ad-yaml"><summary>Show the object</summary><pre>' + yaml(r) + '</pre></details>' +
          '</section>' +
        '</div>';
      el.drawer.innerHTML = html;
      el.drawer.hidden = false; el.back.hidden = false;
      container.classList.add('ad-drawer-open');
      renderRows();
      if (push !== false) {
        try { var u = new URL(location.href); if (!options.modal) { u.searchParams.set('rule', name); history.replaceState(null, '', u); } } catch (e) { /* file: */ }
      }
      var btn = el.drawer.querySelector('[data-act="close"]');
      if (btn && push !== false) btn.focus({ preventScroll: true });
      el.drawer.scrollTop = 0;
    }
    function closeRule() {
      if (!state.open) return;
      var name = state.open;
      state.open = null;
      el.drawer.hidden = true; el.back.hidden = true;
      container.classList.remove('ad-drawer-open');
      renderRows();
      try { var u = new URL(location.href); if (u.searchParams.has('rule')) { u.searchParams.delete('rule'); history.replaceState(null, '', u); } } catch (e) { /* file: */ }
      var row = el.rows.querySelector('[data-rule="' + name + '"]');
      if (row) row.focus({ preventScroll: true });
    }
    el.drawer.addEventListener('click', function (e) { if (e.target.closest('[data-act="close"]')) closeRule(); });
    el.back.addEventListener('click', closeRule);

    // interactions
    container.addEventListener('click', function (e) {
      var r = e.target.closest('[data-rule]');
      if (r && !el.drawer.contains(r)) { openRule(r.getAttribute('data-rule')); return; }
      var f = e.target.closest('[data-finding]');
      if (f && f.tagName === 'BUTTON') { var v = f.getAttribute('data-finding'); state.finding = state.finding === v ? '' : v; render(); return; }
      var t = e.target.closest('[data-team]');
      if (t) { var tv = t.getAttribute('data-team'); state.team = state.team === tv ? '' : tv; render(); }
    });
    el.q.addEventListener('input', function () { state.q = el.q.value; renderRows(); el.clear.hidden = !(state.q || state.team || state.ns || state.loaded || state.finding); });
    el.team.addEventListener('change', function () { state.team = el.team.value; render(); });
    el.ns.addEventListener('change', function () { state.ns = el.ns.value; render(); });
    el.state.addEventListener('change', function () { state.loaded = el.state.value; render(); });
    el.finding.addEventListener('change', function () { state.finding = el.finding.value; render(); });
    el.clear.addEventListener('click', function () { state.q = state.team = state.ns = state.loaded = state.finding = ''; render(); });
    var onKey = function (e) { if (e.key === 'Escape' && state.open) { e.stopPropagation(); closeRule(); } };
    document.addEventListener('keydown', onKey, true);

    render();
    if (q.get('rule')) openRule(q.get('rule'), false);

    return {
      isDetailOpen: function () { return !!state.open; },
      closeDetail: closeRule,
      openRule: openRule
    };
  }

  global.AlertDashboard = { mount: mount };
})(window);
