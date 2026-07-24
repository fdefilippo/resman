# ResMan v1.20.0 - IO Limits

## Panoramica

ResMan v1.20.0 introduce il supporto per i limiti di **block I/O** nei cgroups v2 tramite il controller `io`, permettendo di controllare la banda e le operazioni di I/O su disco per ogni utente.

---

## Cos'è il controller `io`?

Il controller `io` di cgroups v2 permette di:
- Limitare la **banda di lettura/scrittura** (bytes/secondo)
- Limitare le **operazioni di I/O** (IOPS) per lettura e scrittura
- Monitorare le statistiche di I/O per dispositivo

### Differenza tra CPU, RAM e IO limits

| Limite | Comportamento quando superato |
|--------|-------------------------------|
| CPU (`cpu.max`) | Throttling (pausa fino al periodo successivo) |
| RAM (`memory.max`) | OOM killer (terminazione processi) |
| RAM (`memory.high`) | Throttling + reclaim aggressivo |
| **IO (`io.max`)** | **Throttling delle operazioni di I/O** |

---

## Configurazione

### Variabili

| Variabile | Default | Descrizione |
|-----------|---------|-------------|
| `IO_LIMIT_ENABLED` | `false` | Abilita/disabilita limiti IO |
| `IO_THRESHOLD` | `75` | Soglia % per attivare limiti |
| `IO_RELEASE_THRESHOLD` | `40` | Soglia % per rilasciare limiti |
| `IO_READ_BPS` | `100M` | Limite banda lettura per utente |
| `IO_WRITE_BPS` | `50M` | Limite banda scrittura per utente |
| `IO_READ_IOPS` | `1000` | Limite IOPS lettura per utente |
| `IO_WRITE_IOPS` | `500` | Limite IOPS scrittura per utente |
| `IO_DEVICE_FILTER` | `all` | Enumera tutti i device interi in `/sys/block`, oppure usa un device `major:minor` specifico |

### Formato bande

Supporta suffissi: `K`, `M`, `G`, `T` (base 1024).

```bash
IO_READ_BPS=104857600   # 100 MB/s in bytes
IO_READ_BPS=100M        # 100 MB/s con suffisso
IO_READ_BPS=max         # Nessun limite
```

### Esempi di configurazione

```bash
# Limiti IO disabilitati (default)
IO_LIMIT_ENABLED=false

# Abilita con limiti moderati
IO_LIMIT_ENABLED=true
IO_THRESHOLD=75
IO_RELEASE_THRESHOLD=40
IO_READ_BPS=100M
IO_WRITE_BPS=50M
IO_READ_IOPS=1000
IO_WRITE_IOPS=500

# Modalità strict (server database)
IO_LIMIT_ENABLED=true
IO_THRESHOLD=80
IO_RELEASE_THRESHOLD=50
IO_READ_BPS=50M
IO_WRITE_BPS=25M
IO_READ_IOPS=500
IO_WRITE_IOPS=250

# Solo limiti banda (nessun limite IOPS)
IO_LIMIT_ENABLED=true
IO_READ_BPS=200M
IO_WRITE_BPS=100M
IO_READ_IOPS=0
IO_WRITE_IOPS=0
```

---

## Monitoraggio

### Metriche Prometheus

```
resman_user_io_read_bytes_total{uid, username}
resman_user_io_write_bytes_total{uid, username}
resman_user_io_read_ops_total{uid, username}
resman_user_io_write_ops_total{uid, username}
```

### Query utili

```promql
# Top 5 utenti per banda di lettura
topk(5, rate(resman_user_io_read_bytes_total[5m]))

# Banda totale di scrittura per hostname
sum by (hostname) (rate(resman_user_io_write_bytes_total[5m]))

# IOPS totali per utente
sum by (username) (rate(resman_user_io_read_ops_total[5m]) + rate(resman_user_io_write_ops_total[5m]))
```

---

## Come funziona

### Attivazione

Quando `IO_LIMIT_ENABLED=true` e l'utente supera `IO_THRESHOLD`:
1. Con `IO_DEVICE_FILTER=all`, ResMan enumera i dispositivi interi presenti in
   `/sys/block` e scrive una riga per ogni `major:minor` in `<cgroup>/io.max`:
   ```
   8:0 rbps=104857600 wbps=52428800 riops=1000 wiops=500
   259:0 rbps=104857600 wbps=52428800 riops=1000 wiops=500
   ```
2. Il kernel limita le operazioni di I/O del cgroup
3. Le operazioni eccedenti vengono ritardate (throttling)

### Disattivazione

Quando l'utente scende sotto `IO_RELEASE_THRESHOLD`:
1. ResMan rimuove i limiti per tutti i device presenti in `<cgroup>/io.max`:
   ```
   8:0 rbps=max wbps=max riops=max wiops=max
   259:0 rbps=max wbps=max riops=max wiops=max
   ```
2. I limiti vengono rimossi

### Statistiche

ResMan legge le statistiche da `<cgroup>/io.stat`:
```
8:0 rios=1234 wios=567 rbytes=104857600 wbytes=52428800
259:0 rios=100 wios=50 rbytes=10485760 wbytes=5242880
```

I valori vengono aggregati per tutti i dispositivi e esposti come metriche Prometheus.

---

## Troubleshooting

### Limiti non applicati

Verificare che:
1. `IO_LIMIT_ENABLED=true`
2. Il kernel supporta il controller `io`:
   ```bash
   cat /sys/fs/cgroup/cgroup.controllers | grep io
   ```
3. Il controller è abilitato:
   ```bash
   cat /sys/fs/cgroup/cgroup.subtree_control | grep io
   ```

### Verifica limiti attivi

```bash
# Controlla limiti IO per un utente
cat /sys/fs/cgroup/resman/user_1000/io.max

# Controlla statistiche IO
cat /sys/fs/cgroup/resman/user_1000/io.stat
```

### Trovare major:minor di un dispositivo

```bash
lsblk -o NAME,MAJ:MIN
# Output: sda   8:0
#         nvme0n1 259:0
```

---

## Riferimenti

- [Kernel Documentation - io controller](https://docs.kernel.org/admin-guide/cgroup-v2.html#io)
- [CGROUP-V2-TECHNICAL.md](CGROUP-V2-TECHNICAL.md)

---

**Versione:** ResMan v1.20.0
