package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/logging"
	"github.com/fdefilippo/resman/mcp"
	"github.com/fdefilippo/resman/metrics"
	"github.com/fdefilippo/resman/reloader"
	"github.com/fdefilippo/resman/state"
)

// App contiene i componenti runtime del daemon.
type App struct {
	cfg        *config.Config
	configPath string
	ctx        context.Context
	cancel     context.CancelFunc
	sigChan    <-chan os.Signal
	logger     *logging.Logger
	err        error

	cgroupMgr          *cgroup.Manager
	metricsCollector   *metrics.Collector
	dbManager          *database.DatabaseManager
	prometheusExporter *metrics.PrometheusExporter
	stateManager       *state.Manager
	configWatcher      *config.Watcher
	mcpServer          *mcp.Server
	psiWatcher         *cgroup.PSIWatcher
	psiEvents          <-chan cgroup.PSIEvent
}

// NewApp crea il builder dell'applicazione.
func NewApp(cfg *config.Config, configPath string, ctx context.Context, cancel context.CancelFunc, sigChan <-chan os.Signal, logger *logging.Logger) *App {
	return &App{
		cfg:        cfg,
		configPath: configPath,
		ctx:        ctx,
		cancel:     cancel,
		sigChan:    sigChan,
		logger:     logger,
	}
}

// WithCgroupManager inizializza il manager cgroup.
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

// Run avvia signal handler, PSI watcher e loop principale.
func (a *App) Run() error {
	if a.err != nil {
		return a.err
	}

	a.startSignalHandler()
	a.startPSIWatcher()
	return a.runControlLoop()
}

func (a *App) startSignalHandler() {
	go func() {
		for {
			select {
			case <-a.ctx.Done():
				return
			case sig := <-a.sigChan:
				switch sig {
				case syscall.SIGHUP:
					a.logger.Info("Received SIGHUP, forcing configuration reload")
					if a.configWatcher != nil {
						go func() {
							time.Sleep(100 * time.Millisecond)
							a.configWatcher.HandleConfigChange()
						}()
					} else {
						a.logger.Warn("Config watcher not available for SIGHUP reload")
					}
				case syscall.SIGINT, syscall.SIGTERM:
					a.logger.Info("Received termination signal, initiating shutdown",
						"signal", sig.String(),
					)
					a.cancel()

					go func() {
						timeout := time.Duration(a.cfg.GetMCPShutdownTimeout()*2) * time.Second
						time.Sleep(timeout)
						a.logger.Warn("Shutdown timeout exceeded — cleanup did not complete. Forcing exit.",
							"timeout_seconds", a.cfg.GetMCPShutdownTimeout()*2,
						)
						syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
					}()
				}
			}
		}
	}()
}

func (a *App) startPSIWatcher() {
	if !a.cfg.GetPSIEventDriven() {
		return
	}

	psiWatcher := cgroup.NewPSIWatcher(uint64(a.cfg.GetPSIWindowUs()))
	psiWatcher.SetThreshold("cpu", uint64(a.cfg.GetPSICPUStallThreshold()))
	psiWatcher.SetThreshold("io", uint64(a.cfg.GetPSIOStallThreshold()))
	if err := psiWatcher.Start(); err != nil {
		a.logger.Warn("Failed to start PSI watcher, falling back to polling", "error", err)
		return
	}

	sysCPUPressure := a.cfg.CgroupRoot + "/cpu.pressure"
	sysIOPressure := a.cfg.CgroupRoot + "/io.pressure"
	if err := psiWatcher.AddMonitor(0, "cpu", sysCPUPressure); err != nil {
		a.logger.Warn("Failed to monitor system cpu.pressure", "error", err)
	}
	if err := psiWatcher.AddMonitor(0, "io", sysIOPressure); err != nil {
		a.logger.Warn("Failed to monitor system io.pressure", "error", err)
	}

	a.psiWatcher = psiWatcher
	a.psiEvents = psiWatcher.Events()
	a.stateManager.RegisterPSIWatcher(psiWatcher)
	a.logger.Info("PSI event-driven mode enabled",
		"cpu_threshold_us", a.cfg.GetPSICPUStallThreshold(),
		"io_threshold_us", a.cfg.GetPSIOStallThreshold(),
		"window_us", a.cfg.GetPSIWindowUs(),
		"note", "PSI events trigger user CPU weight boosts and extra control cycles",
	)
}

