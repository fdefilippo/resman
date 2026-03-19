# Metrics Database - Guida Completa

## Panoramica

A partire dalla versione 1.16.0, CPU Manager supporta la persistenza delle metriche in un database locale SQLite per esporre lo storico delle metriche via MCP (Model Context Protocol).

### Vantaggi

- **Storico delle metriche**: Accedi a dati storici CPU e RAM per analisi temporali
- **Query flessibili**: Ottieni dati per utente, periodo, o metriche di sistema
- **Integrazione MCP**: 4 nuovi tools per interrogare lo storico
- **Basso impatto**: Scrittura asincrona che non blocca il ciclo di controllo
- **Retention automatica**: Cleanup automatico dei dati vecchi

---

## Configurazione

### Variabili di Configurazione

Aggiungi al file `/etc/cpu-manager.conf`:

```bash
# ========================
# METRICS DATABASE (SQLite)
# ========================

# Abilita la persistenza delle metriche (default: false)
METRICS_DB_ENABLED=false

# Percorso del database SQLite (default: /etc/cpu-manager/metrics.db)
METRICS_DB_PATH=/etc/cpu-manager/metrics.db

# Giorni di retention dei dati (default: 30)
METRICS_DB_RETENTION_DAYS=30

# Intervallo di scrittura in secondi (default: 30)
METRICS_DB_WRITE_INTERVAL=30
```

### Esempi di Configurazione

#### Configurazione Base (Consigliata)
```bash
METRICS_DB_ENABLED=true
METRICS_DB_PATH=/etc/cpu-manager/metrics.db
METRICS_DB_RETENTION_DAYS=30
METRICS_DB_WRITE_INTERVAL=30
```

#### Database in RAM (Per Testing)
```bash
METRICS_DB_ENABLED=true
METRICS_DB_PATH=:memory:
```
**Nota**: I dati vengono persi al riavvio del servizio.

#### Retention Estesa (90 giorni)
```bash
METRICS_DB_ENABLED=true
METRICS_DB_PATH=/etc/cpu-manager/metrics.db
METRICS_DB_RETENTION_DAYS=90
METRICS_DB_WRITE_INTERVAL=30
```

#### Scrittura Meno Frequente (Ogni 5 minuti)
```bash
METRICS_DB_ENABLED=true
METRICS_DB_WRITE_INTERVAL=300
```

---

## Schema del Database

### Tabella `user_metrics`

Memorizza le metriche per ogni utente.

```sql
CREATE TABLE user_metrics (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    uid INTEGER NOT NULL,
    username TEXT NOT NULL,
    cpu_usage_percent REAL NOT NULL,
    memory_usage_bytes INTEGER NOT NULL,
    process_count INTEGER NOT NULL,
    cgroup_path TEXT,
    cpu_quota TEXT,
    is_limited BOOLEAN DEFAULT FALSE
);
```

**Indici:**
- `idx_user_metrics_timestamp`: Per query temporali
- `idx_user_metrics_uid`: Per query per utente
- `idx_user_metrics_uid_timestamp`: Per query combinate

### Tabella `system_metrics`

Memorizza le metriche di sistema.

```sql
CREATE TABLE system_metrics (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    total_cpu_usage_percent REAL NOT NULL,
    total_cores INTEGER NOT NULL,
    system_load REAL,
    limits_active BOOLEAN DEFAULT FALSE,
    limited_users_count INTEGER
);
```

**Indici:**
- `idx_system_metrics_timestamp`: Per query temporali

---

## MCP Tools

La nuova funzionalità espone 4 tools MCP per interrogare lo storico:

### 1. `get_user_history`

Ottiene lo storico delle metriche per un utente specifico.

**Parametri:**
- `uid` (int, opzionale): UID dell'utente
- `username` (string, opzionale): Nome utente
- `startTime` (string, opzionale): Inizio periodo (ISO 8601)
- `endTime` (string, opzionale): Fine periodo (ISO 8601)
- `period` (string, opzionale): Periodo predefinito (`today`, `yesterday`, `last_24_hours`, `last_7_days`, `last_30_days`)
- `hours` (int, opzionale): Ultime N ore
- `limit` (int, opzionale): Numero massimo di record (default: 100)

**Esempi di utilizzo:**

