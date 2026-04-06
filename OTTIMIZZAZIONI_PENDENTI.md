# Ottimizzazioni Pendenti per resman

## Ottimizzazioni Implementate

✅ **Consolidate /proc scans** - `GetAllUserMetrics()` ora è l'unica fonte per metriche utente (commit `738215c`)  
✅ **Batch PID moves in cgroup cleanup** - `writePidsBatch()` riduce syscall (commit `738215c`)  
✅ **sync.Map per regex cache** - Eliminata race condition double-check lock (commit `738215c`)  
✅ **Shard mutexes per CPU tracking** - 64 shard per cache CPU processi, riduzione contesa mutex  

## Ottimizzazioni da Implementare

### 1. **Pre-allocare slices in funzioni movimento processi** (`cgroup/manager.go`)
**Problema**: `processNames` e `errors` slices crescono dinamicamente con `append` (linee 453-477).  
**Soluzione**: Stimare capacità iniziale:
```go
estimatedProcesses := len(procs) / 50 // stima conservativa
processNames := make([]string, 0, estimatedProcesses)
errors := make([]string, 0, estimatedProcesses/10)
```
**Impatto**: Riduzione allocazioni heap e GC pressure.

### 2. **Cache invalidation intelligente** (`metrics/collector.go`)
**Problema**: Cache TTL fisso non tiene conto di cambiamenti dinamici (nuovi processi).  
**Soluzione**: Cache generazionale o invalidazione basata su eventi:
- Aumentare TTL per utenti stabili
- Invalidare cache quando nuovi UID appaiono
- Usare `singleflight.Group` per richieste concorrenti identiche

### 3. **Backoff esponenziale nei loop di controllo** (`state/manager.go`)
**Problema**: Loop infiniti senza backoff in caso di errori ripetuti.  
**Soluzione**: Implementare backoff esponenziale con jitter:
```go
retryDelay := time.Second
maxRetryDelay := 30 * time.Second
for {
    err := doWork()
    if err != nil {
        time.Sleep(retryDelay)
        retryDelay = min(retryDelay*2, maxRetryDelay)
        continue
    }
    retryDelay = time.Second // reset on success
}
```

### 4. **Goroutine leak nel timeout shutdown** (`main.go`)
**Problema**: Goroutine timeout (`syscall.Kill`) rimane in attesa anche se shutdown avviene prima.  
**Soluzione**: Usare `context.WithTimeout` o canale di stop:
```go
ctx, cancel := context.WithTimeout(context.Background(), timeout)
defer cancel()
go func() {
    <-ctx.Done()
    if ctx.Err() == context.DeadlineExceeded {
        syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
    }
}()
```

### 5. **Batch reads di cgroup.procs** (`cgroup/manager.go`)
**Problema**: `readPidsFromFile` legge tutto il file ma potrebbe essere ottimizzato.  
**Soluzione**: Usare `bufio.NewReader` con buffer più grande per file grandi.

### 6. **Pool di strutture userData** (`metrics/collector.go`)
**Problema**: Allocazione `&userData{}` per ogni UID nuovo (linea ~1225).  
**Soluzione**: Usare `sync.Pool`:
```go
var userDataPool = sync.Pool{
    New: func() interface{} { return &userData{} },
}
// Nel loop:
data := userDataPool.Get().(*userData)
// ... usa data ...
userDataPool.Put(data)
```
**Nota**: Attenzione a resettare campi prima del riuso.

### 7. **Metriche Prometheus labeling ottimizzato** (`metrics/prometheus.go`)
**Problema**: Labels duplicate per metriche utente (username, UID).  
**Soluzione**: Cache locale di mapping UID→labels per evitare ricreazione costante.

### 8. **Lazy username resolution** (`metrics/collector.go`)
**Problema**: `getUsername(uid)` chiamato per ogni processo/utente, potenzialmente costoso (NSS/LDAP).  
**Soluzione**: Cache LRU per username resolution con TTL.

### 9. **Selective /proc scanning per operazioni specifiche**
**Problema**: `MoveAllUserProcesses` e `GetAllUserMetrics` scansionano tutti i processi anche quando servono solo UID specifici.  
**Soluzione**: Quando possibile, passare lista UID target per scanning selettivo.

## Priorità Raccomandate

1. **Pre-alloc slices** (alta priorità - riduzione allocazioni)
2. **Cache invalidation** (media - accuratezza metriche)
3. **Backoff esponenziale** (bassa - robustezza)
4. **Goroutine leak** (bassa - pulizia risorse)

## Metriche di Successo

- Riduzione syscall I/O (/proc scans)
- Riduzione allocazioni heap
- Riduzione lock contention
- Mantenimento accuratezza metriche
- Nessun regressione funzionale

## Test da Eseguire

- Benchmark `GetAllUserMetrics` prima/dopo
- Profiling mutex contention (`go test -bench . -mutexprofile`)
- Test concorrenza intensiva
- Verifica memory leak sotto carico