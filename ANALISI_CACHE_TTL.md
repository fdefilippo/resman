# Analisi METRICS_CACHE_TTL

## Configurazione Attuale

```bash
POLLING_INTERVAL=30        # Control cycle ogni 30 secondi
METRICS_CACHE_TTL=15       # Cache scade dopo 15 secondi
```

## Come Viene Usata la Cache

### Nel Control Cycle (state/manager.go)

```
RunControlCycle()
  ├─ collectSystemMetrics()
  │   └─ GetAllUserMetrics()         ← Controlla cache
  │       ├─ Se cache valida → restituisci dati cached
  │       └─ Se cache scaduta → scansione completa /proc
  │
  ├─ Pattern Detection
  │   └─ GetAllUserMetrics()         ← Controlla cache (stesso ciclo)
  │
  ├─ IO Remediation
  │   └─ GetLimitedUsers()           ← Controlla cache
  │
  └─ writeDatabaseMetrics()
      └─ GetTotalCores(), etc.       ← Controlla cache
```

### Nelle Richieste MCP

```
MCP Tool: get_user_metrics
  └─ GetAllUserMetrics()            ← Controlla cache

MCP Tool: get_system_status
  └─ GetTotalCPUUsage()             ← Controlla cache
```

## Analisi di Efficacia

### Scenario 1: METRICS_CACHE_TTL < POLLING_INTERVAL (default: 15s < 30s)

```
Tempo    Evento              Stato Cache
─────────────────────────────────────────────────
0s       Ciclo 1 starts      Cache vuota → SCANSIONE COMPLETA
0.5s     GetAllUserMetrics() Cache valida (appena scritta)
15s      Cache SCADUTA
30s      Ciclo 2 starts      Cache scaduta → SCANSIONE COMPLETA
30.5s    GetAllUserMetrics() Cache valida (appena scritta)
45s      Cache SCADUTA
60s      Ciclo 3 starts      Cache scaduta → SCANSIONE COMPLETA
```

**Risultato:** OGNI ciclo fa una scansione completa. Cache **INUTILE** per il control cycle.

### Scenario 2: METRICS_CACHE_TTL >= POLLING_INTERVAL (es: 60s >= 30s)

```
Tempo    Evento              Stato Cache
─────────────────────────────────────────────────
0s       Ciclo 1 starts      Cache vuota → SCANSIONE COMPLETA
0.5s     GetAllUserMetrics() Cache valida
30s      Ciclo 2 starts      Cache valida → DATI CACHED (stale!)
60s      Cache SCADUTA
60s      Ciclo 3 starts      Cache scaduta → SCANSIONE COMPLETA
```

**Risultato:** Cache utile ma dati **stale** per 1-2 cicli.

## Chi Chiama GetAllUserMetrics() per Ciclo

| Chiamante | Quando | Frequenza |
|-----------|--------|-----------|
| `collectSystemMetrics()` | Ogni ciclo | 1x |
| Pattern Detection | Ogni ciclo (se abilitato) | 1x |
| MCP tools | Su richiesta HTTP | Variabile |

**Totale chiamate per ciclo:** 2-3 volte (senza MCP)

## Domanda Critica: Quanto Dura un Ciclo?

Se `collectSystemMetrics()` impiega ~1-2 secondi per scansionare /proc:
- Ciclo 1: t=0s, GetAllUserMetrics() → scansione (2s)
- Ciclo 2: t=30s, GetAllUserMetrics() → cache scaduta (15s < 30s) → scansione (2s)

La cache **NON VIENE MAI USATA** nel control cycle con TTL=15s.

## Quando la Cache è Utile?

### ✅ CASI DOVE FUNZIONA

1. **Richieste MCP frequenti tra un ciclo e l'altro**
   - Se 10 richieste MCP arrivano nei 15s tra un ciclo e l'altro
   - Solo la prima fa scansione, le altre 9 usano cache
   - **Risparmio:** 9 scansioni /proc evitate

2. **POLLING_INTERVAL molto piccolo (es: 5s)**
   - Se TTL=15s e POLLING_INTERVAL=5s
   - Cache valida per 3 cicli consecutivi
   - **Risparmio:** 2 scansioni su 3 evitate

### ❌ CASI DOVE NON FUNZIONA

1. **Default attuale: TTL=15s, POLLING_INTERVAL=30s**
   - Cache SEMPRE scaduta all'inizio di ogni ciclo
   - Ogni ciclo = scansione completa
   - Cache utile SOLO per richieste MCP tra cicli

2. **Nessuna richiesta MCP**
   - Se MCP non usato, cache inutile con TTL < POLLING_INTERVAL

## Raccomandazioni

### Opzione A: Aumentare METRICS_CACHE_TTL
```bash
METRICS_CACHE_TTL=60  # Cache valida per 2 cicli
```
**Pro:** Riduce scansioni /proc del 50%  
**Contro:** Dati stale per fino a 60 secondi

### Opzione B: Allineare TTL a POLLING_INTERVAL
```bash
METRICS_CACHE_TTL=30  # Stesso del polling
```
**Pro:** Cache valida per tutto il ciclo corrente  
**Contro:** Prima richiesta del ciclo nuovo = sempre cache miss

### Opzione C: Rimuovere cache, scansionare sempre
```bash
# Rimuovere getFromCache/setInCache da GetAllUserMetrics()
```
**Pro:** Dati sempre freschi, codice più semplice  
**Contro:** MCP requests costose se frequenti

### Opzione D: Cache separata per MCP (consigliata)
```bash
# Mantenere cache SOLO per chiamate MCP esterne
# GetAllUserMetrics() nel control cycle = sempre scansione fresca
```
**Pro:** Dati freschi per decisioni, cache utile per MCP  
**Contro:** Complessità aggiuntiva

## Conclusione

**Con la configurazione default (TTL=15s, POLLING_INTERVAL=30s):**
- La cache è **INEFFICACE** per il control cycle
- È utile **SOLO** se ci sono richieste MCP frequenti
- Se non usi MCP, puoi impostare TTL=0 senza impatti

**Raccomandazione:** 
- Se MCP usato frequentemente: TTL=60
- Se MCP usato raramente: TTL=0 o rimuovi cache
- Se vuoi dati sempre freschi: rimuovi cache da GetAllUserMetrics()
