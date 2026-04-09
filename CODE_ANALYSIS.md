# Analisi del Codice - resman metrics subsystem

**Data**: 2026-04-08  
**File analizzati**: `metrics/collector.go`, `metrics/prometheus.go`, `state/manager.go` (parziale)

## 1. Strutture Dati Principali

### `UserMetrics` (`metrics/collector.go:46-58`)
```go
type UserMetrics struct {
    UID          int
    Username     string
    CPUUsage     float64 // CPU percentage
    MemoryUsage  uint64  // Memory in bytes (VmRSS)
    ProcessCount int
    IsLimited    bool    // Whether user has CPU limits applied
    IOReadBytes  uint64  // Total bytes read from block devices
    IOWriteBytes uint64  // Total bytes written to block devices
    IOReadOps    uint64  // Total read operations
    IOWriteOps   uint64  // Total write operations
}
```

### `userData` (`metrics/collector.go:68-77`)
Struttura temporanea per accumulare dati durante lo scan dei processi:
- `cpuUsage`, `memoryUsage`, `processCount`
- `ioReadBytes`, `ioWriteBytes`, `ioReadOps`, `ioWriteOps`

### `procCache` (`metrics/collector.go:60-66`)
Cache per i tempi CPU dei processi (calcolo delta):
- `prevProcCPU`: mappa PID → `cpu.TimesStat`
- `prevProcTime`: mappa PID → `time.Time`
- Mutex singolo (non sharding) per semplicità e prevenzione deadlock

## 2. Sistema di Cache

### Cache Generale (`metrics/collector.go:85-88`)
```go
cache           map[string]interface{}
cacheTimestamps map[string]time.Time
cacheMutex      sync.RWMutex
```

**Funzionamento**:
- `getFromCache(key, ttl)`: verifica esistenza e validità TTL
- `setInCache(key, value)`: inserisce con evizione LRU se `MAX_CACHE_SIZE` (10.000) raggiunto
- TTL configurabile via `MetricsCacheTTL` (default 30 secondi)

### Cache Username (`metrics/collector.go:100-104`)
- Mappa `UID → username` con TTL configurabile (`usernameCacheTTL`, default 60 minuti)
- Mutex dedicato `usernameCacheMutex`

### Cache Process CPU (`procCache`)
- Mantiene tempi CPU precedenti per calcolo delta usage per processo
- Pulizia periodica via `periodicCleanup()`

## 3. Raccolta Metriche per Utente (`GetAllUserMetrics`)

### Flusso a Doppio Passaggio

#### **Primo Passaggio (CPU/Memory)** - righe 1047-1076
1. Enumera processi via `gopsutil.Processes()`
2. Per ogni processo:
   - Ottiene UID via `p.Uids()[0]`
   - Applica `isValidUserUID(uid)` (controlla `SystemUIDMin ≤ uid ≤ SystemUIDMax`)
   - Se valido:
     - Incrementa `processCount`
     - Calcola CPU usage via `getProcessCPUUsageSimpleWithHandle(p)` (delta tra letture)
     - Legge RSS memory via `p.MemoryInfo().RSS`
   - Accumula in `tempData[uid]`

#### **Secondo Passaggio (IO)** - righe 1080-1096
1. Stessa enumerazione processi
2. Per ogni processo:
   - Ottiene UID (senza filtro `isValidUserUID`)
   - Inizializza `ioData[uid]` se non esiste
   - Legge `/proc/[pid]/io` via `getProcessIO(pid)`
   - Accumula `read_bytes`, `write_bytes`, `syscr`, `syscw` in `ioData`

#### **Merge** - righe 1098-1107
- Unisce `ioData` in `tempData`
- Se `tempData[uid]` non esiste (UID filtrato nel primo passaggio), crea nuovo `userData{}` vuoto
- Somma campi IO

#### **Conversione a `UserMetrics`** - righe 1110-1134
- Per ogni `uid` in `tempData`:
  - Risolve username via cache (`GetUsernameFromUID`)
  - Crea `UserMetrics` con tutti i campi
  - Imposta `IsLimited` via `c.cfg.IsUserWhitelisted(username)`

### Cache delle Metriche Utente
- Chiave: `"all_user_metrics"`
- TTL: `MetricsCacheTTL` secondi
- Se valida in cache, restituisce direttamente senza ricalcolo

## 4. Raccolta Metriche di Sistema

### CPU Totale (`GetTotalCPUUsage`)
- Usa `cpu.Percent(100ms, false)` via gopsutil
- Fallback a lettura manuale `/proc/stat`
- Cache con TTL `MetricsCacheTTL`

### Memoria Totale (`GetMemoryUsage`, `GetTotalMemoryMB`, `GetCachedMemoryMB`)
- Usa `mem.VirtualMemory()` via gopsutil
- Cache con TTL `MetricsCacheTTL`

### Core Totali (`GetTotalCores`)
- `cpu.Counts(true)` via gopsutil
- Fallback a `/proc/cpuinfo`
- Cache lunga (3600 secondi)

## 5. Esportazione Prometheus (`metrics/prometheus.go`)

