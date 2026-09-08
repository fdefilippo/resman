package app

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/internal/systemdunit"
	"github.com/fdefilippo/resman/mcp"
	"github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/reloader"
	"github.com/fdefilippo/resman/state"
)

func (a *App) WithCgroupManager() *App {
	if a.err != nil {
		return a
	}

	cgroupMgr, err := cgroup.NewManager(a.cfg)
	if err != nil {
		a.logger.Error("Failed to initialize cgroup manager",
			"cgroup_root", a.cfg.CgroupRoot,
			"error", err,
		)
		fmt.Fprintf(os.Stderr, "\nFailed to initialize cgroup manager: %v\n", err)
		fmt.Fprintf(os.Stderr, "\nTroubleshooting:\n")
		fmt.Fprintf(os.Stderr, "  1. Verify cgroups v2 is enabled: mount | grep cgroup\n")
		fmt.Fprintf(os.Stderr, "  2. On Enterprise Linux compatible systems, enable cgroups v2 and PSI: grubby --update-kernel=ALL --args='systemd.unified_cgroup_hierarchy=1 psi=1'\n")
		fmt.Fprintf(os.Stderr, "  3. Reboot and verify: cat /sys/fs/cgroup/cgroup.controllers\n")
		fmt.Fprintf(os.Stderr, "  4. Verify PSI if PSI_EVENT_DRIVEN=true: ls /proc/pressure\n")
		fmt.Fprintf(os.Stderr, "  5. Check permissions on %s\n", a.cfg.CgroupRoot)
		a.err = classifyCgroupStartupError(err)
		return a
	}
	a.cgroupMgr = cgroupMgr
	return a
}

func classifyCgroupStartupError(err error) error {
	wrapped := fmt.Errorf("initialize cgroup manager: %w", err)
	if cgroup.IsRequiredCapabilityError(err) {
		return NewPermanentStartupError(wrapped)
	}
	return wrapped
}

// WithMetricsCollector initializes the metrics collector.
func (a *App) WithMetricsCollector() *App {
	if a.err != nil {
		return a
	}

	metricsCollector, err := metrics.NewCollector(a.cfg)
	if err != nil {
		a.logger.Error("Failed to initialize metrics collector", "error", err)
		fmt.Fprintf(os.Stderr, "\nFailed to initialize metrics collector: %v\n", err)
		a.err = err
		return a
	}
	a.metricsCollector = metricsCollector
	a.cpuSamplingCadence = metricsCollector
	return a
}

// WithDatabase initializes metrics persistence when enabled.
func (a *App) WithDatabase() *App {
	if a.err != nil {
		return a
	}

	if !a.cfg.MetricsDBEnabled {
		a.logger.Info("Metrics database disabled by configuration")
		return a
	}

	dbManager, err := database.NewDatabaseManager(a.cfg.MetricsDBPath)
	if err != nil {
		a.logger.Warn("Failed to initialize metrics database, disabling database writing",
			"path", a.cfg.MetricsDBPath,
			"error", err,
		)
		fmt.Fprintf(os.Stderr, "\nWarning: Failed to initialize metrics database at %s: %v\n", a.cfg.MetricsDBPath, err)
		fmt.Fprintf(os.Stderr, "Database features disabled. To fix:\n")
		dbDir := "."
		if idx := strings.LastIndex(a.cfg.MetricsDBPath, "/"); idx > 0 {
			dbDir = a.cfg.MetricsDBPath[:idx]
		}
		fmt.Fprintf(os.Stderr, "  1. Ensure %s is owned by UID %d, writable, mode 0700, and below a stable non-symlink hierarchy\n", dbDir, os.Geteuid())
		fmt.Fprintf(os.Stderr, "  2. Ensure the database and existing -wal/-shm sidecars are regular files owned by UID %d with mode 0600\n", os.Geteuid())
		fmt.Fprintf(os.Stderr, "  3. Or disable with METRICS_DB_ENABLED=false\n")
		a.cfg.MetricsDBEnabled = false
		return a
	}

	dbWriter := metrics.NewDBWriter(dbManager, a.cfg.MetricsDBWriteInterval)
	a.metricsCollector.SetDBWriter(dbWriter)
	a.dbManager = dbManager

	a.logger.Info("Metrics database initialized",
		"path", a.cfg.MetricsDBPath,
		"retention_days", a.cfg.MetricsDBRetentionDays,
		"write_interval", a.cfg.MetricsDBWriteInterval,
	)

	if deleted, err := dbManager.CleanupOldData(a.cfg.MetricsDBRetentionDays); err != nil {
		a.logger.Warn("Failed to apply metrics database retention at startup",
			"retention_days", a.cfg.MetricsDBRetentionDays,
			"error", err,
		)
	} else if deleted > 0 {
		a.logger.Info("Cleaned up old metrics data", "records_deleted", deleted)
	}

	return a
}

