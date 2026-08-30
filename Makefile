# ResMan Makefile
# Author: Francesco Defilippo <francesco@defilippo.org>
# License: GPLv3

# ============================================================================
# CONFIGURABLE VARIABLES
# ============================================================================

# Project name
PROJECT_NAME = resman
VERSION = 1.30.8
RELEASE = 1
PROJECT_ROOT := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

# Paths
GO = go
GOLANGCI_LINT = golangci-lint
GOLANGCI_LINT_VERSION = v2.12.2
GORELEASER = goreleaser
SHELLCHECK = shellcheck

# Build directories
BUILD_DIR = build
DIST_DIR = dist
RPMBUILD_DIR = $(HOME)/rpmbuild
DEB_BUILD_DIR = $(BUILD_DIR)/deb
BIN_DIR = /usr/bin
CONF_DIR = /etc/resman
STATE_DIR = /var/lib/resman
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

# Supported architectures
ARCHES = amd64 arm64
OSES = linux

# Debian packaging (native CGO build)
DEB_ARCH ?= $(shell dpkg --print-architecture 2>/dev/null)
DEB_PACKAGE_VERSION = $(VERSION)-$(RELEASE)
DEB_PACKAGE_DIR = $(DEB_BUILD_DIR)/$(PROJECT_NAME)_$(DEB_PACKAGE_VERSION)_$(DEB_ARCH)
DEB_PACKAGE_FILE = $(DEB_BUILD_DIR)/$(PROJECT_NAME)_$(DEB_PACKAGE_VERSION)_$(DEB_ARCH).deb
DEB_BINARY = $(DEB_BUILD_DIR)/$(PROJECT_NAME)
DEB_SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct)
DEB_GO_FLAGS = -buildmode=pie
DEB_GO_LDFLAGS = -ldflags="-s -w -linkmode=external -extldflags=-Wl,-z,relro,-z,now -X 'main.version=$(VERSION)-$(RELEASE)'"

# ============================================================================
# PRIMARY TARGETS
# ============================================================================

.PHONY: all build clean test test-sendmail fuzz test-functional-smolvm test-functional-smolvm-memory-only test-functional-smolvm-process-membership test-functional-smolvm-cpu-without-cpuset test-functional-smolvm-missing-io-startup test-functional-smolvm-mcp-filter-reload test-functional-smolvm-container-runtime test-functional-smolvm-block-iops test-functional-smolvm-psi-refresh test-functional-smolvm-preflight \
	test-functional-smolvm-unit test-functional-real-kernel-unit test-functional-real-kernel-psi test-functional-real-kernel-block-io test-functional-final test-functional-final-unit ci-quality ci-test verify-format verify-modules verify-promtool verify-shellcheck verify-contracts lint lint-required lint-install install uninstall rpm deb container-build container-run help

all: clean test lint build

# ============================================================================
# DEVELOPMENT AND BUILD
# ============================================================================

# Local development build
build: deps
	@echo "Building $(PROJECT_NAME)..."
	$(GO) build $(GO_FLAGS) $(GO_LDFLAGS) $(GO_TAGS) -o $(PROJECT_NAME)
	@echo "Build completed: ./$(PROJECT_NAME)"

# Multi-architecture release build
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
	@echo "Release binaries are available in: $(BUILD_DIR)/"

# Static build without C dependencies
static: deps
	@echo "Building static binary..."
	CGO_ENABLED=0 $(GO) build $(GO_FLAGS) $(GO_LDFLAGS) -a -installsuffix cgo -o $(PROJECT_NAME)-static
	@echo "Static binary build completed: ./$(PROJECT_NAME)-static"

# ============================================================================
# TESTS AND QUALITY
# ============================================================================

# Run the authoritative quality-gate sequence used by pull requests and releases.
ci-quality: verify-modules verify-format verify-promtool verify-shellcheck
	@echo "Running CI quality gates..."
	$(GO) build ./...
	$(GO) vet ./...
	$(MAKE) verify-contracts GO="$(GO)"
	$(MAKE) test-functional-final-unit
	$(MAKE) test-functional-real-kernel-unit
	$(MAKE) ci-test GO="$(GO)"
	$(MAKE) lint-required GO="$(GO)"

# Run the race-enabled test command shared by CI and its mutation tests.
ci-test:
	$(GO) test -race -cover ./...

