# Makefile per resman
# Author: Francesco Defilippo <francesco@defilippo.org>
# License: GPLv3

# ============================================================================
# VARIABILI CONFIGURABILI
# ============================================================================

# Nome del progetto
PROJECT_NAME = resman
VERSION = 1.18.1
RELEASE = 1

# Percorsi
GO = go
GOLANGCI_LINT = golangci-lint
GORELEASER = goreleaser

# Build directories
BUILD_DIR = build
DIST_DIR = dist
RPMBUILD_DIR = $(HOME)/rpmbuild
DEB_BUILD_DIR = $(BUILD_DIR)/deb
BIN_DIR = /usr/bin
CONF_DIR = /etc
SYSTEMD_DIR = /usr/lib/systemd/system

# Go parameters
# CGO is required for user name resolution via NSS (LDAP, NIS, SSSD support)
CGO_ENABLED = 1
export CGO_ENABLED

GO_FLAGS = -v
GO_LDFLAGS = -ldflags="-s -w -X 'main.version=$(VERSION)-$(RELEASE)'"
GO_TAGS =

# CGO compiler settings
export CC = gcc
export CGO_CFLAGS = -O2
export CGO_LDFLAGS = -lresolv

# Architetture supportate
ARCHES = amd64 arm64
OSES = linux

# Debian packaging
DEB_ARCH_amd64 = amd64
DEB_ARCH_arm64 = arm64
DEB_MAINTAINER = Francesco Defilippo <francesco@defilippo.org>
DEB_DESCRIPTION = Resource Manager - Sistema di gestione delle risorse

# ============================================================================
# TARGET PRINCIPALI
# ============================================================================

.PHONY: all build clean test lint install uninstall rpm deb docker help

all: clean test lint build

# ============================================================================
# SVILUPPO E BUILD
# ============================================================================

# Build locale per sviluppo
build: deps
	@echo "Building $(PROJECT_NAME)..."
	$(GO) build $(GO_FLAGS) $(GO_LDFLAGS) $(GO_TAGS) -o $(PROJECT_NAME)
	@echo "Build completato: ./$(PROJECT_NAME)"

# Build per release (multi-architettura)
release: deps test lint
	@echo "Building release binaries for multiple architectures..."
	@mkdir -p $(BUILD_DIR)
	@for os in $(OSES); do \
		for arch in $(ARCHES); do \
			echo "Building for $$os/$$arch..."; \
			GOOS=$$os GOARCH=$$arch $(GO) build $(GO_FLAGS) $(GO_LDFLAGS) $(GO_TAGS) \
			-o $(BUILD_DIR)/$(PROJECT_NAME)-$(VERSION)-$$os-$$arch; \
		done \
	done
	@echo "Release binaries disponibili in: $(BUILD_DIR)/"

# Build statico (senza dipendenze C)
static: deps
	@echo "Building static binary..."
	CGO_ENABLED=0 $(GO) build $(GO_FLAGS) $(GO_LDFLAGS) -a -installsuffix cgo -o $(PROJECT_NAME)-static
	@echo "Static binary build completato: ./$(PROJECT_NAME)-static"

# ============================================================================
# TEST E QUALITÀ
# ============================================================================

# Esegui test unitari
test: deps
	@echo "Running tests..."
	$(GO) test -v -cover ./...

# Test con coverage
test-cover: deps
	@echo "Running tests with coverage..."
	$(GO) test -v -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generato: coverage.html"

# Linting del codice
lint: deps
	@echo "Running linters..."
	@if command -v $(GOLANGCI_LINT) >/dev/null 2>&1; then \
		$(GOLANGCI_LINT) run; \
	else \
		echo "golangci-lint non installato, eseguendo go vet..."; \
		$(GO) vet ./...; \
	fi

# Formatta il codice
fmt:
	@echo "Formatting code..."
	$(GO) fmt ./...

# Verifica dipendenze
deps:
	@echo "Checking/updating dependencies..."
	$(GO) mod tidy
	$(GO) mod verify

# ============================================================================
# INSTALLAZIONE
# ============================================================================

# Installa localmente (richiede permessi)
install: build
	@echo "Installing $(PROJECT_NAME) to $(BIN_DIR)..."
	sudo install -m 755 $(PROJECT_NAME) $(BIN_DIR)/
	sudo install -m 644 config/resman.conf.example $(CONF_DIR)/resman.conf
	sudo install -m 644 packaging/systemd/resman.service $(SYSTEMD_DIR)/
	sudo systemctl daemon-reload
	@echo "Installazione completata!"
	@echo "Configurazione: $(CONF_DIR)/resman.conf"
	@echo "Service: $(SYSTEMD_DIR)/resman.service"