```json
// Ultime 24 ore per UID
{
  "tool": "get_user_history",
  "arguments": {"uid": 1000}
}
```

```json
// Ieri per username
{
  "tool": "get_user_history",
  "arguments": {"username": "francesco", "period": "yesterday"}
}
```

```json
// Ultime 6 ore con limite
{
  "tool": "get_user_history",
  "arguments": {"username": "www-data", "hours": 6, "limit": 50}
}
```

```json
// Intervallo personalizzato
{
  "tool": "get_user_history",
  "arguments": {
    "uid": 1000,
    "startTime": "2026-03-18T08:00:00Z",
    "endTime": "2026-03-18T18:00:00Z"
  }
}
```

**Risposta:**
```json
{
  "records": [
    {
      "timestamp": "2026-03-19T06:00:00Z",
      "uid": 1000,
      "username": "francesco",
      "cpu_usage": 45.2,
      "memory_usage": 524288000,
      "process_count": 15,
      "is_limited": false
    }
  ],
  "count": 1,
  "start_time": "2026-03-19T00:00:00Z",
  "end_time": "2026-03-19T06:30:00Z"
}
```

---

### 2. `get_system_history`

Ottiene lo storico delle metriche di sistema.

**Parametri:**
- `startTime` (string, opzionale): Inizio periodo (ISO 8601)
- `endTime` (string, opzionale): Fine periodo (ISO 8601)
- `period` (string, opzionale): Periodo predefinito
- `hours` (int, opzionale): Ultime N ore
- `limit` (int, opzionale): Numero massimo di record (default: 100)

**Esempi:**

```json
// Ultime 24 ore
{
  "tool": "get_system_history",
  "arguments": {}
}
```

```json
// Ultima settimana
{
  "tool": "get_system_history",
  "arguments": {"period": "last_7_days"}
}
```

```json
// Intervallo personalizzato
{
  "tool": "get_system_history",
  "arguments": {
    "startTime": "2026-03-01T00:00:00Z",
    "endTime": "2026-03-31T23:59:59Z"
  }
}
```

**Risposta:**
```json
{
  "records": [
    {
      "timestamp": "2026-03-19T06:00:00Z",
      "total_cpu_usage": 75.2,
      "total_cores": 4,
      "system_load": 2.5,
      "limits_active": true,
      "limited_users": 3
    }
  ],
  "count": 1,
  "start_time": "2026-03-19T00:00:00Z",
  "end_time": "2026-03-19T06:30:00Z"
}
```

---

### 3. `get_user_summary`

Ottiene statistiche aggregate (media, min, max) per un utente.

**Parametri:**
- `uid` (int, opzionale): UID dell'utente
- `username` (string, opzionale): Nome utente
- `startTime` (string, opzionale): Inizio periodo (ISO 8601)
- `endTime` (string, opzionale): Fine periodo (ISO 8601)
- `period` (string, opzionale): Periodo predefinito

**Esempi:**

```json
// Summary ultime 24 ore
{
  "tool": "get_user_summary",
  "arguments": {"uid": 1000}
}
```

```json
// Summary mese scorso
{
  "tool": "get_user_summary",
  "arguments": {"username": "mysql", "period": "last_30_days"}
}
```

**Risposta:**
```json
{
  "uid": 1000,
  "username": "francesco",
  "period_start": "2026-03-18T00:00:00Z",
  "period_end": "2026-03-19T00:00:00Z",
  "cpu_avg": 42.5,
  "cpu_min": 5.2,
  "cpu_max": 95.3,
  "memory_avg": 536870912,
  "memory_min": 268435456,
  "memory_max": 1073741824,
  "process_count_avg": 12.5,
  "limited_time_percent": 15.5,
  "samples": 2880
}
```

---

### 4. `get_database_info`

Ottiene informazioni sul database.

**Parametri:** Nessuno

**Esempio:**
```json
{
  "tool": "get_database_info",
  "arguments": {}
}
```

**Risposta:**
```json
{
  "path": "/etc/cpu-manager/metrics.db",
  "size_mb": 10.5,
  "user_metrics_count": 86400,
  "system_metrics_count": 2880,
  "oldest_record": "2026-02-17T00:00:00Z",
  "newest_record": "2026-03-19T06:30:00Z",
  "retention_days": 30,
  "users_tracked": 15
}
```