# Fail when any tracked Go source is not gofmt-clean.
verify-format:
	@set -eu; \
	unformatted="$$(git ls-files -z -- '*.go' | xargs -0 -r gofmt -l)"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following Go files are not formatted:" >&2; \
		echo "$$unformatted" >&2; \
		exit 1; \
	fi

# Verify dependencies and reject go.mod or go.sum changes produced by tidy.
verify-modules: deps
	git diff --exit-code -- go.mod go.sum

# CI installs promtool deliberately, so its absence must fail rather than warn.
verify-promtool:
	@command -v promtool >/dev/null 2>&1 || { \
		echo "promtool is required by ci-quality but was not found" >&2; \
		exit 1; \
	}

# Check every tracked shell script. CI sets REQUIRE_SHELLCHECK=1 so a missing
# binary cannot silently disable the gate; local runs retain a visible warning.
verify-shellcheck:
	@SHELLCHECK="$(SHELLCHECK)" REQUIRE_SHELLCHECK="$(REQUIRE_SHELLCHECK)" $(PROJECT_ROOT)scripts/verify-shellcheck.sh

# Run unit tests.
test: deps
	@echo "Running tests..."
	$(GO) test -v -cover ./...

# Exercise the sendmail helper with deterministic external commands.
test-sendmail:
	scripts/sendmail_test.sh

# Run tests with coverage.
test-cover: deps
	@echo "Running tests with coverage..."
	$(GO) test -v -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

# Run the isolated functional harness in a disposable SmolVM guest.
test-functional-smolvm:
	test/functional/smolvm/run.sh run

# Verify standalone RAM enforcement without requiring io.max.
test-functional-smolvm-memory-only:
	SMOLVM_SCENARIO=memory-only test/functional/smolvm/run.sh run

# Run the sustained-active process-membership scenario independently.
test-functional-smolvm-process-membership:
	SMOLVM_SCENARIO=process-membership test/functional/smolvm/run.sh run

# Verify CPU quota enforcement in a delegated hierarchy without cpuset.
test-functional-smolvm-cpu-without-cpuset:
	SMOLVM_SCENARIO=cpu-without-cpuset test/functional/smolvm/run.sh run

# Verify fail-closed startup when io is named but io.max is unavailable.
test-functional-smolvm-missing-io-startup:
	SMOLVM_SCENARIO=missing-io-startup test/functional/smolvm/run.sh run

# Verify acknowledged MCP filter persistence and runtime publication.
test-functional-smolvm-mcp-filter-reload:
	SMOLVM_SCENARIO=mcp-filter-reload test/functional/smolvm/run.sh run

# Verify the shipped rootful Podman runtime against host users and cgroup v2.
test-functional-smolvm-container-runtime:
	SMOLVM_SCENARIO=container-runtime test/functional/smolvm/run.sh run

# Verify true block IOPS decisions when the guest exposes io.max.
test-functional-smolvm-block-iops:
	SMOLVM_SCENARIO=block-iops test/functional/smolvm/run.sh run

# Verify PSI observation refresh neutrality, or return BLOCKED when PSI is absent.
test-functional-smolvm-psi-refresh:
	SMOLVM_SCENARIO=psi-refresh-neutrality SMOLVM_REQUIRE_PSI=1 test/functional/smolvm/run.sh run

# Check host SmolVM/KVM prerequisites without building or starting a guest.
test-functional-smolvm-preflight:
	test/functional/smolvm/run.sh preflight

# Exercise the host-side harness contract without requiring KVM.
test-functional-smolvm-unit:
	test/functional/smolvm/host_test.sh

# Exercise packaged-service harness sequencing without a remote host.
test-functional-real-kernel-unit:
	test/functional/real-kernel/host_test.sh

# Run current-revision evidence on an explicitly selected disposable real-kernel host.
test-functional-real-kernel-psi:
	test/functional/real-kernel/remote.sh psi-refresh-neutrality "$(RESMAN_REAL_KERNEL_HOST)"

test-functional-real-kernel-block-io:
	test/functional/real-kernel/remote.sh block-io-all-dimensions "$(RESMAN_REAL_KERNEL_HOST)"

# Compose focused tests, SmolVM scenarios, and capability-specific real-kernel evidence.
test-functional-final:
	test/functional/final/run.sh