# Disinstalla
uninstall:
	@echo "Uninstalling $(PROJECT_NAME)..."
	sudo rm -f $(BIN_DIR)/$(PROJECT_NAME)
	sudo rm -f $(CONF_DIR)/resman.conf
	sudo rm -f $(SYSTEMD_DIR)/resman.service
	sudo systemctl daemon-reload
	@echo "Disinstallazione completata!"

# ============================================================================
# PACCHETTIZZAZIONE RPM
# ============================================================================

# Crea struttura RPM
rpm-dirs:
	@echo "Creating RPM build directories..."
	mkdir -p $(RPMBUILD_DIR)/{BUILD,RPMS,SOURCES,SPECS,SRPMS}

# Crea tarball per RPM
rpm-source: build rpm-dirs
	@echo "Creating source tarball for RPM..."
	mkdir -p $(PROJECT_NAME)-$(VERSION)
	cp -r *.go go.mod go.sum \
		config/ cgroup/ metrics/ state/ logging/ reloader/ \
		README.md LICENSE CHANGELOG.md \
		packaging/ docs/ \
		$(PROJECT_NAME)-$(VERSION)/
	mkdir -p $(PROJECT_NAME)-$(VERSION)/packaging/syslog
	cp packaging/syslog/resman.conf $(PROJECT_NAME)-$(VERSION)/packaging/syslog/ 2>/dev/null || true
	cp packaging/syslog/resman $(PROJECT_NAME)-$(VERSION)/packaging/syslog/ 2>/dev/null || true
	tar czf $(RPMBUILD_DIR)/SOURCES/$(PROJECT_NAME)-$(VERSION).tar.gz $(PROJECT_NAME)-$(VERSION)
	rm -rf $(PROJECT_NAME)-$(VERSION)
	@echo "Source tarball creato: $(RPMBUILD_DIR)/SOURCES/$(PROJECT_NAME)-$(VERSION).tar.gz"

# Build RPM
rpm: rpm-source
	@echo "Building RPM package..."
	cp packaging/rpm/$(PROJECT_NAME).spec $(RPMBUILD_DIR)/SPECS/
	rpmbuild -ba $(RPMBUILD_DIR)/SPECS/$(PROJECT_NAME).spec
	@echo "RPM creato: $(RPMBUILD_DIR)/RPMS/*/$(PROJECT_NAME)-$(VERSION)-$(RELEASE).*.rpm"

