# Multi-Instance Monitoring Guide for CPU Manager Go

## Centralized Prometheus and Grafana Configuration for Multi-Host Deployments

---

## Table of Contents

1. [Overview](#overview)
2. [Architecture](#architecture)
3. [Prerequisites](#prerequisites)
4. [Prometheus Configuration](#prometheus-configuration)
5. [Grafana Configuration](#grafana-configuration)
6. [Network and Security](#network-and-security)
7. [Service Discovery Options](#service-discovery-options)
8. [Troubleshooting](#troubleshooting)
9. [Best Practices](#best-practices)

---

## Overview

This document describes how to configure a centralized monitoring solution for CPU Manager Go deployments across multiple hosts. The solution uses:

- **Prometheus** - Central metrics aggregation and storage
- **Grafana** - Unified visualization and dashboards
- **CPU Manager Go** - Running on multiple hosts (10-1000+ nodes)

### Use Cases

- Monitor CPU and memory usage across all users on all hosts
- Identify resource-intensive users across the infrastructure
- Alert on system-wide resource issues
- Compare resource usage between hosts
- Track capacity planning metrics

---

## Architecture

### Recommended Architecture for 10-100 Hosts

```
┌─────────────────────────────────────────────────────────────────┐
│                      Monitoring Network                          │
│                                                                  │
│  ┌──────────────┐     ┌──────────────┐     ┌──────────────┐    │
│  │   Host 1     │     │   Host 2     │     │   Host N     │    │
│  │  resman │     │  resman │     │  resman │    │
│  │  :9101       │     │  :9101       │     │  :9101       │    │
│  └──────┬───────┘     └──────┬───────┘     └──────┬───────┘    │
│         │                    │                    │             │
│         └────────────────────┼────────────────────┘             │
│                              │                                  │
│                     ┌────────▼────────┐                         │
│                     │   Prometheus    │                         │
│                     │   :9090         │                         │
│                     │   (scrape all)  │                         │
│                     └────────┬────────┘                         │
│                              │                                  │
│                     ┌────────▼────────┐                         │
│                     │     Grafana     │                         │
│                     │   :3000         │                         │
│                     │   (visualize)   │                         │
│                     └─────────────────┘                         │
└─────────────────────────────────────────────────────────────────┘
```

### Architecture for 100-1000+ Hosts (Federated)

```
┌─────────────────────────────────────────────────────────────────┐
│                    Regional Prometheus Instances                 │
│                                                                  │
│  ┌────────────┐         ┌────────────┐         ┌────────────┐  │
│  │  Region 1  │         │  Region 2  │         │  Region N  │  │
│  │ Prometheus │         │ Prometheus │         │ Prometheus │  │
│  │   :9090    │         │   :9090    │         │   :9090    │  │
│  │  (50 hosts)│         │  (50 hosts)│         │  (50 hosts)│  │
│  └─────┬──────┘         └─────┬──────┘         └─────┬──────┘  │
│        │                      │                      │           │
│        └──────────────────────┼──────────────────────┘           │
│                               │                                  │
│                    ┌──────────▼──────────┐                       │
│                    │  Central Prometheus │                       │
│                    │  (federation/Thanos)│                       │
│                    └──────────┬──────────┘                       │
│                               │                                  │
│                    ┌──────────▼──────────┐                       │
│                    │      Grafana        │                       │
│                    └─────────────────────┘                       │
└─────────────────────────────────────────────────────────────────┘
```

---

## Prerequisites

### Network Requirements

| Component | Port | Protocol | Direction |
|-----------|------|----------|-----------|
| CPU Manager Go | 9101 | TCP | Inbound (from Prometheus) |
| Prometheus | 9090 | TCP | Inbound (from Grafana) |
| Grafana | 3000 | TCP | Inbound (from users) |

### Firewall Configuration

```bash
# On each host running CPU Manager Go
# Allow Prometheus to scrape metrics
sudo iptables -A INPUT -p tcp --dport 9101 -s <PROMETHEUS_IP> -j ACCEPT

# Or with firewalld (RHEL/CentOS/Fedora)
sudo firewall-cmd --permanent --add-port=9101/tcp
sudo firewall-cmd --reload

# Or with ufw (Ubuntu/Debian)
sudo ufw allow from <PROMETHEUS_IP> to any port 9101 proto tcp
```

### CPU Manager Configuration

Enable Prometheus on each CPU Manager Go instance:

```bash
# /etc/resman.conf
ENABLE_PROMETHEUS=true
PROMETHEUS_HOST="0.0.0.0"    # Listen on all interfaces
PROMETHEUS_PORT=9101
```

Restart the service:
```bash
sudo systemctl restart resman
```

Verify metrics are exposed:
```bash
curl http://localhost:9101/metrics
```

---

## Prometheus Configuration

### Basic Configuration (Static Targets)

For small deployments (10-50 hosts), use static target configuration:

```yaml
# /etc/prometheus/prometheus.yml
global:
  scrape_interval: 30s      # Match CPU Manager polling interval
  evaluation_interval: 30s
  external_labels:
    monitor: 'resman-monitor'

# Alertmanager configuration (optional)
alerting:
  alertmanagers:
    - static_configs:
        - targets:
          - alertmanager:9093

# Rule files (optional)
rule_files:
  - /etc/prometheus/rules/cpu_manager_alerts.yml

# Scrape configurations
scrape_configs:
  # CPU Manager Go instances
  - job_name: 'resman-go'
    static_configs:
      - targets:
        - 'host1.example.com:9101'
        - 'host2.example.com:9101'
        - 'host3.example.com:9101'
        - '192.168.1.10:9101'
        - '192.168.1.11:9101'
        labels:
          environment: 'production'
          cluster: 'main'
    
    # Relabel to add instance metadata
    relabel_configs:
      - source_labels: [__address__]
        target_label: instance
        regex: '([^:]+):\d+'
        replacement: '${1}'
```

### File-Based Service Discovery

For medium deployments (50-200 hosts), use file-based service discovery:

```yaml
# /etc/prometheus/prometheus.yml
scrape_configs:
  - job_name: 'resman-go'
    file_sd_configs:
      - files:
        - /etc/prometheus/targets/cpu_manager_*.json
        refresh_interval: 30s
    
    # Add common labels
    relabel_configs:
      - source_labels: [__meta_environment]
        target_label: environment
      - source_labels: [__meta_datacenter]
        target_label: datacenter
```

Create target files:

```json
// /etc/prometheus/targets/cpu_manager_production.json
[
  {
    "targets": ["host1.example.com:9101", "host2.example.com:9101"],
    "labels": {
      "__meta_environment": "production",
      "__meta_datacenter": "us-east-1",
      "__meta_role": "worker"
    }
  },
  {
    "targets": ["host3.example.com:9101", "host4.example.com:9101"],
    "labels": {
      "__meta_environment": "production",
      "__meta_datacenter": "us-west-2",
      "__meta_role": "master"
    }
  }
]
```

### DNS-Based Service Discovery

For dynamic environments:

```yaml
scrape_configs:
  - job_name: 'resman-go'
    dns_sd_configs:
      - names:
        - '_resman._tcp.example.com'
        type: 'SRV'
        port: 9101
        refresh_interval: 30s
```

### Kubernetes Service Discovery

For Kubernetes deployments:

```yaml
scrape_configs:
  - job_name: 'resman-go'
    kubernetes_sd_configs:
      - role: pod
        selectors:
          - role: pod
            label: app=resman-go
    
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_app]
        action: keep
        regex: resman-go
      - source_labels: [__meta_kubernetes_namespace]
        target_label: namespace
      - source_labels: [__meta_kubernetes_pod_name]
        target_label: pod
```

### Authentication Options for Prometheus Metrics

CPU Manager Go supports optional authentication for exposing metrics. You can configure either **Basic Authentication** or **JWT (Bearer Token)** authentication based on your security requirements.

#### Configuration on CPU Manager Go

Enable authentication in the configuration file:

```bash
# /etc/resman.conf

# Enable Prometheus metrics
ENABLE_PROMETHEUS=true
PROMETHEUS_HOST="0.0.0.0"
PROMETHEUS_PORT=9101

# Authentication Method: basic, jwt, or none
PROMETHEUS_AUTH_TYPE="basic"

# For Basic Authentication
PROMETHEUS_AUTH_USERNAME="prometheus"
PROMETHEUS_AUTH_PASSWORD_FILE="/etc/resman/prometheus_password"

# For JWT Authentication
PROMETHEUS_AUTH_TYPE="jwt"
PROMETHEUS_JWT_SECRET_FILE="/etc/resman/jwt_secret"
PROMETHEUS_JWT_ISSUER="resman"
PROMETHEUS_JWT_AUDIENCE="prometheus"
PROMETHEUS_JWT_EXPIRY=3600
```

#### Option 1: Basic Authentication

**Server-Side Configuration (CPU Manager Go):**

```bash
# /etc/resman.conf
PROMETHEUS_AUTH_TYPE="basic"
PROMETHEUS_AUTH_USERNAME="prometheus"
PROMETHEUS_AUTH_PASSWORD_FILE="/etc/resman/prometheus_password"
```

Create the password file:
```bash
# Generate secure password
openssl rand -base64 32 | sudo tee /etc/resman/prometheus_password
sudo chmod 600 /etc/resman/prometheus_password
sudo chown root:root /etc/resman/prometheus_password

# Restart CPU Manager
sudo systemctl restart resman
```

**Prometheus Configuration:**

```yaml
scrape_configs:
  - job_name: 'resman-go-basic'
    scheme: https  # Recommended with basic auth
    
    # Basic authentication
    basic_auth:
      username: prometheus
      password_file: /etc/prometheus/credentials/cpu_manager_password
    
    # TLS configuration (recommended)
    tls_config:
      ca_file: /etc/prometheus/certs/ca.crt
      insecure_skip_verify: false
    
    static_configs:
      - targets:
        - 'host1.example.com:9101'
        - 'host2.example.com:9101'
```

**Security Considerations:**
- ✅ Simple to configure
- ✅ Widely supported
- ⚠️ Passwords must be stored securely
- ⚠️ Should only be used with TLS/HTTPS
- ⚠️ No token expiration (passwords don't expire automatically)

---

#### Option 2: JWT (Bearer Token) Authentication

**Server-Side Configuration (CPU Manager Go):**

```bash
# /etc/resman.conf
PROMETHEUS_AUTH_TYPE="jwt"
PROMETHEUS_JWT_SECRET_FILE="/etc/resman/jwt_secret"
PROMETHEUS_JWT_ISSUER="resman"
PROMETHEUS_JWT_AUDIENCE="prometheus"
PROMETHEUS_JWT_EXPIRY=3600  # Token validity in seconds (1 hour)
```

Generate JWT secret:
```bash
# Generate secure JWT secret (minimum 32 bytes)
openssl rand -base64 64 | sudo tee /etc/resman/jwt_secret
sudo chmod 600 /etc/resman/jwt_secret
sudo chown root:root /etc/resman/jwt_secret

# Restart CPU Manager
sudo systemctl restart resman
```

**Generate JWT Token for Prometheus:**

Create a script to generate tokens:

```bash
#!/bin/bash
# /usr/local/bin/generate-resman-jwt.sh

SECRET_FILE="/etc/resman/jwt_secret"
ISSUER="resman"
AUDIENCE="prometheus"
EXPIRY=3600

# Read secret
SECRET=$(cat $SECRET_FILE)

# Generate JWT using Python (install PyJWT: pip install PyJWT)
python3 << EOF
import jwt
import datetime
import hmac
import base64

secret = "$SECRET"
now = datetime.datetime.utcnow()

payload = {
    "iss": "$ISSUER",
    "aud": "$AUDIENCE",
    "iat": now,
    "exp": now + datetime.timedelta(seconds=$EXPIRY),
    "sub": "prometheus-scraper",
    "permissions": ["metrics:read"]
}

token = jwt.encode(payload, secret, algorithm="HS256")
print(token)
EOF
```

Make it executable:
```bash
chmod +x /usr/local/bin/generate-resman-jwt.sh
```

**Prometheus Configuration with JWT:**

```yaml
scrape_configs:
  - job_name: 'resman-go-jwt'
    scheme: https  # Required with JWT
    
    # Bearer token authentication
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/credentials/cpu_manager_jwt_token
    
    # TLS configuration (required)
    tls_config:
      ca_file: /etc/prometheus/certs/ca.crt
      cert_file: /etc/prometheus/certs/client.crt
      key_file: /etc/prometheus/certs/client.key
      insecure_skip_verify: false
    
    static_configs:
      - targets:
        - 'host1.example.com:9101'
        - 'host2.example.com:9101'
```

**Token Rotation Script:**

Create a script to automatically rotate JWT tokens:

```bash
#!/bin/bash
# /usr/local/bin/rotate-resman-jwt.sh

TOKEN_FILE="/etc/prometheus/credentials/cpu_manager_jwt_token"
TOKEN_DIR=$(dirname $TOKEN_FILE)

# Ensure directory exists
mkdir -p $TOKEN_DIR
chmod 700 $TOKEN_DIR

# Generate new token
NEW_TOKEN=$(/usr/local/bin/generate-resman-jwt.sh)

# Write token with secure permissions
echo "$NEW_TOKEN" > $TOKEN_FILE
chmod 600 $TOKEN_FILE
chown prometheus:prometheus $TOKEN_FILE

# Reload Prometheus to pick up new token
curl -X POST http://localhost:9090/-/reload

echo "JWT token rotated successfully at $(date)"
```

**Automate Token Rotation with Cron:**

```bash
# /etc/cron.d/resman-jwt-rotation
# Rotate JWT token every 30 minutes (token expires in 1 hour)
*/30 * * * * prometheus /usr/local/bin/rotate-resman-jwt.sh >> /var/log/resman-jwt-rotation.log 2>&1
```

**Security Considerations:**
- ✅ Token-based authentication (no passwords)
- ✅ Automatic expiration (configurable)
- ✅ Fine-grained permissions support
- ✅ Industry standard (RFC 7519)
- ⚠️ More complex to configure
- ⚠️ Requires token rotation mechanism
- ⚠️ Clock synchronization required between systems

---

#### Option 3: Combined Authentication (Basic + JWT)

For maximum flexibility, you can support both authentication methods:

```bash
# /etc/resman.conf
PROMETHEUS_AUTH_TYPE="both"
PROMETHEUS_AUTH_USERNAME="prometheus"
PROMETHEUS_AUTH_PASSWORD_FILE="/etc/resman/prometheus_password"
PROMETHEUS_JWT_SECRET_FILE="/etc/resman/jwt_secret"
PROMETHEUS_JWT_ISSUER="resman"
PROMETHEUS_JWT_AUDIENCE="prometheus"
```

This allows:
- Legacy systems to use Basic Auth
- New systems to use JWT
- Gradual migration path

---

#### Comparison Table

| Feature | Basic Auth | JWT | Both |
|---------|-----------|-----|------|
| **Configuration Complexity** | Low | Medium | Medium |
| **Security Level** | Medium (with TLS) | High | High |
| **Token Expiration** | No | Yes (configurable) | Yes (JWT only) |
| **Credential Rotation** | Manual | Automated | Automated (JWT) |
| **Permissions Support** | No | Yes | Partial |
| **Best For** | Small deployments | Large/Enterprise | Migration scenarios |
| **TLS Required** | Highly recommended | Required | Required |

---

### Prometheus with TLS (No Authentication)

For internal networks where authentication is handled at the network level:

```yaml
scrape_configs:
  - job_name: 'resman-go-tls-only'
    scheme: https
    
    # TLS only (client certificate for mutual TLS)
    tls_config:
      ca_file: /etc/prometheus/certs/ca.crt
      cert_file: /etc/prometheus/certs/client.crt
      key_file: /etc/prometheus/certs/client.key
      insecure_skip_verify: false
    
    static_configs:
      - targets:
        - 'host1.example.com:9101'
        - 'host2.example.com:9101'
```

---

### Prometheus with Authentication and TLS (Legacy Section)

For secure deployments with basic authentication:

```yaml
scrape_configs:
  - job_name: 'resman-go'
    scheme: https
    tls_config:
      ca_file: /etc/prometheus/certs/ca.crt
      cert_file: /etc/prometheus/certs/client.crt
      key_file: /etc/prometheus/certs/client.key
      insecure_skip_verify: false
    
    basic_auth:
      username: prometheus
      password_file: /etc/prometheus/credentials/cpu_manager_password
    
    static_configs:
      - targets:
        - 'host1.example.com:9101'
        - 'host2.example.com:9101'
```

---

## Grafana Configuration

### Datasource Configuration

#### Option 1: Manual Configuration via UI

1. Navigate to **Configuration** → **Data Sources**
2. Click **Add data source**
3. Select **Prometheus**
4. Configure:
   - **Name**: `CPU Manager Prometheus`
   - **URL**: `http://prometheus:9090`
   - **Access**: `Server` (recommended) or `Browser`
   - **Auth**: Enable if Prometheus requires authentication
5. Click **Save & Test**

#### Option 2: Provisioning (Recommended)

```yaml
# /etc/grafana/provisioning/datasources/resman.yml
apiVersion: 1

datasources:
  - name: CPU Manager Prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
    editable: false
    jsonData:
      timeInterval: "30s"
      queryTimeout: "60s"
      httpMethod: "POST"
    secureJsonData:
      # If Prometheus requires authentication
      basicAuthPassword: ${PROMETHEUS_PASSWORD}
    basicAuth: true
    basicAuthUser: prometheus
```

### Dashboard Configuration

#### Import the Provided Dashboard

1. Download `docs/dashboard-grafana.json` from the CPU Manager Go repository
2. In Grafana, navigate to **Dashboards** → **Import**
3. Upload the JSON file
4. Select the Prometheus datasource
5. Click **Import**

#### Multi-Instance Dashboard Variables

Add template variables for filtering:

```json
// Dashboard templating configuration
{
  "templating": {
    "list": [
      {
        "name": "datacenter",
        "type": "query",
        "datasource": "CPU Manager Prometheus",
        "query": "label_values(cpu_manager_cpu_total_usage_percent, datacenter)",
        "refresh": 1,
        "includeAll": true,
        "multi": true
      },
      {
        "name": "instance",
        "type": "query",
        "datasource": "CPU Manager Prometheus",
        "query": "label_values(cpu_manager_cpu_total_usage_percent{datacenter=~\"$datacenter\"}, instance)",
        "refresh": 1,
        "includeAll": true,
        "multi": true
      },
      {
        "name": "username",
        "type": "query",
        "datasource": "CPU Manager Prometheus",
        "query": "label_values(cpu_manager_user_cpu_usage_percent, username)",
        "refresh": 1,
        "includeAll": true,
        "multi": true
      }
    ]
  }
}
```

### Multi-Instance Panel Examples

#### Total CPU Usage Across All Hosts

```promql
# Sum of CPU usage across all instances
sum(cpu_manager_cpu_total_usage_percent)
```

#### Top 10 Hosts by CPU Usage

```promql
# Top 10 hosts by total CPU usage
topk(10, cpu_manager_cpu_total_usage_percent)
```

#### CPU Usage Per User Across All Hosts

```promql
# Aggregate CPU usage by username across all hosts
sum by (username) (cpu_manager_user_cpu_usage_percent)
```

#### Memory Usage Per Host

```promql
# Memory usage per instance
cpu_manager_memory_usage_megabytes
```

#### Users with Highest Memory (All Hosts)

```promql
# Top 10 users by memory across all hosts
topk(10, sum by (username, instance) (cpu_manager_user_memory_usage_bytes))
```

#### Hosts with Active CPU Limits

```promql
# Count of limited users per host
sum by (instance) (cpu_manager_user_cpu_limited)
```

#### CPU Limit Activation Rate by Host

```promql
# Rate of limit activations per host
sum by (instance) (rate(cpu_manager_limits_activated_total[5m]))
```

---

## Network and Security

### Authentication Overview

CPU Manager Go supports multiple authentication methods for securing Prometheus metrics endpoints. See the [Authentication Options](#authentication-options-for-prometheus-metrics) section above for detailed configuration.

**Quick Reference:**

| Method | Security | Complexity | Best For |
|--------|----------|------------|----------|
| **None** | Low | None | Isolated networks only |
| **Basic Auth** | Medium (with TLS) | Low | Small deployments |
| **JWT** | High | Medium | Enterprise/Production |
| **mTLS** | Very High | High | Regulated environments |

### Network Segmentation

Create a dedicated monitoring network:

```
Management Network: 10.0.0.0/24
├── Prometheus:     10.0.0.10
├── Grafana:        10.0.0.11
└── Alertmanager:   10.0.0.12

Production Network: 192.168.1.0/24
├── Host 1:         192.168.1.10 (resman :9101)
├── Host 2:         192.168.1.11 (resman :9101)
└── Host N:         192.168.1.N  (resman :9101)

Firewall Rules:
- Allow 10.0.0.10 → 192.168.1.0/24:9101 (Prometheus scrape)
- Allow 10.0.0.11 → 10.0.0.10:9090 (Grafana query)
- Deny all other traffic to :9101
```

### TLS Configuration for CPU Manager Go

Currently, CPU Manager Go exposes metrics over HTTP. For production environments:

**Option 1: Reverse Proxy with TLS**

```nginx
# /etc/nginx/conf.d/resman-prometheus.conf
upstream cpu_manager {
    server 127.0.0.1:9101;
}

server {
    listen 443 ssl;
    server_name resman.example.com;
    
    ssl_certificate /etc/ssl/certs/resman.crt;
    ssl_certificate_key /etc/ssl/private/resman.key;
    ssl_protocols TLSv1.2 TLSv1.3;
    
    # Basic authentication
    auth_basic "Prometheus Metrics";
    auth_basic_user_file /etc/nginx/.prometheus_credentials;
    
    location /metrics {
        proxy_pass http://cpu_manager;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }
}
```

Update Prometheus configuration:
```yaml
scrape_configs:
  - job_name: 'resman-go'
    scheme: https
    basic_auth:
      username: prometheus
      password: secure_password
    static_configs:
      - targets:
        - 'resman.example.com:443'
```

**Option 2: SSH Tunnel**

```bash
# On Prometheus server, create SSH tunnel to each host
for host in host1 host2 host3; do
    ssh -f -N -L 9101:localhost:9101 user@$host &
done
```

### Authentication Options

| Method | Complexity | Security | Recommended For |
|--------|------------|----------|-----------------|
| Network isolation | Low | Medium | Internal networks |
| Basic auth + TLS | Medium | High | Small-medium deployments |
| mTLS | High | Very High | Regulated environments |
| OAuth/OIDC | High | Very High | Large enterprises |

---

## Service Discovery Options

### Consul Integration

```yaml
# Prometheus configuration
scrape_configs:
  - job_name: 'resman-go'
    consul_sd_configs:
      - server: 'consul:8500'
        services:
          - 'resman-go'
        tags:
          - 'production'
    
    relabel_configs:
      - source_labels: [__meta_consul_service]
        target_label: service
      - source_labels: [__meta_consul_tags]
        target_label: tags
```

Register CPU Manager Go service in Consul:
```json
{
  "service": {
    "name": "resman-go",
    "port": 9101,
    "tags": ["production", "monitoring"],
    "check": {
      "http": "http://localhost:9101/metrics",
      "interval": "30s"
    }
  }
}
```

### AWS EC2 Service Discovery

```yaml
scrape_configs:
  - job_name: 'resman-go'
    ec2_sd_configs:
      - region: us-east-1
        port: 9101
        filters:
          - name: tag:Monitoring
            values:
              - resman
          - name: instance-state-name
            values:
              - running
    
    relabel_configs:
      - source_labels: [__meta_ec2_tag_Name]
        target_label: instance_name
      - source_labels: [__meta_ec2_availability_zone]
        target_label: az
```

### Azure Service Discovery

```yaml
scrape_configs:
  - job_name: 'resman-go'
    azure_sd_configs:
      - subscription_id: <SUBSCRIPTION_ID>
        tenant_id: <TENANT_ID>
        client_id: <CLIENT_ID>
        client_secret: <CLIENT_SECRET>
        port: 9101
        resource_group: <RESOURCE_GROUP>
```

---

## Troubleshooting

### Prometheus Cannot Scrape Targets

**Symptoms**: Targets show as DOWN in Prometheus UI

**Check**:
```bash
# From Prometheus server
curl -v http://<target-host>:9101/metrics

# Check firewall
telnet <target-host> 9101

# Check CPU Manager service
systemctl status resman
```

**Solutions**:
1. Verify firewall allows traffic on port 9101
2. Check CPU Manager is running and listening on correct interface
3. Verify Prometheus configuration syntax: `promtool check config prometheus.yml`

### Grafana Cannot Query Prometheus

**Symptoms**: "No data" or connection errors in Grafana

**Check**:
```bash
# From Grafana server
curl http://prometheus:9090/api/v1/query?query=up

# Check Prometheus is running
systemctl status prometheus

# Check network connectivity
ping prometheus
```

### High Cardinality Issues

**Symptoms**: Prometheus memory usage grows, queries slow down

**Cause**: Too many unique label combinations (users × hosts × time)

**Solutions**:
1. Reduce scrape interval if not needed
2. Use recording rules for expensive aggregations
3. Implement metric relabeling to drop unnecessary labels:

```yaml
metric_relabel_configs:
  # Drop metrics older than 7 days
  - action: drop
    source_labels: [__name__]
    regex: 'cpu_manager_.*'
```

### Missing Metrics

**Symptoms**: Some metrics not appearing in Grafana

**Check**:
```promql
# Check if metrics exist
count(cpu_manager_user_cpu_usage_percent)

# Check which instances are reporting
label_values(cpu_manager_user_cpu_usage_percent, instance)

# Check for gaps in data
delta(cpu_manager_cpu_total_usage_percent[5m])
```

---

## Best Practices

### Scaling Recommendations

| Hosts | Architecture | Storage | Retention |
|-------|-------------|---------|-----------|
| 1-50 | Single Prometheus | Local SSD | 15-30 days |
| 50-200 | Single Prometheus | Dedicated storage | 30-90 days |
| 200-500 | Federated Prometheus | Thanos/Cortex | 90+ days |
| 500+ | Thanos/Cortex cluster | Object storage | 1+ year |

### Performance Tuning

**Prometheus**:
```yaml
# prometheus.yml
global:
  scrape_interval: 30s      # Match CPU Manager cycle
  scrape_timeout: 25s       # Less than interval
  evaluation_interval: 30s

# Increase limits for large deployments
storage:
  tsdb:
    retention.time: 30d
    retention.size: 50GB
    max-block-duration: 2h
```

**Grafana**:
- Use `Server` access mode (not `Browser`)
- Enable query caching
- Set appropriate refresh intervals (30s-5m)
- Use recording rules for complex queries

### Alerting Strategy

```yaml
# /etc/prometheus/rules/cpu_manager_alerts.yml
groups:
  - name: cpu_manager_infrastructure
    rules:
      # Host down
      - alert: CPUManagerHostDown
        expr: up{job="resman-go"} == 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "CPU Manager host {{ $labels.instance }} is down"
      
      # High CPU across multiple hosts
      - alert: CPUManagerWidespreadHighCPU
        expr: count(cpu_manager_cpu_total_usage_percent > 80) > 5
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "High CPU on {{ $value }} hosts"
      
      # User consuming excessive resources cluster-wide
      - alert: CPUManagerResourceHogUser
        expr: sum by (username) (cpu_manager_user_cpu_usage_percent) > 200
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "User {{ $labels.username }} using {{ $value }}% CPU cluster-wide"
```

### Backup and Recovery

**Prometheus Data**:
```bash
# Backup Prometheus data directory
tar -czf prometheus-backup-$(date +%Y%m%d).tar.gz /var/lib/prometheus

# Backup configuration
tar -czf prometheus-config-$(date +%Y%m%d).tar.gz /etc/prometheus

# Restore
tar -xzf prometheus-backup-*.tar.gz -C /
systemctl restart prometheus
```

**Grafana Dashboards**:
```bash
# Export dashboards via API
curl -H "Authorization: Bearer <API_KEY>" \
  http://grafana:3000/api/dashboards/uid/resman-dashboard > dashboard-backup.json

# Backup provisioning files
cp -r /etc/grafana/provisioning /backup/grafana-provisioning
```

### Security Checklist

#### Network Security
- [ ] Firewall rules restrict access to port 9101
- [ ] Prometheus access restricted to Grafana server
- [ ] Dedicated monitoring network/VLAN configured
- [ ] Network segmentation implemented

#### Authentication & Authorization
- [ ] Authentication enabled (Basic Auth or JWT)
- [ ] TLS enabled for all external communication
- [ ] Certificates managed and rotated regularly
- [ ] Service accounts use minimal privileges
- [ ] JWT tokens rotated automatically (if using JWT)
- [ ] Passwords stored securely with proper permissions (if using Basic Auth)

#### Application Security
- [ ] Grafana requires authentication
- [ ] Prometheus requires authentication (if exposed)
- [ ] Regular security updates applied
- [ ] Audit logging enabled
- [ ] Security scanning enabled (gosec, Trivy, etc.)

#### Data Protection
- [ ] Backup strategy implemented and tested
- [ ] Credentials backed up securely
- [ ] Disaster recovery plan documented
- [ ] Retention policies configured

#### Monitoring & Alerting
- [ ] Alerting configured for security events
- [ ] Monitoring of authentication failures
- [ ] Dashboard for security metrics
- [ ] Incident response procedure documented

---

## Appendix A: Complete Example Configuration

### Prometheus Full Configuration

```yaml
# /etc/prometheus/prometheus.yml
global:
  scrape_interval: 30s
  evaluation_interval: 30s
  external_labels:
    cluster: 'production'
    monitor: 'resman'

alerting:
  alertmanagers:
    - static_configs:
        - targets: ['alertmanager:9093']

rule_files:
  - /etc/prometheus/rules/*.yml

scrape_configs:
  - job_name: 'prometheus'
    static_configs:
      - targets: ['localhost:9090']
  
  - job_name: 'resman-go'
    file_sd_configs:
      - files:
        - /etc/prometheus/targets/cpu_manager_*.json
        refresh_interval: 30s
    
    relabel_configs:
      - source_labels: [__address__]
        target_label: instance
        regex: '([^:]+):\d+'
        replacement: '${1}'
    
    metric_relabel_configs:
      # Keep only relevant metrics
      - source_labels: [__name__]
        regex: 'cpu_manager_.*|up|scrape_.*'
        action: keep
```

### Grafana Dashboard JSON Template

See `docs/dashboard-grafana.json` for the complete multi-instance dashboard.

---

## Appendix B: Quick Start Script

```bash
#!/bin/bash
# setup-monitoring.sh - Quick setup for multi-instance monitoring

PROMETHEUS_SERVER="prometheus.example.com"
GRAFANA_SERVER="grafana.example.com"
HOSTS=("host1" "host2" "host3")

# Generate Prometheus targets file
cat > cpu_manager_targets.json << EOF
[
  {
    "targets": [
$(printf '      "%s:9101",\n' "${HOSTS[@]}" | sed '$ s/,$//')
    ],
    "labels": {
      "environment": "production",
      "job": "resman-go"
    }
  }
]
EOF

# Copy to Prometheus server
scp cpu_manager_targets.json $PROMETHEUS_SERVER:/etc/prometheus/targets/

# Restart Prometheus
ssh $PROMETHEUS_SERVER "systemctl restart prometheus"

echo "Monitoring setup complete!"
echo "Prometheus: http://$PROMETHEUS_SERVER:9090"
echo "Grafana: http://$GRAFANA_SERVER:3000"
```

---

## Document Information

| Field | Value |
|-------|-------|
| **Version** | 1.0 |
| **Last Updated** | February 2026 |
| **Author** | CPU Manager Go Team |
| **Audience** | System Administrators, DevOps Engineers |
| **Prerequisites** | Basic Prometheus and Grafana knowledge |

---

## Related Documentation

- [CPU Manager Go README](../README.md)
- [Prometheus Queries](prometheus-queries.md)
- [Alerting Rules](alerting-rules.yml)
- [CPU Manager Man Page](resman.8)

## Support

For issues and feature requests, please open an issue at:
https://github.com/fdefilippo/resman-go/issues
