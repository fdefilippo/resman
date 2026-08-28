# Grafana Multi-Cluster Dashboard Guide

## Overview

The shipped operations dashboard supports filtering and grouping by:

- Prometheus cluster and environment labels;
- the ResMan `server_role` label;
- hostname;
- one or more selected values for each variable.

Cluster and environment are scrape-time labels. Hostname and server role are exported
by ResMan on every relevant metric family.

## Prometheus configuration

Add stable external labels to each Prometheus instance:

```yaml
# /etc/prometheus/prometheus.yml
global:
  external_labels:
    cluster: production-eu
    environment: production

scrape_configs:
  - job_name: resman
    scheme: https
    static_configs:
      - targets:
          - db-prod-01.example.com:1974
          - web-prod-01.example.com:1974
    tls_config:
      ca_file: /etc/prometheus/resman-ca.crt
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/resman-token
```

Use a different `cluster` label for every Prometheus cluster and a consistent
`environment` vocabulary such as `production`, `staging`, and `development`.

The example in [`prometheus.yml`](prometheus.yml) is the authoritative scrape template.

## ResMan server roles

Set a stable role on each host:

```ini
SERVER_ROLE=database
```

Recommended values include `database`, `web-frontend`, `web-backend`, `batch`, `cache`,
`monitoring`, and `development`. Consistency matters more than the exact vocabulary.
Do not encode the hostname or environment into the role.

## Importing the dashboard

1. Open Grafana and select **Dashboards → New → Import**.
2. Upload [`dashboard-grafana-operations.json`](dashboard-grafana-operations.json).
3. Select the Prometheus data source.
4. Save the dashboard and verify the cluster, environment, role, and hostname variables.

The variables support multi-selection and an **All** value. Narrow cluster and
environment first, then role and hostname, to keep high-cardinality queries focused.

## Query patterns

### CPU usage by cluster and role

```promql
sum by (cluster) (resman_all_users_cpu_usage_percent)

sum by (cluster, environment, server_role) (
  resman_all_users_cpu_usage_percent
)

sum by (cluster, environment, server_role, hostname) (
  resman_all_users_cpu_usage_percent
)
```

### Highest per-user consumption

```promql
topk(5, resman_user_cpu_usage_percent)

topk(5, resman_user_memory_usage_bytes)
```

### Enforcement transitions

```promql
sum by (cluster, environment, server_role, hostname) (
  increase(resman_limits_activated_total[15m])
)

sum by (cluster, environment, server_role, hostname) (
  increase(resman_limits_deactivated_total[15m])
)
```

The transition counters carry hostname and server-role labels. Cluster and environment
come from Prometheus external labels or scrape relabeling.

## Multi-cluster alerting

Group alerts by labels that identify the affected deployment:

```yaml
groups:
  - name: resman-multi-cluster
    rules:
      - alert: ResManLimitsActive
        expr: resman_limits_active == 1
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: >-
            Limits active on {{ $labels.hostname }}
            ({{ $labels.cluster }}/{{ $labels.environment }}, {{ $labels.server_role }})
```

Start from [`alerting-rules.yml`](alerting-rules.yml), whose metric names and semantics
are checked by `promtool` and the repository contract checker.

## Troubleshooting

### The cluster variable is empty

Confirm that the series carry `cluster` and `environment`:

```promql
count by (cluster, environment) ({__name__=~"resman_.*"})
```

If the labels are absent, add external labels or scrape relabeling and reload Prometheus.

### The server role is absent

Confirm `SERVER_ROLE` in `/etc/resman/resman.conf`, restart ResMan when required by the
configuration lifecycle, and inspect a current series:

```promql
count by (server_role) (resman_all_users_count)
```

### Hostname is `unknown`

Check the operating-system hostname before changing ResMan:

```bash
hostnamectl status
hostname
```

Set a stable hostname, restart ResMan, and allow Prometheus to scrape the new series.

### Panels disappear after selecting a host

Confirm that the selected metric family carries the `hostname` and `server_role` labels.
Series produced before an upgrade that added those labels have a different identity and
end at the upgrade boundary; new series start from zero for counters.

## Operational conventions

- Use stable, lowercase cluster, environment, and role values.
- Keep production and staging distinguishable even when they share Prometheus or Grafana.
- Create team-specific dashboards by applying variables, not by copying and editing the
  metric names.
- Use TLS and authentication for every non-loopback Prometheus bind.
- Keep wildcard binds explicit and protect them with firewall policy.
