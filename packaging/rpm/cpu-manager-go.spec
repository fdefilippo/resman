# SPEC file per cpu-manager-go
# Build con: rpmbuild -ba cpu-manager-go.spec
#
# Questo spec crea un UNICO pacchetto RPM contenente:
# - Binario
# - File di configurazione
# - Systemd service
# - Man page
# - Documentazione
# - Script generazione certificati TLS

Name:    cpu-manager-go
Version: 1.13.1
Release: 1%{?dist}
Summary: Dynamic CPU resource management tool using cgroups v2

License: GPLv3
URL:     https://github.com/fdefilippo/cpu-manager-go
Source0: %{name}-%{version}.tar.gz

## Disable debug packages.
%define debug_package %{nil}

## Disable build_id
%define _build_id_links none

%if 0%{?rhel} == 8
%define __brp_mangle_shebangs /usr/bin/true
%endif

# Dichiara che il package contiene una man page
%global _has_manpage 1

BuildRequires:  golang >= 1.21
BuildRequires:  systemd
BuildRequires:  groff-base
BuildRequires:  openssl
Requires:       systemd
Requires:       openssl

# Dipendenze cgroups
Requires(post): systemd-units
Requires(preun): systemd-units
Requires(postun): systemd-units

%description
Enterprise-grade CPU resource management tool with cgroups v2 support.
Automatically limits CPU for non-system users based on configurable thresholds.

**IMPORTANT: CGO is required for this package**

CGO is enabled by default in this RPM and is required for:
- User name resolution via NSS (Name Service Switch)
- Support for LDAP, NIS, SSSD authentication backends
- Proper integration with system authentication services

Features:
- Dynamic CPU limiting for non-system users (UID >=1000)
- Configurable activation/release thresholds
- Absolute CPU limits using cpu.max cgroup controller
- Prometheus metrics export with comprehensive dashboard
- Per-user metrics: CPU%, Memory (bytes), Process count
- Systemd service integration with hardening
- Automatic configuration reload on file changes
- Detailed process logging with process name tracking
- Load average awareness (optional)
- Graceful shutdown with cleanup
- Complete man page documentation
- Unit tests for core packages
- MCP server for AI assistant integration (Model Context Protocol)
- Comprehensive CPU and memory reporting
- Server role identification for multi-server environments

MCP Server Features (v1.3+):
- 11 MCP tools for querying system status and generating reports
- 6 MCP resources for URI-based data access
- 3 pre-built prompts for common queries
- HTTP and stdio transport support
- Hostname and server role in all metric outputs
- Comprehensive logging middleware

Latest Changes (v1.5.0):
- Renamed Prometheus variables for clarity (PROMETHEUS_METRICS_BIND_HOST/PORT)
- Default Prometheus port changed to 1974
- Default MCP port changed to 1969
- All bind addresses default to 0.0.0.0 for remote access
- Added SERVER_ROLE configuration for server identification
- Enhanced documentation with log level descriptions

%prep
%setup -q

%build
# Build del binario Go
export GO111MODULE=on
export GOPROXY=direct
export CGO_ENABLED=1
go build -v -ldflags="-s -w -X 'main.version=%{version}-%{release}'" -o %{name}

# Prepara man page
mkdir -p %{_builddir}/%{name}-%{version}/man
cp docs/cpu-manager.8 %{_builddir}/%{name}-%{version}/man/
gzip -9 %{_builddir}/%{name}-%{version}/man/cpu-manager.8

%install
# Crea directory
mkdir -p %{buildroot}/%{_bindir}
mkdir -p %{buildroot}/%{_sysconfdir}
mkdir -p %{buildroot}/%{_unitdir}
mkdir -p %{buildroot}/%{_sharedstatedir}/cpu-manager
mkdir -p %{buildroot}/%{_localstatedir}/log
mkdir -p %{buildroot}/%{_mandir}/man8
mkdir -p %{buildroot}/%{_docdir}/%{name}

# Installa binario
install -m 755 %{name} %{buildroot}/%{_bindir}/%{name}

# Installa file di configurazione
install -m 644 config/cpu-manager.conf.example %{buildroot}/%{_sysconfdir}/cpu-manager.conf

# Installa service systemd
install -m 644 packaging/systemd/cpu-manager.service %{buildroot}/%{_unitdir}/