// WithPrometheus initializes the Prometheus exporter when enabled.
func (a *App) WithPrometheus() *App {
	if a.err != nil {
		return a
	}

	if !a.cfg.EnablePrometheus {
		a.logger.Info("Prometheus exporter disabled by configuration")
		return a
	}

	if !checkPortAvailable(a.cfg.PrometheusMetricsBindHost, a.cfg.PrometheusMetricsBindPort) {
		a.logger.Warn("Prometheus port already in use, disabling exporter",
			"host", a.cfg.PrometheusMetricsBindHost,
			"port", a.cfg.PrometheusMetricsBindPort,
		)
		fmt.Fprintf(os.Stderr, "\nWarning: Prometheus metrics port %s:%d already in use, disabling exporter\n", a.cfg.PrometheusMetricsBindHost, a.cfg.PrometheusMetricsBindPort)
		fmt.Fprintf(os.Stderr, "To fix:\n")
		fmt.Fprintf(os.Stderr, "  1. Check what's using the port: lsof -i :%d or netstat -tlnp | grep %d\n", a.cfg.PrometheusMetricsBindPort, a.cfg.PrometheusMetricsBindPort)
		fmt.Fprintf(os.Stderr, "  2. Change port: PROMETHEUS_METRICS_BIND_PORT=%d\n", a.cfg.PrometheusMetricsBindPort+1)
		fmt.Fprintf(os.Stderr, "  3. Or disable: ENABLE_PROMETHEUS=false\n")
		a.cfg.EnablePrometheus = false
		return a
	}

	prometheusExporter, err := metrics.NewPrometheusExporter(a.cfg)
	if err != nil {
		a.logger.Error("Failed to create Prometheus exporter", "error", err)
		fmt.Fprintf(os.Stderr, "\nWarning: Failed to create Prometheus exporter: %v\n", err)
		fmt.Fprintf(os.Stderr, "Metrics will not be exposed. To fix:\n")
		fmt.Fprintf(os.Stderr, "  1. Check configuration\n")
		fmt.Fprintf(os.Stderr, "  2. Or disable: ENABLE_PROMETHEUS=false\n")
		return a
	}

	if prometheusExporter == nil {
		return a
	}
	if a.metricsCollector != nil {
		prometheusExporter.SetUsernameResolver(a.metricsCollector.GetUsernameFromUID)
	}

	if err := prometheusExporter.Start(a.ctx); err != nil {
		a.logger.Error("Failed to start Prometheus exporter", "error", err)
		fmt.Fprintf(os.Stderr, "\nWarning: Failed to start Prometheus exporter: %v\n", err)
		return a
	}

	a.prometheusExporter = prometheusExporter
	a.logger.Info("Prometheus exporter started",
		"host", a.cfg.PrometheusMetricsBindHost,
		"port", a.cfg.PrometheusMetricsBindPort,
	)
	return a
}

