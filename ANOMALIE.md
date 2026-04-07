# Report Anomalie - Analisi Codice Ultimi 3 Commit ResMan

**Data:** 2026-04-06  
**Analisi effettuata sui commit:**  
- `2f9e1b7` - docs + perf: shard mutexes  
- `738215c` - perf: consolidate /proc scans  
- `d87d9e0` - chore: remove binary + .gitignore  

---

## 🔴 PROBLEMI CRITICI

### C1. GetUserCPUUsage() Ora Fa Scansione Completa Inutile
**File:** `metrics/collector.go:379-395`

#### Descrizione
La funzione `GetUserCPUUsage(uid)` ora chiama `GetAllUserMetrics()` per ottenere la CPU di un singolo utente:

```go
func (c *Collector) GetUserCPUUsage(uid int) float64 {
    // PROBLEMA: Chiama GetAllUserMetrics() che scansiona TUTTI i processi
    // solo per ottenere la CPU di UN singolo utente!
    allMetrics := c.GetAllUserMetrics()  // ← Scansiona 1000+ processi
    var totalUsage float64
    if metrics, exists := allMetrics[uid]; exists {
        totalUsage = metrics.CPUUsage
    }
    c.setInCache(cacheKey, totalUsage)
    return totalUsage
}
```

#### Impatto Performance
| Prima | Dopo |
|-------|------|
| Scansiona solo processi dell'utente | Scansiona TUTTI i processi del sistema |
| Complessità: O(P/N) dove P=processi, N=utenti | Complessità: O(P) |
| Se chiamato 10 volte → 10 scansioni parziali | Se chiamato 10 volte → 10 scansioni COMPLETE |

**Esempio pratico:**
- Sistema con 500 processi, 10 utenti
- Prima: ~50 processi scansionati per chiamata
- Dopo: 500 processi scansionati per chiamata
- **Overhead: 10x**

#### Raccomandazione
Tornare a scansione per-UID o implementare cache centrale condivisa tra tutte le funzioni.

---

### C2. getProcessCPUUsage() - Codice Morto Con Stima Errata
**File:** `metrics/collector.go:446-524`

#### Descrizione
Funzione usata come fallback in `getUserCPUUsageFromProc()`:

```go
func (c *Collector) getProcessCPUUsage(pid int) (float64, error) {
    // ... parsing /proc/[pid]/stat ...
    
    // Stima semplificata: (utime + stime) in jiffies
    totalJiffies := float64(utime + stime)
    cpuSeconds := totalJiffies / 100.0
    
    return cpuSeconds * 0.1, nil // ← Stima molto approssimativa
}
```

#### Problemi Identificati
1. **Formula senza base tecnica**: `cpuSeconds * 0.1` non ha alcuna relazione con CPU% reale
2. **Commento sviluppatore**: "Stima molto approssimativa" ammette che non è corretta
3. **Dead code**: Usata solo da `getUserCPUUsageFromProc()` che è chiamata da `getUserCPUUsageFallback()` che non è mai chiamata nel path normale

#### Raccomandazione
Rimuovere completamente o implementare calcolo delta corretto con cache.

---

### C3. Shard Mutex Implementation - Lock Ordering Issue
**File:** `metrics/collector.go:71-85`

#### Descrizione
La funzione `findAndRemoveOldestPID()` locka TUTTI gli shard in ordine 0..N:

```go
func (c *Collector) findAndRemoveOldestPID() bool {
    // Lock all shards in consistent order
    for i := 0; i < procCacheShardCount; i++ {
        c.procCacheShards[i].mu.Lock()  // ← Se un altro thread fa ordine inverso → DEADLOCK
    }
    // ... trova più vecchio ...
    // ... rimuove ...
}
```

#### Scenario di Deadlock
```
Thread A:                          Thread B:
findAndRemoveOldestPID()           getProcessCPUUsageSimpleWithHandle(pid=100)
  → Lock shard[0]                    → Lock shard[100 % N]
  → Lock shard[1]                    → Trova cache piena
  → Lock shard[2]...                 → Chiama findAndRemoveOldestPID()
                                        → Tenta Lock shard[0] ← BLOCKED (Thread A lo ha già)
                                     
Thread A tenta Lock shard[100 % N] ← BLOCKED (Thread B lo ha già)

→ DEADLOCK
```

