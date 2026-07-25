# Autoremediation Analysis

> **Status:** Analysis complete, not implemented
> **Date:** 2026-04-01

---

## 1. Auto-detect Workload Patterns

### Obiettivo
Riconoscere automaticamente il pattern di utilizzo di ogni utente (batch notturno, interattivo diurno, misto) e applicare policy di limiti differenziate senza configurazione manuale.

### Pattern da riconoscere

| Pattern | Caratteristiche | Esempio |
|---------|----------------|---------|
| **Batch notturno** | Alta CPU/RAM/IO tra 22:00-06:00, basso il resto | Backup, build CI/CD, ETL |
| **Interattivo diurno** | Uso moderato e costante 08:00-18:00 | Sviluppatori, utenti desktop |
| **Misto** | Picchi irregolari durante il giorno | Data scientist, analisi ad-hoc |
| **Sempre attivo** | Uso costante 24/7 | Servizi, daemon utente |
| **Sporadico** | Uso raro ma intenso quando attivo | Admin, deploy occasionali |

### Architettura proposta

```
state/
  pattern_detector.go    # Rilevamento pattern per utente
  policy_engine.go       # Applicazione policy basata su pattern

config/
  config.go              # Nuovi campi per autoremediation

metrics/
  collector.go           # Accumulo dati storici per pattern detection
```

### Configurazione

```bash
# Workload Pattern Detection
AUTODETECT_PATTERNS=false
PATTERN_HISTORY_HOURS=168          # Finestra storica (7 giorni default)
PATTERN_MIN_SAMPLES=24             # Minimo bucket orari distinti per decidere
PATTERN_CONFIDENCE_THRESHOLD=0.7   # Soglia confidenza (0.0-1.0)

# Policy per pattern
BATCH_NIGHT_CPU_QUOTA=200000       # CPU quota più alta per batch (200%)
BATCH_NIGHT_RAM_QUOTA=4G           # RAM quota più alta per batch
INTERACTIVE_CPU_QUOTA=50000        # CPU quota standard (50%)
INTERACTIVE_RAM_QUOTA=1G           # RAM quota standard
```

### Algoritmo di detection

```
Ogni POLLING_INTERVAL secondi:
  1. Accumula metriche per utente (CPU%, RAM, IO) in sliding window
  2. Ogni ora, calcola statistiche per fasce orarie (0-23)
  3. Classifica pattern basato su:
     - Varianza oraria (alta = batch, bassa = costante)
     - Fascia oraria dominante (notte/giorno)
     - Intensità media (alta/bassa)
  4. Se confidenza > threshold → applica policy corrispondente
  5. Logga cambio policy con motivo
```

### Dove integrare

```go
// In state/manager.go, nel control cycle:
func (m *Manager) controlCycle() {
    // ... existing logic ...
    
    if m.cfg.AutodetectPatterns {
        m.patternDetector.Update(metrics)
        
        for uid, pattern := range m.patternDetector.GetPatterns() {
            policy := m.policyEngine.GetPolicy(pattern)
            m.applyUserPolicy(uid, policy)
        }
    }
}
```

### Fattori nascosti

1. **Cold start**: Serve tempo (almeno 24-48 ore) prima che il detector abbia dati sufficienti. Nel frattempo, usare policy default.

2. **Pattern ibridi**: Un utente potrebbe essere "interattivo di giorno + batch di notte". Serve supportare policy multiple per fasce orarie.

3. **False positive**: Un deploy occasionale alle 3 di notte non deve classificare l'utente come "batch notturno". Serve smoothing e minimo campioni.

4. **Configurazione vs autodetect**: Se l'admin ha già configurato USER_INCLUDE_LIST/EXCLUDE_LIST, l'autodetect deve rispettarli, non sovrascriverli.

5. **Rollback automatico**: Se una policy applicata causa problemi (es. utente batch che diventa interattivo improvvisamente), serve un fallback rapido alla policy default.

6. **Storage**: I dati storici per la detection richiedono memoria. Con 100 utenti e 168 ore di storia, sono ~16800 datapoint. Meglio usare SQLite (già presente) che RAM.

### Stima effort

| Componente | Righe | Complessità |
|------------|-------|-------------|
| `state/pattern_detector.go` | ~200 | Alta |
| `state/policy_engine.go` | ~150 | Media |
| `config/config.go` (campi) | ~40 | Bassa |
| `database/` (storage pattern) | ~80 | Media |
| Integrazione in control cycle | ~60 | Media |
| Test + mock | ~150 | Alta |
| Docs + config example | ~50 | Bassa |
| **Totale** | **~730 righe** | **~5-6 ore** |

---

## 2. Auto-remediate IO Starvation

### Obiettivo
Rilevare quando un utente è IO-throttled per troppo tempo e aumentare temporaneamente i limiti IO per prevenire degradazione prolungata delle prestazioni.

### Cos'è IO starvation

Quando `io.max` limita un utente e questo continua a superare il limite, il kernel throttla le operazioni IO. Se la situazione persiste:
- I processi dell'utente diventano lentissimi
- Le code IO si accumulano
- L'utente percepisce il sistema come "bloccato"
- Possibile cascata: processi che accumulano memoria in attesa di IO → OOM

### Metriche di detection