// WithStateManager initializes the decision engine.
func (a *App) WithStateManager() *App {
	if a.err != nil {
		return a
	}

	reserve, err := cpupoints.NewReservePoints(uint64(a.cfg.GetCPUReservePoints()))
	if err != nil {
		return a.failCPUPointsStartup("invalid CPU Points reserve", err, true)
	}
	root, err := cpupoints.NewRootPoints(uint64(a.cfg.GetCPURootPoints()))
	if err != nil {
		return a.failCPUPointsStartup("invalid CPU Points root entitlement", err, true)
	}
	bestEffort, err := cpupoints.NewBestEffortPoints(uint64(a.cfg.GetCPUBestEffortPoints()))
	if err != nil {
		return a.failCPUPointsStartup("invalid CPU Points best-effort entitlement", err, true)
	}
	mapPath, err := cpupoints.NewPolicyMapPath(a.cfg.GetCPUPointsFile())
	if err != nil {
		return a.failCPUPointsStartup("invalid CPU Points map path", err, true)
	}
	policy, err := cpupoints.NewPolicyLoader().Load(cpupoints.PolicyInputs{
		Reserve: reserve, Root: root, BestEffort: bestEffort, MapPath: mapPath,
	}, cpupoints.NSSIdentityResolver{})
	if err != nil {
		return a.failCPUPointsStartup("load CPU Points policy", err, true)
	}
	capacity, err := cpupoints.NewLiveCapacityProvider(cpupoints.NewSysfsOnlineCPUSource(), policy.Pool())
	if err != nil {
		return a.failCPUPointsStartup("initialize CPU Points live capacity", err, false)
	}
	status := a.cgroupMgr.EnforcementStatus()
	options := []state.ManagerOption{
		state.WithCPUPointsRuntime(policy, capacity),
		state.WithEnforcementStatus(status),
	}
	var systemdAdapter *systemdunit.Adapter
	if status.Mode == cgroup.EnforcementModeObservationOnly && status.Reason == cgroup.EnforcementReasonSystemdOwnsHostWorkloads {
		systemdAdapter, err = systemdunit.New(a.ctx, a.cfg.CgroupRoot, systemdunit.DefaultCallTimeout)
		if err != nil {
			a.logger.Warn("Systemd-native CPU enforcement unavailable; remaining observation-only",
				"reason", status.Reason,
				"error", err,
			)
		} else {
			logSystemdLeaseRecovery(a.logger, systemdAdapter.RecoveryReport())
			options = append(options, state.WithSystemdCPUEnforcement(systemdAdapter))
			a.logger.Info("Systemd-native CPU enforcement selected",
				"enforcement_mode", cgroup.EnforcementModeSystemdNative,
				"reason", cgroup.EnforcementReasonSystemdNativeAdapter,
			)
		}
	}

	stateManager, err := state.NewManager(
		a.cfg,
		a.metricsCollector,
		a.cgroupMgr,
		a.prometheusExporter,
		options...,
	)
	if err != nil {
		if systemdAdapter != nil {
			systemdAdapter.Close()
		}
		a.logger.Error("Failed to initialize state manager", "error", err)
		fmt.Fprintf(os.Stderr, "\nFailed to initialize state manager: %v\n", err)
		a.err = err
		return a
	}
	a.stateManager = stateManager
	return a
}

func logSystemdLeaseRecovery(logger appLogger, report []systemdunit.LeaseRecoveryOutcome) {
	if logger == nil || len(report) == 0 {
		return
	}
	counts := map[systemdunit.LeaseRecoveryState]int{
		systemdunit.LeaseRecoveryReclaimed: 0,
		systemdunit.LeaseRecoveryOrphaned:  0,
		systemdunit.LeaseRecoveryInactive:  0,
		systemdunit.LeaseRecoveryPending:   0,
		systemdunit.LeaseRecoveryConflict:  0,
	}
	for _, outcome := range report {
		if _, known := counts[outcome.State]; known {
			counts[outcome.State]++
		}
	}
	logger.Info("Systemd property lease recovery completed",
		"total_units", len(report),
		"reclaimed_units", counts[systemdunit.LeaseRecoveryReclaimed],
		"orphaned_units", counts[systemdunit.LeaseRecoveryOrphaned],
		"inactive_cleaned_units", counts[systemdunit.LeaseRecoveryInactive],
		"pending_units", counts[systemdunit.LeaseRecoveryPending],
		"conflicted_units", counts[systemdunit.LeaseRecoveryConflict],
	)
}

