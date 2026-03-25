# Analisi Approfondita ResMan: Fattori Nascosti e Problemi Critici

## Executive Summary

Questa analisi identifica fattori nascosti, problemi di sicurezza e design issues che la maggior parte degli sviluppanti trascura nel progetto ResMan. Il sistema, pur essendo funzionale, presenta diverse vulnerabilità sottili e race conditions che potrebbero causare comportamenti imprevedibili in produzione.

---

## 1. CRITICAL SECURITY ISSUES

### 1.1 Race Condition nel Cleanup dei Cgroups (cgroup/manager.go:798-920)

**Problema**: La funzione `CleanupAll()` rilascia il lock prima di iterare su `m.createdCgroups`:

```go
func (m *Manager) CleanupAll() error {
    m.mu.Lock()
    m.logger.Info("Starting cgroup cleanup", "tracked_count", len(m.createdCgroups))
    m.wg.Wait()  // Aspetta goroutine
    m.mu.Unlock()  // RILASCIA IL LOCK QUI!

    // Poi accede a m.createdCgroups senza lock!
    for uid := range m.createdCgroups {
        if err := m.CleanupUserCgroup(uid); err != nil {
```

**Rischio**: Se un altro thread modifica `createdCgroups` durante l'iterazione, si verifica un panic per concurrent map iteration and write.

**Fix**: Tenere il lock durante l'intera operazione o fare una copia atomica:
```go
m.mu.Lock()
uids := make([]int, 0, len(m.createdCgroups))
for uid := range m.createdCgroups {
    uids = append(uids, uid)
}
m.mu.Unlock()
// Ora itera su uids, non su createdCgroups
```

### 1.2 TOCTOU nel MoveProcessToCgroup (cgroup/manager.go:382-418)

**Problema**: Il check UID 0 e l'operazione di spostamento non sono atomici:

```go
func (m *Manager) MoveProcessToCgroup(pid int, uid int) error {
    // SECURITY: Never move any process to UID 0 cgroup
    if uid == 0 {
        return fmt.Errorf("processes cannot be moved to UID 0 (root) cgroups")
    }
    // ... dopo molte righe di codice ...
    if err := os.WriteFile(cgroupProcsFile, []byte(pidStr), 0644); err != nil {
```

**Rischio**: Un processo potrebbe essere prelevato da un altro thread tra il check e l'operazione.

### 1.3 File Permission Race Condition (cgroup/manager.go:267-282)

**Problema**: `ApplyCPULimit` tenta di fare chmod in caso di permission denied:

```go
if err := os.WriteFile(cpuMaxFile, []byte(quota), 0644); err != nil {
    if os.IsPermission(err) {
        if chmodErr := os.Chmod(cpuMaxFile, 0644); chmodErr != nil {
            // Se chmod fallisce, continua comunque con retry
        }
        time.Sleep(100 * time.Millisecond)
        err = os.WriteFile(cpuMaxFile, []byte(quota), 0644)
    }
```

**Rischio**: TOCTOU - Il file potrebbe essere stato sostituito (symlink attack) tra il chmod e la scrittura.

### 1.4 Path Traversal nei Cgroup Names (cgroup/manager.go:196-202)

**Problema**: Nessuna validazione su `ScriptCgroupBase`:

```go
func (m *Manager) getUserCgroupPath(uid int) string {
    return filepath.Join(m.getBaseCgroupPath(), fmt.Sprintf("user_%d", uid))
}
```

Se `ScriptCgroupBase` contiene `../..`, potrebbe scrivere fuori dal cgroup root.

---

## 2. CONCURRENCY ISSUES

### 2.1 Deadlock Potenziale nel Collector (metrics/collector.go:1307-1358)

**Problema**: `getProcessCPUUsageSimple` prende lock in ordine non consistente:

```go
c.procCPUMutex.Lock()
defer c.procCPUMutex.Unlock()

// Se getProcessCPUUsageSimple viene chiamato mentre periodicCleanup() sta eseguendo,
// e periodicCleanup() ha già preso cacheMutex ma aspetta procCPUMutex...
```

