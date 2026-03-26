# GitHub Actions Workflows

Questa directory contiene i workflow GitHub Actions per automatizzare la build, test e release di ResMan.

## Workflow Disponibili

### 1. `release-packages.yml` - Build e Release

**Trigger:**
- Push su `main`/`master`
- Pull request
- Tag `v*` (release)

**Jobs:**
- **test**: Esegue test e linting
- **build-deb**: Build pacchetti Debian/Ubuntu (amd64, arm64)
- **build-rpm**: Build pacchetti RPM (amd64, arm64)
- **build-static**: Build binari statici (amd64, arm64)
- **verify-packages**: Verifica l'installazione dei pacchetti
- **release**: Crea GitHub Release con tutti gli artifact (solo su tag)

### 2. `nightly-build.yml` - Build Nightly

**Trigger:**
- Schedule: ogni giorno alle 2:00 UTC
- Manuale (`workflow_dispatch`)

**Caratteristiche:**
- Crea build giornaliere dal branch main
- Pubblica come release "nightly" con tag `nightly`
- Gli artifact vengono mantenuti per 7 giorni

### 3. `docker-publish.yml` - Docker

**Trigger:**
- Push su `main`/`master`
- Tag `v*`

**Caratteristiche:**
- Build multi-architettura (amd64, arm64)
- Pubblica su GitHub Container Registry (ghcr.io)
- Genera SBOM (Software Bill of Materials)
- Usa cache per velocizzare build

## Configurazione

### Secrets Richiesti

Il workflow usa `GITHUB_TOKEN` (autogenerato), non richiede secrets aggiuntivi per il funzionamento base.

### Secrets Opzionali

Se vuoi firmare i pacchetti GPG o pubblicare su repository esterni:

| Secret | Descrizione |
|--------|-------------|
| `GPG_PRIVATE_KEY` | Chiave privata GPG per firma pacchetti |
| `GPG_PASSPHRASE` | Passphrase della chiave GPG |
| `PACKAGECLOUD_TOKEN` | Token per pubblicazione su PackageCloud |

## Come Usare

### Creare una Release

1. Crea un tag semver:
   ```bash
   git tag -a v1.18.2 -m "Release v1.18.2"
   git push origin v1.18.2
   ```

2. Il workflow partirà automaticamente
3. Al completamento, troverai la release su GitHub con:
   - `.deb` per Debian/Ubuntu
   - `.rpm` per RHEL/CentOS/Rocky Linux
   - Binari statici
   - Container Docker su ghcr.io

### Installazione dai Pacchetti

**Debian/Ubuntu:**
```bash
# Scarica l'ultima release
curl -LO https://github.com/OWNER/resman/releases/latest/download/resman-v1.18.2-linux-amd64.deb

# Installa
sudo dpkg -i resman-v1.18.2-linux-amd64.deb
sudo apt-get install -f  # Fix dipendenze

# Avvia
sudo systemctl enable --now resman
```

**RHEL/CentOS/Rocky Linux:**
```bash
# Scarica
curl -LO https://github.com/OWNER/resman/releases/latest/download/resman-v1.18.2-linux-amd64.rpm

# Installa
sudo rpm -ivh resman-v1.18.2-linux-amd64.rpm

# Avvia
sudo systemctl enable --now resman
```

**Docker:**
```bash
docker pull ghcr.io/OWNER/resman:latest
```

## Troubleshooting

### Build RPM fallisce

Se il build RPM fallisce, verifica:
- Il file `packaging/rpm/resman.spec` ha la versione corretta
- Le dipendenze BuildRequires sono disponibili nel container

### Build ARM64 lento

Il build ARM64 usa QEMU ed è significativamente più lento. Considera:
- Usare runner self-hosted con architettura ARM64
- O eseguire build ARM64 solo su tag release

### Artifact non trovati

Verifica che i pacchetti siano stati effettivamente creati:
```bash
# Controlla i log del workflow
# Cerca "Package: " nel log di build
```

## Personalizzazioni

### Aggiungere Architetture

Modifica la matrix nei workflow:
```yaml
strategy:
  matrix:
    arch: [amd64, arm64, arm/v7]  # Aggiungi architetture
```

### Cambiare Distribuzioni RPM

Per supportare multiple distribuzioni RPM:
```yaml
strategy:
  matrix:
    include:
      - distro: rockylinux
        version: "9"
      - distro: fedora
        version: "39"
```

### Firma GPG

Per firmare i pacchetti, aggiungi questo step:
```yaml
- name: Import GPG key
  uses: crazy-max/ghaction-import-gpg@v5
  with:
    gpg_private_key: ${{ secrets.GPG_PRIVATE_KEY }}
    passphrase: ${{ secrets.GPG_PASSPHRASE }}

- name: Sign packages
  run: |
    gpg --armor --detach-sign *.rpm
    gpg --armor --detach-sign *.deb
```

## Badge

Aggiungi questo badge al README:
```markdown
![Build Packages](https://github.com/OWNER/resman/workflows/Build%20and%20Release%20Packages/badge.svg)
```