# Prove that the final gate cannot turn missing or failed required rows into PASS.
test-functional-final-unit:
	test/functional/final/gate_test.sh

# Verify mechanically checkable development-guide contracts.
verify-contracts:
	@echo "Verifying architectural contracts..."
	$(GO) run ./scripts/generate-config-reference --check
	$(GO) test -count=1 ./config -run '^(TestEveryEnvironmentFieldUsesAValidatedHandler|TestLoadFromFileRejectsUnknownKeyWithPath|TestPublicConfigReferenceMatchesRuntimeContract|TestExampleConfigMatchesRuntimeDefaults|TestEmptyIncludeListMeaningsMatchEligibility|TestSecondaryConfigurationReferencesStayFocusedAndSecure)$$'
	$(MAKE) test-sendmail
	$(GO) run ./scripts/verify-contracts

# Fuzz the parsing and protocol boundaries for a bounded time. Go fuzzes one
# target per invocation, so each is driven in turn; FUZZTIME sets the budget per
# target. The seed corpora live in the targets themselves and run under `make
# test`, so this gate adds generated inputs on top of the committed ones.
FUZZTIME ?= 30s
FUZZ_TARGETS = \
	./config:FuzzLoadAndValidate \
	./config:FuzzParseByteQuota \
	./metrics:FuzzParseCPUQuota \
	./cgroup:FuzzReadIOStatsFile \
	./mcp:FuzzMCPHTTPHandler

fuzz: deps
	@set -eu; \
	for target in $(FUZZ_TARGETS); do \
		pkg="$${target%%:*}"; \
		name="$${target##*:}"; \
		echo "Fuzzing $$name in $$pkg for $(FUZZTIME)..."; \
		$(GO) test "$$pkg" -run '^$$' -fuzz "^$$name$$" -fuzztime $(FUZZTIME); \
	done

# Lint the code. The unlimited issue flags disable golangci-lint's default
# deduplication, which would otherwise hide repeated findings after the first three.
lint: deps
	@echo "Running linters..."
	@if command -v $(GOLANGCI_LINT) >/dev/null 2>&1; then \
		$(GOLANGCI_LINT) run --max-same-issues=0 --max-issues-per-linter=0 ./...; \
	else \
		echo "golangci-lint is not installed (run 'make lint-install'); running go vet instead..."; \
		$(GO) vet ./...; \
	fi

# The authoritative gate requires a linter binary; CI passes its pinned binary explicitly.
lint-required: deps
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { \
		echo "$(GOLANGCI_LINT) is required by ci-quality but was not found" >&2; \
		exit 1; \
	}
	$(GOLANGCI_LINT) run --max-same-issues=0 --max-issues-per-linter=0 ./...

# Install the pinned golangci-lint version in $(GOPATH)/bin.
lint-install:
	@echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..."
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@echo "golangci-lint installed in $$($(GO) env GOPATH)/bin"

# Format the code.
fmt:
	@echo "Formatting code..."
	$(GO) fmt ./...

# Verify dependencies.
deps:
	@echo "Checking/updating dependencies..."
	$(GO) mod tidy
	$(GO) mod verify

# ============================================================================
# INSTALLATION
# ============================================================================

# Install locally; this requires elevated permissions.
install: build
	@echo "Installing $(PROJECT_NAME) to $(BIN_DIR)..."
	sudo install -m 755 $(PROJECT_NAME) $(BIN_DIR)/
	sudo install -d -m 0700 $(CONF_DIR) $(STATE_DIR)
	sudo install -m 0600 config/resman.conf.example $(CONF_DIR)/resman.conf
	sudo install -m 644 packaging/systemd/resman.service $(SYSTEMD_DIR)/
	sudo systemctl daemon-reload
	@echo "Installation completed!"
	@echo "Configuration: $(CONF_DIR)/resman.conf"
	@echo "Service: $(SYSTEMD_DIR)/resman.service"

# Uninstall ResMan.
uninstall:
	@echo "Uninstalling $(PROJECT_NAME)..."
	sudo rm -f $(BIN_DIR)/$(PROJECT_NAME)
	sudo rm -f $(CONF_DIR)/resman.conf
	sudo rm -f $(SYSTEMD_DIR)/resman.service
	sudo systemctl daemon-reload
	@echo "Uninstallation completed!"

# ============================================================================
# RPM PACKAGING
# ============================================================================

