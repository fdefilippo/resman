# ResMan v1.19.0 - memory.high Soft Limits

## Panoramica

ResMan v1.19.0 introduce il supporto per i limiti **memory.high** nei cgroups v2, fornendo un sistema di gestione della memoria più sofisticato con degradazione graduale prima dell'OOM killer.

---

## Cos'è memory.high?

### Differenza tra memory.high e memory.max

| Caratteristica | memory.high (Soft Limit) | memory.max (Hard Limit) |
|----------------|--------------------------|-------------------------|
| **Comportamento** | Throttling + reclaim aggressivo | OOM killer |
| **Processi uccisi** | ❌ Mai | ✅ Sì |
| **Superamento temporaneo** | ✅ Possibile | ⚠️ Raro |
| **Use case** | Warning, monitoring | Enforcement assoluto |

### Come Funziona

```
Configurazione Tipica (v1.19.0):
- memory.high = RAM_QUOTA_PER_USER × RAM_HIGH_RATIO (default: 80%)
- memory.max = RAM_QUOTA_PER_USER (100%)

Esempio con RAM_QUOTA_PER_USER=512M:
- memory.high = 512M × 0.8 = 410MB
- memory.max = 512M

Quando un utente supera 410MB:
→ Il kernel applica throttling sulle allocazioni
→ Reclaim aggressivo della memoria
→ NESSUN OOM killer

Quando un utente supera 512MB:
→ OOM killer termina i processi
```

---

## Configurazione

### Variabile RAM_HIGH_RATIO

```bash
# /etc/resman.conf

# Default: memory.high = 80% di memory.max
RAM_HIGH_RATIO=0.8

# Più conservativo (warning precedente al 70%)
RAM_HIGH_RATIO=0.7

# Più aggressivo (warning più tardi al 90%)
RAM_HIGH_RATIO=0.9

# Disabilita memory.high (comportamento legacy pre-v1.19.0)
RAM_HIGH_RATIO=0
```

### Validazione

Il valore deve essere tra `0.0` e `1.0`. Valori fuori range vengono rifiutati dalla validazione della configurazione.

---

## Monitoraggio

### Nuova Metrica Prometheus

```promql
resman_user_memory_high_breaches_total{uid, username, hostname, server_role}
```

**Descrizione:** Conta il numero di volte che ogni utente ha superato il limite soft `memory.high`.

**Esempi di Query:**

```promql
# Utenti con più breach negli ultimi 5 minuti
increase(resman_user_memory_high_breaches_total[5m]) > 0

# Top 5 utenti per memory pressure
topk(5, increase(resman_user_memory_high_breaches_total[1h]))

# Breach totali per hostname
sum by (hostname) (increase(resman_user_memory_high_breaches_total[24h]))
```

### Grafana Dashboard

Il dashboard aggiornato (v1.19.0) include il nuovo pannello:

**"Memory High Breaches (NEW v1.19.0)"**
- Posizione: Accanto a "All Users - Memory Usage Per User"
- Soglia gialla: 1 breach
- Soglia rossa: 10 breach
- Mostra max e last value per utente

---

## Interpretazione dei Dati

### Scenario Normale
```
memory.high breaches: 0-1/ora
→ Memoria ben gestita
→ Nessun action richiesta
```

### Scenario di Attenzione
```
memory.high breaches: 5-10/ora
→ Utente sotto pressione di memoria
→ Considerare aumento quota o ottimizzazione applicazione
```

### Scenario Critico
```
memory.high breaches: >10/ora OPPURE
memory.high breaches: costante aumento
→ Rischio OOM killer imminente
→ Action immediata richiesta
```

---

## Alerting

### Esempio Alert Rules (Prometheus)

```yaml
groups:
  - name: resman-memory-high
    interval: 30s
    rules:
      # Warning: utente con memory pressure
      - alert: ResManMemoryHighPressure
        expr: increase(resman_user_memory_high_breaches_total[15m]) > 5
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Utente {{ $labels.username }} sotto pressione di memoria"
          description: "{{ $labels.username }} ha superato memory.high {{ $value }} volte in 15 minuti"

      # Critical: rischio OOM
      - alert: ResManMemoryHighCritical
        expr: increase(resman_user_memory_high_breaches_total[5m]) > 10
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "Rischio OOM killer per utente {{ $labels.username }}"
          description: "{{ $labels.username }} ha superato memory.high {{ $value }} volte in 5 minuti - OOM killer imminente"

      # Info: trend crescente
      - alert: ResManMemoryHighTrend
        expr: predict_linear(resman_user_memory_high_breaches_total[1h], 3600) > 100
        for: 10m
        labels:
          severity: info
        annotations:
          summary: "Trend crescente memory breaches per {{ $labels.username }}"
          description: "Proiezione: {{ $value }} breach/ora tra 1 ora"
```