# Installa man page
install -m 644 %{_builddir}/%{name}-%{version}/man/cpu-manager.8.gz %{buildroot}/%{_mandir}/man8/

# Installa documentazione aggiuntiva
install -m 644 README.md %{buildroot}/%{_docdir}/%{name}/ 2>/dev/null || true
install -m 644 LICENSE %{buildroot}/%{_docdir}/%{name}/ 2>/dev/null || true
install -m 644 config/cpu-manager.conf.example %{buildroot}/%{_docdir}/%{name}/

# Installa documentazione TLS
install -m 644 docs/TLS-CONFIGURATION.md %{buildroot}/%{_docdir}/%{name}/ 2>/dev/null || true
install -m 644 docs/MULTI-INSTANCE-MONITORING.md %{buildroot}/%{_docdir}/%{name}/ 2>/dev/null || true
install -m 644 docs/prometheus-queries.md %{buildroot}/%{_docdir}/%{name}/ 2>/dev/null || true
install -m 644 docs/alerting-rules.yml %{buildroot}/%{_docdir}/%{name}/ 2>/dev/null || true

# Installa script generazione certificati TLS
install -d %{buildroot}/%{_docdir}/%{name}/scripts
install -m 755 docs/generate-tls-certs.sh %{buildroot}/%{_docdir}/%{name}/scripts/ 2>/dev/null || true

# Installa CHANGELOG (solo se esiste nel tarball)
install -m 644 CHANGELOG.md %{buildroot}/%{_docdir}/%{name}/ 2>/dev/null || true

# Installazione file di configurazione syslog
install -d %{buildroot}%{_sysconfdir}/rsyslog.d
install -p -m 0644 packaging/syslog/cpu-manager-go.conf %{buildroot}%{_sysconfdir}/rsyslog.d/cpu-manager-go.conf

# Installazione file di configurazione logrotate
install -d %{buildroot}%{_sysconfdir}/logrotate.d
install -p -m 0644 packaging/syslog/cpu-manager-go %{buildroot}%{_sysconfdir}/logrotate.d/cpu-manager-go

# Crea directory per runtime files (buildroot)
install -d -m 755 %{buildroot}/%{_sharedstatedir}/cpu-manager
install -d -m 755 %{buildroot}/var/run/cpu-manager

# Crea directory per certificati TLS (vuota, verrà popolata dall'admin)
install -d -m 700 %{buildroot}/%{_sysconfdir}/cpu-manager/tls

%pre
# Pre-install script
if [ $1 -eq 1 ]; then
    # Nuova installazione
    echo "Preparing for CPU Manager installation..."

    # Verifica cgroups v2
    if [ ! -f /sys/fs/cgroup/cgroup.controllers ]; then
        echo "WARNING: cgroups v2 not detected. Please enable with:"
        echo "  grubby --update-kernel=ALL --args='systemd.unified_cgroup_hierarchy=1'"
        echo "  reboot"
    fi
fi

%post
# Post-install script
%systemd_post cpu-manager.service

# Crea directory per i file di runtime
mkdir -p /var/run/cpu-manager
chmod 755 /var/run/cpu-manager
chown root:root /var/run/cpu-manager

# Crea file di log
touch /var/log/cpu-manager.log
chmod 644 /var/log/cpu-manager.log

# Aggiorna database man page
%{_bindir}/mandb -q 2>/dev/null || true

# Abilita cgroup controllers se non già abilitati
if ! grep -q "+cpu" /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null; then
    echo "+cpu" >> /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
fi
if ! grep -q "+cpuset" /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null; then
    echo "+cpuset" >> /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
fi

echo "CPU Manager installed successfully!"
echo ""
echo "Configuration file: /etc/cpu-manager.conf"
echo "Log file: /var/log/cpu-manager.log"
echo "Runtime directory: /var/run/cpu-manager"
echo "Service: systemctl start cpu-manager"
echo "Documentation: man cpu-manager"
echo ""
echo "Please review /etc/cpu-manager.conf before starting the service."

%preun
# Pre-uninstall script
%systemd_preun cpu-manager.service

%postun
# Post-uninstall script
%systemd_postun_with_restart cpu-manager.service

# Aggiorna database man page
%{_bindir}/mandb -q 2>/dev/null || true

# Rimuove directory runtime (solo se vuota)
rmdir /var/run/cpu-manager 2>/dev/null || true