**Rischio**: Se `periodicCleanup()` e `getProcessCPUUsageSimple` vengono eseguiti contemporaneamente da thread diversi, possono verificarsi deadlock.

### 2.2 Unsynchronized Access a Config (config/config.go)

**Problema**: La struct `Config` ha un mutex `mu sync.RWMutex` ma è raramente usata:

```go
type Config struct {
    mu sync.RWMutex
    // ... campi pubblici accessibili direttamente
}
```

La maggior parte del codice accede direttamente ai campi senza lock:
- `metrics/collector.go` accede a `c.cfg.MetricsCacheTTL` senza lock
- `state/manager.go` accede a `m.cfg.CPUThreshold` senza lock

**Rischio**: Se la configurazione viene ricaricata durante l'esecuzione, i campi possono essere in stato inconsistente.

### 2.3 Goroutine Leak nel Main Loop (main.go:276-357)

**Problema**: Il ciclo principale crea un nuovo `cycleComplete` channel ad ogni iterazione:

```go
for {
    select {
    case <-ticker.C:
        cycleComplete = make(chan struct{})  // NUOVO OGNI VOLTA
        // ...
        close(cycleComplete)
    }
}
```

Se il ciclo termina prematuramente (panic o return), il channel precedente rimane aperto.

---

## 3. MEMORY MANAGEMENT ISSUES

### 3.1 Unbounded Cache Growth (metrics/collector.go:97-103)

**Problema**: Le cache hanno limiti ma l'eviction LRU è implementata male:

```go
if len(c.cache) >= MAX_CACHE_SIZE {
    oldestKey := ""
    oldestTime := time.Now()

    for k, ts := range c.cacheTimestamps {
        if ts.Before(oldestTime) {
            oldestTime = ts
            oldestKey = k
        }
    }

    if oldestKey != "" {
        delete(c.cache, oldestKey)
        delete(c.cacheTimestamps, oldestKey)
    }
}
```

**Problema**: Se la cache è esattamente a MAX_CACHE_SIZE e arriva una nuova chiave, cerca di rimuovere la più vecchia. Ma se ci sono più chiavi con lo stesso timestamp (possibile con time.Now() a risoluzione limitata), potrebbe rimuovere una chiova diversa ogni volta, causando thrashing.

### 3.2 Memory Leak in Username Cache (metrics/collector.go:700-728)

**Problema**: La funzione `cacheUsername` ha un bug nell'eviction:

```go
if len(c.usernameCache) >= MAX_USERNAME_CACHE_SIZE {
    oldestUID := 0
    oldestTime := time.Now()

    for uid, ts := range c.usernameCacheTime {
        if ts.Before(oldestTime) {
            oldestTime = ts
            oldestUID = uid
        }
    }

    if oldestUID != 0 {
        delete(c.usernameCache, oldestUID)
        delete(c.usernameCacheTime, oldestUID)
    }
}
```

**Bug**: Se tutti gli elementi hanno timestamp futuri (impossibile ma considerare clock skew), `oldestUID` rimane 0 e non viene fatto nulla. La cache cresce oltre il limite.

### 3.3 File Handle Leak (metrics/collector.go:408-431)

**Problema**: `getUIDFromStatusFile` apre file senza garantire la chiusura in caso di panic:

```go
func (c *Collector) getUIDFromStatusFile(statusFile string) (int, error) {
    file, err := os.Open(statusFile)
    if err != nil {
        return 0, err
    }
    defer file.Close()  // OK, ma se il caller panic dopo?
```

Anche se `defer` dovrebbe funzionare, se il processo viene killato violentemente, i file handle rimangono aperti.

---

## 4. ERROR HANDLING ISSUES

### 4.1 Silent Failures nel Cgroup Cleanup (cgroup/manager.go:836-838)

**Problema**: Gli errori durante lo spostamento dei processi sono ignorati:

```go
for _, pid := range pids {
    os.WriteFile(rootCgroupProcs, []byte(fmt.Sprintf("%d", pid)), 0644)
}
```

Nessun check dell'errore! Se lo spostamento fallisce, il processo rimane nel cgroup che stiamo cercando di eliminare.

### 4.2 Ignored Errors in Process Movement (cgroup/manager.go:461-468)

**Problema**: Se `MoveProcessToCgroup` fallisce, l'errore è solo loggato:

```go
if err := m.MoveProcessToCgroup(pid, uid); err != nil {
    errors = append(errors, fmt.Sprintf("%s[%d]: %v", processName, pid, err))
} else {
    movedCount++
}
```

Alla fine viene restituito `nil` anche se ci sono stati errori:

```go
return nil  // Sempre nil!
```

### 4.3 Incomplete Error Propagation (state/manager.go:449-480)

**Problema**: `releaseIdleUsers` restituisce solo il primo errore:

```go
var firstError error
// ...
for _, uid := range usersToRelease {
    if err := m.cgroupManager.ApplyCPULimit(uid, m.cfg.CPUQuotaNormal); err != nil {
        if firstError == nil {
            firstError = err
        }
        continue
    }
}
return firstError
```

Se falliscono multipli utenti, solo il primo errore è visibile.

---

## 5. TIMING AND SYNCHRONIZATION ISSUES

### 5.1 Hardcoded Timeouts (cgroup/manager.go:309-334)

**Problema**: Timeout fisso di 5 secondi senza possibilità di configurazione:

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

done := make(chan error, 1)
go func() {
    defer close(done)
    time.Sleep(100 * time.Millisecond)
    done <- m.MoveAllUserProcesses(uid)
}()