#### Impatto
- Su sistemi con molti processi concorrenti, deadlock possibile
- Locking di tutti gli shard è O(N) dove N = numero shard

#### Raccomandazione
Opzioni:
1. Usare `sync.Mutex` singolo invece di sharding (più semplice, meno contention se cache piccola)
2. Usare try-lock con timeout invece di lock bloccante
3. Implementare lock-free LRU cache

---

## 🟡 PROBLEMI MEDI

### M1. GetAllUserMetrics() Chiamato Ripetutamente Nello Stesso Ciclo
**File:** `metrics/collector.go` righe 379, 478, 498, 522, 540, 558

#### Descrizione
Ogni funzione di metrica chiama `GetAllUserMetrics()` indipendentemente:

```go
func (c *Collector) GetAllUsersCPUUsage() float64 {
    allMetrics := c.GetAllUserMetrics()  // ← Scansione 1
    // ...
}

func (c *Collector) GetLimitedUsersCPUUsage() float64 {
    allMetrics := c.GetAllUserMetrics()  // ← Scansione 2
    // ...
}

func (c *Collector) GetAllUsersMemoryUsage() uint64 {
    allMetrics := c.GetAllUserMetrics()  // ← Scansione 3
    // ...
}

func (c *Collector) GetLimitedUsersMemoryUsage() uint64 {
    allMetrics := c.GetAllUserMetrics()  // ← Scansione 4
    // ...
}
```

Se `collectSystemMetrics()` chiama tutte queste funzioni → **4+ scansioni complete di /proc** nello stesso ciclo!

#### Raccomandazione
Implementare cache a livello di ciclo di controllo:
```go
// Nel control cycle:
cachedMetrics := c.GetAllUserMetrics()  // UNA sola scansione
cpuUsage := c.GetCPUUsageFromCache(cachedMetrics)
memUsage := c.GetMemoryUsageFromCache(cachedMetrics)
```

---

### M2. IsLimited Field Non Aggiornato Dinamicamente
**File:** `metrics/collector.go:1253`

#### Descrizione
```go
userMetrics[uid] = &UserMetrics{
    // ...
    IsLimited: c.cfg.IsUserWhitelisted(username),  // ← Controlla config, non stato reale!
}
```

**Problema:**
- `IsLimited` dovrebbe riflettere se l'utente ha limiti **attivi** in questo momento
- Invece controlla se l'utente **può essere limitato** secondo la configurazione
- Un utente potrebbe essere in USER_INCLUDE_LIST ma non avere limiti attivi perché sotto soglia

**Esempio:**
- USER_INCLUDE_LIST=.*
- CPU_THRESHOLD=75, CPU attuale=30%
- Risultato: `IsLimited=true` ma limiti NON applicati!

#### Raccomandazione
Passare stato limiti da state manager a metrics collector, o usare `activeUsers` map come sorgente di verità.

---

### M3. Username Cache Senza Cleanup Periodico
**File:** `metrics/collector.go:640-698`

#### Descrizione
```go
func (c *Collector) cacheUsername(uid int, username string) {
    c.usernameCache[uid] = username
    c.usernameCacheTime[uid] = time.Now()
    // Nessun cleanup se utente non esiste più!
}
```

**Problema:**
- Cache cresce indefinitamente per utenti che esistono ma non sono più attivi
- LRU eviction solo quando cache piena (MAX_USERNAME_CACHE_SIZE=10000)
- Utenti LDAP disconnessi restano in cache per sempre

#### Raccomandazione
Aggiungere cleanup periodico in `CleanupAll()` o timer automatico.

---

## 🟢 PROBLEMI MINORI

### L1. Dead Code Non Rimosso

**Funzioni mai chiamate nel path normale:**