| Metrica | Sorgente | Soglia |
|---------|----------|--------|
| **IO throttle duration** | Tempo continuo sopra `io.max` | > 300 secondi |
| **IO pressure (PSI)** | `/sys/fs/cgroup/.../io.pressure` | some avg10 > 50% |
| **IO ops queued** | Delta tra ops richieste e ops completate | > 100 ops in coda |
| **IO latency** | Tempo medio per operazione | > 100ms |

### Architettura proposta

```
state/
  io_remediation.go    # Detection + remediation IO starvation

cgroup/
  manager.go           # ApplyIOLimit() con limiti temporanei
  psi.go               # Lettura PSI (Pressure Stall Information)

config/
  config.go            # Nuovi campi IO remediation
```

### Configurazione

```bash
# IO Starvation Auto-Remediation
IO_REMEDIATION_ENABLED=false
IO_STARVATION_THRESHOLD=300        # Secondi di throttling continuo prima di agire
IO_STARVATION_CHECK_INTERVAL=30    # Frequenza check (secondi)
IO_BOOST_MULTIPLIER=2.0            # Moltiplicatore per limiti temporanei (2x)
IO_BOOST_DURATION=600              # Durata del boost (secondi, default 10 min)
IO_BOOST_MAX_PER_HOUR=3            # Max boost per utente per ora
IO_PSI_THRESHOLD=50                # PSI some avg10 % sopra cui considerare starvation
IO_REVERT_ON_NORMAL=true           # Revert limiti originali quando IO torna normale
```

### Algoritmo di remediation

```
Ogni IO_STARVATION_CHECK_INTERVAL secondi:
  Per ogni utente limitato per IO:
    1. Leggi io.pressure (PSI) dal cgroup
    2. Calcola durata throttling continuo
    3. Se PSI > threshold E durata > starvation_threshold:
       a. Verifica boost_max_per_hour non superato
       b. Applica limiti temporanei (limiti * BOOST_MULTIPLIER)
       c. Logga evento con motivo
       d. Avvia timer per revert (BOOST_DURATION)
    4. Se il timer di boost è scaduto, indipendentemente dal PSI corrente:
       a. Revert limiti originali
       b. Logga evento
```

### Dove integrare

```go
// In state/manager.go, nel control cycle:
func (m *Manager) controlCycle() {
    // ... existing logic ...
    
    if m.cfg.IOEnabled && m.cfg.IORemediationEnabled {
        m.ioRemediation.CheckAndRemediate(m.cgroupManager, m.cfg)
    }
}

// In cgroup/manager.go:
func (m *Manager) ApplyTemporaryIOLimit(uid int, multiplier float64) error {
    // Applica limiti temporanei = limiti_config * multiplier
    // Salva limiti originali per revert
}

func (m *Manager) RevertIOLimit(uid int) error {
    // Reverte ai limiti originali configurati
}
```

### PSI (Pressure Stall Information)

Il kernel Linux espone PSI in `/sys/fs/cgroup/<cgroup>/io.pressure`:

```
some avg10=25.00 avg60=18.50 avg300=12.30 total=1234567
full avg10=10.00 avg60=8.20 avg300=5.10 total=567890
```

- `some`: % di tempo in cui almeno un task è stallato per IO
- `full`: % di tempo in cui TUTTI i task sono stallati per IO
- `avg10/60/300`: medie mobili a 10/60/300 secondi
- `total`: microsecondi totali di stall

### Fattori nascosti

1. **PSI non disponibile su tutti i kernel**: Serve kernel >= 4.20 con `CONFIG_PSI=y`. Su kernel vecchi, fallback a euristica basata su `io.stat` e durata throttling.

2. **Boost infinito**: Se l'utente continua a superare anche i limiti boostati, serve un circuito di sicurezza. Dopo N boost consecutivi, non boostare più e loggare warning critico.

3. **Interazione con CPU/RAM limits**: Se un utente è IO-throttled ma anche CPU-limited, aumentare solo IO potrebbe non bastare. Servirebbe una visione olistica.

4. **Storage device differences**: SSD vs HDD hanno performance IO molto diverse. Un limite di 100MB/s è generoso su HDD ma stretto su NVMe. Idealmente, i limiti dovrebbero essere adattati al tipo di device.

5. **Multi-device**: Se un utente usa `/dev/sda` (lento) e `/dev/nvme0n1` (veloce), il boost dovrebbe essere applicato solo al device bottleneck.

6. **Notifiche**: Il boost dovrebbe triggerare una notifica (Telegram se configurato) per awareness dell'admin.

7. **Persistenza**: Se resman si riavvia durante un boost, deve sapere quali utenti avevano limiti temporanei al restart.

### Stima effort

| Componente | Righe | Complessità |
|------------|-------|-------------|
| `state/io_remediation.go` | ~250 | Alta |
| `cgroup/psi.go` (lettura PSI) | ~80 | Media |
| `cgroup/manager.go` (temporary limits) | ~60 | Media |
| `config/config.go` (campi) | ~40 | Bassa |
| Integrazione in control cycle | ~40 | Bassa |
| Test + mock | ~120 | Alta |
| Docs + config example | ~50 | Bassa |
| **Totale** | **~640 righe** | **~4-5 ore** |

---

## Priorità di implementazione

| Feature | Complessità | Valore | Priorità |
|---------|-------------|--------|----------|
| IO Remediation | Media | Alto | **1** |
| Workload Pattern Detection | Alta | Medio-Alto | **2** |

**Motivazione**: IO Remediation è più mirato, risolve un problema concreto (starvation) con algoritmo chiaro. Workload Pattern Detection richiede più dati storici e ha più edge case.
