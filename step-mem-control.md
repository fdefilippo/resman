# Piano: Aggiungere Limitazione RAM

### Componenti Coinvolti (CPU)
- `cgroup/manager.go`: Operazioni cgroup (`ApplyCPULimit`, `CreateUserCgroup`, etc.)
- `state/manager.go`: Logica di controllo, decisioni
- `metrics/collector.go`: Raccolta metriche per utente
- `config/config.go`: Parametri configurazione
- `prometheus/prometheus.go`: Esportazione metriche Prometheus

---

## Implementazione Dettagliata

### FASE 1: Configurazione (`config/config.go`)

**Nuovi parametri da aggiungere:**

```go
// RAM Limits
RAMThreshold       int    `config:"RAM_THRESHOLD"`        // % RAM usage to activate limits
RAMReleaseThreshold int  `config:"RAM_RELEASE_THRESHOLD"`  // % RAM usage to release limits
RAMQuotaLimited   string `config:"RAM_QUOTA_LIMITED"`     // es. "536870912" (bytes) o "512M"
```

**Validazione da aggiungere:**
```go
if cfg.RAMThreshold < 0 || cfg.RAMThreshold > 100 { ... }
if cfg.RAMReleaseThreshold < 0 || cfg.RAMReleaseThreshold > 100 { ... }
// Validare formato RAMQuotaLimited (bytes, k, M, G)
```

---

### FASE 2: Gestione Cgroup (`cgroup/manager.go`)

**File cgroup.v2 per memoria:**
- `/sys/fs/cgroup/<path>/memory.max` - Limite massimo memoria
- `/sys/fs/cgroup/<path>/memory.current` - Utilizzo corrente
- `/sys/fs/cgroup/<path>/memory.high` - Soglia per throttling

**Nuovi metodi da aggiungere:**

```go
// ApplyRAMLimit applica un limite di RAM a un cgroup utente
func (m *Manager) ApplyRAMLimit(uid int, limit string) error {
    // 1. Verifica che il cgroup esista
    // 2. Valida formato limit (bytes, k, M, G)
    // 3. Scrivi in memory.max
    // 4. Verifica applicazione
}

// RemoveRAMLimit rimuove il limite di RAM
func (m *Manager) RemoveRAMLimit(uid int) error

// GetRAMUsage restituisce l'uso RAM per un cgroup
func (m *Manager) GetCgroupRAMUsage(uid int) (uint64, error)

// GetUserRAMUsage restituisce l'uso RAM per un utente
func (m *Manager) GetUserRAMUsage(uid int) (uint64, error)
```

**Possibili problemi:**
- `memory.max` usa bytes, non percentuale
- Necessità conversione: config in % → bytes dal sistema
- Kernel OOM killer potrebbe intervenire

---

### FASE 3: Metriche (`metrics/collector.go`)

**Modifiche a struttura `UserMetrics`:**
```go
type UserMetrics struct {
    // ... esistente ...
    RAMUsage uint64 // bytes
    RAMLimit uint64 // bytes (0 = no limit)
}
```

**Nuovi metodi:**
```go
// GetUserRAMUsage calcola RAM usata da tutti i processi utente
func (c *Collector) GetUserRAMUsage(uid int) uint64 {
    // 1. Leggi /proc/[pid]/status per ogni processo utente
    // 2. Somma VmRSS (Resident Set Size)
    // 3. Filtra per UID reale
    // 4. Usa cache per performance
}

// GetTotalRAMUsage restituisce RAM totale sistema
func (c *Collector) GetTotalRAMUsage() float64 // percentuale
```

**Cache:** Aggiungere chiavi tipo `"ram_usage_uid_%d"`.

---

### FASE 4: Logica di Controllo (`state/manager.go`)

**Modifiche a `SystemMetrics`:**
```go
type SystemMetrics struct {
    // ... esistente ...
    TotalRAMUsage float64
    RAMUsagePercent float64
}
```

**Modifiche a `makeDecision()`:**
```go
func (m *Manager) makeDecision(metrics *SystemMetrics) (string, string) {
    // Decisione CPU (esistente)
    cpuDecision := ...
    
    // Decisione RAM (NUOVO)
    if metrics.RAMUsagePercent > m.cfg.RAMThreshold {
        return "ACTIVATE_RAM_LIMITS", "RAM above threshold"
    }
    if metrics.RAMUsagePercent < m.cfg.RAMReleaseThreshold {
        return "DEACTIVATE_RAM_LIMITS", "RAM below threshold"
    }
    
    // Combinare decisioni CPU + RAM
    ...
}
```

**Modifiche a `activateLimits()` / `deactivateLimits()`:**
- Applicare/rimuovere sia CPU che RAM limits

---

### FASE 5: Prometheus (`metrics/prometheus.go`)

**Nuove metriche:**
```go
cpu_manager_total_ram_usage_bytes
cpu_manager_total_ram_usage_percent
cpu_manager_user_ram_usage_bytes{uid="X"}
cpu_manager_user_ram_limit_bytes{uid="X"}
cpu_manager_ram_limits_active
```

---

### FASE 6: Test (`cgroup/manager_test.go`, etc.)

- Test `ApplyRAMLimit`
- Test `GetUserRAMUsage`
- Test validazione config
- Test integrazione con CPU limits

---

## Problemi Architetturali da Risolvere

| Problema | Soluzione Proposta |
|----------|-------------------|
| **Unità diverse** | Config in %, cgroups in bytes → convertire dinamicamente |
| **OOM Killer** | Impostare `memory.high` come soft limit, `memory.max` come hard |
| **Swap** | Considerare `memory.swap.max` = 0 per evitare swap |
| **Cache filesystem** | `memory.current` include cache → usare `memory.numa_stat` o `memory.stat` |
| **Shared memory** | Considerare `memory.shared` per memoria condivisa |

---

## Dipendenze

Nessuna nuova dipendenza - `gopsutil` già usato per metriche ha `mem.VirtualMemory()`.

---

## Ordine di Implementazione Suggerito

1. Config (`config/config.go`) - Parametri + validazione
2. Metriche (`metrics/collector.go`) - `GetUserRAMUsage()`
3. Cgroup (`cgroup/manager.go`) - `ApplyRAMLimit()`, `RemoveRAMLimit()`
4. State (`state/manager.go`) - Logica decisione + integrazione
5. Prometheus (`metrics/prometheus.go`) - Nuove metriche
6. Tests
7. Documentazione