---

## Migration Guide

### Da v1.18.x a v1.19.0

**Default Behavior Change:**
- Se `RAM_LIMIT_ENABLED=true`, memory.high viene applicato automaticamente
- Default: 80% di memory.max

**Per mantenere comportamento legacy:**
```bash
# Disabilita memory.high
RAM_HIGH_RATIO=0
```

**Configurazione Consigliata:**
```bash
# Produzione (bilanciato)
RAM_LIMIT_ENABLED=true
RAM_QUOTA_PER_USER=512M
RAM_HIGH_RATIO=0.8
DISABLE_SWAP=false

# Sviluppo (più aggressivo)
RAM_LIMIT_ENABLED=true
RAM_QUOTA_PER_USER=1G
RAM_HIGH_RATIO=0.9
DISABLE_SWAP=false

# High-performance (minimo throttling)
RAM_LIMIT_ENABLED=true
RAM_QUOTA_PER_USER=2G
RAM_HIGH_RATIO=0.7
DISABLE_SWAP=true
```

---

## Troubleshooting

### memory.high breaches ma nessun OOM

**Normale:** memory.high è progettato per prevenire OOM.

**Action:**
1. Verifica se i breach sono frequenti (>10/ora)
2. Controlla trend con `increase()[1h]`
3. Se stabile, nessun action necessaria

### memory.high = memory.max (nessun effetto)

**Causa:** `RAM_HIGH_RATIO=1.0` o `RAM_HIGH_RATIO=0`

**Fix:**
```bash
# Imposta ratio valido
RAM_HIGH_RATIO=0.8

# Riavvia resman
systemctl restart resman
```

### Verifica applicazione limiti

```bash
# Controlla memory.high per un utente
cat /sys/fs/cgroup/resman/user-1000.slice/memory.high

# Controlla memory.max
cat /sys/fs/cgroup/resman/user-1000.slice/memory.max

# Verifica breach
cat /sys/fs/cgroup/resman/user-1000.slice/memory.events | grep high
```

---

## API e Interfaccia

### Nuove Funzioni Cgroup Manager

```go
// Applica soft limit
ApplyRAMHigh(uid int, limit string) error

// Applica entrambi i limiti
ApplyRAMLimitWithHigh(uid int, maxLimit string, highLimit string) error

// Applica con swap disabilitato
ApplyRAMLimitWithHighAndSwapDisabled(uid int, maxLimit string, highLimit string) error

// Rimuove soft limit
RemoveRAMHigh(uid int) error

// Conta breach
GetMemoryHighEvents(uid int) (uint64, error)
```

### Esempio Utilizzo

```go
// Calcola memory.high come 80% di memory.max
quotaBytes, _ := config.ParseRAMQuota("512M")
highBytes := uint64(float64(quotaBytes) * 0.8)

// Applica entrambi i limiti
err := mgr.ApplyRAMLimitWithHigh(
    uid,
    "536870912",  // memory.max = 512M
    "429496730",  // memory.high = 410M (80%)
)
```

---

## Performance Impact

### Overhead memory.high

- **Lettura memory.events:** < 1ms per utente
- **Scrittura memory.high:** < 5ms
- **Throttling kernel:** Variabile (dipende da pressione memoria)

### Benchmark

In test con 50 utenti attivi:
- Ciclo di controllo: +2-3ms con memory.high abilitato
- Memoria aggiuntiva: ~100KB per cache eventi

---

## Best Practices

### 1. Monitoraggio Proattivo

```promql
# Dashboard query per identificare problemi
topk(10, increase(resman_user_memory_high_breaches_total[1h]))
```

### 2. Sizing Quote

- **Development:** RAM_HIGH_RATIO=0.9 (più margine)
- **Production:** RAM_HIGH_RATIO=0.8 (bilanciato)
- **Critical:** RAM_HIGH_RATIO=0.7 (warning precoce)

### 3. Alerting a Livelli

1. **Info:** Trend crescente (>50/ora proiezione)
2. **Warning:** >5 breach in 15min
3. **Critical:** >10 breach in 5min

### 4. Capacity Planning

Usa i dati storici per:
- Identificare utenti con crescita costante
- Pianificare aumenti quota prima di OOM
- Ottimizzare distribuzione risorse

---

## Riferimenti

- [Kernel Documentation - memory.high](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory)
- [CGROUP-V2-TECHNICAL.md](CGROUP-V2-TECHNICAL.md) - Technical reference completo
- [CHANGELOG.md](../CHANGELOG.md) - Release notes v1.19.0

---

## Supporto

Per issue o domande:
- GitHub Issues: https://github.com/fdefilippo/resman/issues
- Documentazione: /usr/share/doc/resman/

---

**Ultimo Aggiornamento:** 2026-03-31  
**Versione:** ResMan v1.19.0
