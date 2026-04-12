/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <https://www.gnu.org/licenses/>.
 */
// main.go
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
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

var version = "1.22.0"

// checkPortAvailable verifica se una porta TCP è disponibile
func checkPortAvailable(host string, port int) bool {
	timeout := time.Second
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return true // Porta disponibile
	}
	if conn != nil {
		conn.Close()
		return false // Porta già in uso
	}
	return true
}

func main() {
	// Parsing dei flag
	configPath := flag.String("config", "/etc/resman.conf", "Path to configuration file")
	showVersion := flag.Bool("version", false, "Show version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("resman %s\n", version)
		return
	}

	// Caricamento configurazione iniziale
	cfg, err := config.LoadAndValidate(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration from %s: %v\n\n", *configPath, err)
		fmt.Fprintf(os.Stderr, "Common issues:\n")
		fmt.Fprintf(os.Stderr, "  - File does not exist: create /etc/resman.conf from example\n")
		fmt.Fprintf(os.Stderr, "  - Invalid syntax: check key=value format\n")
		fmt.Fprintf(os.Stderr, "  - Invalid values: verify thresholds, ports, and paths\n")
		os.Exit(1)
	}

	// Inizializzazione logger con valori dalla configurazione
	logging.InitLogger(cfg.LogLevel, cfg.LogFile, cfg.LogMaxSize, cfg.UseSyslog)
	logger := logging.GetLogger()

	logger.Info("Starting Resource Manager", "version", version)
	logger.Info("Configuration loaded successfully",
		"log_level", cfg.LogLevel,
		"log_file", cfg.LogFile,
		"use_syslog", cfg.UseSyslog,
	)

	if cfg.UseSyslog {
		logger.Info("Syslog logging enabled")
	} else {
		logger.Debug("File logging enabled")
	}

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Canale per i segnali
	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// Inizializzazione componenti
	logger.Info("Initializing components:")

	// 1. Cgroup Manager
	cgroupMgr, err := cgroup.NewManager(cfg)
	if err != nil {
		logger.Error("Failed to initialize cgroup manager",
			"cgroup_root", cfg.CgroupRoot,
			"cgroup_base", cfg.CgroupBase,
			"error", err,
		)
		fmt.Fprintf(os.Stderr, "\nFailed to initialize cgroup manager: %v\n", err)
		fmt.Fprintf(os.Stderr, "\nTroubleshooting:\n")
		fmt.Fprintf(os.Stderr, "  1. Verify cgroups v2 is enabled: mount | grep cgroup\n")
		fmt.Fprintf(os.Stderr, "  2. Enable cgroups v2: grubby --update-kernel=ALL --args='systemd.unified_cgroup_hierarchy=1'\n")
		fmt.Fprintf(os.Stderr, "  3. Reboot and verify: cat /sys/fs/cgroup/cgroup.controllers\n")
		fmt.Fprintf(os.Stderr, "  4. Check permissions on %s\n", cfg.CgroupRoot)
		os.Exit(1)
	}
	logger.Info("Cgroup manager initialized",
		"cgroup_root", cfg.CgroupRoot,
		"cgroup_base", cfg.CgroupBase,
	)

	// 2. Metrics Collector
	metricsCollector, err := metrics.NewCollector(cfg)
	if err != nil {
		logger.Error("Failed to initialize metrics collector", "error", err)
		fmt.Fprintf(os.Stderr, "\nFailed to initialize metrics collector: %v\n", err)
		os.Exit(1)
	}

	// 2b. Database Manager (se abilitato)
	var dbManager *database.DatabaseManager
	var dbWriter *metrics.DBWriter

	if cfg.MetricsDBEnabled {
		dbManager, err = database.NewDatabaseManager(cfg.MetricsDBPath)
		if err != nil {
			logger.Warn("Failed to initialize metrics database, disabling database writing",
				"path", cfg.MetricsDBPath,
				"error", err,
			)
			fmt.Fprintf(os.Stderr, "\nWarning: Failed to initialize metrics database at %s: %v\n", cfg.MetricsDBPath, err)
			fmt.Fprintf(os.Stderr, "Database features disabled. To fix:\n")
			fmt.Fprintf(os.Stderr, "  1. Ensure directory exists: mkdir -p %s\n", cfg.MetricsDBPath[:strings.LastIndex(cfg.MetricsDBPath, "/")])
			fmt.Fprintf(os.Stderr, "  2. Check write permissions\n")
			fmt.Fprintf(os.Stderr, "  3. Or disable with METRICS_DB_ENABLED=false\n")
			cfg.MetricsDBEnabled = false
		} else {
			dbWriter = metrics.NewDBWriter(dbManager, cfg.MetricsDBWriteInterval)
			metricsCollector.SetDBWriter(dbWriter)
			logger.Info("Metrics database initialized",
				"path", cfg.MetricsDBPath,
				"retention_days", cfg.MetricsDBRetentionDays,
				"write_interval", cfg.MetricsDBWriteInterval,
			)

			// Imposta TTL cache username
			metricsCollector.SetUsernameCacheTTL(time.Duration(cfg.UsernameCacheTTL) * time.Minute)
			logger.Info("Username cache configured",
				"ttl_minutes", cfg.UsernameCacheTTL,
			)

			// Cleanup iniziale dei dati vecchi
			if deleted, err := dbManager.CleanupOldData(cfg.MetricsDBRetentionDays); err == nil && deleted > 0 {
				logger.Info("Cleaned up old metrics data", "records_deleted", deleted)
			}
		}
	} else {
		logger.Info("Metrics database disabled by configuration")
	}

	// 3. Prometheus Exporter
	var prometheusExporter *metrics.PrometheusExporter

	if cfg.EnablePrometheus {
		// Verifica che la porta sia disponibile
		if !checkPortAvailable(cfg.PrometheusMetricsBindHost, cfg.PrometheusMetricsBindPort) {
			logger.Warn("Prometheus port already in use, disabling exporter",
				"host", cfg.PrometheusMetricsBindHost,
				"port", cfg.PrometheusMetricsBindPort,
			)
			fmt.Fprintf(os.Stderr, "\nWarning: Prometheus metrics port %s:%d already in use, disabling exporter\n", cfg.PrometheusMetricsBindHost, cfg.PrometheusMetricsBindPort)
			fmt.Fprintf(os.Stderr, "To fix:\n")
			fmt.Fprintf(os.Stderr, "  1. Check what's using the port: lsof -i :%d or netstat -tlnp | grep %d\n", cfg.PrometheusMetricsBindPort, cfg.PrometheusMetricsBindPort)
			fmt.Fprintf(os.Stderr, "  2. Change port: PROMETHEUS_METRICS_BIND_PORT=%d\n", cfg.PrometheusMetricsBindPort+1)
			fmt.Fprintf(os.Stderr, "  3. Or disable: ENABLE_PROMETHEUS=false\n")
			cfg.EnablePrometheus = false
		} else {
			prometheusExporter, err = metrics.NewPrometheusExporter(cfg)
			if err != nil {
				logger.Error("Failed to create Prometheus exporter", "error", err)
				fmt.Fprintf(os.Stderr, "\nWarning: Failed to create Prometheus exporter: %v\n", err)
				fmt.Fprintf(os.Stderr, "Metrics will not be exposed. To fix:\n")
				fmt.Fprintf(os.Stderr, "  1. Check configuration\n")
				fmt.Fprintf(os.Stderr, "  2. Or disable: ENABLE_PROMETHEUS=false\n")
				prometheusExporter = nil
			} else if prometheusExporter != nil {
				if err := prometheusExporter.Start(ctx); err != nil {
					logger.Error("Failed to start Prometheus exporter", "error", err)
					fmt.Fprintf(os.Stderr, "\nWarning: Failed to start Prometheus exporter: %v\n", err)
					prometheusExporter = nil
				} else {
					logger.Info("Prometheus exporter started",
						"host", cfg.PrometheusMetricsBindHost,
						"port", cfg.PrometheusMetricsBindPort,
					)
				}
			}
		}
	} else {
		logger.Info("Prometheus exporter disabled by configuration")
	}

	// 4. State Manager
	stateManager, err := state.NewManager(cfg, metricsCollector, cgroupMgr, prometheusExporter)
	if err != nil {
		logger.Error("Failed to initialize state manager", "error", err)
		fmt.Fprintf(os.Stderr, "\nFailed to initialize state manager: %v\n", err)
		os.Exit(1)
	}

	// 5. Config Reloader e Watcher
	var configWatcher *config.Watcher

	if *configPath != "" {
		reloader := reloader.NewReloader(stateManager, cgroupMgr, metricsCollector, prometheusExporter)

		configWatcher, err = config.NewWatcher(*configPath, cfg, reloader)
		if err != nil {
			logger.Warn("Failed to create config watcher, continuing without auto-reload",
				"error", err,
			)
			fmt.Fprintf(os.Stderr, "\nWarning: Failed to create config watcher: %v\n", err)
			fmt.Fprintf(os.Stderr, "Configuration auto-reload disabled. To fix:\n")
			fmt.Fprintf(os.Stderr, "  1. Check file permissions: ls -la %s\n", *configPath)
			fmt.Fprintf(os.Stderr, "  2. Verify inotify limits: cat /proc/sys/fs/inotify/max_user_watches\n")
		} else {
			if err := configWatcher.Start(); err != nil {
				logger.Warn("Failed to start config watcher", "error", err)
				fmt.Fprintf(os.Stderr, "\nWarning: Failed to start config watcher: %v\n", err)
			} else {
				logger.Info("Configuration auto-reload enabled", "file", *configPath)
			}
		}
	}

	// 6. MCP Server
	var mcpServer *mcp.Server

	if cfg.MCPEnabled {
		mcpServer, err = mcp.NewServer(cfg, stateManager, metricsCollector, cgroupMgr, dbManager)
		if err != nil {
			logger.Error("Failed to initialize MCP server", "error", err)
			fmt.Fprintf(os.Stderr, "\nWarning: Failed to initialize MCP server: %v\n", err)
			fmt.Fprintf(os.Stderr, "MCP features disabled. To fix:\n")
			fmt.Fprintf(os.Stderr, "  1. Check configuration\n")
			fmt.Fprintf(os.Stderr, "  2. Or disable: MCP_ENABLED=false\n")
			mcpServer = nil
		} else {
			if err := mcpServer.Start(ctx); err != nil {
				logger.Error("Failed to start MCP server", "error", err)
				fmt.Fprintf(os.Stderr, "\nWarning: Failed to start MCP server: %v\n", err)
				fmt.Fprintf(os.Stderr, "MCP server unavailable. Check:\n")
				fmt.Fprintf(os.Stderr, "  1. Transport type: %s\n", cfg.MCPTransport)
				if cfg.MCPTransport == "http" {
					fmt.Fprintf(os.Stderr, "  2. Port availability: %d\n", cfg.MCPHTTPPort)
				}
				mcpServer = nil
			} else {
				logger.Info("MCP server started",
					"transport", cfg.MCPTransport,
					"port", cfg.MCPHTTPPort,
				)
			}
		}
	} else {
		logger.Info("MCP server disabled by configuration")
	}

	// Goroutine per gestione segnali
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-sigChan:
				switch sig {
				case syscall.SIGHUP:
					logger.Info("Received SIGHUP, forcing configuration reload")
					if configWatcher != nil {
						go func() {
							time.Sleep(100 * time.Millisecond)
							configWatcher.HandleConfigChange()
						}()
					} else {
						logger.Warn("Config watcher not available for SIGHUP reload")
					}
				case syscall.SIGINT, syscall.SIGTERM:
					logger.Info("Received termination signal, initiating shutdown",
						"signal", sig.String(),
					)
					cancel()

					// Monitor shutdown and force kill only if resman cleanup hangs
					// This ensures cleanup() deferred functions always run
					go func() {
						// Wait 2x the MCP shutdown timeout to give cleanup a chance
						timeout := time.Duration(cfg.GetMCPShutdownTimeout()*2) * time.Second
						time.Sleep(timeout)
						logger.Warn("Shutdown timeout exceeded — cleanup did not complete. Forcing exit.",
							"timeout_seconds", cfg.GetMCPShutdownTimeout()*2,
						)
						// Use SIGKILL to force exit (bypasses everything)
						syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
					}()
				}
			}
		}
	}()

	// Loop principale di controllo
	logger.Info("Entering main control loop", "polling_interval_seconds", cfg.PollingInterval)
	ticker := time.NewTicker(time.Duration(cfg.PollingInterval) * time.Second)
	defer ticker.Stop()

	// Esecuzione immediata del primo controllo
	if err := stateManager.RunControlCycle(ctx); err != nil {
		logger.Error("Error in initial control cycle",
			"cycle_id", "initial",
			"error", err,
		)
		fmt.Fprintf(os.Stderr, "\nWarning: Error in initial control cycle: %v\n", err)
		fmt.Fprintf(os.Stderr, "This may indicate:\n")
		fmt.Fprintf(os.Stderr, "  1. Cgroup setup issues\n")
		fmt.Fprintf(os.Stderr, "  2. Permission problems\n")
		fmt.Fprintf(os.Stderr, "  3. Invalid configuration\n")
		fmt.Fprintf(os.Stderr, "Check logs for details: tail -f %s\n", cfg.LogFile)
	}

	// Backpressure channel - signals when control cycle completes
	cycleComplete := make(chan struct{})
	close(cycleComplete) // Initially complete so first cycle runs

	// Main loop
	for {
		select {
		case <-ctx.Done():
			logger.Info("Shutting down main control loop")

			if configWatcher != nil {
				configWatcher.Stop()
			}

			if err := stateManager.Cleanup(); err != nil {
				logger.Error("Error during state manager cleanup", "error", err)
				fmt.Fprintf(os.Stderr, "\nWarning: Error during cleanup: %v\n", err)
			}

			// Stop MCP server
			if mcpServer != nil {
				if err := mcpServer.Stop(); err != nil {
					logger.Error("Error stopping MCP server", "error", err)
				}
			}

			// Close database manager
			if dbManager != nil {
				if err := dbManager.Close(); err != nil {
					logger.Error("Error closing database manager", "error", err)
				}
			}

			// Stop metrics collector (stops background goroutines)
			if metricsCollector != nil {
				metricsCollector.Stop()
				logger.Info("Metrics collector stopped")
			}

			logger.Info("Shutdown completed")
			return

		case <-ticker.C:
			startTime := time.Now()

			// Backpressure: Skip cycle if previous is still running
			select {
			case <-cycleComplete:
				// Previous cycle completed, start new one
			default:
				// Previous cycle still running - skip this cycle
				logger.Warn("Skipping control cycle - previous cycle still running",
					"reason", "backpressure",
					"polling_interval_ms", cfg.PollingInterval*1000,
				)
				continue
			}

			cycleComplete = make(chan struct{})

			if err := stateManager.RunControlCycle(ctx); err != nil {
				logger.Error("Error in control cycle", "error", err)
			}

			duration := time.Since(startTime)
			close(cycleComplete) // Signal cycle completion

			if duration > time.Duration(cfg.PollingInterval/2)*time.Second {
				logger.Warn("Control cycle took longer than expected",
					"duration_ms", duration.Milliseconds(),
					"polling_interval_ms", cfg.PollingInterval*1000,
				)
			} else {
				logger.Debug("Control cycle completed",
					"duration_ms", duration.Milliseconds(),
				)
			}
		}
	}
}
