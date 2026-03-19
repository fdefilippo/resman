# LDAP/NIS Username Resolution Guide

## Panoramica

CPU Manager Go supporta la risoluzione dei nomi utente da LDAP/NIS quando compilato con CGO abilitato.

## Requisiti

### 1. Sistema Operativo

- Linux con NSS (Name Service Switch) configurato
- Pacchetti NSS LDAP installati:
  - **RHEL/CentOS**: `nss-pam-ldapd` o `sssd-ldap`
  - **Debian/Ubuntu**: `libnss-ldap` o `sssd`
  - **SUSE**: `nss-pam-ldap`

### 2. Configurazione NSS

Verifica che `/etc/nsswitch.conf` includa LDAP per gli utenti:

```bash
# /etc/nsswitch.conf
passwd:         files ldap
group:          files ldap
shadow:         files ldap
```

### 3. Test Risoluzione Utenti

Verifica che la risoluzione LDAP funzioni:

```bash
# Test con getent (funziona con LDAP/NIS)
getent passwd 10001
# Dovrebbe restituire: username:x:10001:10001:User Name:/home/username:/bin/bash

# Test con id
id 10001
# Dovrebbe restituire: uid=10001(username) gid=10001(groupname) ...
```

## Compilazione con CGO

### 1. Installa GCC

```bash
# RHEL/CentOS
sudo yum install gcc

# Debian/Ubuntu
sudo apt-get install gcc

# SUSE
sudo zypper install gcc
```

### 2. Compila con CGO Abilitato

```bash
# Build standard con CGO
CGO_ENABLED=1 go build -v -o cpu-manager-go .

# Build ottimizzata per produzione
CGO_ENABLED=1 go build -v -ldflags="-s -w" -o cpu-manager-go .

# Build con simboli di debug
CGO_ENABLED=1 go build -v -gcflags="all=-N -l" -o cpu-manager-go .
```

### 3. Verifica Build

```bash
# Verifica che il binario sia linkato con libc
ldd /usr/bin/cpu-manager-go | grep libc
# Dovrebbe mostrare: libc.so.6 => /lib64/libc.so.6

# Verifica versione
./cpu-manager-go --version
```

## Configurazione

### 1. CPU Manager Configuration

Nessuna configurazione speciale necessaria. CPU Manager userà automaticamente NSS per risolvere gli UID.

```bash
# /etc/cpu-manager.conf
# Nessuna impostazione speciale necessaria
# La risoluzione LDAP è automatica
```

### 2. Prometheus Metrics

Le metriche includeranno i nomi utente risolti da LDAP:

```promql
# Prima (senza LDAP):
cpu_manager_user_cpu_usage_percent{uid="10001", username="10001"}

# Dopo (con LDAP):
cpu_manager_user_cpu_usage_percent{uid="10001", username="ldap-user-01"}
```

## Troubleshooting

### Problema: Username ancora numerici

**Sintomi:**
- Le metriche mostrano `username="10001"` invece di `username="ldap-user-01"`

**Cause possibili:**

1. **CGO non abilitato in compilazione**
   ```bash
   # Verifica
   ldd /usr/bin/cpu-manager-go | grep libc
   # Se non mostra libc, CGO non era abilitato
   
   # Ricompila
   CGO_ENABLED=1 go build -o cpu-manager-go .
   ```

2. **NSS non configurato per LDAP**
   ```bash
   # Verifica
   grep "^passwd:" /etc/nsswitch.conf
   # Dovrebbe mostrare: passwd: files ldap
   
   # Se non c'è ldap, aggiungi
   sudo vi /etc/nsswitch.conf
   ```

3. **LDAP non raggiungibile**
   ```bash
   # Test connessione LDAP
   getent passwd 10001
   # Se non restituisce nulla, LDAP non è raggiungibile
   
   # Verifica servizio LDAP
   systemctl status nslcd    # Per nss-pam-ldapd
   systemctl status sssd     # Per SSSD
   ```

4. **Utente non esiste in LDAP**
   ```bash
   # Cerca utente in LDAP
   ldapsearch -x -b "dc=example,dc=com" "(uidNumber=10001)"
   
   # O con getent
   getent passwd 10001
   ```

### Problema: Risoluzione lenta

**Sintomi:**
- CPU Manager impiega molto tempo per avviare
- Log mostrano timeout nella risoluzione UID

**Soluzioni:**

1. **Aumenta timeout NSS**
   ```bash
   # /etc/nsswitch.conf
   passwd:         files ldap [NOTFOUND=return]
   # [NOTFOUND=return] evita di cercare oltre se non trovato
   ```

2. **Configura cache SSSD**
   ```bash
   # /etc/sssd/sssd.conf
   [sssd]
   entry_cache_timeout = 600
   entry_cache_negative_timeout = 120
   ```