# Create the RPM build structure.
rpm-dirs:
	@echo "Creating RPM build directories..."
	mkdir -p \
		$(RPMBUILD_DIR)/BUILD \
		$(RPMBUILD_DIR)/RPMS \
		$(RPMBUILD_DIR)/SOURCES \
		$(RPMBUILD_DIR)/SPECS \
		$(RPMBUILD_DIR)/SRPMS

# Create the RPM source tarball.
rpm-source: build rpm-dirs
	@echo "Creating source tarball for RPM..."
	mkdir -p $(PROJECT_NAME)-$(VERSION)
	cp -r *.go go.mod go.sum \
		config/ cgroup/ metrics/ state/ logging/ reloader/ mcp/ database/ internal/ \
		README.md LICENSE \
		packaging/ docs/ \
		$(PROJECT_NAME)-$(VERSION)/
	mkdir -p $(PROJECT_NAME)-$(VERSION)/packaging/syslog
	cp packaging/syslog/resman.conf $(PROJECT_NAME)-$(VERSION)/packaging/syslog/ 2>/dev/null || true
	cp packaging/syslog/resman $(PROJECT_NAME)-$(VERSION)/packaging/syslog/ 2>/dev/null || true
	tar czf $(RPMBUILD_DIR)/SOURCES/$(PROJECT_NAME)-$(VERSION).tar.gz $(PROJECT_NAME)-$(VERSION)
	rm -rf $(PROJECT_NAME)-$(VERSION)
	@echo "Source tarball created: $(RPMBUILD_DIR)/SOURCES/$(PROJECT_NAME)-$(VERSION).tar.gz"

# Build RPM
rpm: rpm-source
	@echo "Building RPM package..."
	cp packaging/rpm/$(PROJECT_NAME).spec $(RPMBUILD_DIR)/SPECS/
	rpmbuild --define "_topdir $(RPMBUILD_DIR)" -ba $(RPMBUILD_DIR)/SPECS/$(PROJECT_NAME).spec
	@echo "RPM created: $(RPMBUILD_DIR)/RPMS/*/$(PROJECT_NAME)-$(VERSION)-$(RELEASE).*.rpm"

