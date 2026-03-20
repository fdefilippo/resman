# Project Rename: cpu-manager-go → resman

Guida step-by-step per rinominare il progetto mantenendo la cronologia git.

---

## FASE 1: Preparazione Locale

### 1.1 Crea branch per il rename
```bash
git checkout -b rename-to-resman
```

### 1.2 Identifica tutti i file con riferimenti al nome

**Package/Import path (devono cambiare):**
- [ ] `github.com/fdefilippo/cpu-manager-go` → `github.com/fdefilippo/resman`
- [ ] `cpu-manager-go` → `resman` in tutti i package comment

**File e directory:**
- [ ] Rinomina directory principale: `cpu-manager-go/` → `resman/`
- [ ] File README.md
- [ ] File LICENSE (se necessario)
- [ ] File goreleaser.yml
- [ ] File .goreleaser.yml
- [ ] Directory `packaging/` (RPM spec file name)
- [ ] Directory `docs/` (reference ai file)

### 1.3 Go Module

**go.mod:**
```diff
- module github.com/fdefilippo/cpu-manager-go
+ module github.com/fdefilippo/resman
```

**Tutti i file .go:**
```bash
# Cerca tutti i riferimenti
grep -r "cpu-manager-go" --include="*.go" .

# Sostituisci import paths
sed -i 's|github.com/fdefilippo/cpu-manager-go|github.com/fdefilippo/resman|g' \
  $(find . -name "*.go" -type f)

# Sostituisci package comments
sed -i 's|cpu-manager-go|resman|g' \
  $(find . -name "*.go" -type f)
```

### 1.4 Documentazione

```bash
# README.md
sed -i 's|cpu-manager-go|resman|g' README.md
sed -i 's|CPU Manager Go|ResMan|g' README.md

# CHANGELOG.md
sed -i 's|cpu-manager-go|resman|g' CHANGELOG.md

# File .md nella directory docs/
sed -i 's|cpu-manager-go|resman|g' docs/*.md

# File di configurazione
sed -i 's|cpu-manager-go|resman|g' *.yml *.yaml *.toml 2>/dev/null
```

### 1.5 RPM Package

**packaging/rpm/cpu-manager-go.spec:**
- [ ] Rinomina file: `cpu-manager-go.spec` → `resman.spec`
- [ ] Contenuto: `Name: cpu-manager-go` → `Name: resman`
- [ ] Tutti i riferimenti `cp /usr/bin/cpu-manager-go` → `cp /usr/bin/resman`
- [ ] User/group `cpu-manager` → `resman`
- [ ] Directory `/etc/cpu-manager/` → `/etc/resman/`
- [ ] Directory `/var/run/cpu-manager/` → `/var/run/resman/`
- [ ] Directory `/var/log/cpu-manager/` → `/var/log/resman/`
- [ ] Systemd unit `cpu-manager.service` → `resman.service`

### 1.6 Systemd Service

**packaging/systemd/cpu-manager.service:**
- [ ] Rinomina: `cpu-manager.service` → `resman.service`
- [ ] `ExecStart=` riferimento al binary
- [ ] `User=` e `Group=` (se usi cpu-manager)
- [ ] `RuntimeDirectory=cpu-manager` → `RuntimeDirectory=resman`
- [ ] `LogsDirectory=cpu-manager` → `LogsDirectory=resman`
- [ ] `ConfigurationDirectory=cpu-manager` → `ConfigurationDirectory=resman`

### 1.7 Scripts

**docs/scripts/ (o packaging/scripts/):**
- [ ] `cpu-manager.sh` → `resman.sh`
- [ ] Tutti gli script con riferimenti al nome

### 1.8 GitHub Actions (se presenti)

**`.github/workflows/*.yml`:**
- [ ] Riferimenti al modulo
- [ ] Nomi dei job
- [ ] Path conditions

### 1.9 Docker (se presente)

- [ ] Dockerfile
- [ ] docker-compose.yml
- [ ] .dockerignore
- [ ] Image names nei workflow

---

## FASE 2: Verifica Locale

```bash
# Verifica che non ci siano più riferimenti
grep -r "cpu-manager-go" --include="*.go" .
grep -r "cpu-manager" --include="*.go" --include="*.md" --include="*.yml" --include="*.yaml" --include="*.spec" .

# Build locale
go build -o resman ./...

# Test
go test ./...

# Verifica RPM spec (se disponibile)
rpmlint packaging/rpm/resman.spec
```

---

## FASE 3: Commit Locale

```bash
git add -A
git commit -m "Rename project: cpu-manager-go → resman"
```

---

## FASE 4: GitHub Repository Rename

### 4.1 Sul sito GitHub
1. Vai su: https://github.com/fdefilippo/cpu-manager-go/settings
2. Clicca "Rename" sotto "Repository name"
3. Cambia `cpu-manager-go` → `resman`
4. Conferma

### 4.2 Dopo il rename su GitHub
```bash
# Aggiorna remote URL
git remote set-url origin https://github.com/fdefilippo/resman.git

# Verifica
git remote -v
```

---

## FASE 5: Push e Merge

```bash
# Push al nuovo remote
git push -u origin rename-to-resman

# Crea Pull Request su GitHub
gh pr create --title "Rename project: cpu-manager-go → resman" \
  --body "Rinominato il progetto da cpu-manager-go a resman per migliore identificazione del brand."

# Merge (via GitHub UI o CLI)
gh pr merge --squash
```

---

## FASE 6: Post-Rename

### 6.1 Aggiorna bookmark e clone locali
```bash
# Altri developer devono:
git remote set-url origin https://github.com/fdefilippo/resman.git
```

### 6.2 Aggiorna dipendenze esterne
Se altri progetti dipendono da `github.com/fdefilippo/cpu-manager-go`:
```bash
# In quei progetti
go get github.com/fdefilippo/resman@latest
```

### 6.3 Redirect GitHub (automatico)
GitHub dovrebbe creare automaticamente un redirect da `cpu-manager-go` → `resman`.

### 6.4 Update integrations
- [ ] Codecov/Coveralls
- [ ] Dependabot
- [ ] Travis CI / CircleCI / altre CI
- [ ] Package registries (Homebrew, etc.)

---

## Checklist Finale

- [ ] Go module path aggiornato
- [ ] Tutti gli import path aggiornati
- [ ] README e documentazione aggiornata
- [ ] RPM spec rinominato e aggiornato
- [ ] Systemd unit rinominata e aggiornata
- [ ] Scripts rinominati e aggiornati
- [ ] GitHub repo rinominato
- [ ] Remote URL aggiornato
- [ ] Build funziona
- [ ] Test passano
- [ ] RPM package si costruisce correttamente

---

## Comandi Quick-Reference

```bash
# Sostituzione massiva (da eseguire nella root del progetto)
find . -type f \( -name "*.go" -o -name "*.md" -o -name "*.yml" -o -name "*.yaml" -o -name "*.spec" -o -name "*.service" -o -name "Dockerfile" \) \
  -exec sed -i 's|cpu-manager-go|resman|g; s|CPU Manager Go|ResMan|g; s|cpu-manager\\.service|resman.service|g; s|Name: cpu-manager-go|Name: resman|g' {} +

# Rinomina directory principale
cd .. && mv cpu-manager-go resman && cd resman

# Verifica finale
grep -r "cpu-manager" --include="*.go" . && echo "ANCORA RIFERIMENTI!" || echo "OK - Nessun riferimento residuo"
```
