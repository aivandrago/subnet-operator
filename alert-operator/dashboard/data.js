/* Demo data for the Alert Operator dashboard: a fictional cluster, made up but shaped like the
   real thing. Field names follow the AlertRule and AlertScope objects of alerts.hypersurgery.dev/v1alpha1;
   the findings and their messages are derived here by the same rules and wording the operator
   uses (docs/findings.md), so the demo cannot claim a verdict the operator would not give. */
(function (global) {
  'use strict';

  var HOUR = 3600e3, DAY = 24 * HOUR, MIN = 60e3;

  // Two AlertScopes: one per Prometheus. "edge" has a 15-day retention and no Alertmanager.
  var SCOPES = [
    { name: 'main', prometheus: 'http://prometheus-operated.monitoring.svc:9090',
      resourceRef: 'monitoring/kps-kube-prometheus-stack-prometheus', ruleSelector: 'release=kps',
      alertmanager: 'http://alertmanager-operated.monitoring.svc:9093', window: '30d', windowDays: 30,
      lastSyncMin: 2, retention: '30d' },
    { name: 'edge', prometheus: 'http://prometheus.edge.svc:9090',
      resourceRef: 'edge/edge-prometheus', ruleSelector: 'tier=edge',
      alertmanager: null, window: '15d', windowDays: 15, configuredWindow: '30d',
      lastSyncMin: 6, retention: '15d' }
  ];

  // hs/team on namespaces (the third source of the default teamFrom).
  var NS_TEAM = {
    'monitoring': 'platform', 'team-payments': 'payments', 'team-search': 'search',
    'data-pipeline': 'data', 'edge': 'edge', 'team-checkout': '', 'team-web': '',
    'ingress-nginx': '', 'cert-manager': '', 'alert-operator': ''
  };
  // hs/team on PrometheusRule objects (the second source).
  var PR_TEAM = { 'checkout-alerts': 'checkout' };

  var KPS = 'https://runbooks.prometheus-operator.dev/runbooks/';
  var RB = 'https://runbooks.example.com/';

  // [scope, namespace, PrometheusRule (or file: path), group, alert, severity, details]
  var RAW = [
    // kube-prometheus-stack in monitoring (team platform from the namespace label)
    ['main', 'monitoring', 'kps-kubernetes-apps', 'kubernetes-apps', 'KubePodCrashLooping', 'warning',
      { expr: 'max_over_time(kube_pod_container_status_waiting_reason{reason="CrashLoopBackOff",job="kube-state-metrics"}[5m]) >= 1', for: '15m',
        rb: KPS + 'kubernetes/kubepodcrashlooping', summary: 'Pod is crash looping.', act: 37, mfd: 42, ff: 0.031, last: 95 }],
    ['main', 'monitoring', 'kps-kubernetes-apps', 'kubernetes-apps', 'KubePodNotReady', 'warning',
      { expr: 'sum by (namespace, pod, cluster) (max by (namespace, pod, cluster) (kube_pod_status_phase{job="kube-state-metrics",phase=~"Pending|Unknown|Failed"}) * on (namespace, pod, cluster) group_left (owner_kind) topk by (namespace, pod, cluster) (1, max by (namespace, pod, owner_kind, cluster) (kube_pod_owner{owner_kind!="Job"}))) > 0', for: '15m',
        rb: KPS + 'kubernetes/kubepodnotready', summary: 'Pod has been in a non-ready state for more than 15 minutes.', act: 22, mfd: 31, ff: 0.012, last: 60 * 20 }],
    ['main', 'monitoring', 'kps-kubernetes-apps', 'kubernetes-apps', 'KubeDeploymentReplicasMismatch', 'warning',
      { expr: '(kube_deployment_spec_replicas{job="kube-state-metrics"} > kube_deployment_status_replicas_available{job="kube-state-metrics"}) and (changes(kube_deployment_status_replicas_updated{job="kube-state-metrics"}[10m]) == 0)', for: '15m',
        rb: KPS + 'kubernetes/kubedeploymentreplicasmismatch', summary: 'Deployment has not matched the expected number of replicas.', act: 9, mfd: 26, ff: 0.004, last: 60 * 52 }],
    ['main', 'monitoring', 'kps-kubernetes-apps', 'kubernetes-apps', 'KubeJobFailed', 'warning',
      { expr: 'kube_job_failed{job="kube-state-metrics"} > 0', for: '15m',
        rb: KPS + 'kubernetes/kubejobfailed', summary: 'Job failed to complete.', act: 64, mfd: 380, ff: 0.41, sf: 0.93, last: 14 }],
    ['main', 'monitoring', 'kps-kubernetes-resources', 'kubernetes-resources', 'KubeCPUOvercommit', 'warning',
      { expr: 'sum(namespace_cpu:kube_pod_container_resource_requests:sum{}) by (cluster) - (sum(kube_node_status_allocatable{job="kube-state-metrics",resource="cpu"}) by (cluster) - max(kube_node_status_allocatable{job="kube-state-metrics",resource="cpu"}) by (cluster)) > 0', for: '10m',
        rb: KPS + 'kubernetes/kubecpuovercommit', summary: 'Cluster has overcommitted CPU resource requests.', act: 4, mfd: 190, ff: 0.018, last: 60 * 24 * 6 }],
    ['main', 'monitoring', 'kps-kubernetes-resources', 'kubernetes-resources', 'KubeMemoryOvercommit', 'warning',
      { expr: 'sum(namespace_memory:kube_pod_container_resource_requests:sum{}) by (cluster) - (sum(kube_node_status_allocatable{resource="memory", job="kube-state-metrics"}) by (cluster) - max(kube_node_status_allocatable{resource="memory", job="kube-state-metrics"}) by (cluster)) > 0', for: '10m',
        rb: KPS + 'kubernetes/kubememoryovercommit', summary: 'Cluster has overcommitted memory resource requests.', act: 0, known: 64 }],
    ['main', 'monitoring', 'kps-kubernetes-resources', 'kubernetes-resources', 'CPUThrottlingHigh', 'info',
      { expr: 'sum(increase(container_cpu_cfs_throttled_periods_total{container!="", job="kubelet", metrics_path="/metrics/cadvisor"}[5m])) without (id, metrics_path, name, image, endpoint, job, node) / sum(increase(container_cpu_cfs_periods_total{job="kubelet", metrics_path="/metrics/cadvisor"}[5m])) without (id, metrics_path, name, image, endpoint, job, node) > (25 / 100)', for: '15m',
        rb: KPS + 'kubernetes/cputhrottlinghigh', summary: 'Processes experience elevated CPU throttling.', act: 418, mfd: 7, ff: 0.27, sf: 0.88, last: 3 }],
    ['main', 'monitoring', 'kps-kubernetes-storage', 'kubernetes-storage', 'KubePersistentVolumeFillingUp', 'critical',
      { expr: 'kubelet_volume_stats_available_bytes{job="kubelet",metrics_path="/metrics"} / kubelet_volume_stats_capacity_bytes{job="kubelet",metrics_path="/metrics"} < 0.03 and kubelet_volume_stats_used_bytes{job="kubelet",metrics_path="/metrics"} > 0', for: '1m',
        rb: KPS + 'kubernetes/kubepersistentvolumefillingup', summary: 'PersistentVolume is filling up.', act: 2, mfd: 55, ff: 0.003, last: 60 * 24 * 11 }],
    ['main', 'monitoring', 'kps-node-exporter', 'node-exporter', 'NodeFilesystemSpaceFillingUp', 'warning',
      { expr: '(node_filesystem_avail_bytes{job="node-exporter",fstype!="",mountpoint!=""} / node_filesystem_size_bytes{job="node-exporter",fstype!="",mountpoint!=""} * 100 < 15 and predict_linear(node_filesystem_avail_bytes{job="node-exporter",fstype!="",mountpoint!=""}[6h], 24*60*60) < 0 and node_filesystem_readonly{job="node-exporter",fstype!="",mountpoint!=""} == 0)', for: '1h',
        rb: KPS + 'node/nodefilesystemspacefillingup', summary: 'Filesystem is predicted to run out of space within the next 24 hours.', act: 6, mfd: 140, ff: 0.02, last: 60 * 24 * 3 }],
    ['main', 'monitoring', 'kps-node-exporter', 'node-exporter', 'NodeMemoryHighUtilization', 'warning',
      { expr: '100 - (node_memory_MemAvailable_bytes{job="node-exporter"} / node_memory_MemTotal_bytes{job="node-exporter"} * 100) > 90', for: '15m',
        rb: KPS + 'node/nodememoryhighutilization', summary: 'Host is running out of memory.', act: 14, mfd: 36, ff: 0.012, last: 60 * 9 }],
    ['main', 'monitoring', 'kps-node-exporter', 'node-exporter', 'NodeNetworkReceiveErrs', 'warning',
      { expr: 'rate(node_network_receive_errs_total{job="node-exporter"}[2m]) / rate(node_network_receive_packets_total{job="node-exporter"}[2m]) > 0.01', for: '1h',
        rb: KPS + 'node/nodenetworkreceiveerrs', summary: 'Network interface is reporting many receive errors.', act: 0, known: 64 }],
    ['main', 'monitoring', 'kps-node-exporter', 'node-exporter', 'NodeClockNotSynchronising', 'warning',
      { expr: 'min_over_time(node_timex_sync_status{job="node-exporter"}[5m]) == 0 and node_timex_maxerror_seconds{job="node-exporter"} >= 16', for: '10m',
        rb: KPS + 'node/nodeclocknotsynchronising', summary: 'Clock not synchronising.', act: 0, known: 64 }],
    ['main', 'monitoring', 'kps-general-rules', 'general.rules', 'TargetDown', 'warning',
      { expr: '100 * (count(up == 0) BY (cluster, job, namespace, service) / count(up) BY (cluster, job, namespace, service)) > 10', for: '10m',
        rb: KPS + 'general/targetdown', summary: 'One or more targets are unreachable.', act: 131, mfd: 18, ff: 0.09, sf: 0.34, last: 41 }],
    ['main', 'monitoring', 'kps-general-rules', 'general.rules', 'Watchdog', 'none',
      { expr: 'vector(1)', rb: KPS + 'general/watchdog', summary: 'An alert that should always be firing to certify that Alertmanager is working properly.',
        act: 1, mfd: 60 * 24 * 30, ff: 1, last: 1 }],
    ['main', 'monitoring', 'kps-prometheus', 'prometheus', 'PrometheusRuleFailures', 'critical',
      { expr: 'increase(prometheus_rule_evaluation_failures_total{job="kps-kube-prometheus-stack-prometheus",namespace="monitoring"}[5m]) > 0', for: '15m',
        rb: KPS + 'prometheus/prometheusrulefailures', summary: 'Prometheus is failing rule evaluations.', act: 3, mfd: 25, ff: 0.002, last: 60 * 30 }],
    ['main', 'monitoring', 'kps-prometheus', 'prometheus', 'PrometheusNotConnectedToAlertmanagers', 'warning',
      { expr: 'max_over_time(prometheus_notifications_alertmanagers_discovered{job="kps-kube-prometheus-stack-prometheus",namespace="monitoring"}[5m]) < 1', for: '10m',
        rb: KPS + 'prometheus/prometheusnotconnectedtoalertmanagers', summary: 'Prometheus is not connected to any Alertmanagers.', act: 0, known: 64 }],
    ['main', 'monitoring', 'kps-alertmanager', 'alertmanager.rules', 'AlertmanagerFailedReload', 'critical',
      { expr: 'max_over_time(alertmanager_config_last_reload_successful{job="kps-kube-prometheus-stack-alertmanager",namespace="monitoring"}[5m]) == 0', for: '10m',
        rb: KPS + 'alertmanager/alertmanagerfailedreload', summary: 'Reloading an Alertmanager configuration has failed.', act: 1, mfd: 12, ff: 0.0003, last: 60 * 24 * 19 }],
    ['main', 'monitoring', 'kps-kubelet', 'kubelet.rules', 'KubeletTooManyPods', 'info',
      { expr: 'count by (cluster, node) ((kube_pod_status_phase{job="kube-state-metrics",phase="Running"} == 1) * on (instance,pod,namespace,cluster) group_left(node) topk by (instance,pod,namespace,cluster) (1, kube_pod_info{job="kube-state-metrics"})) / max by (cluster, node) (kube_node_status_capacity{job="kube-state-metrics",resource="pods"} != 1) > 0.95', for: '15m',
        rb: KPS + 'kubernetes/kubelettoomanypods', summary: 'Kubelet is running at capacity.', act: 0, known: 64 }],
    ['main', 'monitoring', 'platform-extras', 'platform.etcd', 'EtcdBackupMissing', 'critical',
      { expr: 'time() - etcd_backup_last_success_timestamp_seconds > 26 * 3600', for: '30m',
        rb: RB + 'platform/etcd-backup', summary: 'No successful etcd backup in 26 hours.', dead: ['etcd_backup_last_success_timestamp_seconds'], act: 0, known: 48 }],
    ['main', 'monitoring', 'platform-extras', 'platform.dns', 'CoreDNSLatencyHigh', 'warning',
      { expr: 'histogram_quantile(0.99, sum(rate(coredns_dns_request_duration_seconds_bucket{job="kube-dns"}[5m])) by (server, zone, le)) > 0.25', for: '10m',
        summary: 'CoreDNS p99 latency above 250 ms.', act: 27, mfd: 14, ff: 0.011, last: 60 * 7 }],

    // team-payments: team from the namespace label
    ['main', 'team-payments', 'payments-api', 'payments.latency', 'PaymentsHighLatency', 'warning',
      { expr: 'histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{job="payments"}[5m]))) > 0.5', for: '5m',
        summary: 'p99 latency above 500 ms', act: 212, mfd: 4, ff: 0.06, sf: 0.02, last: 40 }],
    ['main', 'team-payments', 'payments-api', 'payments.errors', 'PaymentsErrorRateHigh', 'critical',
      { expr: 'sum(rate(http_requests_total{job="payments",code=~"5.."}[5m])) / sum(rate(http_requests_total{job="payments"}[5m])) > 0.02', for: '5m',
        rb: RB + 'payments/error-rate', summary: 'More than 2% of payment requests fail.', act: 8, mfd: 21, ff: 0.004, last: 60 * 24 * 2 }],
    ['main', 'team-payments', 'payments-api', 'payments.providers', 'PaymentsProviderTimeouts', 'warning',
      { expr: 'sum by (provider) (rate(payments_provider_requests_total{result="timeout"}[2m])) > 0.5', for: '1m', kff: '',
        rb: RB + 'payments/provider-timeouts', summary: 'A payment provider is timing out.', act: 486, mfd: 3, ff: 0.034, last: 12 }],
    ['main', 'team-payments', 'payments-api', 'payments.queue', 'PaymentsWebhookBacklog', 'warning',
      { expr: 'max(payments_webhook_queue_depth) > 1000', for: '10m',
        rb: RB + 'payments/webhook-backlog', summary: 'Outgoing webhooks are backing up.', act: 5, mfd: 48, ff: 0.006, last: 60 * 24 * 4 }],
    ['main', 'team-payments', 'payments-ledger', 'payments.ledger', 'PaymentsLedgerLag', 'critical',
      { expr: 'time() - max(ledger_last_reconciled_timestamp_seconds) > 900', for: '5m',
        rb: RB + 'payments/ledger-lag', summary: 'The ledger has not been reconciled for 15 minutes.', act: 0, known: 41 }],
    ['main', 'team-payments', 'payments-ledger', 'payments.ledger', 'PaymentsLedgerMismatch', 'critical',
      { expr: 'sum(ledger_balance_cents) by (account) != on (account) sum(bank_balance_cents) by (account)', for: '30m',
        rb: RB + 'payments/ledger-mismatch', summary: 'Ledger and bank balances disagree.',
        loadErr: 'found duplicate series for the match group {account="ops-eu"} on the right hand-side of the operation: [{__name__="bank_balance_cents", account="ops-eu", bank="b1"}, {__name__="bank_balance_cents", account="ops-eu", bank="b2"}];many-to-many matching not allowed: matching labels must be unique on one side',
        act: 0, known: 9 }],

    // team-checkout: the PrometheusRule lacks release=kps, so Prometheus never loads it
    ['main', 'team-checkout', 'checkout-alerts', 'checkout', 'CheckoutCartErrors', 'critical',
      { team: 'checkout', expr: 'sum(rate(cart_operations_total{result="error"}[5m])) / sum(rate(cart_operations_total[5m])) > 0.05', for: '5m',
        rb: RB + 'checkout/cart-errors', summary: 'More than 5% of cart operations fail.', loaded: false, nlr: 'selector', known: 23 }],
    ['main', 'team-checkout', 'checkout-alerts', 'checkout', 'CheckoutSlow', 'warning',
      { expr: 'histogram_quantile(0.95, sum by (le) (rate(checkout_duration_seconds_bucket[5m]))) > 2', for: '10m',
        rb: RB + 'checkout/slow', summary: 'Checkout p95 above 2 s.', loaded: false, nlr: 'selector', known: 23 }],
    ['main', 'team-checkout', 'checkout-alerts', 'checkout', 'CheckoutQueueStuck', 'warning',
      { expr: 'rate(checkout_orders_processed_total[15m]) == 0 and checkout_orders_pending > 0', for: '15m',
        summary: 'Orders are waiting but none are processed.', loaded: false, nlr: 'selector', known: 23 }],

    // team-search
    ['main', 'team-search', 'search-cluster', 'search.index', 'SearchIndexLag', 'warning',
      { expr: 'max(es_index_lag_seconds{cluster="search-prod"}) > 300', for: '10m',
        rb: RB + 'search/index-lag', summary: 'Index updates are more than 5 minutes behind.', dead: ['es_index_lag_seconds{cluster="search-prod"}'], act: 0, known: 120 }],
    ['main', 'team-search', 'search-cluster', 'search.query', 'SearchQueryLatency', 'warning',
      { expr: 'histogram_quantile(0.99, sum by (le) (rate(search_query_duration_seconds_bucket[5m]))) > 1', for: '5m',
        rb: RB + 'search/query-latency', summary: 'Search p99 above 1 s.', act: 33, mfd: 11, ff: 0.009, last: 60 * 5 }],
    ['main', 'team-search', 'search-cluster', 'search.cluster', 'SearchShardUnassigned', 'warning',
      { expr: 'elasticsearch_cluster_health_unassigned_shards{cluster="search-prod"} > 0', for: '15m',
        rb: RB + 'search/unassigned-shards', summary: 'Shards are unassigned.', act: 7, mfd: 60 * 46, ff: 0.45, sf: 0.97, last: 25 }],
    ['main', 'team-search', 'search-cluster', 'search.cluster', 'SearchNodeDown', 'critical',
      { expr: 'elasticsearch_cluster_health_number_of_nodes{cluster="search-prod"} < 3', for: '2m',
        rb: RB + 'search/node-down', summary: 'The search cluster lost a node.', act: 2, mfd: 9, ff: 0.0005, last: 60 * 24 * 13 }],

    // team-web: no hs/team anywhere, and the object misses the release label
    ['main', 'team-web', 'web-frontend', 'web', 'WebDown', 'critical',
      { expr: 'up{job="web-frontend"} == 0', for: '2m',
        rb: RB + 'web/down', summary: 'The web frontend is down.', loaded: false, nlr: 'selector', known: 35 }],
    ['main', 'team-web', 'web-frontend', 'web', 'Web5xxRate', 'warning',
      { expr: 'sum(rate(nginx_http_requests_total{service="web",status=~"5.."}[5m])) / sum(rate(nginx_http_requests_total{service="web"}[5m])) > 0.01', for: '5m',
        summary: 'More than 1% of web requests return 5xx.', loaded: false, nlr: 'selector', known: 35 }],
    ['main', 'team-web', 'web-certs', 'web.certs', 'WebCertExpiring', 'warning',
      { expr: 'probe_ssl_earliest_cert_expiry{job="blackbox",instance=~"https://www.*"} - time() < 14 * 86400', for: '1h',
        summary: 'A web certificate expires within 14 days.', act: 1, mfd: 60 * 30, ff: 0.04, last: 60 * 24 * 21 }],

    // data-pipeline
    ['main', 'data-pipeline', 'kafka', 'kafka.consumers', 'KafkaConsumerLag', 'warning',
      { expr: 'sum by (consumergroup, topic) (kafka_consumergroup_lag) > 10000', for: '2m',
        rb: RB + 'data/consumer-lag', summary: 'A consumer group is falling behind.', act: 377, mfd: 6, ff: 0.05, sf: 0.12, last: 6 }],
    ['main', 'data-pipeline', 'kafka', 'kafka.brokers', 'KafkaUnderReplicatedPartitions', 'critical',
      { expr: 'sum(kafka_server_replicamanager_underreplicatedpartitions) > 0', for: '10m',
        rb: RB + 'data/under-replicated', summary: 'Partitions are under-replicated.', act: 4, mfd: 38, ff: 0.004, last: 60 * 24 * 8 }],
    ['main', 'data-pipeline', 'airflow', 'airflow', 'AirflowDagFailed', 'high',
      { expr: 'increase(airflow_dag_run_failed_total[1h]) > 0',
        rb: RB + 'data/airflow-dag-failed', summary: 'An Airflow DAG run failed.', act: 29, mfd: 60, ff: 0.04, last: 60 * 14 }],
    ['main', 'data-pipeline', 'airflow', 'airflow', 'AirflowSchedulerHeartbeatMissing', 'critical',
      { expr: 'time() - airflow_scheduler_heartbeat_timestamp > 120', for: '5m',
        rb: RB + 'data/airflow-scheduler', summary: 'The Airflow scheduler stopped reporting.', act: 0, known: 77 }],
    ['main', 'data-pipeline', 'spark', 'spark', 'SparkJobSlow', 'info',
      { expr: 'spark_job_duration_seconds{job_name="nightly-aggregate"} > 3 * 3600', for: '0m',
        summary: 'The nightly aggregate took over 3 hours.', act: 0, known: 52 }],
    ['main', 'data-pipeline', 'spark', 'spark', 'SparkExecutorOOM', 'warning',
      { expr: 'increase(spark_executor_oom_killed_total[30m]) > 0', for: '0m',
        rb: RB + 'data/spark-oom', summary: 'Spark executors are being OOM-killed.', act: 11, mfd: 30, ff: 0.008, last: 60 * 24 }],

    // ingress-nginx and cert-manager: rules from their Helm charts, nobody's
    ['main', 'ingress-nginx', 'ingress-nginx', 'ingress-nginx', 'NginxHighHttp5xxErrorRate', 'warning',
      { expr: '100 * (sum by (ingress) (rate(nginx_ingress_controller_requests{status=~"5.."}[2m])) / sum by (ingress) (rate(nginx_ingress_controller_requests[2m]))) > 5', for: '1m',
        summary: 'Too many HTTP 5xx on an ingress.', act: 93, mfd: 5, ff: 0.012, last: 33 }],
    ['main', 'ingress-nginx', 'ingress-nginx', 'ingress-nginx', 'NginxConfigFailed', 'critical',
      { expr: 'count(nginx_ingress_controller_config_last_reload_successful == 0) > 0',
        summary: 'Bad ingress config: the controller could not reload.', act: 0, known: 64 }],
    ['main', 'ingress-nginx', 'ingress-nginx', 'ingress-nginx', 'NginxLatencyHigh', 'warning',
      { expr: 'histogram_quantile(0.99, sum(rate(nginx_ingress_controller_request_duration_seconds_bucket[2m])) by (host, node, le)) > 3', for: '2m',
        summary: 'p99 latency above 3 s on a host.', act: 19, mfd: 8, ff: 0.004, last: 60 * 3 }],
    ['main', 'cert-manager', 'cert-manager', 'cert-manager', 'CertManagerCertExpirySoon', 'warning',
      { expr: 'avg by (exported_namespace, namespace, name) (certmanager_certificate_expiration_timestamp_seconds - time()) < (21 * 24 * 3600)', for: '1h',
        summary: 'A certificate expires in less than 21 days.', act: 2, mfd: 60 * 20, ff: 0.05, last: 60 * 24 * 9 }],
    ['main', 'cert-manager', 'cert-manager', 'cert-manager', 'CertManagerAbsent', 'critical',
      { expr: 'absent(up{job="cert-manager"})', for: '10m',
        summary: 'cert-manager has disappeared from Prometheus service discovery.', act: 0, known: 64 }],

    // edge: its own Prometheus, 15-day retention, no Alertmanager configured
    ['edge', 'edge', 'edge-proxy', 'edge.proxy', 'EdgeProxyDown', 'critical',
      { expr: 'up{job="edge-proxy"} == 0', for: '1m',
        rb: RB + 'edge/proxy-down', summary: 'An edge proxy is down.', act: 6, mfd: 4, ff: 0.002, last: 60 * 24 * 2 }],
    ['edge', 'edge', 'edge-proxy', 'edge.proxy', 'EdgeTLSHandshakeErrors', 'warning',
      { expr: 'sum by (pop) (rate(edge_tls_handshake_errors_total[5m])) > 5', for: '5m',
        rb: RB + 'edge/tls-errors', summary: 'TLS handshakes fail at a POP.', act: 141, mfd: 9, ff: 0.07, last: 50 }],
    ['edge', 'edge', 'edge-proxy', 'edge.proxy', 'EdgeRateLimited', 'warning',
      { expr: 'sum by (pop) (rate(edge_requests_total{result="rate_limited"}[1m])) > 100', for: '0m',
        rb: RB + 'edge/rate-limited', summary: 'A POP is rate-limiting clients.', act: 233, mfd: 2, ff: 0.012, last: 8 }],
    ['edge', 'edge', 'edge-cache', 'edge.cache', 'EdgeCacheHitRatioLow', '',
      { expr: 'sum(rate(edge_cache_hits_total[10m])) / sum(rate(edge_cache_requests_total[10m])) < 0.7', for: '30m',
        rb: RB + 'edge/cache-hit-ratio', summary: 'Cache hit ratio below 70%.', act: 12, mfd: 75, ff: 0.04, last: 60 * 30 }],
    ['edge', 'edge', 'edge-cache', 'edge.cache', 'EdgeOriginLatency', 'warning',
      { expr: 'histogram_quantile(0.95, sum by (le, origin) (rate(edge_origin_duration_seconds_bucket[5m]))) > 1.5', for: '10m',
        rb: RB + 'edge/origin-latency', summary: 'An origin answers the edge slowly.', act: 16, mfd: 23, ff: 0.017, last: 60 * 11 }],
    ['edge', 'edge', 'edge-cache', 'edge.cache', 'EdgePurgeBacklog', 'warning',
      { expr: 'max(edge_purge_queue_length) > 500', for: '15m',
        rb: RB + 'edge/purge-backlog', summary: 'Cache purges are queueing.', act: 0, known: 33 }],
    ['edge', 'alert-operator', 'file:/etc/prometheus/rules/edge-base.yml', 'edge-base', 'EdgeNodeDown', 'critical',
      { expr: 'up{job="node", role="edge"} == 0', for: '5m',
        summary: 'An edge node is unreachable.', act: 3, mfd: 17, ff: 0.003, last: 60 * 24 * 5 }],
    ['edge', 'alert-operator', 'file:/etc/prometheus/rules/edge-base.yml', 'edge-base', 'EdgeDiskFull', 'warning',
      { expr: 'node_filesystem_avail_bytes{role="edge",mountpoint="/var/cache"} / node_filesystem_size_bytes{role="edge",mountpoint="/var/cache"} < 0.05', for: '10m',
        summary: 'Edge cache disk almost full.', act: 0, known: 90 }]
  ];

  // Silences Alertmanager still holds for the main scope: active ones and those expired within
  // its five-day retention.
  var SILENCES = [
    { id: '2c1f9e4a-6d0b-4a51-9b7e-0b3f1d7a8e21', alert: 'CPUThrottlingHigh', matchers: 'alertname="CPUThrottlingHigh", namespace=~"team-.*"',
      createdBy: 'platform-oncall', comment: 'Known: requests too low on the batch nodes. Revisit after the node pool upgrade.', startsDays: -26, endsDays: 4 },
    { id: '8a04d7c2-1e5f-4f0e-a2b6-5d9c0e3f7b10', alert: 'KubeJobFailed', matchers: 'alertname="KubeJobFailed", job_name=~"report-.*"',
      createdBy: 'data-oncall', comment: 'Report jobs retry by themselves.', startsDays: -12, endsDays: 2 },
    { id: 'f3b8c6d1-0a2e-47c9-8d15-9e6a4b2c7f03', alert: 'KubeJobFailed', matchers: 'alertname="KubeJobFailed", job_name=~"report-.*"',
      createdBy: 'data-oncall', comment: 'Report jobs retry by themselves.', startsDays: -19, endsDays: -12 },
    { id: '5e7a1b9c-3d4f-4c2a-8b6e-1f0d9a8c7b54', alert: 'SearchShardUnassigned', matchers: 'alertname="SearchShardUnassigned"',
      createdBy: 'search-team', comment: 'Single-node test indices, always yellow.', startsDays: -3, endsDays: 25 },
    { id: '0d9c8b7a-6e5f-4d3c-9b2a-1e0f9d8c7b66', alert: 'TargetDown', matchers: 'alertname="TargetDown", job="spark-history"',
      createdBy: 'platform-oncall', comment: 'Spark history server decommissioned, scrape config still there.', startsDays: -4, endsDays: 3 },
    { id: '7b6a5c4d-3e2f-4a1b-8c9d-0e1f2a3b4c99', alert: 'PaymentsHighLatency', matchers: 'alertname="PaymentsHighLatency"',
      createdBy: 'payments-oncall', comment: 'Provider maintenance window.', startsDays: -2.1, endsDays: -1.9 },
    { id: '1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c11', alert: 'KafkaConsumerLag', matchers: 'alertname="KafkaConsumerLag", consumergroup="backfill"',
      createdBy: 'data-oncall', comment: 'Backfill running, lag expected.', startsDays: -4.5, endsDays: -3.5 }
  ];

  // What each finding means and how it is fixed, from docs/findings.md.
  var FINDINGS = [
    { type: 'NotLoaded', severity: 'critical', from: 'inventory',
      means: 'The rule is in a PrometheusRule in scope, but Prometheus does not report it in /api/v1/rules. Whoever wrote it believes they are covered; they are not.',
      fix: 'Add the label the Prometheus ruleSelector asks for (kube-prometheus-stack: release: <name>), or widen the selectors.' },
    { type: 'LoadError', severity: 'critical', from: 'inventory, history',
      means: 'Prometheus loaded the rule but its last evaluation failed, or the expression does not parse.',
      fix: 'The message carries Prometheus\' error. promtool check rules reproduces parse errors locally.' },
    { type: 'DeadQuery', severity: 'warning', from: 'history',
      means: 'The expression can never return anything: selectors in it match no series. The metric was renamed, a label value is wrong or the exporter is gone.',
      fix: 'Point the selector at the series that exist now, or delete the rule. A series that appears less than once an hour looks dead between appearances.' },
    { type: 'NeverFired', severity: 'info', from: 'history',
      means: 'No activation in the window, for a rule the operator has known for at least 30 days. A quiet rule can be a good rule; together with DeadQuery it is not.',
      fix: 'Nothing, if the rule guards something rare. Check that it still can fire.' },
    { type: 'Noisy', severity: 'warning', from: 'history',
      means: 'The alert became active more than 50 times in the window. Rules like this teach people to ignore pages.',
      fix: 'Raise for:, add keep_firing_for:, fix the threshold, or route it somewhere that is not a pager.' },
    { type: 'Flapping', severity: 'warning', from: 'history',
      means: 'More than 10 activations per day with a typical firing duration under 10 minutes.',
      fix: 'for: and keep_firing_for: smooth a signal that hovers around its threshold.' },
    { type: 'AlwaysSilenced', severity: 'warning', from: 'history',
      means: 'More than 80% of the rule\'s firing time was covered by an Alertmanager silence matching its alerts. A rule that is always silenced is a rule nobody wants.',
      fix: 'Delete or fix the rule rather than renewing the silence.' },
    { type: 'Unowned', severity: 'warning', from: 'inventory',
      means: 'No source names a team: not the rule label team, not the label hs/team on the PrometheusRule, not the label hs/team on its namespace.',
      fix: 'Label the namespace (kubectl label ns <namespace> hs/team=<team>): one label covers every rule in it.' },
    { type: 'NoRunbook', severity: 'info', from: 'inventory',
      means: 'The runbook_url annotation is missing or empty.',
      fix: 'Add runbook_url, so whoever is paged knows what to do.' },
    { type: 'NoSeverity', severity: 'info', from: 'inventory',
      means: 'The severity label is missing or not one of critical, warning, info.',
      fix: 'Set severity to one of the allowed values.' }
  ];
  var FINDING_INDEX = {};
  FINDINGS.forEach(function (f, i) { FINDING_INDEX[f.type] = i; });
  var SEV_RANK = { critical: 0, warning: 1, info: 2 };

  function fmtDuration(min) {
    if (min >= 60 * 24 && min % (60 * 24) === 0) return (min / 1440) + 'd';
    if (min >= 60 && min % 60 === 0) return (min / 60) + 'h';
    if (min >= 120) return Math.round(min / 60) + 'h';
    return Math.max(1, Math.round(min)) + 'm';
  }
  function hash(s) {
    var h = 2166136261;
    for (var i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 16777619); }
    return (h >>> 0).toString(16);
  }
  function pad8(s) { return ('00000000' + s).slice(-8); }

  // build turns the table above into AlertRule objects, as of now.
  function build(now) {
    now = now || Date.now();
    var scopes = {};
    SCOPES.forEach(function (s) { scopes[s.name] = s; });
    var rules = RAW.map(function (r) {
      var sc = scopes[r[0]], ns = r[1], obj = r[2], group = r[3], alert = r[4], sev = r[5], d = r[6];
      var isFile = obj.indexOf('file:') === 0;
      var labels = {};
      if (sev && sev !== '') labels.severity = sev;
      if (d.team) labels.team = d.team;
      var annotations = { summary: d.summary };
      if (d.rb) annotations.runbook_url = d.rb;
      var source = isFile ? { kind: 'File', file: obj.slice(5), group: group, alert: alert }
        : { kind: 'PrometheusRule', namespace: ns, name: obj, group: group, alert: alert };
      var identity = pad8(hash(sc.name + '/' + ns + '/' + obj + '/' + group + '/' + alert));
      var name = alert.toLowerCase() + '-' + identity.slice(0, 6);
      var loaded = d.loaded !== false;
      var teamFrom = '', team = '';
      if (labels.team) { team = labels.team; teamFrom = 'rule label team'; }
      else if (!isFile && PR_TEAM[obj]) { team = PR_TEAM[obj]; teamFrom = 'PrometheusRule label hs/team'; }
      else if (!isFile && NS_TEAM[ns]) { team = NS_TEAM[ns]; teamFrom = 'namespace label hs/team'; }
      var knownDays = d.known || 64;
      var findings = [];
      var add = function (t, msg) { findings.push({ type: t, severity: FINDINGS[FINDING_INDEX[t]].severity, message: msg }); };
      var notLoadedReason = '';
      if (!loaded) {
        notLoadedReason = 'the labels of PrometheusRule ' + ns + '/' + obj + ' do not match ruleSelector of Prometheus ' +
          sc.resourceRef + ' (' + sc.ruleSelector + ')';
        add('NotLoaded', notLoadedReason);
      }
      if (d.loadErr) add('LoadError', 'last evaluation failed: ' + d.loadErr);
      var quality = null;
      if (loaded) {
        var act = d.act || 0, mfd = d.mfd || 0, perDay = act / sc.windowDays;
        quality = { activations: act, activationsPerDay: Math.round(perDay * 10) / 10,
          medianFiringDuration: act ? fmtDuration(mfd) : '', firingFraction: d.ff || 0,
          silencedFraction: d.sf || 0, lastFired: d.last ? new Date(now - d.last * MIN).toISOString() : null };
        if (d.dead) add('DeadQuery', 'selects no series in the last 1h, so the rule cannot fire: ' + d.dead.join(', '));
        if (act === 0 && knownDays >= 30) add('NeverFired', 'no activation in ' + sc.window);
        if (act > 50) add('Noisy', act + ' activations in ' + sc.window + ' (threshold 50); median firing time ' + fmtDuration(mfd));
        if (act >= 3 && perDay > 10 && mfd < 10)
          add('Flapping', perDay.toFixed(1) + ' activations per day (threshold 10) with a median firing time of ' + fmtDuration(mfd) + ' (threshold 10m)');
        if (sc.alertmanager && quality.firingFraction > 0 && quality.silencedFraction > 0.8)
          add('AlwaysSilenced', 'silenced for ' + Math.round(quality.silencedFraction * 100) + '% of its firing time in ' + sc.window + ' (threshold 80%)');
      }
      if (!team) add('Unowned', 'no team from rule label team, PrometheusRule label hs/team, namespace label hs/team');
      if (!annotations.runbook_url) add('NoRunbook', 'annotation runbook_url is missing');
      if (!labels.severity) add('NoSeverity', 'label severity is missing; allowed: critical, warning, info');
      else if (!(labels.severity in SEV_RANK)) add('NoSeverity', 'severity "' + labels.severity + '" is not one of critical, warning, info');
      findings.sort(function (a, b) {
        return SEV_RANK[a.severity] - SEV_RANK[b.severity] || FINDING_INDEX[a.type] - FINDING_INDEX[b.type];
      });
      var worst = findings.filter(function (f) { return f.severity !== 'info'; })[0];
      var healthy = worst ? { type: 'Healthy', status: 'False', reason: worst.type, message: worst.message }
        : { type: 'Healthy', status: 'True', reason: 'NoProblems', message: 'no critical or warning finding' };
      return {
        metadata: { name: name, namespace: ns, labels: { 'alerts.hypersurgery.dev/scope': sc.name,
          'alerts.hypersurgery.dev/managed-by': 'alert-operator', 'hs/team': team || '<none>' } },
        spec: { scopeRef: sc.name, source: source, expr: d.expr, for: d.for || '', keepFiringFor: d.kff || '',
          labels: labels, annotations: annotations },
        status: { identity: identity, loaded: loaded, notLoadedReason: notLoadedReason, team: team || '<none>',
          window: sc.window, firstSeen: new Date(now - knownDays * DAY).toISOString(),
          lastEvaluated: loaded ? new Date(now - (sc.lastSyncMin + 1) * MIN).toISOString() : null,
          quality: quality, findings: findings,
          findingsSummary: findings.map(function (f) { return f.type; }).join(','),
          conditions: [healthy] },
        teamFrom: teamFrom,
        silences: sc.alertmanager ? SILENCES.filter(function (s) { return s.alert === alert; }).map(function (s) {
          return { id: s.id, matchers: s.matchers, createdBy: s.createdBy, comment: s.comment,
            startsAt: new Date(now + s.startsDays * DAY).toISOString(), endsAt: new Date(now + s.endsDays * DAY).toISOString(),
            state: s.endsDays > 0 ? 'active' : 'expired' };
        }) : null,
        daily: dailyActivations(identity, loaded && d.act ? d.act : 0, sc.windowDays, d.last)
      };
    });

    var scopeObjs = SCOPES.map(function (s) {
      var mine = rules.filter(function (r) { return r.spec.scopeRef === s.name; });
      var counts = { total: mine.length, loaded: 0, notLoaded: 0 }, byType = {}, teams = {}, total = 0;
      mine.forEach(function (r) {
        if (r.status.loaded) counts.loaded++; else counts.notLoaded++;
        var t = teams[r.status.team] || (teams[r.status.team] = { team: r.status.team, rules: 0, findings: 0 });
        t.rules++; t.findings += r.status.findings.length; total += r.status.findings.length;
        r.status.findings.forEach(function (f) { byType[f.type] = (byType[f.type] || 0) + 1; });
      });
      return {
        metadata: { name: s.name },
        spec: { prometheus: { url: s.prometheus, resourceRef: s.resourceRef }, alertmanager: s.alertmanager ? { url: s.alertmanager } : null,
          quality: { window: s.configuredWindow || s.window, resyncInterval: '15m' } },
        status: { lastSync: new Date(now - s.lastSyncMin * MIN).toISOString(), effectiveWindow: s.window,
          rules: counts, findingsTotal: total, findings: byType,
          teams: Object.keys(teams).sort().map(function (k) { return teams[k]; }),
          conditions: [{ type: 'Ready', status: 'True', reason: 'Synced', message: counts.total + ' rules, ' + total + ' findings' }] },
        retention: s.retention
      };
    });
    return { now: now, rules: rules, scopes: scopeObjs, findingTypes: FINDINGS };
  }

  // dailyActivations spreads a rule's activations over the window, one bucket per day,
  // deterministically, with the last activation on the day it happened.
  function dailyActivations(identity, total, days, lastMin) {
    var out = [];
    for (var i = 0; i < days; i++) out.push(0);
    if (!total) return out;
    var seed = parseInt(identity, 16) || 1;
    function rnd() { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed / 0x7fffffff; }
    var weights = out.map(function (_, i) { return 0.3 + rnd() * (i % 7 === 5 || i % 7 === 6 ? 0.6 : 1.4); });
    var lastDay = days - 1 - Math.floor((lastMin || 0) / 1440);
    for (var j = lastDay + 1; j < days; j++) weights[j] = 0;
    if (total < days / 2) { weights = weights.map(function () { return 0; }); }
    var sum = weights.reduce(function (a, b) { return a + b; }, 0);
    var left = total;
    if (sum > 0) {
      weights.forEach(function (w, i) { var n = Math.floor(total * w / sum); out[i] = n; left -= n; });
    }
    var k = Math.max(0, lastDay);
    while (left > 0) { out[k] += 1; left--; k = (k * 7 + 3) % Math.max(1, lastDay + 1); }
    if (lastDay >= 0 && out[lastDay] === 0) { out[lastDay] = 1; for (var m = 0; m < days; m++) if (m !== lastDay && out[m] > 0) { out[m]--; break; } }
    return out;
  }

  global.AlertDemoData = { build: build, findingTypes: FINDINGS, fmtDuration: fmtDuration };
})(window);