| Funzione | File | Utilizzo |
|----------|------|----------|
| `getUserCPUUsageFallback()` | collector.go | Solo fallback, mai testata |
| `getProcessCPUUsage()` | collector.go | Stima errata, solo fallback |
| `getActiveUsersFromProc()` | collector.go | Solo fallback quando gopsutil fallisce |
| `getUserCPUUsageFromProc()` | collector.go | Chiama `getProcessCPUUsage()` |

#### Raccomandazione
Rimuovere o documentare chiaramente come "fallback di emergenza non testato".

---

### L2. Formattazione Test Incoerente

**Commit:** `2f9e1b7`  
**File:** `metrics/collector_test.go`  
**Modifiche:** +281 -281 linee (solo spazi → tab)

**Problema:**
- 562 linee di puro reformatting mescolate con cambiamenti funzionali
- Diff impossibile da leggere
- Violazione best practice git: "separare reformatting da functional changes"

#### Raccomandazione
In futuro, commit di reformatting separati da commit funzionali.

---

### L3. File Temporanei Committati nel Repo Principale

**Commit:** `2f9e1b7`

| File | Righe | Tipo |
|------|-------|------|
| `ANALISI_MANUALE.md` | 81 | Note di analisi temporanee |
| `OTTIMIZZAZIONI_PENDENTI.md` | 110 | Todo list temporanea |

**Problema:**
- Non sono documentazione ufficiale
- Occupano spazio nel repo
- Non referenziati da README o docs ufficiali

#### Raccomandazione
Spostare in `/tmp/`, `.qwen/`, o branch separato `dev-notes/`.

---

### L4. Binario Committato e Poi Rimosso

**Commit:** `d87d9e0`  
**File:** `resman` (15MB binario)

**Problema:**
- Binario committato in commit precedente
- Rimosso in commit successivo
- **Git history contiene ancora il binario** → repo più grande del necessario

#### Raccomandazione
Per pulizia completa: `git filter-branch --tree-filter 'rm -f resman' HEAD`

---

## 📈 Metriche di Qualità del Codice

| Metrica | Valore Attuale | Target Ideale |
|---------|---------------|---------------|
| Scansioni /proc per ciclo | 4-6 | 1 |
| Codice morto presente | 3+ funzioni | 0 |
| Lock ordering garantito | ❌ No | ✅ Sì |
| Cache coherence | ⚠️ Parziale | ✅ Completa |
| Test coverage | ⚠️ Reformatting only | ✅ Functional tests |
| Dead code | 4 funzioni | 0 |
| File temporanei | 2 | 0 |

---

## 🎯 Raccomandazioni Prioritarie

### Priorità ALTA (risolvere subito)
1. **Refactor `GetUserCPUUsage()`** - Non chiamare `GetAllUserMetrics()` per singolo utente
2. **Fix lock ordering** in `findAndRemoveOldestPID()` o usare try-lock/timeout

### Priorità MEDIA (prossimo sprint)
3. **Cache a livello di ciclo** per `GetAllUserMetrics()` - Evitare scansioni multiple
4. **Correggere `IsLimited`** - Riflettere stato runtime reale, non config
5. **Rimuovere file temporanei** dal repo principale

### Priorità BASSA (backlog)
6. **Rimuovere dead code** - Funzioni fallback non testate
7. **Git history cleanup** - Rimuovere binario da history
8. **Separare reformatting** da cambiamenti funzionali nei commit futuri

---

## 📝 Note Aggiuntive

### Punti di Forza Identificati
✅ gopsutil integration corretta con fallback  
✅ Cache TTL implementata per metriche  
✅ Shard mutex per CPU cache (concetto corretto, implementazione da fixare)  
✅ Build funziona senza errori  
✅ .gitignore aggiornato correttamente  

### Domande Aperte
❓ Perché `GetAllUserMetrics()` non ha cache a livello di ciclo?  
❓ I fallback manuali sono mai stati testati in produzione?  
❓ Quanti shard sono configurati per `procCacheShards`?  

---

**Report generato:** 2026-04-06 19:30  
**Analisi basata su:** `git log -n 3`, `git diff`, lettura codice sorgente  
**File analizzati:** `metrics/collector.go`, `cgroup/manager.go`, `config/config.go`, `main.go`, `state/manager.go`
