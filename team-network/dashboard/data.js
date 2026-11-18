/* Demo data for the Team Network dashboard: a fictional cluster with eight teams, made up but
   shaped like the objects of teams.hypersurgery.dev/v1alpha1 (TeamNetworkClass, TeamNetwork).
   The condition types, reasons and messages follow the API sketch of the concept, so the demo
   shows what the operator is meant to write, not more. Nothing here is released yet. */
(function (global) {
  'use strict';

  var MIN = 60e3, HOUR = 60 * MIN, DAY = 24 * HOUR;

  var CLASS = {
    name: 'prod',
    account: '222222222222',
    region: 'eu-central-1',
    networkID: 'vpc-0a1b2c3d4e5f60718',
    scopeRef: 'organization',
    claimNamespace: 'team-networks',
    clusterSecurityGroup: 'sg-0c1d2e3f405162738',
    nodeRole: 'KarpenterNodeRole-prod',
    tailnet: 'example.com',
    allowedGroups: ['group:payments', 'group:search', 'group:data', 'group:ml', 'group:checkout',
      'group:analytics', 'group:risk', 'group:identity'],
    route: { mode: 'external', deviceTag: 'tag:subnet-router', routerSecurityGroup: 'sg-0e7f8091a2b3c4d5e' }
  };

  // The existing EC2 subnet routers, outside the cluster: two, in different zones, one broad route.
  var ROUTERS = [
    { name: 'router-euc1a', zone: 'eu-central-1a', ip: '100.101.12.4', instance: 'i-0a7c3e91d2b4f6081', advertised: ['10.20.0.0/16'], approved: ['10.20.0.0/16'], online: true, seen: 1 },
    { name: 'router-euc1b', zone: 'eu-central-1b', ip: '100.101.40.9', instance: 'i-0d18b4f7a2c9e5306', advertised: ['10.20.0.0/16'], approved: ['10.20.0.0/16'], online: true, seen: 1 }
  ];

  // A stable made-up hex id from a seed (FNV-1a per 8-character chunk).
  function hex(seed, n) {
    var out = '';
    for (var k = 0; out.length < n; k++) {
      var h = 2166136261 ^ k;
      for (var i = 0; i < seed.length; i++) { h ^= seed.charCodeAt(i); h = Math.imul(h, 16777619) >>> 0; }
      out += ('0000000' + h.toString(16)).slice(-8);
    }
    return out.slice(0, n);
  }

  /* One entry per team. ago* are minutes before "now". */
  var TEAMS = [
    { team: 'payments', prefix: 22, zones: [['eu-central-1a', '10.20.8.0/22'], ['eu-central-1b', '10.20.12.0/22']],
      nodes: { mode: 'Managed', count: 3, cpu: '64' }, route: 'external', approval: 'propose', access: 'Proposed', age: 9 * DAY },
    { team: 'search', prefix: 22, zones: [['eu-central-1a', '10.20.16.0/22'], ['eu-central-1b', '10.20.20.0/22']],
      nodes: { mode: 'Managed', count: 5, cpu: '96' }, route: 'external', approval: 'auto', access: 'Granted', etag: '3f1c9a07e2b54d18', grantedAgo: 2 * DAY + 3 * HOUR, age: 41 * DAY },
    { team: 'data', prefix: 21, zones: [['eu-central-1a', '10.20.32.0/21'], ['eu-central-1b', '10.20.40.0/21']],
      nodes: { mode: 'Managed', count: 8, cpu: '256' }, route: 'connector', replicas: 2, approval: 'auto', access: 'Granted', etag: '9b02d4e1a7c36f55', grantedAgo: 5 * DAY + 7 * HOUR, age: 63 * DAY },
    { team: 'ml', prefix: 22, zones: [['eu-central-1a', '10.20.48.0/22'], ['eu-central-1b', '10.20.52.0/22'], ['eu-central-1c', '10.20.56.0/22']],
      nodes: { mode: 'TagOnly', count: 0 }, route: 'external', approval: 'propose', access: 'InPolicy', age: 27 * DAY },
    { team: 'checkout', prefix: 22, zones: [['eu-central-1a', '10.20.64.0/22'], ['eu-central-1b', '10.20.68.0/22']],
      nodes: { mode: 'Managed', count: 4, cpu: '64' }, route: 'external', approval: 'auto', access: 'MarkersMissing', age: 12 * DAY },
    { team: 'analytics', prefix: 22, zones: [['eu-central-1a', '10.31.4.0/22'], ['eu-central-1b', '10.31.8.0/22']],
      nodes: { mode: 'Managed', count: 2, cpu: '32' }, route: 'external', approval: 'propose', access: 'WaitingForRoute', notCovered: true, age: 6 * DAY },
    { team: 'identity', prefix: 23, zones: [['eu-central-1a', '10.20.72.0/23'], ['eu-central-1b', '10.20.74.0/23']],
      nodes: { mode: 'Managed', count: 2, cpu: '16' }, route: 'external', approval: 'auto', access: 'Granted', etag: 'c47e1d90b8a2f316', grantedAgo: 19 * HOUR, age: 18 * DAY },
    { team: 'risk', prefix: 22, zones: [['eu-central-1a', '10.20.76.0/22'], ['eu-central-1b', null]],
      nodes: { mode: 'Managed', count: 0, cpu: '32' }, route: 'external', approval: 'auto', access: 'Waiting', allocating: true, age: 4 * MIN }
  ];

  function joinAnd(list) {
    if (list.length < 2) return list.join('');
    return list.slice(0, -1).join(', ') + ' and ' + list[list.length - 1];
  }

  function grant(group, cidrs) {
    return '{"src": ["' + group + '"], "dst": [' + cidrs.map(function (c) { return '"' + c + '"'; }).join(', ') + '], "ip": ["*"]}';
  }

  function build() {
    var now = Date.now();
    function iso(agoMs) { return new Date(now - agoMs).toISOString().replace(/\.\d+Z$/, 'Z'); }

    var teams = TEAMS.map(function (t) {
      var group = 'group:' + t.team;
      var subnets = t.zones.map(function (z) {
        return { zone: z[0], cidr: z[1], subnetID: z[1] ? 'subnet-0' + hex(t.team + z[0], 16) : null, state: z[1] ? 'available' : 'pending' };
      });
      var cidrs = subnets.filter(function (s) { return s.cidr; }).map(function (s) { return s.cidr; });
      var sg = 'sg-0' + hex('sg' + t.team, 16);
      var created = t.age;
      var conds = [];
      function c(type, status, reason, message, agoMs) {
        conds.push({ type: type, status: status, reason: reason, message: message, lastTransitionTime: iso(agoMs) });
      }
      var step = created - 2 * MIN;
      var accessType = t.approval === 'propose' ? 'AccessProposed' : 'AccessGranted';

      // 1. Subnets
      if (t.allocating) {
        c('SubnetsReady', 'False', 'Allocating', 'SubnetClaim ' + CLASS.claimNamespace + '/' + t.team + ' is not Ready: 1 of 2 subnets created, eu-central-1b is being allocated', 3 * MIN);
      } else {
        c('SubnetsReady', 'True', 'Allocated', subnets.length + ' subnets in vpc-0a1b… (' + cidrs.join(', ') + ')', step);
      }
      // 2. Isolation
      if (t.allocating) c('IsolationReady', 'Unknown', 'Waiting', 'Waits for SubnetsReady', 4 * MIN);
      else c('IsolationReady', 'True', 'Applied', sg.slice(0, 7) + '… allows only ' + cidrs.join(', ') + ', the cluster and the ' + (t.route === 'connector' ? 'connector' : 'router'), step - MIN);
      // 3. Nodes
      if (t.allocating) c('NodesReady', 'Unknown', 'Waiting', 'Waits for IsolationReady', 4 * MIN);
      else if (t.nodes.mode === 'TagOnly') c('NodesReady', 'True', 'TagOnly', 'Nodes are managed elsewhere (TagOnly): subnets tagged hs/team=' + t.team, step - 2 * MIN);
      else c('NodesReady', 'True', 'NodePoolReady', 'NodePool ' + t.team + ': ' + t.nodes.count + ' nodes', step - 6 * MIN);
      // 4. Route
      if (t.allocating) c('RouteReady', 'Unknown', 'Waiting', 'Waits for NodesReady', 4 * MIN);
      else if (t.notCovered) c('RouteReady', 'False', 'NotCovered', joinAnd(cidrs) + (cidrs.length > 1 ? ' are' : ' is') + ' outside every approved route of tag:subnet-router (10.20.0.0/16)', step - 7 * MIN);
      else if (t.route === 'connector') c('RouteReady', 'True', 'ConnectorReady', 'Connector ' + t.team + ': ' + t.replicas + ' replicas, routes ' + cidrs.join(', ') + ' approved', step - 9 * MIN);
      else c('RouteReady', 'True', 'Covered', joinAnd(cidrs) + ' ' + (cidrs.length > 1 ? 'are' : 'is') + ' covered by 10.20.0.0/16, advertised and approved on router-euc1a, router-euc1b (tag:subnet-router)', step - 7 * MIN);
      // 5. Access
      var rule = grant(group, cidrs);
      var access = { state: 'Pending', rule: rule };
      switch (t.access) {
        case 'Proposed':
          c(accessType, 'True', 'Proposed', 'Add this grant between // hs:team-network begin and end in the tailnet policy file', step - 8 * MIN);
          access = { state: 'Proposed', rule: rule, proposedAt: iso(step - 8 * MIN) };
          break;
        case 'InPolicy':
          c(accessType, 'True', 'InPolicy', 'The proposed grant is in the policy file between the markers (added by a person)', 20 * DAY);
          access = { state: 'Proposed', inPolicy: true, rule: rule, proposedAt: iso(step - 8 * MIN), foundAt: iso(20 * DAY) };
          break;
        case 'Granted':
          c(accessType, 'True', 'Granted', 'Grant written to the policy file (ETag ' + t.etag.slice(0, 4) + '…), validated by Tailscale', t.grantedAgo);
          access = { state: 'Granted', rule: rule, etag: t.etag, grantedAt: iso(t.grantedAgo) };
          break;
        case 'MarkersMissing':
          c(accessType, 'False', 'MarkersMissing', 'The policy file has no // hs:team-network begin/end markers', 37 * MIN);
          access = { state: 'Failed', rule: rule, reason: 'MarkersMissing' };
          break;
        case 'WaitingForRoute':
          c(accessType, 'False', 'WaitingForRoute', 'No grant is proposed while RouteReady is False: access always comes last', step - 7 * MIN);
          access = { state: 'Pending', rule: rule, reason: 'WaitingForRoute' };
          break;
        default:
          c(accessType, 'Unknown', 'Waiting', 'Waits for RouteReady', 4 * MIN);
          access = { state: 'Pending', rule: null, reason: 'Waiting' };
      }
      // Ready
      var ready;
      if (t.allocating) ready = ['False', 'SubnetsNotReady', 'Waiting for SubnetsReady: Allocating', 4 * MIN];
      else if (t.notCovered) ready = ['False', 'RouteNotReady', 'Waiting for RouteReady: NotCovered', step - 7 * MIN];
      else if (t.access === 'MarkersMissing') ready = ['False', 'AccessNotGranted', 'Waiting for AccessGranted: MarkersMissing', 37 * MIN];
      else if (t.access === 'Proposed') ready = ['False', 'AwaitingApproval', 'Everything but access is in place; the proposed grant is not in the policy file yet', step - 8 * MIN];
      else if (t.access === 'InPolicy') ready = ['True', 'Ready', 'group:' + t.team + ' reaches ' + cidrs.join(', ') + ' over the VPN', 20 * DAY];
      else ready = ['True', 'Ready', 'group:' + t.team + ' reaches ' + cidrs.join(', ') + ' over the VPN', t.grantedAgo];
      c('Ready', ready[0], ready[1], ready[2], ready[3]);

      // Route detail
      var route;
      if (t.route === 'connector') {
        route = { mode: 'connector', connector: { name: t.team, namespace: 'tailscale', replicas: t.replicas, tag: 'tag:hs-team-' + t.team,
          devices: [0, 1].map(function (i) {
            return { name: 'ts-' + t.team + '-connector-' + i, zone: i ? 'eu-central-1b' : 'eu-central-1a', ip: '100.88.' + (40 + i) + '.' + (11 + 7 * i), advertised: cidrs, approved: cidrs, online: true };
          }) } };
      } else {
        route = { mode: 'external', deviceTag: CLASS.route.deviceTag, coveredBy: t.notCovered ? null : '10.20.0.0/16', devices: ROUTERS };
      }

      var obj = {
        apiVersion: 'teams.hypersurgery.dev/v1alpha1', kind: 'TeamNetwork',
        metadata: { name: t.team, creationTimestamp: iso(created) },
        spec: {
          classRef: CLASS.name, team: t.team,
          subnets: { prefixLength: t.prefix, zones: t.zones.map(function (z) { return z[0]; }) },
          nodes: t.nodes.mode === 'TagOnly' ? { mode: 'TagOnly' } : { limits: { cpu: t.nodes.cpu } },
          vpn: { group: group, approval: t.approval, route: t.route === 'connector' ? { mode: 'connector', connector: { replicas: t.replicas, tag: 'tag:hs-team-' + t.team } } : { mode: 'external' } }
        },
        status: {
          conditions: conds,
          subnets: subnets,
          securityGroupID: t.allocating ? null : sg,
          nodeClass: t.nodes.mode === 'TagOnly' || t.allocating ? null : t.team,
          nodePool: t.nodes.mode === 'TagOnly' || t.allocating ? null : t.team,
          nodes: t.nodes.count,
          route: route,
          access: access
        }
      };
      obj.ui = {
        nodesMode: t.nodes.mode, cpu: t.nodes.cpu, allocating: !!t.allocating,
        sgRules: t.allocating ? [] : sgRules(cidrs, t.route),
        nodeDetail: t.nodes.mode === 'TagOnly' ? null : {
          nodeClass: { name: t.team, role: CLASS.nodeRole, ami: 'al2023@latest', subnetSelector: 'hs/team=' + t.team, securityGroup: t.allocating ? '–' : sg },
          nodePool: { name: t.team, limits: 'cpu: ' + t.nodes.cpu, taint: 'team=' + t.team + ':NoSchedule', label: 'team=' + t.team, nodes: t.nodes.count }
        }
      };
      return obj;
    });

    return { now: now, cls: CLASS, routers: ROUTERS, teams: teams };
  }

  function sgRules(cidrs, routeMode) {
    var rules = cidrs.map(function (c) { return { dir: 'ingress', from: c, ports: 'all', why: 'the team\'s own subnet' }; });
    rules.push({ dir: 'ingress', from: CLASS.clusterSecurityGroup, ports: 'all', why: routeMode === 'connector'
      ? 'cluster security group: control plane, system pods and the Connector pods (VPN traffic)'
      : 'cluster security group: control plane and system pods' });
    if (routeMode !== 'connector') rules.push({ dir: 'ingress', from: CLASS.route.routerSecurityGroup, ports: 'all', why: 'subnet routers (VPN traffic)' });
    rules.push({ dir: 'egress', from: '0.0.0.0/0', ports: 'all', why: 'outbound, as the VPC allows' });
    return rules;
  }

  global.TeamNetworkDemoData = { build: build };
})(window);
