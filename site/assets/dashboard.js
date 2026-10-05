/* Subnet dashboard.
 *
 * Two data sources behind one look:
 *
 * - demo: a synthetic inventory of a fictional organization, rendered the way the operator's
 *   own metrics would be. No network calls: the numbers are generated here and drift every
 *   few seconds so the page shows something alive. The panel on the project page and the
 *   public app at /dashboard/ use it. It has AWS accounts, GCP projects and an Azure
 *   subscription (the Azure provider is from 3.0); a switch shows one cloud or all.
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

  // Clouds. Each names its things its own way: a GCP account is a project, a network is a
  // global VPC network and a subnet a regional subnetwork; an Azure account is a subscription,
  // a network a virtual network and a region a location. A provider the dashboard does not
  // know yet is shown by its name, with the neutral words.
  var PROVIDER_KEY = 'subnet-dashboard-provider';
  var PROVIDER_WORDS = {
    AWS: { account: 'account', network: 'VPC', subnet: 'subnet', region: 'region' },
    GCP: { account: 'project', network: 'VPC network', subnet: 'subnetwork', region: 'region' },
    Azure: { account: 'subscription', network: 'virtual network', subnet: 'subnet', region: 'location' }
  };
  function words(provider) {
    return PROVIDER_WORDS[provider] || { account: 'account', network: 'network', subnet: 'subnet', region: 'region' };
  }
  // The operator's default tag keys per provider (api/v1 OperatorTagKeysFor): a GCP tag key
  // and an Azure tag name cannot contain "/".
  function defaultKeys(provider) {
    return provider === 'GCP' || provider === 'Azure'
      ? { owner: 'hs-owner', env: 'hs-env', tier: 'hs-tier', managed: 'hs-managed' }
      : { owner: 'hs/owner', env: 'hs/env', tier: 'hs/tier', managed: 'hs/managed' };
  }
  function isNetworkKind(kind) { return kind === 'vpc' || kind === 'network' || kind === 'vnet'; }
  // namedByPath says whether the provider's IDs are resource paths (GCP resource names, Azure
  // resource IDs): long, and ending in the name the cloud's own console shows. Such a resource
  // is shown by that name, with the whole ID in its title, and carries its name itself, so an
  // import gives it no Name tag.
  function namedByPath(provider) { return provider === 'GCP' || provider === 'Azure'; }
  function lastSegment(id) { return String(id).split('/').pop(); }
  // The most tags Azure allows on one resource, and where the dashboard starts to say so: the
  // threshold of the NetworkTagBudgetLow alert (prometheusRule.thresholds.networkTags).
  var AZURE_TAG_LIMIT = 50, AZURE_TAG_WARN = 45;

  // providerName is a provider as the API spells it (spec.provider: AWS, GCP, Azure) for a
  // name typed in any case; a name the dashboard does not know is left in capitals, which is
  // how every provider before Azure was spelt.
  function providerName(name) {
    var known = Object.keys(PROVIDER_WORDS).filter(function (p) { return p.toLowerCase() === String(name).toLowerCase(); })[0];
    return known || String(name).toUpperCase();
  }

  // initialProvider is the cloud the page starts with: ?provider=aws|gcp|azure|all wins, then
  // the viewer's last choice; '' shows every cloud.
  function initialProvider() {
    var q = new URLSearchParams(location.search).get('provider');
    if (q) return q.toLowerCase() === 'all' ? '' : providerName(q);
    var stored = '';
    try { stored = localStorage.getItem(PROVIDER_KEY) || ''; } catch (e) { stored = ''; }
    return stored;
  }
  function rememberProvider(p) {
    try { localStorage.setItem(PROVIDER_KEY, p); } catch (e) { /* private mode */ }
  }
  // providerSwitch draws one button per provider, and one for all of them, with DOM calls so
  // the live dashboard can use it for provider names that come from the cluster.
  function providerSwitch(host, providers, current) {
    while (host.firstChild) host.removeChild(host.firstChild);
    [''].concat(providers).forEach(function (p) {
      var b = document.createElement('button');
      b.setAttribute('type', 'button');
      b.setAttribute('data-provider', p);
      b.setAttribute('aria-pressed', String(p === current));
      b.textContent = p || 'All clouds';
      host.appendChild(b);
    });
  }

  // Three AWS accounts, two GCP projects (a Shared VPC host project and a data project) and
  // two Azure subscriptions of a landing zone. Subscription IDs are UUIDs; the demo's are made up.
  var AZURE_SUB = '5b3c2a10-0000-4000-8000-00000000a2e1', AZURE_SUB_DATA = '5b3c2a10-0000-4000-8000-00000000a2e2';
  var AZURE_VNET = '/subscriptions/' + AZURE_SUB + '/resourcegroups/rg-network-prod/providers/microsoft.network/virtualnetworks/';
  var ACCOUNTS = [
    { id: '111111111111', name: 'platform', slot: 1, provider: 'AWS' },
    { id: '222222222222', name: 'payments', slot: 2, provider: 'AWS' },
    { id: '333333333333', name: 'data', slot: 3, provider: 'AWS' },
    { id: 'net-host-prod', name: 'net-host-prod', slot: 4, provider: 'GCP' },
    { id: AZURE_SUB, name: 'lz-prod', slot: 5, provider: 'Azure' }
  ];
  var ENVS = [
    { env: 'prod', used: { AWS: 0.78, GCP: 0.64, Azure: 0.71 } },
    { env: 'staging', used: { AWS: 0.46, GCP: 0.38, Azure: 0.33 } },
    { env: 'dev', used: { AWS: 0.31, GCP: 0.22, Azure: 0.27 } },
    { env: 'sandbox', used: { AWS: 0.12, GCP: 0.09, Azure: 0.06 } }
  ];
  // How much of the fictional estate each cloud is, for the figures that add the clouds up.
  var ESTATE = { AWS: 0.6, GCP: 0.25, Azure: 0.15 };
  // AWS reserves 5 addresses of a subnet, GCP 4 of a subnetwork's primary range, Azure 5 of a
  // subnet's. A GCP subnetwork's secondary ranges (a GKE cluster's Pods and Services) are not
  // counted in it. An Azure subnet cannot carry tags: "inherited" marks one whose tags are its
  // virtual network's, the others have an entry of their own on it (hs-subnet-<name>).
  var SUBNETS = [
    { id: 'subnet-0e4f5a6b', provider: 'AWS', account: '222222222222', region: 'eu-central-1', owner: 'team-payments', total: 251, free: 31, missing: [] },
    { id: 'subnet-0b19c7d2', provider: 'AWS', account: '111111111111', region: 'eu-west-1', owner: 'team-web', total: 251, free: 44, missing: [] },
    { id: 'subnet-07c8d9e0', provider: 'AWS', account: '333333333333', region: 'eu-central-1', owner: '', total: 4091, free: 812, missing: ['hs/owner'] },
    { id: 'subnet-0a1b2c3d', provider: 'AWS', account: '111111111111', region: 'eu-central-1', owner: 'team-search', total: 507, free: 126, missing: [] },
    { id: 'subnet-0d5e6f70', provider: 'AWS', account: '333333333333', region: 'eu-central-1', owner: 'team-data', total: 1019, free: 301, missing: ['hs/tier'] },
    { id: 'subnet-0c3b2a19', provider: 'AWS', account: '222222222222', region: 'eu-west-1', owner: 'team-platform', total: 251, free: 96, missing: [] },
    { id: 'payments-checkout', provider: 'GCP', account: 'net-host-prod', region: 'europe-west1', network: 'shared-prod', owner: 'team-payments', total: 252, free: 22, missing: [], secondary: [] },
    { id: 'gke-prod-nodes', provider: 'GCP', account: 'net-host-prod', region: 'europe-west1', network: 'shared-prod', owner: 'team-platform', total: 1020, free: 356, missing: [],
      secondary: [{ name: 'pods', cidr: '10.64.0.0/14' }, { name: 'services', cidr: '10.68.0.0/20' }] },
    { id: 'batch-ew4', provider: 'GCP', account: 'net-host-prod', region: 'europe-west4', network: 'shared-prod', owner: '', total: 508, free: 390, missing: ['hs-owner'], secondary: [] },
    { id: 'aks-prod-nodes', provider: 'Azure', account: AZURE_SUB, region: 'westeurope', network: 'vnet-prod', owner: 'team-platform', total: 1019, free: 148, missing: [], inherited: true },
    { id: 'payments-checkout', provider: 'Azure', account: AZURE_SUB, region: 'westeurope', network: 'vnet-prod', owner: 'team-payments', total: 251, free: 87, missing: [], inherited: false },
    { id: 'legacy-db', provider: 'Azure', account: AZURE_SUB, region: 'northeurope', network: 'vnet-legacy', owner: '', total: 123, free: 61, missing: ['hs-owner'], inherited: true }
  ];
  var TARGETS = [
    { provider: 'AWS', account: '111111111111', region: 'eu-central-1', ok: true, subnets: 96 },
    { provider: 'AWS', account: '111111111111', region: 'eu-west-1', ok: true, subnets: 64 },
    { provider: 'AWS', account: '222222222222', region: 'eu-central-1', ok: true, subnets: 88 },
    { provider: 'AWS', account: '222222222222', region: 'eu-west-1', ok: true, subnets: 41 },
    { provider: 'AWS', account: '333333333333', region: 'eu-central-1', ok: false, error: 'AccessDenied', subnets: 29 },
    { provider: 'GCP', account: 'net-host-prod', region: 'europe-west1', ok: true, subnets: 34 },
    { provider: 'GCP', account: 'net-host-prod', region: 'europe-west4', ok: true, subnets: 18 },
    { provider: 'GCP', account: 'data-lake-prod', region: 'europe-west4', ok: false, error: 'cannot impersonate subnet-reader@data-lake-prod', subnets: 12 },
    { provider: 'Azure', account: AZURE_SUB, region: 'westeurope', ok: true, subnets: 27 },
    { provider: 'Azure', account: AZURE_SUB, region: 'northeurope', ok: true, subnets: 9 },
    { provider: 'Azure', account: AZURE_SUB_DATA, region: 'westeurope', ok: false, error: 'cannot authenticate as client 3f0c6a1e-…-00b2: AADSTS70021 (no matching federated identity credential)', subnets: 6 }
  ];
  // What the tiles say about each cloud beyond what the rows above add up to: networks whose
  // ranges overlap (GCP also tells the peered ones apart), and Azure virtual networks close to
  // the 50 tags a resource may carry, their subnets' ownership entries included.
  var CLOUD_FACTS = {
    AWS: { overlapping: 2, peered: 0, moreMissing: 9 },
    GCP: { overlapping: 1, peered: 1, moreMissing: 0 },
    Azure: { overlapping: 1, peered: 0, moreMissing: 0, tagsLow: 1 }
  };
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
  // On GCP the creator is the principalEmail of the audit log entry the change events bring.
  var GCP_CREATORS = [
    { name: 'R. Chen', handle: 'r.chen', principal: 'r.chen@example.com', via: 'console' },
    { name: 'Deploy bot', handle: 'deploy-bot', principal: 'deploy-bot@net-host-prod.iam.gserviceaccount.com', via: 'gcloud' }
  ];
  var POLICY = {
    fromCreator: [
      { match: 'assumed-role/payments-', owner: 'team-payments' },
      { match: 'assumed-role/data-platform-', owner: 'team-data' },
      { match: 'assumed-role/web-', owner: 'team-web' }
    ],
    skip: [{ match: 'assumed-role/terraform-' }],
    inheritFromVPC: {
      'vpc-0aa11bb2': { owner: 'team-platform', env: 'prod' }, 'vpc-0cc3d4e5': { owner: 'team-payments', env: 'prod' },
      'projects/net-host-prod/global/networks/shared-prod': { owner: 'team-platform', env: 'prod' }
    }
  };
  POLICY.inheritFromVPC[AZURE_VNET + 'vnet-prod'] = { owner: 'team-platform', env: 'prod' };
  var UNMANAGED = [
    { kind: 'vpc', id: 'vpc-0f3e2a1b7c9d4e56', name: 'legacy-shared', account: '333333333333', region: 'eu-central-1', cidr: '10.90.0.0/16', subnets: 6, tf: true, by: CREATORS[3], ago: 3100 },
    { kind: 'subnet', id: 'subnet-04d1c2b3a4e5f607', name: 'data-lake-a', account: '333333333333', region: 'eu-central-1', cidr: '10.30.4.0/24', vpc: 'vpc-0dd4e5f6', tf: false, by: CREATORS[1], ago: 240 },
    { kind: 'subnet', id: 'subnet-0b2c3d4e5f6a7b89', name: '', account: '222222222222', region: 'eu-west-1', cidr: '10.20.40.0/22', vpc: 'vpc-0cc3d4e5', tf: false, by: CREATORS[2], ago: 250 },
    { kind: 'vpc', id: 'vpc-09e8d7c6b5a43210', name: 'sandbox-ml', account: '111111111111', region: 'eu-west-1', cidr: '172.31.0.0/16', subnets: 3, tf: false, by: CREATORS[2], ago: 5400 },
    { kind: 'subnet', id: 'subnet-0c9b8a7f6e5d4c3b', name: 'ops-tools', account: '111111111111', region: 'eu-central-1', cidr: '10.20.9.0/24', vpc: 'vpc-0aa11bb2', tf: true, by: CREATORS[3], ago: 8600 },
    // GCP: a subnetwork whose owner the policy inherits from its network, and a network made
    // before change events were set up, whose creator nobody recorded.
    { provider: 'GCP', kind: 'subnetwork', id: 'projects/net-host-prod/regions/europe-west1/subnetworks/ml-scratch', name: 'ml-scratch', account: 'net-host-prod', region: 'europe-west1', cidr: '10.61.8.0/22', vpc: 'projects/net-host-prod/global/networks/shared-prod', tf: false, by: GCP_CREATORS[0], ago: 1900 },
    { provider: 'GCP', kind: 'network', id: 'projects/net-host-prod/global/networks/analytics-sandbox', name: 'analytics-sandbox', account: 'net-host-prod', region: '', cidr: '', subnets: 2, tf: false, by: null, ago: 7200 },
    // Azure: the resource events do not say who created a resource, so there is never a creator
    // to go by: a subnet can inherit its owner from its virtual network, a virtual network
    // needs an account default or a person.
    { provider: 'Azure', kind: 'subnet', id: AZURE_VNET + 'vnet-prod/subnets/ml-scratch', name: 'ml-scratch', account: AZURE_SUB, region: 'westeurope', cidr: '10.70.8.0/24', vpc: AZURE_VNET + 'vnet-prod', tf: false, by: null, ago: 2600 },
    { provider: 'Azure', kind: 'vnet', id: AZURE_VNET + 'vnet-sandbox', name: 'vnet-sandbox', account: AZURE_SUB, region: 'northeurope', cidr: '10.90.0.0/16', subnets: 3, tf: false, by: null, ago: 9400 }
  ];
  UNMANAGED.forEach(function (r) { r.provider = r.provider || 'AWS'; });
  // Subscription IDs say nothing to a reader; the demo shows the subscription's name next to
  // an Azure resource, as the portal does.
  var ACCOUNT_NAMES = {};
  ACCOUNT_NAMES[AZURE_SUB] = 'lz-prod';
  ACCOUNT_NAMES[AZURE_SUB_DATA] = 'lz-data';
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
      ] },
    // GCP: one regional subnetwork per claim, carved from spec.gcp.poolCIDRs, named namePrefix.
    { provider: 'GCP', ns: 'payments', name: 'checkout', vpc: 'shared-prod', prefix: 24, mode: 'Create', owner: 'team-payments', pool: '10.60.0.0/16',
      allocations: [
        { az: '', name: 'payments-checkout', cidr: '10.60.0.0/24', subnet: 'payments-checkout', state: 'Created' }
      ] },
    { provider: 'GCP', ns: 'data', name: 'etl-ew4', vpc: 'shared-prod', prefix: 23, mode: 'Create', owner: 'team-data', pool: '10.60.0.0/16',
      allocations: [
        { az: '', name: 'etl-ew4', cidr: '10.60.2.0/23', subnet: '', state: 'Failed', error: 'TagValueMissing · 123456789012/hs-owner/team-data' }
      ] },
    // Azure: one regional subnet per claim, carved from the virtual network's address space and
    // named namePrefix. Its ownership is written first, as a tag on the virtual network, so a
    // network that already carries 50 tags gets no new subnet.
    { provider: 'Azure', ns: 'payments', name: 'checkout', vpc: 'vnet-prod', prefix: 24, mode: 'Create', owner: 'team-payments',
      allocations: [
        { az: '', name: 'payments-checkout', cidr: '10.70.4.0/24', subnet: 'payments-checkout', state: 'Created' }
      ] },
    { provider: 'Azure', ns: 'data', name: 'reporting', vpc: 'vnet-legacy', prefix: 26, mode: 'Create', owner: 'team-data',
      allocations: [
        { az: '', name: 'reporting', cidr: '10.80.3.0/26', subnet: '', state: 'Failed', error: 'TagBudgetExceeded · vnet-legacy carries 50 tags' }
      ] }
  ];
  CLAIMS.forEach(function (c) { c.provider = c.provider || 'AWS'; });
  var AUTO_KEY = 'subnet-dashboard-autoimport';
  var autoMode = 'dryrun';   // off | dryrun | apply
  var autoLog = [];          // what the policy did, newest first
  var spawnCounter = 0;

  var EVENT_NAMES = ['CreateSubnet', 'CreateTags', 'AssociateRouteTable', 'DeleteSubnet', 'CreateVpc', 'CreateRoute'];
  // Audit log methods on GCP: Compute's for networks and subnetworks, Resource Manager's for tags.
  var GCP_EVENT_NAMES = ['subnetworks.insert', 'subnetworks.patch', 'subnetworks.expandIpCidrRange', 'CreateTagBinding', 'networks.addPeering'];
  // Event Grid resource events on Azure: a write or a delete that succeeded, and what it was on.
  // A tag write on the virtual network is also how a subnet's ownership changes.
  var AZURE_EVENT_NAMES = ['ResourceWriteSuccess · subnets', 'ResourceWriteSuccess · virtualNetworks', 'ResourceWriteSuccess · tags',
    'ResourceDeleteSuccess · subnets', 'ResourceWriteSuccess · virtualNetworkPeerings'];
  var EVENT_NAMES_BY_PROVIDER = { GCP: GCP_EVENT_NAMES, Azure: AZURE_EVENT_NAMES };
  // What an import's tag write comes back as in the change events of each cloud.
  var TAG_EVENT = { GCP: 'CreateTagBinding', Azure: 'ResourceWriteSuccess · tags' };

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
        '<div class="dash-themes dash-providers" role="group" aria-label="Cloud provider" data-el="providers"></div>' +
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
          '<p class="sub">Last 24 hours, one line per AWS account, GCP project or Azure subscription.</p>' +
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
          '<h4>Accounts, projects, subscriptions and regions</h4>' +
          '<p class="sub">Discovery state per target, and the events that triggered the last syncs.</p>' +
          '<table><thead><tr><th class="mono">Account / project / subscription</th><th>Region</th><th>State</th><th class="hide-sm">Subnets</th></tr></thead>' +
          '<tbody data-el="targets"></tbody></table>' +
          '<p class="sub" style="margin:14px 0 8px">Change events (CloudTrail on AWS, audit logs through Pub/Sub on GCP, Event Grid on Azure)</p>' +
          '<ul class="dash-feed" data-el="feed"></ul>' +
        '</div>' +
      '</div>' +
      '<div class="dash-card dash-unmanaged">' +
        '<div class="dash-unmanaged-head">' +
          '<div><h4>Unmanaged resources <span class="dash-count" data-el="unmanagedCount"></span></h4>' +
          '<p class="sub">Discovered, but not tagged for the operator. On AWS and GCP the change events say who created each one (CloudTrail, audit logs); Azure\'s do not. The policy decides what happens next.</p></div>' +
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
        '<table><thead><tr><th class="mono">Claim</th><th class="hide-sm">Network</th><th>Mode</th><th>Allocations</th><th>State</th></tr></thead>' +
        '<tbody data-el="claims"></tbody></table>' +
      '</div>' +
      '<p class="dash-proto" data-el="costNote"><b>Prototype.</b> The cost panels below are an idea, not a feature: the operator ' +
        'computes and exports no cost data. They show synthetic AWS figures only.</p>' +
      '<div class="dash-costs" data-el="costTiles"></div>' +
      '<div class="dash-grid" data-el="costGrid">' +
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
    '<div class="dash-foot">Synthetic data from a fictional organization: nothing here comes from a real AWS account, Google Cloud project or Azure subscription. ' +
      'The Azure rows show what the Azure provider of 3.0 reports. ' +
      'In a real installation the inventory panels ship as a Grafana dashboard with the Helm chart. ' +
      'The cost panels are a prototype: resources attributed by tag and priced from the public list, not billed amounts, and nothing the operator computes. ' +
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

    var DEMO_PROVIDERS = ['AWS', 'GCP', 'Azure'];
    var only = initialProvider();
    if (DEMO_PROVIDERS.indexOf(only) < 0) only = '';
    // shown says whether a row of the demo data belongs to the cloud the switch shows.
    function shown(x) { return !only || x.provider === only; }
    // An environment's utilization across the clouds shown; the AWS side is the larger estate.
    function envUsed(e) {
      return only ? e.used[only] : DEMO_PROVIDERS.reduce(function (n, p) { return n + e.used[p] * ESTATE[p]; }, 0);
    }
    // fact adds one of CLOUD_FACTS up over the clouds shown.
    function fact(name) {
      return DEMO_PROVIDERS.reduce(function (n, p) { return n + (shown({ provider: p }) ? CLOUD_FACTS[p][name] || 0 : 0); }, 0);
    }

    var series = ACCOUNTS.map(function (a, i) {
      var base = [9200, 6400, 4100, 5200, 3100][i], points = [];
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

    function visibleSeries() { return series.filter(function (s) { return shown(s.account); }); }

    function renderTiles() {
      var targets = TARGETS.filter(shown), subnets = SUBNETS.filter(shown);
      var totalSubnets = targets.reduce(function (n, t) { return n + t.subnets; }, 0);
      var free = visibleSeries().reduce(function (n, s) { return n + s.points[s.points.length - 1]; }, 0);
      var hot = subnets.filter(function (s) { return usedRatio(s) >= 0.8; }).length;
      // The table shows a few subnets; the tile counts the whole (fictional) estate.
      var missing = subnets.filter(function (s) { return s.missing.length; }).length + fact('moreMissing');
      var down = targets.filter(function (t) { return !t.ok; }).length;
      var overlapping = fact('overlapping'), peered = fact('peered');
      var tiles = [
        { v: fmt(totalSubnets), l: 'subnets tracked', c: '' },
        { v: fmt(free), l: 'free IPv4 addresses', c: '' },
        { v: hot, l: 'at 80% or more', c: hot ? 'warn' : 'ok' },
        { v: missing, l: 'missing required tags', c: missing ? 'warn' : 'ok' },
        // On GCP the ranges compared are the subnetworks', and peered networks are told apart.
        { v: overlapping, l: 'networks with overlapping ' + (only === 'GCP' ? 'ranges' : 'CIDRs') + (peered ? ', ' + peered + ' peered' : ''), c: 'bad' },
        { v: down, l: 'unreachable ' + (only ? words(only).account + 's' : 'targets'), c: down ? 'bad' : 'ok' }
      ];
      // Only Azure has a tag budget: a virtual network's 50 tags also hold its subnets' ownership.
      if (shown({ provider: 'Azure' })) {
        tiles.push({ v: fact('tagsLow'), l: 'virtual networks at ' + AZURE_TAG_WARN + '+ of ' + AZURE_TAG_LIMIT + ' tags', c: fact('tagsLow') ? 'warn' : 'ok' });
      }
      el.tiles.classList.toggle('dash-tiles-7', tiles.length > 6);
      el.tiles.innerHTML = tiles.map(function (t) {
        return '<div class="dash-tile ' + t.c + '"><b>' + t.v + '</b><span>' + t.l + '</span></div>';
      }).join('');
    }

    function renderBars() {
      el.bars.innerHTML = ENVS.map(function (e) {
        var used = envUsed(e);
        return '<div class="dash-bar"><span>' + e.env + '</span>' +
          '<span class="track"><span class="fill" style="width:' + (used * 100).toFixed(1) +
          '%;background:' + statusColor(used) + '"></span></span>' +
          '<span class="val">' + pct(used) + '</span></div>';
      }).join('');
    }

    function renderSpark() {
      var series = visibleSeries();
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
          (s.account.provider === 'GCP' ? 'GCP project ' + s.account.id
            : s.account.provider === 'Azure' ? 'Azure subscription ' + s.account.name
              : s.account.name + ' · ' + s.account.id) + '</span>';
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
          visibleSeries().map(function (s) {
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
      var rows = SUBNETS.filter(shown).sort(function (a, b) { return usedRatio(b) - usedRatio(a); }).slice(0, 6);
      el.subnets.innerHTML = rows.map(function (s) {
        var r = usedRatio(s);
        // A GCP subnetwork is regional and named by its project; its secondary ranges have usage
        // of their own, which the operator reports next to the primary range, not in it.
        // An Azure subnet has no tags of its own: what it shows is its entry on its virtual
        // network, or the virtual network's own tags.
        var where = s.provider === 'GCP'
          ? 'GCP subnetwork · ' + s.account + ' · ' + s.region + (s.secondary.length ? ' · ' + s.secondary.length + ' secondary ranges' : '')
          : s.provider === 'Azure'
            ? 'Azure subnet · ' + s.network + ' · ' + s.region + (s.inherited ? ' · tags of its virtual network' : '')
            : 'AWS subnet · ' + s.account + ' · ' + s.region;
        return '<tr><td class="mono">' + s.id + '<div class="dash-sub2">' + esc(where) + '</div></td>' +
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
      el.targets.innerHTML = TARGETS.filter(shown).map(function (t) {
        return '<tr><td class="mono">' + t.account + '<div class="dash-sub2">' + t.provider + ' ' + words(t.provider).account +
          (ACCOUNT_NAMES[t.account] ? ' ' + ACCOUNT_NAMES[t.account] : '') + '</div></td>' +
          '<td>' + t.region + '</td>' +
          '<td><span class="dash-state ' + (t.ok ? 'ok' : 'bad') + '" title="' + esc(t.error || '') + '"><i></i>' +
          (t.ok ? 'synced' : esc(t.provider === 'GCP' ? 'impersonation denied' : t.provider === 'Azure' ? 'no token for the identity' : t.error)) + '</span></td>' +
          '<td class="hide-sm">' + t.subnets + '</td></tr>';
      }).join('');
    }

    function renderFeed() {
      el.feed.innerHTML = feed.filter(shown).slice(0, 5).map(function (e, i) {
        return '<li class="' + (i === 0 && e.fresh ? 'new' : '') + '"><span class="ev">' + e.name +
          ' · ' + (ACCOUNT_NAMES[e.account] || e.account) + '/' + (e.region || 'global') + '</span><span class="ago">' + e.ago + 's ago</span></li>';
      }).join('');
    }

    function usd(n) { return '$' + Math.round(n).toLocaleString('en-US'); }

    function envTotal(e) {
      return COST_PARTS.reduce(function (n, p) { return n + e[p.key]; }, 0);
    }

    function renderCosts() {
      // The prototype has AWS prices only; with another cloud alone it has nothing to say.
      var costs = shown({ provider: 'AWS' });
      el.costTiles.hidden = !costs;
      el.costGrid.hidden = !costs;
      el.costNote.innerHTML = costs
        ? '<b>Prototype.</b> The cost panels below are an idea, not a feature: the operator computes and exports no cost data. They show synthetic AWS figures only.'
        : '<b>Prototype.</b> The cost panels are an idea shown with synthetic AWS figures only, and are hidden for ' +
          (only === 'Azure' ? 'Azure.' : 'Google Cloud.');
      if (!costs) return;
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
    // A resource nobody saw being created (no change event for it) has no creator: only the
    // network and the account defaults can name its owner.
    function decide(r) {
      var principal = r.by ? r.by.principal : '';
      if (r.tf || (principal && POLICY.skip.some(function (k) { return principal.indexOf(k.match) === 0; }))) {
        return { kind: 'skip', text: 'skipped · Terraform-owned' };
      }
      var rule = principal && POLICY.fromCreator.filter(function (k) { return principal.indexOf(k.match) === 0; })[0];
      if (rule) return { kind: 'auto', owner: rule.owner, text: 'owner from creator rule ' + rule.match + '*' };
      var inh = !isNetworkKind(r.kind) && POLICY.inheritFromVPC[r.vpc];
      if (inh) return { kind: 'auto', owner: inh.owner, env: inh.env, text: 'owner inherited from ' + (namedByPath(r.provider) ? lastSegment(r.vpc) : r.vpc) };
      return { kind: 'alert', text: 'no owner rule · alert sent' };
    }

    function agoText(sec) {
      if (sec < 60) return sec + 's ago';
      if (sec < 3600) return Math.round(sec / 60) + 'm ago';
      return Math.round(sec / 3600) + 'h ago';
    }

    function renderUnmanaged() {
      var rows = UNMANAGED.filter(shown);
      el.unmanagedCount.textContent = rows.length ? rows.length + ' found' : 'all managed';
      el.unmanaged.innerHTML = rows.map(function (r) {
        var d = decide(r);
        var decision;
        if (autoMode === 'off') decision = '<span class="dash-decision muted">manual</span>';
        else if (d.kind === 'skip') decision = '<span class="dash-decision warn">' + esc(d.text) + '</span>';
        else if (d.kind === 'alert') decision = '<span class="dash-decision bad">' + esc(d.text) + '</span>';
        else decision = '<span class="dash-decision ok">' + (autoMode === 'apply' ? 'importing as ' : 'would import as ') + esc(d.owner) + '</span>' +
          '<span class="dash-decision-why">' + esc(d.text) + '</span>';
        var action = (autoMode === 'dryrun' && d.kind === 'auto')
          ? '<button type="button" class="dash-import-btn" data-auto="' + esc(r.id) + '">Apply now</button>'
          : '<button type="button" class="dash-import-btn" data-import="' + esc(r.id) + '">Import</button>';
        var creator = r.by
          ? '<span class="dash-who">' + esc(r.by.name) + '</span><div class="dash-sub2 mono">' + esc(r.by.principal) + ' · ' + esc(r.by.via) + ' · ' + agoText(r.ago) + '</div>'
          : '<span class="dash-who">unknown</span><div class="dash-sub2">' + (r.provider === 'Azure'
            ? 'Azure\'s events do not say who created a resource' : 'created before change events were set up') + ' · seen ' + agoText(r.ago) + '</div>';
        var where = namedByPath(r.provider)
          ? r.provider + ' ' + words(r.provider)[isNetworkKind(r.kind) ? 'network' : 'subnet'] + ' · ' +
            (r.provider === 'Azure' ? 'subscription ' + ACCOUNT_NAMES[r.account] : r.account) + ' · ' + (r.region || 'global')
          : (r.name || 'no name') + ' · ' + r.account + ' · ' + r.region;
        return '<tr data-id="' + esc(r.id) + '"><td><span class="dash-kind">' + r.kind + '</span></td>' +
          '<td class="mono">' + esc(namedByPath(r.provider) ? r.name : r.id) + (r.tf ? '<span class="dash-tf" title="carries managed-by=terraform">terraform</span>' : '') +
            '<div class="dash-sub2">' + esc(where) + '</div></td>' +
          '<td>' + creator + '</td>' +
          '<td class="hide-sm mono">' + (r.cidr || (r.provider === 'GCP' ? '<span class="dash-sub2">no range: global</span>' : '')) + '</td>' +
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
          esc(e.id) + (e.who ? ' · created by ' + esc(e.who) : '') + '</span><span class="ago">' + e.ago + 's ago</span></li>';
      }).join('') + '</ul>';
    }

    function setAutoMode(mode) {
      autoMode = mode;
      try { localStorage.setItem(AUTO_KEY, mode); } catch (e) { /* private mode */ }
      renderUnmanaged();
      if (mode === 'apply') UNMANAGED.filter(shown).forEach(function (r) { var d = decide(r); if (d.kind === 'auto') autoImport(r, d); });
    }

    // autoImport is what Apply mode does: tag it, log it, let the inventory catch up.
    function autoImport(r, d) {
      UNMANAGED = UNMANAGED.filter(function (x) { return x.id !== r.id; });
      autoLog.unshift({ verdict: 'auto-imported as ' + d.owner, cls: 'ok', id: namedByPath(r.provider) ? r.name : r.id, who: r.by ? r.by.name : '', ago: 0, fresh: true });
      autoLog = autoLog.slice(0, 6);
      tagged(r);
    }

    // tagged is what the inventory does after an import: the tag call comes back as a change
    // event (CreateTags in CloudTrail, CreateTagBinding in the GCP audit logs, a write of the
    // virtual network's tags on Azure).
    function tagged(r) {
      feed.unshift({ provider: r.provider, name: TAG_EVENT[r.provider] || 'CreateTags', account: r.account, region: r.region, ago: 0, fresh: true });
      feed = feed.slice(0, 10);
      TARGETS.forEach(function (t) {
        if (t.account === r.account && (t.region === r.region || !r.region)) t.subnets += (isNetworkKind(r.kind) ? r.subnets : 1);
      });
      syncedSecondsAgo = 0;
      el.sync.textContent = 'resynced from an event just now';
    }

    // spawn is the Friday-afternoon subnet: somebody just made one by hand.
    function spawn() {
      spawnCounter++;
      var who = CREATORS[Math.floor(rand() * CREATORS.length)];
      // The demo's hand-made subnets turn up in AWS accounts.
      var aws = TARGETS.filter(function (x) { return x.provider === 'AWS'; });
      var t = aws[Math.floor(rand() * aws.length)];
      var vpcs = ['vpc-0aa11bb2', 'vpc-0cc3d4e5', 'vpc-0dd4e5f6'];
      var r = {
        kind: 'subnet', provider: 'AWS', id: 'subnet-0' + (Math.floor(rand() * 0xfffffff)).toString(16).padStart(7, '0') + spawnCounter.toString(16).padStart(8, '0').slice(-8),
        name: '', account: t.account, region: t.region,
        cidr: '10.' + (20 + Math.floor(rand() * 40)) + '.' + Math.floor(rand() * 250) + '.0/24',
        vpc: vpcs[Math.floor(rand() * vpcs.length)], tf: who.handle === 'terraform-ci', by: who, ago: 0
      };
      feed.unshift({ provider: 'AWS', name: 'CreateSubnet', account: r.account, region: r.region, ago: 0, fresh: true });
      feed = feed.slice(0, 10);
      var d = decide(r);
      if (autoMode === 'apply' && d.kind === 'auto') { autoImport(r, d); return; }
      UNMANAGED.unshift(r);
      if (d.kind === 'alert') {
        autoLog.unshift({ verdict: 'alert sent', cls: 'bad', id: r.id, who: r.by.name, ago: 0, fresh: true });
        if (shown(r)) toast('Unmanaged subnet by ' + r.by.name + ' · alert sent to #network-inventory');
      } else if (d.kind === 'skip') {
        autoLog.unshift({ verdict: 'skipped', cls: 'warn', id: r.id, who: r.by.name, ago: 0, fresh: true });
      } else if (autoMode === 'dryrun') {
        autoLog.unshift({ verdict: 'dry run: would import as ' + d.owner, cls: 'ok', id: r.id, who: r.by.name, ago: 0, fresh: true });
      }
      autoLog = autoLog.slice(0, 6);
    }

    function importYAML(r, form) {
      var named = namedByPath(r.provider);
      var keys = defaultKeys(r.provider);
      var name = (named ? r.name : r.id.slice(0, 15)) + '-import';
      var scopes = { GCP: 'gcp-shared-vpc', Azure: 'azure-landing-zone' };
      var lines = [
        ['apiVersion', 'network.hypersurgery.dev/v1'], ['kind', 'ResourceImport'],
        ['metadata', null], ['  name', name], ['  namespace', form.namespace],
        ['spec', null], ['  scopeRef', scopes[r.provider] || 'organization'], ['  account', named ? r.account : '"' + r.account + '"']
      ];
      // A GCP network is global: its import names no region.
      if (r.region) lines.push(['  region', r.region]);
      lines.push(['  resourceID', r.id], ['  tags', null],
        ['    ' + keys.managed, '"true"'], ['    ' + keys.owner, form.owner]);
      if (form.env) lines.push(['    ' + keys.env, form.env]);
      if (!isNetworkKind(r.kind) && form.tier) lines.push(['    ' + keys.tier, form.tier]);
      // GCP and Azure resources carry their name themselves; a Name tag would be a key nobody
      // created on GCP, and one more of a virtual network's 50 tags on Azure.
      if (form.name && !named) lines.push(['    Name', form.name]);
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
      var gcp = r.provider === 'GCP', azure = r.provider === 'Azure';
      el.drawer.innerHTML = '<div class="dash-drawer">' +
        '<div><h5>Import ' + esc(namedByPath(r.provider) ? r.name : r.id) + '</h5><div class="dash-form">' +
          '<label>Owner <select name="owner">' + opts(OWNERS, form.owner) + '</select></label>' +
          '<label>Environment <select name="env">' + opts(['prod', 'staging', 'dev', 'sandbox'], form.env) + '</select></label>' +
          (!isNetworkKind(r.kind) ? '<label>Tier <select name="tier">' + opts(['private', 'public', 'db'], form.tier) + '</select></label>' : '') +
          (namedByPath(r.provider) ? '' : '<label>Name <input name="name" value="' + esc(form.name) + '" placeholder="Name tag"></label>') +
          '<label>Namespace <input name="namespace" value="' + esc(form.namespace) + '"></label>' +
          '<label>Requested by <input name="requestedBy" value="' + esc(form.requestedBy) + '"></label>' +
          '<div class="dash-actions"><button type="button" class="primary" data-act="apply">Apply import</button>' +
          '<button type="button" data-act="copy">Copy YAML</button><button type="button" data-act="cancel">Cancel</button></div>' +
        '</div></div>' +
        '<div><h5>What gets applied</h5><pre class="dash-yaml" data-el="yaml">' + importYAML(r, form) + '</pre>' +
          (gcp
            ? '<p class="sub" style="margin:8px 0 0">The operator binds these tag values with <code>tagBindings.create</code>, never removes a binding, ' +
              'and refuses a key the resource already carries with another value. Keys and values must exist under the scope\'s tag parent ' +
              '(or <code>createTagValues</code> must be on); the resource shows up in the inventory on the resync the import triggers.</p></div>'
            : azure
              ? '<p class="sub" style="margin:8px 0 0">The operator adds these tags through the Tags API and never replaces a value. ' +
                (isNetworkKind(r.kind) ? 'They become the virtual network\'s own tags, which its subnets inherit. '
                  : 'An Azure subnet cannot carry tags, so they go into its entry <code>hs-subnet-' + esc(r.name) + '</code> on its virtual network, ' +
                    'one of the 50 tags the network may carry. ') +
                'The resource shows up in the inventory on the resync the import triggers.</p></div>'
              : '<p class="sub" style="margin:8px 0 0">The operator applies these tags with <code>ec2:CreateTags</code> and nothing else; ' +
              'the resource shows up in the inventory on the next CloudTrail event.</p></div>') +
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
      var row = Array.prototype.filter.call(el.unmanaged.querySelectorAll('tr[data-id]'), function (tr) {
        return tr.getAttribute('data-id') === r.id;
      })[0];
      if (row) row.classList.add('gone');
      UNMANAGED = UNMANAGED.filter(function (x) { return x.id !== r.id; });
      draft = null;
      // The import itself is a tag change, which is exactly what the event feed reports.
      tagged(r);
      setTimeout(function () { renderAll(); }, 400);
      toast((r.provider === 'GCP' ? 'Tag values bound to ' + r.name : 'Tags applied to ' + (namedByPath(r.provider) ? r.name : r.id)) + ' as ' + form.owner + ' · inventory resyncing');
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
          if (ar) { autoImport(ar, decide(ar)); renderAll(); toast('Imported ' + (namedByPath(ar.provider) ? ar.name : ar.id) + ' by policy'); }
          return;
        }
        var b = e.target.closest('button[data-import]');
        if (!b) return;
        var r = UNMANAGED.filter(function (x) { return x.id === b.dataset.import; })[0];
        if (!r) return;
        var d = decide(r);
        draft = { resource: r, form: { owner: d.owner || OWNERS[0], env: d.env || 'prod', tier: 'private', name: r.name, namespace: namedByPath(r.provider) ? 'payments' : 'platform', requestedBy: 'you' } };
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
      var claims = CLAIMS.filter(shown);
      claims.forEach(function (c) {
        c.allocations.forEach(function (a) {
          if (a.state === 'Pending') pending++;
          if (a.state === 'Failed') failed++;
        });
      });
      el.claimCount.textContent = claims.length + ' claims' + (failed ? ' · ' + failed + ' failed' : '');
      el.claims.innerHTML = claims.map(function (c) {
        var bad = c.allocations.filter(function (a) { return a.state === 'Failed'; })[0];
        var state = bad
          ? '<span class="dash-state bad"><i></i>Ready=False</span><div class="dash-sub2">' + esc(bad.error) + '</div>'
          : c.mode === 'Allocate'
            ? '<span class="dash-state ok"><i></i>allocated</span><div class="dash-sub2">Terraform creates them</div>'
            : '<span class="dash-state ok"><i></i>created</span>';
        var allocs = c.allocations.map(function (a) {
          var cls = a.state === 'Created' ? 'ok' : a.state === 'Failed' ? 'bad' : 'warn';
          return '<div class="dash-alloc"><span class="dash-decision ' + cls + '">' + esc(a.cidr) + '</span>' +
            // A GCP subnetwork and an Azure subnet are regional: one allocation per claim, named
            // after the subnet.
            '<span class="dash-sub2 mono">' + esc((a.az ? a.az.slice(-1) + ' · ' : '') + (a.subnet || a.state.toLowerCase())) + '</span></div>';
        }).join('');
        return '<tr><td class="mono">' + esc(c.ns + '/' + c.name) + '<div class="dash-sub2">' + esc(c.provider + ' · ' + c.owner + ' · /' + c.prefix +
            (c.pool ? ' from ' + c.pool : '')) + '</div></td>' +
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
      var ok = TARGETS.filter(function (x) { return x.ok; });
      var t = ok[Math.floor(rand() * ok.length)];
      var names = EVENT_NAMES_BY_PROVIDER[t.provider] || EVENT_NAMES;
      feed.unshift({
        provider: t.provider, name: names[Math.floor(rand() * names.length)],
        account: t.account, region: t.region, ago: 0, fresh: !!fresh
      });
      // Kept longer than shown, so that each cloud still has a few when the switch shows one.
      feed = feed.slice(0, 10);
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
      ENVS.forEach(function (e) {
        Object.keys(e.used).forEach(function (p) { e.used[p] = Math.max(0.05, Math.min(0.97, e.used[p] + (rand() - 0.5) * 0.012)); });
      });
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

    for (var i = 0; i < 8; i++) { pushEvent(false); }
    feed.forEach(function (e, idx) { e.ago = 6 + idx * 17; });

    try { var savedMode = localStorage.getItem(AUTO_KEY); if (['off', 'dryrun', 'apply'].indexOf(savedMode) >= 0) autoMode = savedMode; } catch (e) { /* private mode */ }
    providerSwitch(el.providers, DEMO_PROVIDERS, only);
    el.providers.addEventListener('click', function (e) {
      var b = e.target.closest('button[data-provider]');
      if (!b) return;
      only = b.getAttribute('data-provider');
      rememberProvider(only);
      draft = null;
      renderDrawer();
      providerSwitch(el.providers, DEMO_PROVIDERS, only);
      renderAll();
    });
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

  var API_GROUP_PATH = '/apis/network.hypersurgery.dev/v1/';
  // The only call outside the group: who the API server says the credentials belong to.
  var SELF_SUBJECT_REVIEW_PATH = '/apis/authentication.k8s.io/v1/selfsubjectreviews';
  var DEFAULT_PROXY = 'http://127.0.0.1:8001';
  var TOKEN_KEY = 'subnet-dashboard-token';
  // How often the kinds that are not watched are listed again.
  var REFRESH_MS = 15000;
  var LIVE_RESOURCES = ['networkscopes', 'networks', 'subnets', 'subnetclaims', 'resourceimports'];

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
  // are few, networks change rarely, and claims are made elsewhere and progress at AWS's pace, so
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

  // resourceKind names a network or subnet ID the way its cloud does: AWS IDs start with vpc-
  // or subnet-, GCP IDs are resource names (projects/<p>/global/networks/<n>,
  // projects/<p>/regions/<r>/subnetworks/<n>), Azure IDs are resource IDs, which the operator
  // keeps in lowercase (/subscriptions/<s>/resourcegroups/<g>/providers/microsoft.network/
  // virtualnetworks/<n>[/subnets/<n>]). An Azure subnet's ID has its virtual network's in it,
  // so the subnet is looked for first.
  function resourceKind(id) {
    var lower = id.toLowerCase();
    if (lower.indexOf('/virtualnetworks/') >= 0) return lower.indexOf('/subnets/') >= 0 ? 'subnet' : 'vnet';
    if (id.indexOf('/subnetworks/') >= 0) return 'subnetwork';
    if (id.indexOf('/networks/') >= 0) return 'network';
    return id.indexOf('vpc-') === 0 ? 'vpc' : 'subnet';
  }

  // summarize turns the API lists into what the panels show, for every provider or only the
  // one named. It is pure, so it is tested on its own (hack/dashboard/dashboard.test.js).
  // Networks and subnets carry their provider in spec.provider; targets, unmanaged resources,
  // claims and imports have their scope's.
  function summarize(data, only) {
    only = str(only);
    var scopes = arr(obj(data.networkscopes).items);
    var providerOf = {}, providers = [];
    scopes.forEach(function (sc) {
      var p = str(obj(obj(sc).spec).provider);
      providerOf[str(obj(obj(sc).metadata).name)] = p;
      if (p && providers.indexOf(p) < 0) providers.push(p);
    });
    providers.sort();
    function keep(p) { return !only || p === only; }
    var networks = arr(obj(data.networks).items).filter(function (n) { return keep(str(obj(obj(n).spec).provider)); });
    var subnetItems = arr(obj(data.subnets).items);
    var imports = arr(obj(data.resourceimports).items).filter(function (i) {
      return keep(providerOf[str(obj(obj(i).spec).scopeRef)] || '');
    });

    var subnets = subnetItems.map(function (s) {
      var spec = obj(s.spec), st = obj(s.status);
      // A provider that cannot count the free addresses leaves them out; such a subnet has no
      // capacity to report, which is not the same as being full.
      var known = typeof st.availableIPs === 'number' && typeof st.totalIPs === 'number';
      var total = known ? num(st.totalIPs) : 0, free = known ? num(st.availableIPs) : 0;
      return {
        id: str(spec.id) || str(obj(s.metadata).name), name: str(st.name), provider: str(spec.provider),
        account: str(spec.account), region: str(spec.region), vpc: str(spec.networkID),
        cidr: str(st.cidrBlock), owner: str(st.owner), env: str(st.env),
        total: total, free: free, used: total > 0 ? 1 - free / total : 0,
        missing: arr(st.missingTags).map(str),
        // GCP secondary ranges: their addresses are not in total and free. (An Azure subnet's
        // further prefixes are in the same field and are counted; the row does not list them.)
        secondary: arr(st.secondaryCIDRBlocks).length,
        // Azure: whether the subnet's tags are its own entry on its virtual network (Subnet)
        // or the virtual network's (Network).
        ownership: str(st.ownershipSource)
      };
    }).filter(function (s) { return keep(s.provider); });

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
    var seen = {};
    scopes.forEach(function (sc) {
      var name = str(obj(sc.metadata).name), provider = providerOf[name] || '';
      if (!keep(provider)) return;
      arr(obj(sc.status).targets).forEach(function (t) {
        t = obj(t);
        targets.push({
          scope: name, provider: provider, account: str(t.account), region: str(t.region),
          subnets: num(t.subnets), vpcs: num(t.networks), error: str(t.error), synced: !!t.lastSyncTime
        });
        arr(t.unmanagedIDs).forEach(function (id) {
          id = str(id);
          var kind = resourceKind(id);
          // A GCP network is global: every region's target of its project reports it, and it
          // is one resource, imported without a region.
          var global = kind === 'network';
          if (seen[name + '\n' + id]) return;
          seen[name + '\n' + id] = true;
          unmanaged.push({
            kind: kind, provider: provider, id: id, scope: name,
            account: str(t.account), region: global ? '' : str(t.region), importing: importing[id] || ''
          });
        });
      });
    });

    var claims = arr(obj(data.subnetclaims).items).map(function (c) {
      var spec = obj(c.spec), md = obj(c.metadata), ready = readyCondition(c);
      return {
        ns: str(md.namespace), name: str(md.name), provider: providerOf[str(spec.scopeRef)] || '',
        vpc: str(spec.networkID), prefix: num(spec.prefixLength),
        mode: str(spec.mode) || 'Create', owner: str(spec.owner),
        ready: ready ? str(ready.status) : '', message: ready ? str(ready.message) : '',
        allocations: arr(obj(c.status).allocations).map(function (a) {
          a = obj(a);
          return { az: str(a.zone), name: str(a.name), cidr: str(a.cidrBlock), subnet: str(a.subnetID), state: str(a.state) || 'Pending', error: str(a.error) };
        })
      };
    }).filter(function (c) { return keep(c.provider); });

    var recentImports = imports.map(function (i) {
      var md = obj(i.metadata), spec = obj(i.spec), st = obj(i.status);
      return {
        ns: str(md.namespace), name: str(md.name), created: str(md.creationTimestamp),
        // createdBy is the user the API server authenticated, written by the operator's webhook;
        // requestedBy is free text anybody applying the import can fill in, so it is shown as
        // "for", never as the person who did it.
        createdBy: str(obj(md.annotations)['network.hypersurgery.dev/created-by']) || 'unknown',
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
        overlapping: networks.filter(function (v) { return arr(obj(v.status).overlapsWith).length; }).length,
        // GCP says which overlapping networks are peered: those overlaps break a peering already.
        peered: networks.filter(function (v) {
          return arr(obj(obj(v.status).gcp).overlaps).some(function (o) { return obj(o).peered === true; });
        }).length,
        // Azure allows 50 tags on a virtual network, and its subnets' ownership entries are
        // among them. tagNetworks is how many networks report a tag count at all, so that the
        // tile is only shown where there is an Azure network to speak of.
        tagNetworks: networks.filter(function (v) { return typeof obj(obj(v.status).azure).tagCount === 'number'; }).length,
        tagsLow: networks.filter(function (v) { return num(obj(obj(v.status).azure).tagCount) >= AZURE_TAG_WARN; }).length,
        down: targets.filter(function (t) { return t.error; }).length
      },
      envs: envs,
      fullest: subnets.slice().sort(function (a, b) { return b.used - a.used; }).slice(0, 8),
      targets: targets,
      unmanaged: unmanaged,
      claims: claims,
      imports: recentImports,
      scopes: scopes,
      providers: providers,
      only: only
    };
  }

  // importObject is the ResourceImport the Import button creates. requestedBy is left out on
  // purpose: the operator's webhook fills it in with the user the API server authenticated,
  // which the viewer cannot type wrong or claim to be somebody else.
  function importObject(resource, form, scope) {
    var spec = obj(obj(scope).spec);
    var provider = str(spec.provider) || str(resource.provider);
    var keys = obj(spec.tagKeys);
    // The webhook fills tagKeys in with the provider's defaults; these are for a scope that
    // was created while it was not running.
    var defaults = defaultKeys(provider);
    var network = isNetworkKind(resource.kind);
    var tags = {};
    if (network) {
      // Discovery picks a network up by the scope's network selector, so those are the tags
      // that make it managed. An empty selector value matches any value; "true" is as good as any.
      var sel = obj(obj(spec.networkSelector).matchTags);
      Object.keys(sel).forEach(function (k) { tags[k] = str(sel[k]) || 'true'; });
      if (!Object.keys(sel).length) tags[defaults.managed] = 'true';
    }
    if (form.owner) tags[str(keys.owner) || defaults.owner] = form.owner;
    if (form.env) tags[str(keys.env) || defaults.env] = form.env;
    if (!network && form.tier) tags[str(keys.tier) || defaults.tier] = form.tier;
    // A GCP or Azure network or subnet carries its name itself; a Name tag there would be a tag
    // key nobody created on GCP, and one of the 50 tags of an Azure virtual network (or of the
    // 256 characters of a subnet's entry on it).
    if (form.name && !namedByPath(provider)) tags.Name = form.name;
    // generateName must be a valid name: a GCP resource name and an Azure resource ID have
    // slashes, and an Azure name may have capitals, dots and underscores.
    var base = lastSegment(resource.id).toLowerCase().replace(/[^a-z0-9-]/g, '-').replace(/^-+/, '').slice(0, 40) || 'import';
    var o = {
      apiVersion: 'network.hypersurgery.dev/v1',
      kind: 'ResourceImport',
      metadata: { generateName: base + '-', namespace: form.namespace },
      spec: {
        scopeRef: resource.scope, account: resource.account, region: resource.region,
        resourceID: resource.id, tags: tags
      }
    };
    // A global network (GCP) is imported without a region.
    if (!resource.region) delete o.spec.region;
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
        '<div class="dash-themes dash-providers" role="group" aria-label="Cloud provider" data-el="providers" hidden></div>' +
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
          '<h4>Accounts, projects, subscriptions and regions</h4>' +
          '<p class="sub">Discovery state per NetworkScope target.</p>' +
          '<table><thead><tr><th class="mono">Account / project / subscription</th><th>Region</th><th>State</th><th class="hide-sm">Subnets</th></tr></thead>' +
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
    var tiles = [
      { v: fmt(t.subnets), l: 'subnets tracked', c: '' },
      { v: fmt(t.free), l: 'free IPv4 addresses', c: '' },
      { v: fmt(t.hot), l: 'at 80% or more', c: t.hot ? 'warn' : 'ok' },
      { v: fmt(t.missing), l: 'missing required tags', c: t.missing ? 'warn' : 'ok' },
      // A GCP VPC network has no CIDR of its own: its subnetworks' ranges are compared.
      { v: fmt(t.overlapping),
        l: (view.only === 'GCP' ? 'networks with overlapping ranges' : 'networks with overlapping CIDRs') +
          (t.peered ? ', ' + fmt(t.peered) + ' peered' : ''),
        c: t.overlapping ? 'bad' : 'ok' },
      { v: fmt(t.down), l: 'unreachable targets', c: t.down ? 'bad' : 'ok' }
    ];
    // Only Azure has a tag budget, so the tile is there only when there is an Azure network.
    if (t.tagNetworks) {
      tiles.push({ v: fmt(t.tagsLow), l: 'virtual networks at ' + AZURE_TAG_WARN + '+ of ' + AZURE_TAG_LIMIT + ' tags', c: t.tagsLow ? 'warn' : 'ok' });
    }
    el.tiles.setAttribute('class', 'dash-tiles' + (tiles.length > 6 ? ' dash-tiles-7' : ''));
    fill(el.tiles, tiles.map(function (x) { return h('div', { 'class': 'dash-tile ' + x.c }, [h('b', {}, [x.v]), h('span', {}, [x.l])]); }));

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
          h('td', { 'class': 'mono' }, [x.account, h('div', { 'class': 'dash-sub2' }, ['scope ' + x.scope +
            (x.provider ? ' · ' + x.provider + ' ' + words(x.provider).account : '')])]),
          h('td', {}, [x.region]),
          h('td', { title: x.error || null }, [x.error ? state(false, 'error') : state(true, x.synced ? 'synced' : 'waiting')]),
          h('td', { 'class': 'hide-sm' }, [fmt(x.subnets)])
        ]);
      }) : [emptyRow(4, 'No NetworkScope has synced a target yet.')]);

    fill(el.subnets, data.subnets.error ? [deniedRow(5, data.subnets.error)]
      : view.fullest.length ? view.fullest.map(function (s) {
        return h('tr', {}, [
          // A GCP ID is a long resource name, and the subnetwork's name says the same.
          s.provider === 'GCP'
            ? h('td', { 'class': 'mono', title: s.id }, [s.name || s.id, h('div', { 'class': 'dash-sub2' }, ['GCP subnetwork · ' + s.account + ' · ' + s.region +
              (s.secondary ? ' · ' + s.secondary + ' secondary range' + (s.secondary > 1 ? 's' : '') : '')])])
            // So is an Azure ID; the virtual network is named too, since that is where the
            // subnet's tags are kept, or come from.
            : s.provider === 'Azure'
              ? h('td', { 'class': 'mono', title: s.id }, [s.name || lastSegment(s.id), h('div', { 'class': 'dash-sub2' }, ['Azure subnet · ' +
                lastSegment(s.vpc) + ' · ' + s.account + ' · ' + s.region + (s.ownership === 'Network' ? ' · tags of its virtual network' : '')])])
              : h('td', { 'class': 'mono' }, [s.id, h('div', { 'class': 'dash-sub2' }, [(s.name || 'no name') + ' · ' + s.account + ' · ' + s.region])]),
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
          h('td', { 'class': 'mono', title: r.id }, [namedByPath(r.provider) ? lastSegment(r.id) : r.id,
            h('div', { 'class': 'dash-sub2' }, [(namedByPath(r.provider) ? r.provider + ' ' + words(r.provider)[isNetworkKind(r.kind) ? 'network' : 'subnet'] + ' · ' : '') +
              r.account + ' · ' + (r.region || 'global')])]),
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
              // Without a zone (GCP) the allocation is the claim's one subnetwork, by name.
              h('span', { 'class': 'dash-sub2 mono' }, [(a.az ? a.az.slice(-1) : a.name) + ' · ' + (a.subnet ? (a.az ? a.subnet : 'created') : a.state.toLowerCase())])
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
    var gcp = r.provider === 'GCP', azure = r.provider === 'Azure', named = namedByPath(r.provider);
    function field(label, name, value, placeholder) {
      return h('label', {}, [label, h('input', { name: name, value: value, placeholder: placeholder || null, autocomplete: 'off', spellcheck: 'false' })]);
    }
    var dry = h('input', { name: 'dryRun', type: 'checkbox' });
    dry.checked = !!f.dryRun;
    fill(host, [h('form', { 'class': 'dash-drawer', 'data-el': 'importForm' }, [
      h('div', {}, [
        h('h5', {}, ['Import ' + (named ? lastSegment(r.id) : r.id)]),
        h('div', { 'class': 'dash-form' }, [
          field('Owner', 'owner', f.owner, 'team-…'),
          field('Environment', 'env', f.env, 'prod'),
          !isNetworkKind(r.kind) ? field('Tier', 'tier', f.tier, 'private') : null,
          named ? null : field('Name', 'name', f.name, 'Name tag, optional'),
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
          (gcp
            ? 'It binds these tag values to the resource and never removes a binding: a key the resource already carries with another ' +
              'value is refused. Keys must exist under the scope\'s tag parent, and values too unless the scope sets createTagValues.'
            : azure
              ? 'It adds these tags through the Tags API and never replaces a value' + (isNetworkKind(r.kind)
                ? ': they become the virtual network\'s own tags, which its subnets inherit.'
                : '. An Azure subnet cannot carry tags, so they go into its entry hs-subnet-' + lastSegment(r.id) +
                  ' on its virtual network, one of the 50 tags the network may carry.')
            : r.provider === 'AWS' || !r.provider ? 'It applies these tags with ec2:CreateTags and nothing else.' : 'It applies these tags and nothing else.')])
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
    var only = initialProvider();

    // resummarize rebuilds the view for the cloud the switch shows. The switch appears once the
    // cluster has scopes of more than one provider, or when a choice from before hides some.
    function resummarize() {
      view = summarize(data, only);
      el.providers.hidden = view.providers.length < 2 && !only;
      providerSwitch(el.providers, view.providers.indexOf(only) >= 0 || !only ? view.providers : view.providers.concat([only]), only);
    }
    el.providers.addEventListener('click', function (e) {
      var b = e.target.closest('button[data-provider]');
      if (!b) return;
      only = b.getAttribute('data-provider');
      rememberProvider(only);
      draft = null;
      renderLiveDrawer(el.drawer, null);
      resummarize();
      redraw();
    });
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
        resummarize();
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
        resummarize(); loadedAt = Date.now();
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
      var form = { owner: '', env: '', tier: isNetworkKind(r.kind) ? '' : 'private', name: '', namespace: ns, dryRun: false };
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
      liveSync: liveSync, renderWho: renderWho, WATCHED_RESOURCES: WATCHED_RESOURCES, providerSwitch: providerSwitch,
      initialProvider: initialProvider
    }
  };
})(window);