3. **Riduci SYSTEM_UID_MAX**
   ```bash
   # /etc/cpu-manager.conf
   SYSTEM_UID_MAX=10000  # Invece di 60000
   # Monitora solo UID fino a 10000
   ```

### Problema: Errori "user: unknown userid"

**Sintomi:**
- Log mostrano: `Failed to lookup user: user: unknown userid 10001`

**Soluzioni:**

1. **Verifica che l'utente esista**
   ```bash
   getent passwd 10001
   ```

2. **Controlla permessi di lettura LDAP**
   ```bash
   # Test con utente di bind
   ldapsearch -x -D "cn=admin,dc=example,dc=com" -W -b "dc=example,dc=com" "(uidNumber=10001)"
   ```

3. **Abilita logging NSS**
   ```bash
   # Per debug avanzato
   export NSS_DEBUG=1
   /usr/bin/cpu-manager-go --config /etc/cpu-manager.conf
   ```

## Esempio Configurazione Completa

### RHEL/CentOS 8+ con SSSD

```bash
# 1. Installa pacchetti
sudo yum install sssd-ldap sssd-tools nss-pam-ldapd

# 2. Configura SSSD
sudo vi /etc/sssd/sssd.conf
[sssd]
services = nss, pam
domains = ldap

[nss]
filter_users = root
filter_groups = root
entry_cache_timeout = 600

[domain/ldap]
id_provider = ldap
ldap_uri = ldap://ldap.example.com
ldap_search_base = dc=example,dc=com
ldap_id_use_start_tls = True
cache_credentials = True

# 3. Configura NSS
sudo vi /etc/nsswitch.conf
passwd:     files sss
group:      files sss
shadow:     files sss

# 4. Riavvia servizi
sudo systemctl enable sssd
sudo systemctl start sssd

# 5. Test
getent passwd 10001

# 6. Compila CPU Manager
cd /path/to/cpu-manager-go
CGO_ENABLED=1 go build -v -ldflags="-s -w" -o cpu-manager-go .

# 7. Installa
sudo cp cpu-manager-go /usr/bin/
sudo systemctl restart cpu-manager

# 8. Verifica metriche
curl -s http://localhost:1974/metrics | grep user_cpu
```

### Debian/Ubuntu con libnss-ldap

```bash
# 1. Installa pacchetti
sudo apt-get install libnss-ldap libpam-ldap ldap-utils

# 2. Configura NSS
sudo vi /etc/nsswitch.conf
passwd:         files ldap
group:          files ldap
shadow:         files ldap

# 3. Configura LDAP
sudo vi /etc/libnss-ldap.conf
uri ldap://ldap.example.com
base dc=example,dc=com
bind_policy soft

# 4. Test
getent passwd 10001

# 5. Compila CPU Manager
cd /path/to/cpu-manager-go
CGO_ENABLED=1 go build -v -ldflags="-s -w" -o cpu-manager-go .

# 6. Installa
sudo cp cpu-manager-go /usr/bin/
sudo systemctl restart cpu-manager

# 7. Verifica metriche
curl -s http://localhost:1974/metrics | grep user_cpu
```

## Best Practices

### 1. Cache

Configura cache per ridurre query LDAP:

```bash
# SSSD: /etc/sssd/sssd.conf
[sssd]
entry_cache_timeout = 600           # 10 minuti
entry_cache_negative_timeout = 120  # 2 minuti per negativi
```

### 2. Timeout

Imposta timeout ragionevoli:

```bash
# /etc/nsswitch.conf
passwd:     files ldap [NOTFOUND=return]
# [NOTFOUND=return] evita query inutili
```

### 3. Monitoring

Monitora lo stato LDAP:

```bash
# Script di monitoring
#!/bin/bash
if ! getent passwd 10001 > /dev/null; then
    echo "CRITICAL: LDAP non raggiungibile"
    exit 2
fi
echo "OK: LDAP funzionante"
exit 0
```

### 4. Fallback

Configura fallback locale:

```bash
# /etc/nsswitch.conf
passwd:     files ldap [UNAVAIL=return]
# Se LDAP non disponibile, usa solo files locali
```

## Verifica Finale

Dopo aver configurato tutto, verifica:

```bash
# 1. Risoluzione UID
getent passwd 10001

# 2. Metriche Prometheus
curl -s http://localhost:1974/metrics | grep "uid=\"10001\""
# Dovresti vedere username, non "10001"

# 3. Log CPU Manager
tail -f /var/log/cpu-manager.log | grep -i "user"
# Dovresti vedere username, non UID numerici
```

---

**Versione:** 1.0  
**Compatibilità:** CPU Manager Go v1.13.1+  
**Ultimo Aggiornamento:** Marzo 2026
