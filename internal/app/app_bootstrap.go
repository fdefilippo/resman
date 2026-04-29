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
			"cgroup_base", a.cfg.CgroupBase,
			"error", err,
		)
		fmt.Fprintf(os.Stderr, "\nFailed to initialize cgroup manager: %v\n", err)
		fmt.Fprintf(os.Stderr, "\nTroubleshooting:\n")
		fmt.Fprintf(os.Stderr, "  1. Verify cgroups v2 is enabled: mount | grep cgroup\n")
		fmt.Fprintf(os.Stderr, "  2. Enable cgroups v2: grubby --update-kernel=ALL --args='systemd.unified_cgroup_hierarchy=1'\n")
		fmt.Fprintf(os.Stderr, "  3. Reboot and verify: cat /sys/fs/cgroup/cgroup.controllers\n")
		fmt.Fprintf(os.Stderr, "  4. Check permissions on %s\n", a.cfg.CgroupRoot)
		a.err = err
		return a
	}
	a.cgroupMgr = cgroupMgr
	return a
}

// WithMetricsCollector inizializza il collector delle metriche.
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
	return a
}

// WithDatabase inizializza il database metriche se abilitato.
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
		fmt.Fprintf(os.Stderr, "  1. Ensure directory exists: mkdir -p %s\n", dbDir)
		fmt.Fprintf(os.Stderr, "  2. Check write permissions\n")
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

	a.metricsCollector.SetUsernameCacheTTL(time.Duration(a.cfg.UsernameCacheTTL) * time.Minute)
	a.logger.Info("Username cache configured",
		"ttl_minutes", a.cfg.UsernameCacheTTL,
	)

	if deleted, err := dbManager.CleanupOldData(a.cfg.MetricsDBRetentionDays); err == nil && deleted > 0 {
		a.logger.Info("Cleaned up old metrics data", "records_deleted", deleted)
	}

	return a
}

// WithPrometheus inizializza l'exporter Prometheus se abilitato.
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

// WithStateManager inizializza il decision engine.
func (a *App) WithStateManager() *App {
	if a.err != nil {
		return a
	}

	stateManager, err := state.NewManager(a.cfg, a.metricsCollector, a.cgroupMgr, a.prometheusExporter)
	if err != nil {
		a.logger.Error("Failed to initialize state manager", "error", err)
		fmt.Fprintf(os.Stderr, "\nFailed to initialize state manager: %v\n", err)
		a.err = err
		return a
	}
	a.stateManager = stateManager
	return a
}

// WithConfigWatcher abilita il reload automatico della configurazione.
func (a *App) WithConfigWatcher() *App {
	if a.err != nil || a.configPath == "" {
		return a
	}

	reloader := reloader.NewReloader(a.stateManager, a.cgroupMgr, a.metricsCollector, a.prometheusExporter)
	configWatcher, err := config.NewWatcher(a.configPath, a.cfg, reloader)
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

// WithMCPServer avvia il server MCP se abilitato.
func (a *App) WithMCPServer() *App {
	if a.err != nil {
		return a
	}

	if !a.cfg.MCPEnabled {
		a.logger.Info("MCP server disabled by configuration")
		return a
	}

	mcpServer, err := mcp.NewServer(a.cfg, a.stateManager, a.metricsCollector, a.cgroupMgr, a.dbManager)
	if err != nil {
		a.logger.Error("Failed to initialize MCP server", "error", err)
		fmt.Fprintf(os.Stderr, "\nWarning: Failed to initialize MCP server: %v\n", err)
		fmt.Fprintf(os.Stderr, "MCP features disabled. To fix:\n")
		fmt.Fprintf(os.Stderr, "  1. Check configuration\n")
		fmt.Fprintf(os.Stderr, "  2. Or disable: MCP_ENABLED=false\n")
		return a
	}

	if err := mcpServer.Start(a.ctx); err != nil {
		a.logger.Error("Failed to start MCP server", "error", err)
		fmt.Fprintf(os.Stderr, "\nWarning: Failed to start MCP server: %v\n", err)
		fmt.Fprintf(os.Stderr, "MCP server unavailable. Check:\n")
		fmt.Fprintf(os.Stderr, "  1. Transport type: %s\n", a.cfg.MCPTransport)
		if a.cfg.MCPTransport == "http" {
			fmt.Fprintf(os.Stderr, "  2. Port availability: %d\n", a.cfg.MCPHTTPPort)
		}
		return a
	}

	a.mcpServer = mcpServer
	a.logger.Info("MCP server started",
		"transport", a.cfg.MCPTransport,
		"port", a.cfg.MCPHTTPPort,
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
		conn.Close()
		return false
	}
	return true
}