select {
case err := <-done:
    // ...
case <-ctx.Done():
    return fmt.Errorf("timeout moving processes to cgroup for UID %d", uid)
}
```

Su sistemi con molti processi, 5 secondi potrebbero non bastare.

### 5.2 Busy Waiting nel Threshold Tracker (state/manager.go:1121-1151)

**Problema**: `ShouldActivateLimits` incrementa `totalCycles` anche quando non dovrebbe:

```go
func (t *ThresholdTracker) ShouldActivateLimits(...) bool {
    if currentCPU >= threshold {
        // ...
        t.totalCycles++  // Qui
        // ...
    } else {
        // ...
    }
    t.totalCycles++  // E di nuovo qui! (sempre)
    return false
}
```

Questo causa un doppio incremento quando `currentCPU >= threshold`.

### 5.3 Magic Numbers (cgroup/manager.go:36-44)

**Problema**: Costanti temporali sparse nel codice:

```go
const (
    defaultFilePerm       = 0644
    sharedCgroupQuota     = 100000
    cleanupRetryDelay     = 100 * time.Millisecond
    processMoveDelay      = 500 * time.Millisecond
    processMoveDelayShort = 300 * time.Millisecond
    verificationDelay     = 50 * time.Millisecond
    kernelProcessDelay    = 200 * time.Millisecond
)
```

Nessuna di queste è configurabile e non c'è documentazione sul perché questi valori.

---

## 6. DATA CONSISTENCY ISSUES

### 6.1 Stale Cache in GetAllUsers (metrics/collector.go:526-558)

**Problema**: La cache può restituire dati obsoleti quando gli utenti cambiano:

```go
func (c *Collector) GetAllUsers() []int {
    cacheKey := "all_users"
    if val, valid := c.getFromCache(cacheKey, time.Duration(c.cfg.MetricsCacheTTL)*time.Second); valid {
        return val.([]int)  // Potrebbe essere vecchio!
    }
    // ...
}
```

Se un utente termina tutti i suoi processi durante il TTL della cache, continua ad apparire.

### 6.2 Inconsistent Username Resolution (state/manager.go:865-868)

**Problema**: `getUsername` restituisce solo l'UID come stringa:

```go
func (m *Manager) getUsername(uid int) string {
    return strconv.Itoa(uid)
}
```

Ma `Collector.getUsername` implementa caching e lookup LDAP. Questa inconsistenza causa metriche Prometheus con username diversi per lo stesso UID.

---

## 7. DESIGN SMELLS

### 7.1 Circular Dependencies Risk

Il package `state` dipende da `metrics`, ma `metrics` accede a `config` che contiene riferimenti impliciti allo stato. Questo crea un potenziale per dipendenze circolari.

### 7.2 God Object Anti-pattern

`state.Manager` (1163 linee) e `cgroup.Manager` (1270 linee) sono troppo grandi. Hanno troppe responsabilità:
- Gestione stato
- Decision making
- Cleanup
- Logging
- Metriche

### 7.3 Interface Segregation Violation

Le interfacce `MetricsCollector`, `CgroupManager`, `PrometheusExporter` in `state/manager.go` hanno troppi metodi. Implementazioni mock/testing devono implementare tutto.

### 7.4 Global Mutable State

```go
var (
    currentLogger *Logger
    once          sync.Once
)
```

Il logger globale rende i test paralleli impossibili e causa side effects nascosti.

---

## 8. PERFORMANCE ISSUES

### 8.1 O(n²) nel Process Scanning

`GetAllUserMetrics` (metrics/collector.go:1117) itera su tutti i processi per ogni utente. Con molti utenti, diventa quadratico.

### 8.2 Inefficient String Concatenation

In `cgroup/manager.go:1130-1143`:

```go
cmdline := strings.ReplaceAll(string(data), "\x00", " ")
cmdline = strings.TrimSpace(cmdline)
```

Crea copie multiple della stringa.

### 8.3 No Batch Operations

Ogni operazione cgroup è fatta singolarmente. Con 1000 processi, sono 1000 syscall separate.

---

## 9. SECURITY BEST PRACTICES VIOLATIONS

### 9.1 JWT Secret Handling (metrics/prometheus.go:195-200)

Il secret JWT è caricato in memoria come `[]byte`. Nessun meccanismo di rotazione.

### 9.2 Basic Auth Password in Memory

La password basic auth rimane in memoria come stringa plain text, esposta a core dump.

### 9.3 No Input Validation su Regex

Configurazione regex (`UserIncludeList`, `UserExcludeList`) non ha validazione. Regex malformati possono causare ReDoS (Regular Expression Denial of Service).

---

## 10. RECOMMENDATIONS

### Alta Priorità
1. **Fissare la race condition in CleanupAll()** - Potenziale crash
2. **Implementare atomicità nei cgroup operations** - Prevenire TOCTOU
3. **Aggiungere validazione path** - Prevenire path traversal
4. **Fissare il double increment in ThresholdTracker**

### Media Priorità
1. **Rimuovere magic numbers** - Usare configurazione
2. **Implementare graceful degradation** - Non fallire silenziosamente
3. **Aggiungere context cancellation** - Migliorare responsività
4. **Rivedere la gestione errori** - Propagare tutti gli errori

### Bassa Priorità
1. **Refactoring delle interfacce** - Segregare responsabilità
2. **Ottimizzare cache eviction** - Usare librerie esistenti
3. **Aggiungere tracing** - Migliorare debuggabilità
4. **Documentare assunzioni** - Rendere espliciti i trade-off

---

## Conclusione

ResMan è un sistema ben architettato nel complesso, ma soffre di problemi classici di sistemi concorrenti che operano su risorse di sistema. Le race conditions nel cgroup management sono particolarmente preoccupanti perché potrebbero causare instabilità del sistema o bypass dei limiti di risorse.

La maggior parte dei problemi identificati sono "fattori nascosti" che non emergono in testing standard ma si manifestano in produzione sotto carico o con timing specifici.

**Stima rischio complessivo**: MEDIO-ALTO
- Probabilità: MEDIA (richiede timing specifico)
- Impatto: ALTO (potenziale bypass sicurezza, crash sistema)
