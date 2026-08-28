# Grafana Dashboard Multi-Cluster Guide

## Panoramica

La dashboard Grafana di ResMan supporta ora:
- **Multi-cluster**: Visualizza metriche da più cluster Prometheus
- **Environment**: Distingue production, staging e development
- **Server Role**: Filtra per ruolo del server (database, web-frontend, batch, etc.)
- **Hostname**: Filtra per hostname specifico

## Configurazione Prometheus

### 1. External Labels

Aggiungi external labels alla configurazione Prometheus per identificare il cluster:

```yaml
# prometheus.yml
global:
  external_labels:
    cluster: 'primary'         # Nome del cluster
    environment: 'production' # Ambiente operativo
    region: 'eu-west-1'        # Opzionale: regione
```

**Esempio multi-cluster:**
```yaml
# Primary production cluster
global:
  external_labels:
    cluster: 'primary'
    environment: 'production'

# Secondary staging cluster
global:
  external_labels:
    cluster: 'secondary'
    environment: 'staging'
```

### 2. ResMan Configuration

Configura `SERVER_ROLE` in `/etc/resman/resman.conf`:

```bash
# Ruoli predefiniti suggeriti:
# - database: Server database (MySQL, PostgreSQL, MongoDB)
# - web-frontend: Server web (Nginx, Apache)
# - web-backend: Application server (Node.js, Python, Java)
# - batch: Batch processing server
# - cache: Cache server (Redis, Memcached)
# - monitoring: Monitoring server
# - development: Development server

SERVER_ROLE=database
```

### 3. Metriche con Label

Tutte le metriche ResMan includono ora:
- `hostname`: Hostname del server (automatico)
- `server_role`: Ruolo configurato (da SERVER_ROLE)
- `cluster`: Label esterna Prometheus (da prometheus.yml)
- `environment`: Label esterna Prometheus (da prometheus.yml)

**Esempio metrica:**
```
resman_cpu_total_usage_percent{
  hostname="db-prod-01",
  server_role="database",
  cluster="primary",
  environment="production"
} 75.5
```

## Importare la Dashboard

### 1. Via Grafana UI

```bash
1. Apri Grafana
2. Dashboards → Import
3. Carica file: docs/dashboard-grafana-operations.json
4. Seleziona datasource Prometheus
5. Clicca Import
```

### 2. Dashboard Variables

La dashboard include le seguenti variabili:

| Variabile | Label | Query | Multi-Select |
|-----------|-------|-------|--------------|
| `cluster` | Cluster | `label_values(resman_cpu_total_usage_percent, cluster)` | ✅ Yes |
| `environment` | Environment | `label_values(resman_cpu_total_usage_percent{cluster=~"$cluster"}, environment)` | ✅ Yes |
| `server_role` | Server Role | `label_values(resman_cpu_total_usage_percent{cluster=~"$cluster", environment=~"$environment"}, server_role)` | ✅ Yes |
| `hostname` | Hostname | `label_values(resman_cpu_total_usage_percent{cluster=~"$cluster", environment=~"$environment", server_role=~"$server_role"}, hostname)` | ✅ Yes |
| `username` | Username | `label_values(resman_user_cpu_usage_percent{cluster=~"$cluster", environment=~"$environment", server_role=~"$server_role", hostname=~"$hostname"}, username)` | ✅ Yes |

### 3. Utilizzo dei Filtri

**Filtrare per cluster:**
1. Clicca sul dropdown "Cluster" in alto
2. Seleziona uno o più cluster
3. Tutti i panel mostrano solo dati dai cluster selezionati

**Filtrare per server role:**
1. Seleziona prima cluster ed environment
2. Clicca sul dropdown "Server Role"
3. Seleziona uno o più ruoli (es: "database", "web-frontend")

**Filtrare per hostname:**
1. Seleziona cluster, environment e server role
2. Clicca sul dropdown "Hostname"
3. Seleziona uno o più hostname specifici

## Esempi di Query

### CPU Usage per Cluster e Role

```promql
# CPU totale per cluster
sum by (cluster) (resman_cpu_total_usage_percent)

# CPU totale per server role
sum by (server_role) (resman_cpu_total_usage_percent{cluster=~"$cluster"})

# CPU per hostname
sum by (hostname) (resman_cpu_total_usage_percent{cluster=~"$cluster", server_role=~"$server_role"})
```

### Top Users CPU Usage

```promql
# Top 5 utenti per CPU usage
topk(5, resman_user_cpu_usage_percent{cluster=~"$cluster", server_role=~"$server_role", hostname=~"$hostname"})

# Top 5 utenti per memoria
topk(5, resman_user_memory_usage_bytes{cluster=~"$cluster", server_role=~"$server_role", hostname=~"$hostname"})
```