func (a *App) failCPUPointsStartup(operation string, err error, permanent bool) *App {
	wrapped := fmt.Errorf("%s: %w", operation, err)
	a.logger.Error("Failed to initialize CPU Points", "operation", operation, "error", err)
	fmt.Fprintf(os.Stderr, "\nFailed to initialize CPU Points: %v\n", wrapped)
	if permanent {
		a.err = NewPermanentStartupError(wrapped)
	} else {
		a.err = wrapped
	}
	return a
}

// WithConfigWatcher enables automatic configuration reloads.
func (a *App) WithConfigWatcher() *App {
	if a.err != nil || a.configPath == "" {
		return a
	}

	reloader := reloader.NewReloader(a.stateManager, a.cgroupMgr, a.metricsCollector, a.applyReloadedConfig)
	var watcherOptions []config.WatcherOption
	if a.prometheusExporter != nil {
		watcherOptions = append(watcherOptions, config.WithReloadObserver(a.prometheusExporter))
	}
	configWatcher, err := config.NewWatcher(a.configPath, a.currentConfig(), reloader, watcherOptions...)
	if err != nil {
		a.logger.Warn("Failed to create config watcher, continuing without auto-reload",
			"error", err,
		)
		fmt.Fprintf(os.Stderr, "\nWarning: Failed to create config watcher: %v\n", err)
		fmt.Fprintf(os.Stderr, "Configuration auto-reload disabled. To fix:\n")
		fmt.Fprintf(os.Stderr, "  1. Check file permissions: ls -la %s\n", a.configPath)
		fmt.Fprintf(os.Stderr, "  2. Verify inotify limits: cat /proc/sys/fs/inotify/max_user_watches\n")
		return a
	}

	if err := configWatcher.Start(); err != nil {
		a.logger.Warn("Failed to start config watcher", "error", err)
		fmt.Fprintf(os.Stderr, "\nWarning: Failed to start config watcher: %v\n", err)
		return a
	}

	a.configWatcher = configWatcher
	a.logger.Info("Configuration auto-reload enabled", "file", a.configPath)
	return a
}

// WithMCPServer starts the MCP server when enabled.
func (a *App) WithMCPServer() *App {
	if a.err != nil {
		return a
	}

	cfg := a.currentConfig()
	if !cfg.MCPEnabled {
		a.logger.Info("MCP server disabled by configuration")
		return a
	}

	mcpServer, err := mcp.NewServer(cfg, a.stateManager, a.metricsCollector, a.dbManager, a.configWatcher)
	if err != nil {
		a.logger.Error("Failed to initialize MCP server", "error", err)
		fmt.Fprintf(os.Stderr, "\nFailed to initialize MCP server: %v\n", err)
		fmt.Fprintf(os.Stderr, "Daemon startup aborted. To fix:\n")
		fmt.Fprintf(os.Stderr, "  1. Check configuration\n")
		fmt.Fprintf(os.Stderr, "  2. Or disable: MCP_ENABLED=false\n")
		a.err = NewPermanentStartupError(fmt.Errorf("initialize MCP server: %w", err))
		return a
	}

	if err := mcpServer.Start(a.ctx); err != nil {
		a.logger.Error("Failed to start MCP server", "error", err)
		fmt.Fprintf(os.Stderr, "\nFailed to start MCP server: %v\n", err)
		fmt.Fprintf(os.Stderr, "Daemon startup aborted. Check:\n")
		fmt.Fprintf(os.Stderr, "  1. Transport type: %s\n", cfg.MCPTransport)
		if cfg.MCPTransport == "http" {
			fmt.Fprintf(os.Stderr, "  2. Port availability: %d\n", cfg.MCPHTTPPort)
		}
		a.err = fmt.Errorf("start MCP server: %w", err)
		return a
	}

	a.mcpServer = mcpServer
	a.logger.Info("MCP server started",
		"transport", cfg.MCPTransport,
		"port", cfg.MCPHTTPPort,
	)
	return a
}

func checkPortAvailable(host string, port int) bool {
	timeout := time.Second
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return true
	}
	if conn != nil {
		_ = conn.Close()
		return false
	}
	return true
}