func (a *App) runControlLoop() error {
	pollingInterval := a.stateManager.GetConfig().GetPollingInterval()
	a.logger.Info("Entering main control loop", "polling_interval_seconds", pollingInterval)

	ticker := time.NewTicker(time.Duration(pollingInterval) * time.Second)
	defer func() {
		ticker.Stop()
	}()

	if err := a.stateManager.RunControlCycleWithTrigger(a.ctx, state.ControlCycleTriggerInitial); err != nil {
		a.logger.Error("Error in initial control cycle",
			"cycle_id", "initial",
			"error", err,
		)
		fmt.Fprintf(os.Stderr, "\nWarning: Error in initial control cycle: %v\n", err)
		fmt.Fprintf(os.Stderr, "This may indicate:\n")
		fmt.Fprintf(os.Stderr, "  1. Cgroup setup issues\n")
		fmt.Fprintf(os.Stderr, "  2. Permission problems\n")
		fmt.Fprintf(os.Stderr, "  3. Invalid configuration\n")
		fmt.Fprintf(os.Stderr, "Check logs for details: tail -f %s\n", a.cfg.LogFile)
	}

	cycleComplete := make(chan struct{})
	close(cycleComplete)

	for {
		select {
		case <-a.ctx.Done():
			a.shutdown()
			return nil
		case <-ticker.C:
			ticker = a.handleTickerCycle(ticker, &pollingInterval, &cycleComplete)
		case psiEvent, ok := <-a.psiEvents:
			if ok {
				a.handlePSIEvent(psiEvent, &cycleComplete)
			}
		}
	}
}

func (a *App) handleTickerCycle(ticker *time.Ticker, pollingInterval *int, cycleComplete *chan struct{}) *time.Ticker {
	currentPollingInterval := a.stateManager.GetConfig().GetPollingInterval()
	if currentPollingInterval != *pollingInterval {
		ticker.Stop()
		*pollingInterval = currentPollingInterval
		ticker = time.NewTicker(time.Duration(*pollingInterval) * time.Second)
		a.logger.Info("Polling interval updated", "polling_interval_seconds", *pollingInterval)
	}

	startTime := time.Now()
	if !a.acquireCycleSlot(*cycleComplete, "ticker", *pollingInterval) {
		return ticker
	}

	*cycleComplete = make(chan struct{})
	if err := a.stateManager.RunControlCycleWithTrigger(a.ctx, state.ControlCycleTriggerTicker); err != nil {
		a.logger.Error("Error in control cycle", "error", err)
	}

	duration := time.Since(startTime)
	close(*cycleComplete)

	if duration > time.Duration(*pollingInterval/2)*time.Second {
		a.logger.Warn("Control cycle took longer than expected",
			"duration_ms", duration.Milliseconds(),
			"polling_interval_ms", *pollingInterval*1000,
		)
	} else {
		a.logger.Debug("Control cycle completed",
			"duration_ms", duration.Milliseconds(),
		)
	}

	return ticker
}

func (a *App) handlePSIEvent(psiEvent cgroup.PSIEvent, cycleComplete *chan struct{}) {
	a.logger.Debug("PSI event received",
		"type", psiEvent.Type,
		"uid", psiEvent.UID,
		"some_avg10", psiEvent.SomeAvg10,
	)

	if psiEvent.UID > 0 {
		a.stateManager.OnUserPSIEvent(psiEvent)
	}

	psiScope := "system"
	if psiEvent.UID > 0 {
		psiScope = "user"
	}
	if a.prometheusExporter != nil {
		a.prometheusExporter.RecordPSIEvent(psiEvent.Type, psiScope, psiEvent.Timestamp)
	}

	if !a.acquireCycleSlot(*cycleComplete, "psi", 0) {
		return
	}

	*cycleComplete = make(chan struct{})
	trigger := "psi_" + psiScope + "_" + psiEvent.Type
	if err := a.stateManager.RunControlCycleWithTrigger(a.ctx, trigger); err != nil {
		a.logger.Error("Error in control cycle (PSI-triggered)", "error", err)
	}
	close(*cycleComplete)
}

func (a *App) acquireCycleSlot(cycleComplete chan struct{}, source string, pollingInterval int) bool {
	select {
	case <-cycleComplete:
		return true
	default:
		if source == "psi" {
			a.logger.Debug("Skipping PSI-triggered cycle - previous still running")
		} else {
			a.logger.Warn("Skipping control cycle - previous cycle still running",
				"reason", "backpressure",
				"polling_interval_ms", pollingInterval*1000,
			)
		}
		return false
	}
}

func (a *App) shutdown() {
	a.logger.Info("Shutting down main control loop")

	if a.configWatcher != nil {
		a.configWatcher.Stop()
	}

	if err := a.stateManager.Cleanup(); err != nil {
		a.logger.Error("Error during state manager cleanup", "error", err)
		fmt.Fprintf(os.Stderr, "\nWarning: Error during cleanup: %v\n", err)
	}

	if a.mcpServer != nil {
		if err := a.mcpServer.Stop(); err != nil {
			a.logger.Error("Error stopping MCP server", "error", err)
		}
	}

	if a.dbManager != nil {
		if err := a.dbManager.Close(); err != nil {
			a.logger.Error("Error closing database manager", "error", err)
		}
	}

	if a.metricsCollector != nil {
		a.metricsCollector.Stop()
		a.logger.Info("Metrics collector stopped")
	}

	if a.psiWatcher != nil {
		a.psiWatcher.Stop()
		a.logger.Info("PSI watcher stopped")
	}

	a.logger.Info("Shutdown completed")
}

// checkPortAvailable verifica se una porta TCP è disponibile.
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
