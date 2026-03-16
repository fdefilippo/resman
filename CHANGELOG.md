# Changelog

Tutti i cambiamenti significativi a questo progetto sono documentati in questo file.

Il formato è basato su [Keep a Changelog](https://keepachangelog.com/it/1.0.0/),
e questo progetto aderisce al [Semantic Versioning](https://semver.org/lang/it/).

## [1.13.1] - 2026-03-13

### Corretto

#### LDAP/NIS Username Resolution
- **Fix**: `getUsername()` ora usa `os/user.LookupId()` per risolvere gli UID
- Supporto LDAP/NIS quando compilato con `CGO_ENABLED=1`
- Fallback automatico su `/etc/passwd` se LDAP non disponibile
- Fallback finale a UID numerico se username non trovato

**Nota Importante:**
Per il supporto LDAP/NIS, compilare **obbligatoriamente** con:
```bash
CGO_ENABLED=1 go build -o cpu-manager-go .
```

Senza CGO, solo gli utenti locali in `/etc/passwd` sono risolti.

---

## [1.13.0] - 2026-03-13

### Aggiunto

#### Grafana Dashboard Enhancement
- Aggiunte label `hostname` e `server_role` a tutte le metriche Prometheus
- Dashboard aggiornata con variabili: `cluster`, `server_role`, `hostname`
- Selezione multi-cluster tramite label esterna Prometheus `cluster`
- Filtri per server_role e hostname nella dashboard
- Legenda aggiornata per mostrare hostname nei grafici

**Metriche con label:**
- `cpu_manager_cpu_total_usage_percent{hostname, server_role}`
- `cpu_manager_cpu_user_usage_percent{hostname, server_role}`
- `cpu_manager_user_cpu_usage_percent{uid, username, hostname, server_role}`
- `cpu_manager_user_memory_usage_bytes{uid, username, hostname, server_role}`
- Tutte le altre metriche includono hostname e server_role

**Dashboard Variables:**
- `cluster`: label_values(cpu_manager_cpu_total_usage_percent, cluster)
- `server_role`: label_values(cpu_manager_cpu_total_usage_percent{cluster=~"$cluster"}, server_role)
- `hostname`: label_values(cpu_manager_cpu_total_usage_percent{cluster=~"$cluster", server_role=~"$server_role"}, hostname)

**Configurazione Prometheus richiesta:**
```yaml
# prometheus.yml
global:
  external_labels:
    cluster: 'production'  # O il nome del tuo cluster
```

---

## [1.12.0] - 2026-03-13

### Aggiunto

#### Blackout Timeframes
- Nuova variabile `CPU_MANAGER_BLACKOUT` per specificare quando NON applicare limiti CPU
- Formato crontab-like: "giorni ore" (es: "1-5 08-18" per Lun-Ven, 8-18)
- Supporto multipli timeframe separati da punto e virgola
- Timezone di sistema automaticamente rilevata
- Logging ibrido: INFO per entrata/uscita blackout, DEBUG per skip cicli

**Formato:**
- Giorni: 0=Domenica, 1-6=Lun-Sab, * (tutti), 1-5 (lun-ven), 0,6 (weekend)
- Ore: formato 24h (00-23)
- Esempi:
  - `1-5 08-18` - Disabilita orario lavorativo
  - `0,6 00-23` - Disabilita weekend
  - `1-5 08-18;0,6 00-23` - Disabilita orario lavorativo + weekend

**Precedenza:**
- Blackout prevale su USER_INCLUDE_LIST e USER_EXCLUDE_LIST
- Durante blackout, CPU Manager non applica MAI limiti

**Logging:**
```
[INFO] Entering blackout timeframe - CPU limits suspended until 18:00
[DEBUG] Skipping control cycle - blackout timeframe active
[INFO] Exiting blackout timeframe - CPU limits re-enabled
```

---

## [1.11.0] - 2026-03-13

### Aggiunto

#### MCP User Filter Management
Nuovi tool MCP per gestire dinamicamente USER_INCLUDE_LIST e USER_EXCLUDE_LIST:

**Tool: `set_user_exclude_list`**
- Imposta la lista di utenti da escludere dai limiti CPU
- Input: `patterns` (array di regex), `reload` (boolean, default=true)
- Output: `success`, `previous_value`, `new_value`, `reload_triggered`
- Crea backup automatico con timestamp
- Triggera reload automatico della configurazione

**Tool: `set_user_include_list`**
- Imposta la lista di pattern per includere utenti nel monitoraggio
- Input: `patterns` (array di regex), `reload` (boolean, default=true)
- Output: `success`, `previous_value`, `new_value`, `reload_triggered`
- Crea backup automatico con timestamp
- Triggera reload automatico della configurazione

**Tool: `get_user_filters`**
- Ottiene le configurazioni correnti di include/exclude list
- Output: `user_include_list`, `user_exclude_list`, `config_file`

**Tool: `validate_user_filter_pattern`**
- Valida se un pattern regex è valido
- Input: `pattern` (string), `type` ("include" o "exclude")
- Output: `valid`, `pattern`, `type`, `test_matches`, `match_count`
- Testa il pattern contro utenti di esempio

#### Sicurezza
- Tutti i tool di scrittura richiedono `MCP_ALLOW_WRITE_OPS=true`
- Backup automatico prima di ogni modifica (formato: `cpu-manager.conf.backup_YYYYMMDD_HHMMSS`)
- Salvataggio atomico (write temp file + rename)
- Rollback automatico in caso di errore

### Modificato

#### Pacchetto config
- `config.go`: Aggiunti metodi `SetUserExcludeList()`, `SetUserIncludeList()`, `SaveToFile()`
- `config.go`: Implementato backup automatico con timestamp
- `config.go`: Implementato salvataggio atomico della configurazione

#### Pacchetto mcp
- `tools.go`: Implementati 4 nuovi tool per user filter management
- `tools.go`: Aggiunto import per `regexp`

### Esempi di Utilizzo

```bash
# Impostare exclude list
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc":"2.0",
    "method":"tools/call",
    "params":{
      "name":"set_user_exclude_list",
      "arguments":{"patterns":["^test-.*","^dev-.*"],"reload":true}
    },
    "id":1
  }'

# Ottenere filtri correnti
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc":"2.0",
    "method":"tools/call",
    "params":{
      "name":"get_user_filters",
      "arguments":{}
    },
    "id":2
  }'

# Validare pattern
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc":"2.0",
    "method":"tools/call",
    "params":{
      "name":"validate_user_filter_pattern",
      "arguments":{"pattern":"^www.*","type":"exclude"}
    },
    "id":3
  }'
```

---

## [1.10.1] - 2026-03-13

### Corretto

#### Config Watcher Fix
- **Aggiunto controllo periodico** della configurazione (ogni 30 secondi)
- Risolto problema: modifiche al file non rilevate da fsnotify
- Alcuni editor di testo non triggerano correttamente gli eventi fsnotify
- Il controllo periodico garantisce che le modifiche siano sempre rilevate

**Log Migliorato:**
```
[INFO] Config change detected via periodic check, reloading
[INFO] Configuration reloaded successfully
[INFO] Metrics collector configuration updated exclude_list=[francesco,nobody,zabbix,mysql]
```

---

## [1.10.0] - 2026-03-12

### Aggiunto

#### Supporto Regex per USER_EXCLUDE_LIST
- **USER_EXCLUDE_LIST** ora supporta pattern regex (come USER_INCLUDE_LIST)
- Pattern multipli separati da virgola
- Validazione pattern all'avvio (errore se regex invalida)
- Backward compatibility: nomi utente esatti funzionano ancora

**Esempi di Utilizzo:**
```bash
# Escludi utenti specifici (backward compatible)
USER_EXCLUDE_LIST=francesco,www-data,mysql

# Pattern regex per escludere utenti web
USER_EXCLUDE_LIST=^www-.*,^test-.*,^dev-.*

# Pattern per escludere utenti servizio
USER_EXCLUDE_LIST=^svc-.*,^app-.*

# Combinazione di pattern
USER_EXCLUDE_LIST=^test-.*,^dev-.*,francesco
```

### Cambiato

#### Documentazione Aggiornata
- `config/cpu-manager.conf.example`: Documentato supporto regex per USER_EXCLUDE_LIST
- Esempi aggiornati con pattern regex

### Comportamento

| Configurazione | Risultato |
|---------------|-----------|
| `USER_EXCLUDE_LIST=` (vuoto) | NESSUN utente è escluso |
| `USER_EXCLUDE_LIST=francesco` | francesco è escluso (match esatto) |
| `USER_EXCLUDE_LIST=^www-.*` | Tutti gli utenti che iniziano con "www-" sono esclusi |
| `USER_EXCLUDE_LIST=^test-.*,^dev-.*` | Utenti che iniziano con "test-" O "dev-" sono esclusi |

---

## [1.9.0] - 2026-03-12

### Aggiunto

#### USER_INCLUDE_LIST con Supporto Regex
- Nuova variabile `USER_INCLUDE_LIST` per filtrare utenti tramite pattern regex
- Supporto espressioni regolari complete (sintassi Go `regexp`)
- Pattern multipli separati da virgola
- Validazione pattern all'avvio (errore se regex invalida)

**Esempi di Utilizzo:**
```bash
# Solo utenti specifici
USER_INCLUDE_LIST=francesco,www-data,mysql

# Pattern regex per utenti web
USER_INCLUDE_LIST=^www.*,^apache.*,^nginx.*

# Pattern per utenti servizio
USER_INCLUDE_LIST=^svc-.*,^app-.*

# Combinazione di pattern
USER_INCLUDE_LIST=^web.*,^app.*,francesco
```

#### Log Migliorato
- `GetActiveUsers()` ora logga include_list e exclude_list
- Debug più semplice del filtraggio utenti

### Comportamento

| Configurazione | Risultato |
|---------------|-----------|
| `USER_INCLUDE_LIST=` (vuoto) | TUTTI gli utenti sono inclusi |
| `USER_INCLUDE_LIST=francesco` | Solo francesco è incluso |
| `USER_INCLUDE_LIST=^www.*` | Solo utenti che iniziano con "www" |
| `USER_INCLUDE_LIST=^svc-.*,^app-.*` | Utenti che iniziano con "svc-" O "app-" |

### Precedenza

Se entrambe le liste sono specificate:
1. **USER_INCLUDE_LIST** filtra gli utenti inclusi (whitelist)
2. **USER_EXCLUDE_LIST** rimuove utenti dall'insieme (blacklist)

Esempio:
```bash
USER_INCLUDE_LIST=^www.*     # Include tutti gli utenti www-*
USER_EXCLUDE_LIST=www-test   # Ma esclude www-test
# Risultato: www-prod, www-dev inclusi, www-test escluso
```

---

## [1.8.0] - 2026-03-12

### Cambiato

#### USER_EXCLUDE_LIST (Breaking Change)
- **Rinominato**: `USER_WHITELIST` → `USER_EXCLUDE_LIST`
- **Comportamento invertito**: La lista ora ESCLUDE gli utenti dai limiti
- **Retrocompatibilità**: `USER_WHITELIST` funziona ancora ma è deprecato

### Comportamento

```bash
# Vecchio comportamento (USER_WHITELIST):
USER_WHITELIST=francesco  # → SOLO francesco limitato

# Nuovo comportamento (USER_EXCLUDE_LIST):
USER_EXCLUDE_LIST=francesco  # → francesco NON viene limitato
```

### Aggiunto

#### Documentazione Migliorata
- File di esempio aggiornato con `USER_EXCLUDE_LIST`
- Commenti chiari sul comportamento
- Esempi di utilizzo pratici

### Esempio di Utilizzo

```bash
# /etc/cpu-manager.conf

# Escludi francesco dai limiti (non verrà mai limitato)
USER_EXCLUDE_LIST=francesco

# Escludi multipli utenti
USER_EXCLUDE_LIST=francesco,www-data,mysql

# Nessun utente escluso (tutti possono essere limitati)
# USER_EXCLUDE_LIST=
```

---

## [1.7.0] - 2026-03-12

### Aggiunto

#### Process Exclusion (Blacklist Automatica)
- Nuova funzione `IsProcessExcluded()` in `config/config.go`
- Lista di processi di sistema automaticamente esclusi dai limiti CPU:
  - **System**: systemd, dbus-daemon, polkitd, udisks2d
  - **Network**: NetworkManager, wpa_supplicant, sshd
  - **System Services**: cron, rsyslogd, auditd, firewalld
  - **Container**: dockerd, containerd, kubelet, lxcfs
  - **Web Server**: nginx, apache2, httpd, php-fpm
  - **Database**: mysqld, mariadbd, postgres, mongod, redis-server
  - **Mail**: postfix, master
  - **Monitoring**: zabbix_agentd, prometheus, node_exporter, telegraf, grafana-server
  - **Virtualizzazione**: qemu-system, libvirtd, vmtoolsd, VBoxService
  - **Desktop**: gdm, gnome-shell, lightdm, sddm
  - **Altro**: cupsd, avahi-daemon, bluetoothd, chronyd, smartd
- I processi esclusi non vengono conteggiati nel calcolo della CPU usage
- Gli utenti con solo processi esclusi non vengono limitati

### Corretto

#### Whitelist Fix
- **Risolto**: `USER_WHITELIST=` vuoto ora include correttamente tutti gli utenti
- **Risolto**: Whitelist assente o commentata ora include tutti gli utenti
- Documentato comportamento nel file di esempio
- La whitelist ora funziona come previsto:
  - `USER_WHITELIST=` (vuoto) → TUTTI gli utenti
  - `# USER_WHITELIST=` (commentato) → TUTTI gli utenti
  - `USER_WHITELIST=alice,bob` → Solo alice e bob

### Modificato

#### Metrics Collector
- `GetActiveUsers()`: Esclude utenti con solo processi nella blacklist
- `GetUserCPUUsage()`: Esclude processi nella blacklist dal calcolo CPU
- Logging migliorato per debug whitelist e process exclusion

#### Configurazione
- `config/cpu-manager.conf.example`: Documentata whitelist e process exclusion
- `config/config.go`: Aggiunta funzione `IsProcessExcluded()`

### Esempio di Utilizzo

```bash
# /etc/cpu-manager.conf

# Whitelist vuota = tutti gli utenti (systemd, dbus-daemon etc. sono comunque esclusi)
USER_WHITELIST=

# Oppure whitelist specifica (processi di sistema comunque esclusi)
USER_WHITELIST=francesco,www-data

# I seguenti processi NON saranno mai limitati, anche se di utenti nella whitelist:
# - systemd (UID 0 o 1000)
# - dbus-daemon (UID 1000)
# - NetworkManager (UID 0)
# - sshd (UID 0)
# - etc.
```

---

## [1.6.0] - 2026-03-12

### Aggiunto

#### User Whitelist
- Nuova variabile di configurazione `USER_WHITELIST` per filtrare utenti monitorati
- Lista di username separati da virgola (es: `francesco,www-data,mysql`)
- Se vuota o non specificata: tutti gli utenti non-system (comportamento default)
- Se specificata: solo gli utenti nella whitelist sono monitorati e limitati
- Metodo `IsUserWhitelisted()` in config per verificare appartenenza
- Filtraggio applicato in:
  - `GetActiveUsers()` - solo utenti whitelisted
  - `GetAllUserMetrics()` - solo metriche utenti whitelisted

#### CGO Requirement
- **CGO ora è richiesto** per la compilazione
- Necessario per user name resolution tramite NSS (Name Service Switch)
- Supporto completo per backend di autenticazione:
  - Local users (`/etc/passwd`)
  - LDAP/Active Directory
  - NIS
  - SSSD (System Security Services Daemon)
- Documentati requisiti di build nel README.md
- Aggiornato Makefile per abilitare esplicitamente CGO

### Modificato

#### Configurazione
- `config/config.go`: Aggiunto campo `UserWhitelist []string`
- `config/config.go`: Implementato parsing lista username da stringa CSV
- `config/config.go`: Aggiunto metodo `IsUserWhitelisted()` per verifica
- `config/config.go`: **Fix parsing commenti inline** - Ora gestisce correttamente commenti dopo i valori
- `config/cpu-manager.conf.example`: Aggiunta sezione USER_WHITELIST con esempi

#### Metrics Collector
- `metrics/collector.go`: `GetActiveUsers()` filtra per whitelist
- `metrics/collector.go`: `GetAllUserMetrics()` filtra per whitelist
- `metrics/collector.go`: **Rimosso fallback `os/user.LookupId()`** - Usa solo `/etc/passwd` con fallback a UID
- `metrics/collector.go`: Implementato `getUsernameFromPasswd()` per lookup senza CGO

#### Build System
- `Makefile`: Aggiunto `CGO_ENABLED=1` esplicito
- `Makefile`: Aggiunti `CGO_CFLAGS` e `CGO_LDFLAGS`
- `packaging/rpm/cpu-manager-go.spec`: Documentato requisito CGO
- `README.md`: Aggiunta sezione "Build Requirements" con dettagli CGO

### Fix

#### Bug Fix
- Risolto problema parsing configurazione con commenti inline
- Risolto problema "Prometheus exporter disabled" con commenti nel file di config
- **Fix cleanup cgroup**: Ora rimuove correttamente il cgroup condiviso "limited" durante lo shutdown
- **Fix graceful shutdown**: Gli utenti vengono correttamente rimossi dai cgroup quando CPU Manager viene fermato

### Comportamento

| Configurazione | Comportamento |
|---------------|---------------|
| `USER_WHITELIST=` (vuoto) | Tutti gli utenti non-system |
| `USER_WHITELIST=francesco` | Solo utente "francesco" |
| `USER_WHITELIST=alice,bob` | Solo "alice" e "bob" |
| Non specificato | Tutti gli utenti non-system |

### Note di Migrazione

**CGO è ora richiesto:**
- Assicurarsi di avere GCC installato (`yum install gcc` o `apt install gcc`)
- Build RPM/DEB gestiscono automaticamente CGO
- User name resolution ora usa NSS (supporta LDAP, NIS, SSSD)

### Esempio di Utilizzo

```bash
# /etc/cpu-manager.conf

# Monitora e limita solo utenti specifici
USER_WHITELIST=francesco,www-data,mysql

# Oppure lascia vuoto per comportamento default (tutti gli utenti)
# USER_WHITELIST=

# Commenti inline ora funzionano correttamente
ENABLE_PROMETHEUS=true  # Abilita Prometheus
PROMETHEUS_METRICS_BIND_PORT=1974  # Porta default
```

---

## [1.5.0] - 2026-03-11

### Cambiato

#### Prometheus: Rinominati parametri di configurazione
- **`PROMETHEUS_HOST`** → **`PROMETHEUS_METRICS_BIND_HOST`**
- **`PROMETHEUS_PORT`** → **`PROMETHEUS_METRICS_BIND_PORT`**
- Nuovo default host: **`0.0.0.0`** (tutte le interfacce)
- Nuovo default porta: **`1974`**
- Parametri ora commentati di default nel file di esempio
- Mantenuta **retrocompatibilità** con vecchi nomi (`PROMETHEUS_HOST`, `PROMETHEUS_PORT`)

#### MCP: Allineati parametri di configurazione
- **`MCP_HTTP_HOST`** default: **`0.0.0.0`** (tutte le interfacce)
- **`MCP_HTTP_PORT`** default: **`1969`**
- Parametri ora commentati di default nel file di esempio
- Allineato con logica di configurazione Prometheus

### Motivazione

I nuovi nomi e default riflettono correttamente il comportamento:
- Non è l'host/porta di Prometheus o MCP client
- È l'indirizzo su cui CPU Manager **espone** i servizi
- Default `0.0.0.0` permette connessioni remote
- Porte dedicate: `1974` per Prometheus, `1969` per MCP

### Esempio di Configurazione

```bash
# /etc/cpu-manager.conf

# Prometheus metrics (commentato = usa default)
ENABLE_PROMETHEUS=true
# PROMETHEUS_METRICS_BIND_HOST=0.0.0.0  # Default: tutte le interfacce
# PROMETHEUS_METRICS_BIND_PORT=1974     # Default: 1974

# MCP server (commentato = usa default)
MCP_ENABLED=true
MCP_TRANSPORT=http
# MCP_HTTP_HOST=0.0.0.0  # Default: tutte le interfacce
# MCP_HTTP_PORT=1969     # Default: 1969
```

### Endpoint Servizi

```
# Prometheus metrics
http://<server-ip>:1974/metrics

# MCP endpoint
http://<server-ip>:1969/mcp
```

### Configurazione Prometheus

```yaml
# prometheus.yml
scrape_configs:
  - job_name: 'cpu-manager'
    static_configs:
      - targets: ['192.168.1.2:1974']  # IP e porta di CPU Manager
```

### Retrocompatibilità

I vecchi nomi `PROMETHEUS_HOST` e `PROMETHEUS_PORT` continuano a funzionare per non rompere configurazioni esistenti.

---

## [1.4.0] - 2026-03-11

### Aggiunto

#### Server Role Configuration
- Nuova variabile di configurazione `SERVER_ROLE` per identificare il tipo di server
- Valori tipici: `database`, `web-frontend`, `batch`, `application`, `cache`, `api-gateway`
- Campo opzionale, vuoto di default
- Utilizzato per identificazione in ambienti multi-server

#### Server Role nei Tool MCP
- Aggiunto campo `server_role` in tutti i tool MCP:
  - `get_system_status`
  - `get_active_users`
  - `get_limits_status`
  - `get_configuration`
  - `get_cpu_report` (incluso nel testo del report)
  - `get_mem_report` (incluso nel testo del report)
- Permette di identificare il ruolo del server nei report e nelle metriche

### Modificato

#### Configurazione
- `config/config.go`: Aggiunto campo `ServerRole` alla struct Config
- `config/config.go`: Aggiunta gestione `SERVER_ROLE` in `setConfigField`
- `config/cpu-manager.conf.example`: Aggiunta sezione SERVER_ROLE con esempi

#### MCP Tools
- `mcp/tools.go`: Tutti i tool che restituiscono metriche ora includono `server_role`
- `mcp/tools.go`: Report CPU e Memoria includono il server role nel testo formattato

#### Documentazione
- `docs/MCP-README.md`: Documentato campo `server_role` negli output
- `docs/cpu-manager.8`: Aggiunta configurazione SERVER_ROLE nel manuale
- `docs/MCP-BLUEPRINT.md`: Aggiornato con nuova funzionalità

### Esempio di Configurazione

```bash
# /etc/cpu-manager.conf
SERVER_ROLE=database
```

### Esempio di Output MCP

```json
{
  "hostname": "db-prod-01",
  "server_role": "database",
  "total_cpu_usage": 45.5,
  ...
}
```

**Report CPU con Server Role:**
```
Report Utilizzo CPU
Hostname: db-prod-01
Server Role: database
Data: 2026-03-11 19:00:00
...
```

---

## [1.3.0] - 2026-03-11

### Aggiunto

#### Nuovi Tool MCP
- **get_cpu_report**: Genera report dettagliato sull'utilizzo CPU con hostname, data, utenti attivi e stato limiti
- **get_mem_report**: Genera report dettagliato sull'utilizzo memoria con hostname, data, utenti attivi e consumo per utente

#### Hostname in Output MCP
- Aggiunto campo `hostname` in tutti i tool che restituiscono metriche:
  - `get_system_status`
  - `get_active_users`
  - `get_limits_status`
  - `get_configuration`
  - `get_cpu_report`
  - `get_mem_report`
- Utile per ambienti multi-server per identificare la sorgente dei dati

#### Logging MCP
- Implementato middleware HTTP per logging di tutte le richieste MCP
- Log delle richieste in arrivo con metodo, path, remote address
- Log delle risposte con status code e durata
- Log visibili in `/var/log/cpu-manager.log` quando `LOG_LEVEL=DEBUG` o `INFO`

#### Fix Logger
- Risolto problema di inizializzazione logger che bloccava il livello log su INFO
- Logger ora usa correttamente `LOG_LEVEL` dalla configurazione
- Supporto completo per `LOG_LEVEL=DEBUG` per troubleshooting dettagliato

#### Documentazione
- Aggiornato `docs/MCP-README.md` con esempi di report CPU e memoria
- Aggiunti esempi di output con hostname
- Documentati tutti i 11 tool MCP disponibili

### Modificato

#### Pacchetto MCP
- `mcp/tools.go`: Aggiunta funzione `getHostname()` per recuperare hostname di sistema
- `mcp/tools.go`: Aggiunta funzione `joinStrings()` per formattazione report
- `mcp/server.go`: Implementato logging middleware per richieste HTTP
- `mcp/server.go`: Migliorato logging con dettagli aggiuntivi (content-length, duration)

#### Main
- `main.go`: Rimossa doppia inizializzazione logger
- `main.go`: Logger inizializzato una sola volta con configurazione da file

### Corretto

#### Bug Fix
- Risolto problema per cui i log MCP non venivano scritti su file
- Risolto errore "400 Invalid schema" per tool senza parametri
- Tutti i tool con input vuoto ora hanno schema JSON esplicito valido

### Sicurezza

- Nessun cambiamento significativo

### Note di Migrazione

Questa versione è **retrocompatibile**:

- I nuovi campi `hostname` sono aggiuntivi, non rompono client esistenti
- I nuovi tool sono opzionali
- Logging abilitato di default con livello log dalla configurazione

### Esempio di Utilizzo Report

```bash
# Tramite AnythingLLM o client MCP:
"Genera un report CPU"
"Genera un report memoria"

# Output include sempre hostname:
{
  "hostname": "server-web01",
  "report": "Report Utilizzo CPU\nHostname: server-web01\n...",
  "total_cpu": 45.2,
  ...
}
```

---

## [1.2.0] - 2026-03-11

### Aggiunto

#### MCP Server (Model Context Protocol)
- Implementato server MCP per integrazione con assistenti AI
- **9 strumenti MCP**:
  - `get_system_status` - Stato CPU e memoria di sistema
  - `get_user_metrics` - Metriche per utente (CPU, memoria, processi)
  - `get_active_users` - Lista utenti attivi non-sistema
  - `get_limits_status` - Stato limiti CPU attivi
  - `get_cgroup_info` - Informazioni cgroup per utente
  - `get_configuration` - Configurazione corrente
  - `get_control_history` - Storico cicli di controllo
  - `activate_limits` - Attivazione manuale limiti CPU (opzionale)
  - `deactivate_limits` - Disattivazione manuale limiti CPU (opzionale)
- **6 risorse MCP**:
  - `cpu-manager://system/status` - Stato sistema in tempo reale
  - `cpu-manager://users/active` - Utenti attivi
  - `cpu-manager://limits/status` - Stato limiti
  - `cpu-manager://config` - Configurazione
  - `cpu-manager://users/{uid}/metrics` - Metriche per utente
  - `cpu-manager://cgroups/{uid}` - Informazioni cgroup
- **3 prompt pre-costruiti**:
  - `system-health` - Controllo rapido stato sistema
  - `user-analysis` - Analisi utilizzo risorse per utente
  - `troubleshooting` - Diagnostica problemi limiti CPU
- **Supporto multi-trasporto**: stdio, HTTP, SSE
- Autenticazione token per trasporto HTTP/SSE
- Health check endpoint per monitoraggio

#### Configurazione MCP
- Nuove opzioni in `/etc/cpu-manager.conf`:
  - `MCP_ENABLED` - Abilita server MCP
  - `MCP_TRANSPORT` - Tipo di trasporto (stdio, http, sse)
  - `MCP_HTTP_HOST` - Indirizzo bind per HTTP/SSE
  - `MCP_HTTP_PORT` - Porta per HTTP/SSE
  - `MCP_LOG_LEVEL` - Livello log MCP
  - `MCP_ALLOW_WRITE_OPS` - Abilita operazioni di scrittura
  - `MCP_AUTH_TOKEN` - Token autenticazione (opzionale)

#### State Manager
- Metodo `GetConfig()` per recuperare configurazione corrente
- Metodo `GetControlHistory(limit)` per storico cicli di controllo
- Registrazione automatica cicli di controllo in memoria

#### Documentazione
- `docs/MCP-README.md` - Guida completa all'uso del server MCP
- `docs/MCP-BLUEPRINT.md` - Blueprint architetturale e implementativo
- Aggiornato `README.md` con sezione MCP

#### Test
- Test unitari per server MCP (`mcp/server_test.go`)
- Test per configurazione, helper functions, estrazione UID da URI
- Test per avvio/arresto server

### Modificato

#### Struttura Pacchetto
- Creato nuovo pacchetto `mcp/` con:
  - `server.go` - Implementazione server MCP
  - `tools.go` - Strumenti e handler MCP
  - `resources.go` - Risorse e handler MCP
  - `config.go` - Configurazione MCP
  - `server_test.go` - Test unitari

#### Configurazione
- `config/config.go`: Aggiunti campi configurazione MCP
- `config/cpu-manager.conf.example`: Aggiunta sezione MCP

#### Main
- `main.go`: Integrazione inizializzazione server MCP
- `main.go`: Cleanup server MCP durante shutdown

#### State Manager
- `state/manager.go`: Implementato storico cicli di controllo
- `state/manager.go`: Metodo `recordControlCycle()` per tracciamento

#### Dipendenze
- Aggiunto `github.com/modelcontextprotocol/go-sdk v1.4.0`

### Sicurezza

#### Operazioni di Scrittura
- Operazioni `activate_limits` e `deactivate_limits` disabilitate di default
- Richiedono esplicita abilitazione con `MCP_ALLOW_WRITE_OPS=true`
- Documentati rischi e raccomandazioni di sicurezza

#### Autenticazione
- Supporto token bearer per trasporto HTTP/SSE
- Documentate best practice per esposizione in rete

### Note di Migrazione

Questa versione è **retrocompatibile**:

- Il server MCP è disabilitato di default (`MCP_ENABLED=false`)
- Nessuna modifica richiesta alla configurazione esistente
- Tutte le funzionalità esistenti rimangono invariate

### Requisiti di Sistema

Nessun cambiamento nei requisiti di sistema:

- Linux kernel 4.5+ con cgroups v2
- Accesso in scrittura a `/sys/fs/cgroup`
- Privilegi root o capacità CAP_SYS_ADMIN

### Esempio di Utilizzo MCP

```bash
# Abilita server MCP
echo "MCP_ENABLED=true" >> /etc/cpu-manager.conf
echo "MCP_TRANSPORT=stdio" >> /etc/cpu-manager.conf

# Riavvia CPU Manager
sudo systemctl restart cpu-manager

# Verifica avvio
journalctl -u cpu-manager | grep "MCP server"
```

---

## [1.0.0] - 2026-02-22

### Aggiunto

#### Metriche Prometheus per utente
- Nuova metrica `cpu_manager_user_memory_usage_bytes{uid, username}` - Memoria RAM utilizzata per utente (in bytes)
- Nuova metrica `cpu_manager_user_process_count{uid, username}` - Numero di processi per utente
- Nuova metrica `cpu_manager_user_cpu_limited{uid, username}` - Stato limite CPU per utente
- Nuova metrica `cpu_manager_active_users_count` - Numero totale di utenti non-sistema attivi
- Nuova metrica `cpu_manager_system_load_average` - Load average di sistema (1 minuto)
- Nuova metrica `cpu_manager_memory_usage_megabytes` - Memoria totale di sistema utilizzata

#### Dashboard Grafana
- Aggiunto pannello "Memory Usage Per User" per visualizzare memoria per utente
- Aggiunto pannello "Total User Processes" per totale processi utente
- Aggiunto pannello "Processes Per User" per processi per singolo utente
- Aggiunta variabile templating `username` per filtrare per nome utente
- Riorganizzato layout del dashboard per migliore visualizzazione

#### Documentazione
- Aggiornato manuale `docs/cpu-manager.8` con tutte le nuove metriche
- Aggiunti esempi di query Prometheus per utente
- Aggiornato `docs/dashboard-grafana.json` con nuovi pannelli
- Creato file `CHANGELOG.md` per tracciare i cambiamenti

### Corretto

#### Bug fix
- Corretto errore `fmt.Errorf` in `config/config.go` (riga 372) - aggiunto format string costante
- Risolti problemi di compilazione Makefile per pacchetto Debian
- Rimossi loop bash problematici nel Makefile che causavano errori di processi figli

#### Build e Packaging
- Semplificato target `deb-binary` per build sequenziale invece che parallela
- Semplificato target `deb-prepare` per evitare race condition
- Corretto campo `DEB_MAINTAINER` per evitare warning di dpkg-deb
- Build Debian ora completa con successo per architettura amd64

### Modificato

#### API e Interfacce
- Aggiornata interfaccia `MetricsCollector` con nuovo metodo `GetAllUserMetrics()`
- Aggiornata interfaccia `PrometheusExporter` con metodo `CleanupUserMetrics()`
- Modificato `UpdateUserMetrics()` per accettare memoryUsage e processCount come parametri
- Aggiunta struct `UserMetrics` per raggruppare CPU, memoria e processi per utente

#### Implementazione
- `metrics/collector.go`: Implementato `GetAllUserMetrics()` per raccolta efficiente in una sola scansione /proc
- `metrics/collector.go`: Implementato `GetUserMemoryUsage()` per lettura VmRSS da /proc/[pid]/status
- `metrics/collector.go`: Implementato `GetUserProcessCount()` per conteggio processi per UID
- `state/manager.go`: Aggiornato `collectSystemMetrics()` per usare `GetAllUserMetrics()`
- `state/manager.go`: Aggiornato `updatePrometheusMetrics()` per esporre metriche complete per utente

### Rimosso

- Nessun cambiamento di rottura in questa versione

### Note di migrazione

Questa versione è **retrocompatibile**. Tutte le metriche esistenti sono mantenute:

- Le nuove metriche per utente sono additive e non sostituiscono quelle esistenti
- Il dashboard Grafana è stato aggiornato ma rimane importabile come nuovo dashboard
- La configurazione esistente non richiede modifiche

### Requisiti di sistema

Nessun cambiamento nei requisiti di sistema:

- Linux kernel 4.5+ con cgroups v2
- Accesso in scrittura a `/sys/fs/cgroup`
- Privilegi root o capacità CAP_SYS_ADMIN

---

## [0.9.0] - 2026-01-15

### Aggiunto

- Supporto per cgroups v2 con controller CPU e cpuset
- Export metriche Prometheus di base
- Configurazione dinamica con auto-reload
- Graceful shutdown con cleanup
- Logging strutturato con rotazione file
- Supporto syslog opzionale

### Modificato

- Migliorata gestione errori durante il controllo dei cicli
- Ottimizzata cache delle metriche con TTL configurabile

---

## [0.1.0] - 2025-12-01

### Aggiunto

- Implementazione iniziale del daemon CPU Manager
- Controllo soglie CPU per attivazione/disattivazione limiti
- Integrazione con systemd service
- Documentazione base e man page

---

## Formato delle versioni

Il formato delle versioni è `MAJOR.MINOR.PATCH`:

- **MAJOR**: Cambiamenti incompatibili con le versioni precedenti
- **MINOR**: Nuove funzionalità in modo retrocompatibile
- **PATCH**: Correzioni di bug in modo retrocompatibile

## Link

- [1.11.0]: https://github.com/fdefilippo/cpu-manager-go/compare/v1.10.1...v1.11.0
- [1.10.1]: https://github.com/fdefilippo/cpu-manager-go/compare/v1.10.0...v1.10.1
- [1.10.0]: https://github.com/fdefilippo/cpu-manager-go/compare/v1.9.0...v1.10.0
- [1.9.0]: https://github.com/fdefilippo/cpu-manager-go/compare/v1.8.0...v1.9.0
- [1.8.0]: https://github.com/fdefilippo/cpu-manager-go/compare/v1.7.0...v1.8.0
- [1.7.0]: https://github.com/fdefilippo/cpu-manager-go/compare/v1.6.0...v1.7.0
- [1.6.0]: https://github.com/fdefilippo/cpu-manager-go/compare/v1.5.0...v1.6.0
- [1.5.0]: https://github.com/fdefilippo/cpu-manager-go/compare/v1.4.0...v1.5.0
- [1.4.0]: https://github.com/fdefilippo/cpu-manager-go/compare/v1.3.0...v1.4.0
- [1.3.0]: https://github.com/fdefilippo/cpu-manager-go/compare/v1.2.0...v1.3.0
- [1.2.0]: https://github.com/fdefilippo/cpu-manager-go/compare/v1.0.0...v1.2.0
- [1.0.0]: https://github.com/fdefilippo/cpu-manager-go/compare/v0.9.0...v1.0.0
- [0.9.0]: https://github.com/fdefilippo/cpu-manager-go/compare/v0.1.0...v0.9.0
- [0.1.0]: https://github.com/fdefilippo/cpu-manager-go/releases/tag/v0.1.0