# Install RPM (locale)
rpm-install: rpm
	@echo "Installing RPM..."
	sudo rpm -ivh --force $(RPMBUILD_DIR)/RPMS/*/$(PROJECT_NAME)-$(VERSION)-$(RELEASE).*.rpm

# ============================================================================
# PACCHETTIZZAZIONE DEBIAN
# ============================================================================

# Crea directory per build Debian
deb-dirs:
	@echo "Creating Debian build directories..."
	mkdir -p $(DEB_BUILD_DIR)/$(PROJECT_NAME)_$(VERSION)-$(RELEASE)

# Build binario per Debian (per architettura specifica)
deb-binary: deps
	@echo "Building binary for Debian package..."
	@mkdir -p $(DEB_BUILD_DIR)
	$(GO) build $(GO_FLAGS) $(GO_LDFLAGS) $(GO_TAGS) -o $(DEB_BUILD_DIR)/$(PROJECT_NAME)-amd64
	$(GO) build $(GO_FLAGS) $(GO_LDFLAGS) $(GO_TAGS) -o $(DEB_BUILD_DIR)/$(PROJECT_NAME)-arm64

# Prepara struttura pacchetto Debian
deb-prepare: deb-dirs deb-binary
	@echo "Preparing Debian package structure..."
	DEB_ARCH=$(DEB_ARCH_amd64); \
	echo "Preparing package for $$DEB_ARCH..."; \
	PKG_DIR=$(DEB_BUILD_DIR)/$(PROJECT_NAME)_$(VERSION)-$(RELEASE)_$$DEB_ARCH; \
	mkdir -p $$PKG_DIR/DEBIAN; \
	mkdir -p $$PKG_DIR$(BIN_DIR); \
	mkdir -p $$PKG_DIR$(CONF_DIR); \
	mkdir -p $$PKG_DIR$(SYSTEMD_DIR); \
	mkdir -p $$PKG_DIR/usr/share/doc/$(PROJECT_NAME); \
	mkdir -p $$PKG_DIR$(CONF_DIR)/resman/tls; \
	install -m 755 $(DEB_BUILD_DIR)/$(PROJECT_NAME)-amd64 $$PKG_DIR$(BIN_DIR)/$(PROJECT_NAME); \
	install -m 644 config/resman.conf.example $$PKG_DIR$(CONF_DIR)/resman.conf; \
	install -m 644 packaging/systemd/resman.service $$PKG_DIR$(SYSTEMD_DIR)/; \
	install -m 644 README.md $$PKG_DIR/usr/share/doc/$(PROJECT_NAME)/; \
	install -m 644 LICENSE $$PKG_DIR/usr/share/doc/$(PROJECT_NAME)/; \
	install -m 644 CHANGELOG.md $$PKG_DIR/usr/share/doc/$(PROJECT_NAME)/; \
	install -m 644 docs/TLS-CONFIGURATION.md $$PKG_DIR/usr/share/doc/$(PROJECT_NAME)/; \
	install -m 644 docs/MULTI-INSTANCE-MONITORING.md $$PKG_DIR/usr/share/doc/$(PROJECT_NAME)/; \
	install -m 644 docs/prometheus-queries.md $$PKG_DIR/usr/share/doc/$(PROJECT_NAME)/; \
	install -m 644 docs/alerting-rules.yml $$PKG_DIR/usr/share/doc/$(PROJECT_NAME)/; \
	mkdir -p $$PKG_DIR/usr/share/doc/$(PROJECT_NAME)/scripts; \
	install -m 755 docs/generate-tls-certs.sh $$PKG_DIR/usr/share/doc/$(PROJECT_NAME)/scripts/; \
	echo "Package: $(PROJECT_NAME)" > $$PKG_DIR/DEBIAN/control; \
	echo "Version: $(VERSION)-$(RELEASE)" >> $$PKG_DIR/DEBIAN/control; \
	echo "Section: utils" >> $$PKG_DIR/DEBIAN/control; \
	echo "Priority: optional" >> $$PKG_DIR/DEBIAN/control; \
	echo "Architecture: $$DEB_ARCH" >> $$PKG_DIR/DEBIAN/control; \
	echo "Maintainer: $(DEB_MAINTAINER)" >> $$PKG_DIR/DEBIAN/control; \
	echo "Description: $(DEB_DESCRIPTION)" >> $$PKG_DIR/DEBIAN/control; \
	echo "  Resource Manager è un sistema avanzato per la gestione delle risorse ." >> $$PKG_DIR/DEBIAN/control; \
	echo "  Fornisce isolamento, limitazione e monitoraggio delle risorse per container." >> $$PKG_DIR/DEBIAN/control; \
	echo "  Supporta TLS/HTTPS, Basic Auth e JWT authentication." >> $$PKG_DIR/DEBIAN/control; \
	echo '#!/bin/bash' > $$PKG_DIR/DEBIAN/postinst; \
	echo 'set -e' >> $$PKG_DIR/DEBIAN/postinst; \
	echo 'if [ "$$1" = "configure" ]; then' >> $$PKG_DIR/DEBIAN/postinst; \
	echo '    systemctl daemon-reload 2>/dev/null || true' >> $$PKG_DIR/DEBIAN/postinst; \
	echo '    echo "Resource Manager $(VERSION) installed successfully"' >> $$PKG_DIR/DEBIAN/postinst; \
	echo '    echo "TLS certificates: /etc/resman/tls/"' >> $$PKG_DIR/DEBIAN/postinst; \
	echo '    echo "Generate certs: /usr/share/doc/resman/scripts/generate-tls-certs.sh"' >> $$PKG_DIR/DEBIAN/postinst; \
	echo 'fi' >> $$PKG_DIR/DEBIAN/postinst; \
	echo 'exit 0' >> $$PKG_DIR/DEBIAN/postinst; \
	chmod 755 $$PKG_DIR/DEBIAN/postinst; \
	echo '#!/bin/bash' > $$PKG_DIR/DEBIAN/prerm; \
	echo 'set -e' >> $$PKG_DIR/DEBIAN/prerm; \
	echo 'if [ "$$1" = "remove" ] || [ "$$1" = "upgrade" ]; then' >> $$PKG_DIR/DEBIAN/prerm; \
	echo '    systemctl stop resman 2>/dev/null || true' >> $$PKG_DIR/DEBIAN/prerm; \
	echo '    systemctl disable resman 2>/dev/null || true' >> $$PKG_DIR/DEBIAN/prerm; \
	echo 'fi' >> $$PKG_DIR/DEBIAN/prerm; \
	echo 'exit 0' >> $$PKG_DIR/DEBIAN/prerm; \
	chmod 755 $$PKG_DIR/DEBIAN/prerm; \
	echo '#!/bin/bash' > $$PKG_DIR/DEBIAN/postrm; \
	echo 'set -e' >> $$PKG_DIR/DEBIAN/postrm; \
	echo 'if [ "$$1" = "remove" ] || [ "$$1" = "upgrade" ]; then' >> $$PKG_DIR/DEBIAN/postrm; \
	echo '    systemctl daemon-reload 2>/dev/null || true' >> $$PKG_DIR/DEBIAN/postrm; \
	echo 'fi' >> $$PKG_DIR/DEBIAN/postrm; \
	echo 'exit 0' >> $$PKG_DIR/DEBIAN/postrm; \
	chmod 755 $$PKG_DIR/DEBIAN/postrm
	@echo "Debian package structure prepared in $(DEB_BUILD_DIR)/"

# Build pacchetto Debian
deb: deb-prepare
	@echo "Building Debian packages..."
	DEB_ARCH=$(DEB_ARCH_amd64); \
	PKG_DIR=$(DEB_BUILD_DIR)/$(PROJECT_NAME)_$(VERSION)-$(RELEASE)_$$DEB_ARCH; \
	echo "Building .deb for $$DEB_ARCH..."; \
	dpkg-deb --build $$PKG_DIR $(DEB_BUILD_DIR)/$(PROJECT_NAME)_$(VERSION)-$(RELEASE)_$$DEB_ARCH.deb
	@echo "Debian packages created in $(DEB_BUILD_DIR)/"

# Installa pacchetto Debian (locale)
deb-install: deb
	@echo "Installing Debian package..."
	@for arch in $(ARCHES); do \
                DEB_ARCH=$$(eval echo \$$DEB_ARCH_$$arch); \
                if [ "$$(dpkg --print-architecture)" = "$$DEB_ARCH" ]; then \
                        echo "Installing package for $$DEB_ARCH..."; \
                        sudo dpkg -i $(DEB_BUILD_DIR)/$(PROJECT_NAME)_$(VERSION)-$(RELEASE)_$$DEB_ARCH.deb; \
                        break; \
                fi \
	done

# ============================================================================
# DOCKER
# ============================================================================

# Build Docker image
docker-build:
	@echo "Building Docker image..."
	docker build -t $(PROJECT_NAME):$(VERSION) -f packaging/docker/Dockerfile .
	docker tag $(PROJECT_NAME):$(VERSION) $(PROJECT_NAME):latest

# Run Docker container
docker-run:
	@echo "Running Docker container..."
	docker run --rm -it \
                --privileged \
                -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
                -v /etc/resman.conf:/etc/resman.conf:ro \
                $(PROJECT_NAME):latest

# ============================================================================
# UTILITIES
# ============================================================================

# Pulisci build
clean:
	@echo "Cleaning build artifacts..."
	rm -rf $(PROJECT_NAME) $(PROJECT_NAME)-static
	rm -rf $(BUILD_DIR) $(DIST_DIR) coverage.out coverage.html
	rm -rf $(PROJECT_NAME)-$(VERSION)
	$(GO) clean

# ============================================================================
# DOCUMENTAZIONE
# ============================================================================

# Directory per man page
MAN_SRC_DIR = docs
MAN_BUILD_DIR = $(BUILD_DIR)/man
MAN_SOURCE = $(MAN_SRC_DIR)/resman.8
MAN_GZIPPED = $(MAN_BUILD_DIR)/resman.8.gz
MAN_HTML = $(MAN_BUILD_DIR)/resman.html

# Directory di installazione man page
MAN_INSTALL_DIR = /usr/share/man/man8

# Genera directory per man page
man-dirs:
	@mkdir -p $(MAN_BUILD_DIR)

# Genera man page compressa
man: man-dirs $(MAN_SOURCE)
	@echo "Generating man page..."
	@gzip -k -c $(MAN_SOURCE) > $(MAN_GZIPPED)
	@echo "Man page generated: $(MAN_GZIPPED)"

# Genera HTML dalla man page
man-html: man-dirs $(MAN_SOURCE)
	@echo "Generating HTML documentation..."
	@groff -mandoc -Thtml $(MAN_SOURCE) > $(MAN_HTML)
	@echo "HTML documentation generated: $(MAN_HTML)"

# Visualizza man page localmente
view-man: man
	@echo "Displaying man page..."
	@gunzip -c $(MAN_GZIPPED) | nroff -man | less -R || \
	echo "Install 'less' for better viewing, or use: cat $(MAN_SOURCE)"

# Installa man page
install-man: man
	@echo "Installing man page..."
	@sudo install -d $(MAN_INSTALL_DIR)
	@sudo install -m 644 $(MAN_GZIPPED) $(MAN_INSTALL_DIR)/
	@if command -v mandb >/dev/null 2>&1; then \
                sudo mandb -q; \
                echo "Man database updated"; \
	else \
                echo "Note: 'mandb' not found, manual cache update may be needed"; \
	fi
	@echo "Man page installed to $(MAN_INSTALL_DIR)/"

# Disinstalla man page
uninstall-man:
	@echo "Uninstalling man page..."
	@sudo rm -f $(MAN_INSTALL_DIR)/resman.8.gz
	@if command -v mandb >/dev/null 2>&1; then \
                sudo mandb -q; \
                echo "Man database updated"; \
	fi
	@echo "Man page uninstalled"

# Genera tutta la documentazione
docs: man man-html
	@echo "All documentation generated in $(MAN_BUILD_DIR)/"

# ============================================================================
# TARGET ALL-INCLUSIVE
# ============================================================================

# Target che include tutto (binari, RPM, DEB, documentazione)
all-with-packages: clean deps test lint build rpm deb docs
	@echo "Complete build with all packages finished!"
	@echo "RPM: $(RPMBUILD_DIR)/RPMS/*/*.rpm"
	@echo "DEB: $(DEB_BUILD_DIR)/*.deb"
	@echo "Man page: $(MAN_GZIPPED)"
	@echo "HTML docs: $(MAN_HTML)"