### Limits Status Multi-Cluster

```promql
# Cluster con limiti attivi
resman_cpu_limits_active{cluster=~"$cluster"} == 1

# Server role con più utenti limitati
sum by (server_role) (resman_cpu_actively_limited_users_count{cluster=~"$cluster"})
```

## Alerting Multi-Cluster

Esempio di regole di alerting:

```yaml
groups:
  - name: resman-multi-cluster
    rules:
      - alert: HighCPUUsageAllClusters
        expr: resman_cpu_total_usage_percent > 90
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "High CPU on {{ $labels.hostname }} ({{ $labels.cluster }})"
          description: "CPU usage is above 90% on {{ $labels.hostname }} ({{ $labels.server_role }}) in cluster {{ $labels.cluster }}"

      - alert: LimitsActiveLongTime
        expr: resman_cpu_limits_active == 1
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "CPU limits active for 1h on {{ $labels.hostname }}"
          description: "CPU limits have been active on {{ $labels.hostname }} ({{ $labels.server_role }}) for more than 1 hour"
```

## Troubleshooting

### Problema: Variabile cluster vuota

**Causa:** External labels non configurate in Prometheus

**Soluzione:**
```yaml
# prometheus.yml
global:
  external_labels:
    cluster: 'production'  # Aggiungi questo
```

### Problema: server_role non appare

**Causa:** SERVER_ROLE non configurato in ResMan

**Soluzione:**
```bash
# /etc/resman/resman.conf
SERVER_ROLE=database

# Riavvia ResMan
sudo systemctl restart resman
```

### Problema: hostname mostra "unknown"

**Causa:** Impossibile risolvere l'hostname di sistema

**Soluzione:**
```bash
# Verifica hostname di sistema
hostnamectl

# Se necessario, imposta hostname
sudo hostnamectl set-hostname db-prod-01
```

## Best Practices

### 1. Naming Convention

Usa naming convention coerenti per i cluster:
- `production`, `staging`, `development`
- Oppure: `prod-us-east`, `prod-eu-west`, `staging-us`

### 2. Server Role Standardizzati

Definisci una lista di ruoli standard:
```bash
# Ruoli consigliati
SERVER_ROLE=database
SERVER_ROLE=web-frontend
SERVER_ROLE=web-backend
SERVER_ROLE=batch
SERVER_ROLE=cache
SERVER_ROLE=monitoring
SERVER_ROLE=development
```

### 3. Dashboard Separate per Team

Crea dashboard specifiche per team:
- **Team Database**: Filtra su `server_role="database"`
- **Team Web**: Filtra su `server_role=~"web-.*"`
- **Team Operations**: Tutti i cluster e ruoli

### 4. Alerting per Cluster

Configura alert diversi per cluster:
- Production: Soglie più basse, escalation immediata
- Staging: Soglie più alte, notifica email
- Development: Solo logging, nessun alert

## Esempio Configurazione Completa

### Prometheus (production cluster)

```yaml
# /etc/prometheus/prometheus.yml
global:
  external_labels:
    cluster: 'production'
    region: 'eu-west-1'

scrape_configs:
  - job_name: 'resman'
    static_configs:
      - targets: ['db-prod-01:1974']
        labels:
          server_type: 'database'
      - targets: ['web-prod-01:1974']
        labels:
          server_type: 'web-frontend'
```

### ResMan (db-prod-01)

```bash
# /etc/resman/resman.conf
SERVER_ROLE=database
CPU_THRESHOLD=75
CPU_RELEASE_THRESHOLD=40
ENABLE_PROMETHEUS=true
# NON-DEFAULT REMOTE BIND: requires TLS, authentication, and firewall restrictions.
PROMETHEUS_METRICS_BIND_HOST=0.0.0.0
PROMETHEUS_METRICS_BIND_PORT=1974
```

### ResMan (web-prod-01)

```bash
# /etc/resman/resman.conf
SERVER_ROLE=web-frontend
CPU_THRESHOLD=80
CPU_RELEASE_THRESHOLD=45
ENABLE_PROMETHEUS=true
# NON-DEFAULT REMOTE BIND: requires TLS, authentication, and firewall restrictions.
PROMETHEUS_METRICS_BIND_HOST=0.0.0.0
PROMETHEUS_METRICS_BIND_PORT=1974
```

### Grafana Dashboard

1. Importa `docs/dashboard-grafana-operations.json`
2. Seleziona datasource Prometheus
3. Usa i dropdown per filtrare:
   - Cluster: production
   - Server Role: database
   - Hostname: db-prod-01

---

**Versione Dashboard:** 1.1 (Compatibile con ResMan v1.13.0+)
**Ultimo Aggiornamento:** Marzo 2026