%files
%license LICENSE
%doc README.md
%doc CHANGELOG.md
%{_bindir}/%{name}
%config(noreplace) %{_sysconfdir}/cpu-manager.conf
%{_unitdir}/cpu-manager.service
%{_mandir}/man8/cpu-manager.8.gz
%dir %{_sharedstatedir}/cpu-manager
%dir /var/run/cpu-manager
%dir %{_sysconfdir}/cpu-manager/tls
%config(noreplace) %{_sysconfdir}/rsyslog.d/cpu-manager-go.conf
%config %{_sysconfdir}/logrotate.d/cpu-manager-go
%dir %{_docdir}/%{name}
%doc %{_docdir}/%{name}/README.md
%doc %{_docdir}/%{name}/LICENSE
%doc %{_docdir}/%{name}/CHANGELOG.md
%doc %{_docdir}/%{name}/cpu-manager.conf.example
%doc %{_docdir}/%{name}/TLS-CONFIGURATION.md
%doc %{_docdir}/%{name}/MULTI-INSTANCE-MONITORING.md
%doc %{_docdir}/%{name}/prometheus-queries.md
%doc %{_docdir}/%{name}/alerting-rules.yml
%doc %{_docdir}/%{name}/scripts/

%changelog
* Thu Mar 13 2026 Francesco Defilippo <francesco@defilippo.org> - 1.12.0-1
- Added CPU_MANAGER_BLACKOUT configuration for blackout timeframes
- CPU Manager will not apply limits during configured blackout periods
- Crontab-like format: "days hours" (e.g., "1-5 08-18" for Mon-Fri, 8-18)
- Multiple timeframes supported (semicolon-separated)
- System timezone automatically used
- Hybrid logging: INFO for enter/exit blackout, DEBUG for cycle skips
- Blackout takes precedence over USER_INCLUDE_LIST and USER_EXCLUDE_LIST
- Updated man page with blackout documentation

* Thu Mar 13 2026 Francesco Defilippo <francesco@defilippo.org> - 1.11.0-1
- Renamed PROMETHEUS_HOST to PROMETHEUS_METRICS_BIND_HOST
- Renamed PROMETHEUS_PORT to PROMETHEUS_METRICS_BIND_PORT
- Default Prometheus port changed from 9101 to 1974
- Default bind address changed to 0.0.0.0 (all interfaces)
- Renamed MCP_HTTP_HOST and MCP_HTTP_PORT defaults to 0.0.0.0 and 1969
- Added SERVER_ROLE configuration for server identification
- Added server_role field to all MCP tool outputs
- Enhanced documentation with log level descriptions
- Backward compatibility maintained for old variable names
- Updated man page to v1.5

* Thu Mar 13 2026 Francesco Defilippo <francesco@defilippo.org> - 1.11.0-1
- Added MCP User Filter Management tools:
  * get_user_filters: Get current user include/exclude filter configurations
  * set_user_exclude_list: Set users to exclude from CPU limits (regex support)
  * set_user_include_list: Set users to include in monitoring (regex support)
  * validate_user_filter_pattern: Validate regex patterns with example matches
- Automatic configuration backup with timestamp before modifications
- Atomic configuration save with rollback on error
- Automatic config reload trigger after filter changes
- All write operations require MCP_ALLOW_WRITE_OPS=true
- Updated MCP documentation with user filter examples

* Thu Mar 13 2026 Francesco Defilippo <francesco@defilippo.org> - 1.10.1-1
- Added periodic configuration check (every 30 seconds)
- Fixed config watcher not detecting changes from some text editors
- Improved logging for configuration reload events

* Thu Mar 12 2026 Francesco Defilippo <francesco@defilippo.org> - 1.10.0-1
- USER_EXCLUDE_LIST now supports regex patterns (like USER_INCLUDE_LIST)
- Multiple patterns supported (comma-separated)
- Pattern validation on configuration load
- Backward compatibility: exact username matches still work
- Updated documentation with regex examples

* Thu Mar 12 2026 Francesco Defilippo <francesco@defilippo.org> - 1.9.0-1
- Added USER_INCLUDE_LIST with regex support
- Filter users to include in monitoring using regex patterns
- Multiple patterns supported (comma-separated)
- Pattern validation on startup (error on invalid regex)
- Empty list = all users included (default behavior)
- Updated documentation and examples