---

## Query SQL Dirette

Puoi interrogare direttamente il database SQLite:

### Esempi di Query

#### Ultimi 10 record per un utente
```sql
SELECT datetime(timestamp, 'localtime') as time, 
       cpu_usage_percent, memory_usage_bytes, process_count
FROM user_metrics 
WHERE uid = 1000 
ORDER BY timestamp DESC 
LIMIT 10;
```

#### Media CPU nelle ultime 24 ore
```sql
SELECT AVG(cpu_usage_percent) as avg_cpu,
       MAX(cpu_usage_percent) as max_cpu,
       MIN(cpu_usage_percent) as min_cpu
FROM user_metrics 
WHERE timestamp >= datetime('now', '-24 hours');
```

#### Utenti più attivi per CPU usage
```sql
SELECT username, 
       AVG(cpu_usage_percent) as avg_cpu,
       COUNT(*) as samples
FROM user_metrics 
WHERE timestamp >= datetime('now', '-24 hours')
GROUP BY uid, username
ORDER BY avg_cpu DESC
LIMIT 10;
```

#### Quando sono stati attivi i limiti
```sql
SELECT datetime(timestamp, 'localtime') as time,
       limited_users_count
FROM system_metrics
WHERE limits_active = 1
  AND timestamp >= datetime('now', '-24 hours')
ORDER BY timestamp DESC;
```

#### Cleanup manuale dati vecchi
```sql
DELETE FROM user_metrics WHERE timestamp < datetime('now', '-90 days');
DELETE FROM system_metrics WHERE timestamp < datetime('now', '-90 days');
VACUUM;
```

---

## Performance e Best Practices

### Impatto sulle Performance

- **Scrittura asincrona**: Le metriche vengono scritte in modo non bloccante
- **Intervallo configurabile**: Default 30 secondi, regolabile in base alle esigenze
- **Dimensione database**: ~10-20 MB per 30 giorni con 10 utenti attivi

### Best Practices

1. **Retention adeguata**: Imposta `METRICS_DB_RETENTION_DAYS` in base allo spazio disponibile
2. **Intervallo di scrittura**: Non scendere sotto i 5 secondi per evitare sovraccarico
3. **Monitoraggio dimensione**: Controlla periodicamente `get_database_info`
4. **Backup**: Fai backup regolari del file `/etc/cpu-manager/metrics.db`

### Troubleshooting

#### Il database non viene creato
- Verifica che `METRICS_DB_ENABLED=true`
- Controlla i permessi sulla directory `/etc/cpu-manager/`
- Verifica i log: `tail -f /var/log/cpu-manager.log | grep -i database`

#### Scrittura troppo lenta
- Aumenta `METRICS_DB_WRITE_INTERVAL`
- Riduci `METRICS_DB_RETENTION_DAYS`
- Esegui `VACUUM` periodicamente

#### Database troppo grande
- Riduci `METRICS_DB_RETENTION_DAYS`
- Esegui cleanup manuale: `DELETE FROM ... WHERE timestamp < ...`
- Abilita solo per utenti specifici (future enhancement)

---

## Integrazione con Grafana

Puoi usare il database SQLite come fonte dati per Grafana:

1. Installa il plugin SQLite per Grafana
2. Configura il datasource puntando a `/etc/cpu-manager/metrics.db`
3. Crea dashboard con le query SQL sopra

---

## Note Tecniche

### Formato Temporale

I timestamp sono memorizzati in formato ISO 8601 (`YYYY-MM-DDTHH:MM:SSZ`) e SQLite li gestisce nativamente.

### Supporto Timezone

Le query SQL usano `datetime('now', 'localtime')` per convertire in timezone locale.

### Cleanup Automatico

Il cleanup dei dati vecchi viene eseguito:
- All'avvio del servizio
- Periodicamente durante l'esecuzione (ogni ciclo di scrittura)

---

## Changelog

### Versione 1.16.0
- **Aggiunto**: Supporto database SQLite per metriche storiche
- **Aggiunto**: 4 nuovi MCP tools (`get_user_history`, `get_system_history`, `get_user_summary`, `get_database_info`)
- **Aggiunto**: Configurazione `METRICS_DB_*` per controllo persistenza
- **Aggiunto**: Cleanup automatico dati vecchi