### Metriche IO - Delta Calculation
```go
// UpdateUserMetrics - righe 710-730
ioKey := fmt.Sprintf("%d_%s", uid, username)
prevIO := exp.prevIOStats[ioKey]

if ioReadBytes >= prevIO.ReadBytes {
    exp.userIOReadBytes.WithLabelValues(uidStr, username).Add(
        float64(ioReadBytes - prevIO.ReadBytes)
    )
}
// ... simile per write bytes, read ops, write ops

exp.prevIOStats[ioKey] = ioStatsSnapshot{
    ReadBytes:  ioReadBytes,
    WriteBytes: ioWriteBytes,
    ReadOps:    ioReadOps,
    WriteOps:   ioWriteOps,
}
```

**Logica**: Calcola delta solo se nuovo valore ≥ precedente (counter monotoni)

### Metriche Memory High Events
- Simile meccanismo delta per `userMemoryHighEvents`

### Pulizia Metriche Inattive (`CleanupUserMetrics`)
- Rimuove metriche Prometheus per UID non più presenti in `activeUids`
- Gestisce `activeUserMetrics` map per tracciare utenti attivi

## 6. Integrazione con State Manager (`state/manager.go`)

### `collectSystemMetrics()` - righe 351-417
1. Raccolta metriche di sistema (CPU, memory, cores)
2. Chiamata `GetAllUserMetrics()` per dati utente
3. Correzione `IsLimited` basato su stato runtime (`activeUsers`)
4. **Copiatura campi IO** da `UserMetrics` originale a `corrected` (righe 395-398)

### `UpdateUserMetrics` Chiamata - righe 1100-1114
- Passa tutti i campi, inclusi IO, a Prometheus exporter
- Logica fallback: usa cgroup IO se per-user IO è zero (righe 1092-1097)

## 7. Osservazioni e Potenziali Considerazioni

### 1. **Filtro UID Asimmetrico**
- Primo passaggio (CPU/memory) applica `isValidUserUID()`
- Secondo passaggio (IO) **non** applica filtro
- **Conseguenza**: Utenti system (`uid < SystemUIDMin`) hanno `tempData[uid]` creato solo nel merge, con campi CPU/memory a zero ma IO popolati

### 2. **Semantica Counter Prometheus vs /proc/[pid]/io**
- Metriche IO esposte come **counter** (`resman_user_io_*_total`)
- Dati sorgente (`/proc/[pid]/io`) monotoni solo per lifetime processo
- Se processo termina e nuovo inizia, somma per UID può diminuire
- **Conseguenza**: Condizione `newValue >= prevValue` in `UpdateUserMetrics` potrebbe bloccare incrementi

### 3. **Cache Contaminazione**
- Oggetti `UserMetrics` serializzati in cache potrebbero non avere campi IO se creati prima di commit che li aggiunge
- **Conseguenza**: Cache hit restituisce `UserMetrics` con campi IO a zero

### 4. **TTL Cache Configurabile**
- `MetricsCacheTTL` default 30 secondi
- Cache username default 60 minuti
- **Considerazione**: Valori bassi aumentano accuratezza, alti riducono carico sistema

### 5. **Delta Calculation Sensibile**
- `UpdateUserMetrics` calcola delta per IO e memory.high events
- `prevIOStats` e `prevMemoryHighEvents` mantengono snapshot per utente
- **Considerazione**: Reset o rollover contatori può causare delta negativi ignorati

### 6. **Fallback Cgroup IO**
- `state/manager.go:1092-1097`: usa cgroup IO se per-user IO zero
- **Conseguenza**: Utenti senza cgroup (system users) potrebbero avere metriche IO zero anche con attività

### 7. **Debug Logging Limitato**
- Log `Process IO collected` in `getProcessIO` solo se `rB > 0 || wB > 0`
- Nessun log mostra valori passati a `UpdateUserMetrics` o stato `prevIOStats`

## 8. Flusso Dati Complessivo

```
/proc/[pid]/status, /proc/[pid]/stat, /proc/[pid]/io
        ↓
    gopsutil.Processes()
        ↓
    GetAllUserMetrics()
        ├── Primo passaggio: CPU, Memory (con filtro UID)
        ├── Secondo passaggio: IO (senza filtro UID)
        └── Merge e conversione a UserMetrics
        ↓
    Cache "all_user_metrics" (TTL)
        ↓
    state/manager.collectSystemMetrics()
        ↓ (correzione IsLimited, copia campi IO)
    state/manager.writePrometheusMetrics()
        ↓
    prometheus.UpdateUserMetrics()
        ↓ (delta calculation, prevIOStats)
    Prometheus metrics endpoint (:1974/metrics)
```

## 9. Configurazioni Rilevanti

| Configurazione | Default | Descrizione |
|----------------|---------|-------------|
| `METRICS_CACHE_TTL` | 30 | TTL cache metriche (secondi) |
| `SYSTEM_UID_MIN` | 1000 | UID minimo per monitoraggio CPU/memory |
| `SYSTEM_UID_MAX` | 4194304 | UID massimo (pid_max) |
| `USERNAME_CACHE_TTL` | 60 | TTL cache username (minuti) |

---

**Nota**: Questa analisi si basa esclusivamente sulla lettura del codice corrente senza presupporre problemi esistenti o stati runtime. Descrizione del funzionamento ideale come progettato.