# Install the RPM locally.
rpm-install: rpm
	@echo "Installing RPM..."
	sudo rpm -ivh --force $(RPMBUILD_DIR)/RPMS/*/$(PROJECT_NAME)-$(VERSION)-$(RELEASE).*.rpm

# ============================================================================
# DEBIAN PACKAGING
# ============================================================================

# Validate tools and architecture for a native CGO build
deb-check:
	@command -v dpkg >/dev/null 2>&1 || { echo "dpkg is required"; exit 1; }
	@command -v dpkg-deb >/dev/null 2>&1 || { echo "dpkg-deb is required"; exit 1; }
	@command -v dpkg-shlibdeps >/dev/null 2>&1 || { echo "dpkg-dev is required"; exit 1; }
	@test -n "$(DEB_ARCH)" || { echo "unable to determine Debian architecture"; exit 1; }
	@case "$(DEB_ARCH):$$($(GO) env GOARCH)" in \
		amd64:amd64|arm64:arm64) ;; \
		*) echo "unsupported native Debian/Go architecture pair: $(DEB_ARCH):$$($(GO) env GOARCH)"; exit 1 ;; \
	esac

# Create the Debian build directory
deb-dirs:
	@echo "Creating Debian build directory..."
	mkdir -p $(DEB_BUILD_DIR)

# Build the native Debian/Ubuntu binary with CGO/NSS support
deb-binary: deps deb-check deb-dirs
	@echo "Building native Debian binary for $(DEB_ARCH)..."
	CGO_CFLAGS="$(CGO_CFLAGS) -D_FORTIFY_SOURCE=2 -fstack-protector-strong -fPIE" \
		$(GO) build $(GO_FLAGS) $(DEB_GO_FLAGS) $(DEB_GO_LDFLAGS) $(GO_TAGS) -o $(DEB_BINARY)

# Prepare the Debian package structure and metadata
deb-prepare: deb-binary
	@echo "Preparing Debian package structure..."
	SOURCE_DATE_EPOCH=$(DEB_SOURCE_DATE_EPOCH) packaging/deb/prepare-package.sh \
		$(DEB_BINARY) \
		$(VERSION) \
		$(RELEASE) \
		$(DEB_ARCH) \
		$(DEB_PACKAGE_DIR)

# Build the Debian package
deb: deb-prepare
	@echo "Building $(DEB_PACKAGE_FILE)..."
	SOURCE_DATE_EPOCH=$(DEB_SOURCE_DATE_EPOCH) \
		dpkg-deb --build --root-owner-group $(DEB_PACKAGE_DIR) $(DEB_PACKAGE_FILE)
	@echo "Debian package created: $(DEB_PACKAGE_FILE)"

# Install the Debian package and resolve dependencies
deb-install: deb
	@echo "Installing $(DEB_PACKAGE_FILE)..."
	sudo apt-get install -y $(abspath $(DEB_PACKAGE_FILE))

# ============================================================================
# CONTAINER
# ============================================================================

# Build the supported rootful Podman image.
container-build:
	@echo "Building container image with sudo podman..."
	sudo podman build --tag $(PROJECT_NAME):$(VERSION) --file packaging/docker/Dockerfile .

# Run the supported host-wide container contract. The host paths must exist;
# see docs/CONTAINER.md for configuration and NSS/SSSD requirements.
container-run:
	@echo "Running host-wide resman container with sudo podman..."
	sudo podman run --rm --name resman \
		--privileged --pid=host --cgroupns=host --network=host \
		--security-opt label=disable \
		-v /sys/fs/cgroup:/sys/fs/cgroup:rw \
		-v /etc/resman:/etc/resman:rw \
		-v /etc/passwd:/etc/passwd:ro \
		-v /etc/group:/etc/group:ro \
		-v /etc/nsswitch.conf:/etc/nsswitch.conf:ro \
		-v /var/lib/resman:/var/lib/resman:rw \
		-v /var/log/resman:/var/log/resman:rw \
		$(PROJECT_NAME):$(VERSION)

# ============================================================================
# UTILITIES
# ============================================================================

# Clean build artifacts.
clean:
	@echo "Cleaning build artifacts..."
	rm -rf $(PROJECT_NAME) $(PROJECT_NAME)-static
	rm -rf $(BUILD_DIR) $(DIST_DIR) coverage.out coverage.html
	rm -rf $(PROJECT_NAME)-$(VERSION)
	$(GO) clean

# ============================================================================
# DOCUMENTATION
# ============================================================================

# Man-page build directory.
MAN_SRC_DIR = docs
MAN_BUILD_DIR = $(BUILD_DIR)/man
MAN_SOURCE = $(MAN_SRC_DIR)/resman.8
MAN_GZIPPED = $(MAN_BUILD_DIR)/resman.8.gz
MAN_HTML = $(MAN_BUILD_DIR)/resman.html

# Man-page installation directory.
MAN_INSTALL_DIR = /usr/share/man/man8

# Create the man-page build directory.
man-dirs:
	@mkdir -p $(MAN_BUILD_DIR)

# Generate the compressed man page.
man: man-dirs $(MAN_SOURCE)
	@echo "Generating man page..."
	@gzip -k -c $(MAN_SOURCE) > $(MAN_GZIPPED)
	@echo "Man page generated: $(MAN_GZIPPED)"

# Generate HTML from the man page.
man-html: man-dirs $(MAN_SOURCE)
	@echo "Generating HTML documentation..."
	@groff -mandoc -Thtml $(MAN_SOURCE) > $(MAN_HTML)
	@echo "HTML documentation generated: $(MAN_HTML)"

# Display the man page locally.
view-man: man
	@echo "Displaying man page..."
	@gunzip -c $(MAN_GZIPPED) | nroff -man | less -R || \
	echo "Install 'less' for better viewing, or use: cat $(MAN_SOURCE)"

# Install the man page.
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

# Uninstall the man page.
uninstall-man:
	@echo "Uninstalling man page..."
	@sudo rm -f $(MAN_INSTALL_DIR)/resman.8.gz
	@if command -v mandb >/dev/null 2>&1; then \
                sudo mandb -q; \
                echo "Man database updated"; \
	fi
	@echo "Man page uninstalled"

# Generate all documentation.
docs: man man-html
	@echo "All documentation generated in $(MAN_BUILD_DIR)/"

# ============================================================================
# TARGET ALL-INCLUSIVE
# ============================================================================

# Build every distribution artifact: binaries, RPM, DEB, and documentation.
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
	@echo "Available targets:"
	@echo "  DEVELOPMENT:"
	@echo "    build        - Build the local binary"
	@echo "    release      - Build release binaries for multiple architectures"
	@echo "    static       - Build a static binary"
	@echo "    test         - Run unit tests"
	@echo "    test-cover   - Run tests and generate a coverage report"
	@echo "    test-functional-smolvm - Run isolated functional tests in SmolVM"
	@echo "    test-functional-smolvm-memory-only - Run standalone RAM enforcement in SmolVM"
	@echo "    test-functional-smolvm-process-membership - Run active process membership in SmolVM"
	@echo "    test-functional-smolvm-cpu-without-cpuset - Run CPU enforcement without cpuset in SmolVM"
	@echo "    test-functional-smolvm-mcp-filter-reload - Run acknowledged MCP filter reload in SmolVM"
	@echo "    test-functional-smolvm-container-runtime - Verify the shipped sudo podman runtime in SmolVM"
	@echo "    test-functional-smolvm-block-iops - Verify cached syscalls and direct block IOPS in SmolVM"
	@echo "    test-functional-smolvm-psi-refresh - Verify PSI refresh neutrality or report BLOCKED"
	@echo "    test-functional-smolvm-preflight - Check SmolVM/KVM prerequisites"
	@echo "    test-functional-smolvm-unit - Test the host harness without KVM"
	@echo "    test-functional-real-kernel-unit - Test packaged-service host sequencing"
	@echo "    test-functional-real-kernel-psi - Collect PSI evidence on RESMAN_REAL_KERNEL_HOST"
	@echo "    test-functional-real-kernel-block-io - Collect all-dimension I/O evidence on RESMAN_REAL_KERNEL_HOST"
	@echo "    test-functional-final - Run the final required semantic scenario matrix"
	@echo "    test-functional-final-unit - Test final-matrix failure and capability semantics"
	@echo "    ci-quality    - Run the quality gates shared by pull requests and releases"
	@echo "    verify-format - Fail when tracked Go files are not gofmt-clean"
	@echo "    verify-modules - Verify module files are tidy and unchanged"
	@echo "    verify-promtool - Require promtool for the strict CI gate"
	@echo "    verify-contracts - Verify mechanically checkable architectural contracts"
	@echo "    lint         - Run the complete golangci-lint gate"
	@echo "    lint-required - Require golangci-lint without falling back to go vet"
	@echo "    lint-install - Install the pinned golangci-lint version"
	@echo "    fmt          - Format the code"
	@echo ""
	@echo "  INSTALLATION:"
	@echo "    install      - Install locally (binary, config, service)"
	@echo "    install-man  - Install only the man page"
	@echo "    uninstall    - Uninstall ResMan"
	@echo "    uninstall-man - Uninstall only the man page"
	@echo ""
	@echo "  PACKAGING:"
	@echo "    rpm          - Build the RPM"
	@echo "    rpm-install  - Build and install the RPM"
	@echo "    deb          - Build the Debian package"
	@echo "    deb-install  - Build and install the Debian package"
	@echo ""
	@echo "  DOCUMENTATION:"
	@echo "    man          - Generate the compressed man page"
	@echo "    man-html     - Generate HTML documentation"
	@echo "    docs         - Generate all documentation"
	@echo "    view-man     - Display the man page locally"
	@echo ""
	@echo "  CONTAINER:"
	@echo "    container-build - Build the image with sudo podman"
	@echo "    container-run   - Run the supported host-wide container contract"
	@echo ""
	@echo "  UTILITIES:"
	@echo "    clean        - Remove build artifacts"
	@echo "    help         - Show this message"
	@echo ""
	@echo "  META TARGETS:"
	@echo "    all          - clean + test + lint + build"
	@echo "    all-with-packages - clean + test + lint + build + rpm + deb + docs"
	@echo ""
	@echo "Configurable variables:"
	@echo "  VERSION=$(VERSION)"
	@echo "  RELEASE=$(RELEASE)"
	@echo "  ARCHES=$(ARCHES)"
	@echo "  MAN_INSTALL_DIR=$(MAN_INSTALL_DIR)"
	@echo "  DEB_MAINTAINER=$(DEB_MAINTAINER)"

# ============================================================================
# DEFAULT TARGET
# ============================================================================
.DEFAULT_GOAL := help