# ============================================================================
# HELP
# ============================================================================

help:
	@echo "Resource Manager Go - Makefile"
	@echo ""
	@echo "Targets disponibili:"
	@echo "  DEVELOPMENT:"
	@echo "    build        - Build del binario locale"
	@echo "    release      - Build multi-architettura"
	@echo "    static       - Build binario statico"
	@echo "    test         - Esegui test unitari"
	@echo "    test-cover   - Test con report coverage"
	@echo "    lint         - Esegui linting del codice"
	@echo "    fmt          - Formatta il codice"
	@echo ""
	@echo "  INSTALLATION:"
	@echo "    install      - Installa localmente (binario, config, service)"
	@echo "    install-man  - Installa solo man page"
	@echo "    uninstall    - Disinstalla tutto"
	@echo "    uninstall-man - Disinstalla solo man page"
	@echo ""
	@echo "  PACKAGING:"
	@echo "    rpm          - Crea pacchetto RPM"
	@echo "    rpm-install  - Crea e installa RPM"
	@echo "    deb          - Crea pacchetto Debian (.deb)"
	@echo "    deb-install  - Crea e installa pacchetto Debian"
	@echo ""
	@echo "  DOCUMENTATION:"
	@echo "    man          - Genera man page (gzipped)"
	@echo "    man-html     - Genera documentazione HTML"
	@echo "    docs         - Genera tutta la documentazione"
	@echo "    view-man     - Visualizza man page localmente"
	@echo ""
	@echo "  DOCKER:"
	@echo "    docker-build - Crea immagine Docker"
	@echo "    docker-run   - Esegui container Docker"
	@echo ""
	@echo "  UTILITIES:"
	@echo "    clean        - Pulisci file di build"
	@echo "    help         - Mostra questo messaggio"
	@echo ""
	@echo "  META TARGETS:"
	@echo "    all          - clean + test + lint + build"
	@echo "    all-with-packages - clean + test + lint + build + rpm + deb + docs"
	@echo ""
	@echo "Variabili configurabili:"
	@echo "  VERSION=$(VERSION)"
	@echo "  RELEASE=$(RELEASE)"
	@echo "  ARCHES=$(ARCHES)"
	@echo "  MAN_INSTALL_DIR=$(MAN_INSTALL_DIR)"
	@echo "  DEB_MAINTAINER=$(DEB_MAINTAINER)"

# ============================================================================
# TARGET DI DEFAULT
# ============================================================================
.DEFAULT_GOAL := help