* Wed Mar 11 2026 Francesco Defilippo <francesco@defilippo.org> - 1.8.0-1
- Renamed USER_WHITELIST to USER_EXCLUDE_LIST (breaking change)
- Behavior inverted: list now EXCLUDES users from limits
- Backward compatibility: USER_WHITELIST still works but deprecated
- Updated configuration examples and documentation

* Wed Mar 11 2026 Francesco Defilippo <francesco@defilippo.org> - 1.7.0-1
- Added process exclusion blacklist (automatic)
- System processes automatically excluded from CPU limits:
  * systemd, dbus-daemon, polkitd, NetworkManager
  * sshd, cron, rsyslogd, dockerd, containerd
  * nginx, apache2, mysqld, postgres, redis-server
  * And 40+ other infrastructure processes
- Users with only excluded processes are not limited
- Configurable via IsProcessExcluded() function

* Wed Mar 11 2026 Francesco Defilippo <francesco@defilippo.org> - 1.6.0-1
- Fixed USER_WHITELIST parsing (was not working correctly)
- USER_WHITELIST now correctly includes specified users
- Empty or commented whitelist = all users included
- Updated documentation with correct behavior

* Wed Mar 11 2026 Francesco Defilippo <francesco@defilippo.org> - 1.5.0-1
- Renamed PROMETHEUS_HOST to PROMETHEUS_METRICS_BIND_HOST
- Renamed PROMETHEUS_PORT to PROMETHEUS_METRICS_BIND_PORT
- Default Prometheus port changed from 9101 to 1974
- Default bind address changed to 0.0.0.0 (all interfaces)
- Added inline comment support in configuration parser
- Backward compatibility: old variable names still work

* Wed Mar 11 2026 Francesco Defilippo <francesco@defilippo.org> - 1.4.0-1
- Added SERVER_ROLE configuration variable
- Added server_role to MCP tool outputs (get_system_status, get_active_users,
  get_limits_status, get_configuration, get_cpu_report, get_mem_report)
- Updated documentation for multi-server environment identification

* Wed Mar 11 2026 Francesco Defilippo <francesco@defilippo.org> - 1.3.0-1
- Added get_cpu_report MCP tool for comprehensive CPU usage reports
- Added get_mem_report MCP tool for comprehensive memory usage reports
- Added hostname field to all MCP metric outputs
- Implemented HTTP logging middleware for MCP requests
- Fixed logger initialization to respect LOG_LEVEL from config
- All metric tools now include hostname for multi-server environments

* Tue Mar 10 2026 Francesco Defilippo <francesco@defilippo.org> - 1.2.0-1
- Added MCP server for AI assistant integration (Model Context Protocol)
- 11 MCP tools: get_system_status, get_user_metrics, get_active_users,
  get_limits_status, get_cgroup_info, get_configuration, get_control_history,
  activate_limits, deactivate_limits, get_cpu_report, get_mem_report
- 6 MCP resources for URI-based data access
- 3 pre-built prompts: system-health, user-analysis, troubleshooting
- HTTP and stdio transport support
- Comprehensive MCP documentation (MCP-README.md, MCP-BLUEPRINT.md)
- Updated README.md and CHANGELOG.md with MCP information

* Sun Feb 22 2026 Francesco Defilippo <francesco@defilippo.org> - 1.1.0-1
- Added TLS/HTTPS support for Prometheus metrics
- Added TLS certificate generation script (generate-tls-certs.sh)
- Added Basic Authentication support for Prometheus
- Added JWT (Bearer Token) authentication support
- Added per-user metrics: CPU%, Memory (bytes), Process count
- Updated Prometheus metrics documentation
- Added Grafana dashboard with multi-instance support
- Added comprehensive TLS configuration guide
- Added multi-instance monitoring guide
- Added Prometheus alerting rules
- Added Prometheus query examples
- Single RPM package with all components (no separate -doc package)
- Complete cgroups v2 CPU management
- Dynamic configuration reload
- Systemd service integration
- Comprehensive man page documentation

* Thu Jan 22 2026 Francesco Defilippo <francesco@defilippo.org> - 1.0.0-1
- Initial RPM release with man page support
- Complete cgroups v2 CPU management
- Prometheus metrics support
- Dynamic configuration reload
- Systemd service integration
- Comprehensive man page documentation
