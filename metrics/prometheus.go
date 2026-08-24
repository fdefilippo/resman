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
// metrics/prometheus.go
package metrics

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/tlsconfig"
	"github.com/fdefilippo/resman/logging"
	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ioStatsSnapshot tracks previous values used to calculate I/O counter deltas.
type ioStatsSnapshot struct {
	ReadBytes  uint64
	WriteBytes uint64
	ReadOps    uint64
	WriteOps   uint64
}

// PrometheusExporter exports metrics in Prometheus format.
type PrometheusExporter struct {
	cfg      *config.Config
	logger   *logging.Logger
	registry *prometheus.Registry
	server   *http.Server

	// Static labels for every metric (from configuration).
	hostname   string
	serverRole string

	// Base metrics with hostname and server_role labels.
	cpuTotalUsage  prometheus.Gauge
	memoryUsage    prometheus.Gauge
	totalMemoryMB  prometheus.Gauge
	cachedMemoryMB prometheus.Gauge
	limitedUsers   prometheus.Gauge

	// ALL USERS metrics include every non-system user (UID >= SYSTEM_UID_MIN).
	allUsersCPUUsage    prometheus.Gauge
	allUsersMemoryUsage prometheus.Gauge
	allUsersCount       prometheus.Gauge

	// LIMITED USERS metrics include only users eligible for CPU limiting.
	limitedUsersCPUUsage    prometheus.Gauge
	limitedUsersMemoryUsage prometheus.Gauge
	limitedUsersCount       prometheus.Gauge

	limitsActive               prometheus.Gauge
	systemLoad                 prometheus.Gauge
	totalCores                 prometheus.Gauge
	actionCores                prometheus.Gauge
	procFSUnavailableProcesses *prometheus.GaugeVec

	// Metrics with additional labels.
	userCPUUsage         *prometheus.GaugeVec
	userCPUUsageAverage  *prometheus.GaugeVec
	userCPUUsageEMA      *prometheus.GaugeVec
	userMemoryUsage      *prometheus.GaugeVec
	userProcessCount     *prometheus.GaugeVec
	userLimited          *prometheus.GaugeVec
	userMemoryHighEvents *prometheus.CounterVec // NEW: memory.high breach events
	userIOReadBytes      *prometheus.CounterVec
	userIOWriteBytes     *prometheus.CounterVec
	userIOReadOps        *prometheus.CounterVec
	userIOWriteOps       *prometheus.CounterVec
	userWorkloadPattern  *prometheus.GaugeVec
	cgroupCPUQuota       *prometheus.GaugeVec
	cgroupCPUPeriod      *prometheus.GaugeVec
	cgroupMemoryUsage    *prometheus.GaugeVec

	// Track active users for metric cleanup.
	activeUserMetrics    map[string]bool   // "uid_username" -> true
	prevMemoryHighEvents map[string]uint64 // "uid_username" -> last known value
	prevIOStats          map[string]ioStatsSnapshot
	prevUserPatterns     map[string]string // "uid_username" -> previous pattern label
	usernameResolver     atomic.Value      // func(int) string

	// Counters are increment-only metrics.
	limitsActivatedTotal   prometheus.Counter
	limitsDeactivatedTotal prometheus.Counter
	controlCyclesTotal     prometheus.Counter
	controlCycleTriggers   *prometheus.CounterVec
	psiEventsTotal         *prometheus.CounterVec
	psiLastEventTimestamp  *prometheus.GaugeVec
	errorsTotal            *prometheus.CounterVec

	// Histograms record operation durations.
	controlCycleDuration      prometheus.Histogram
	metricsCollectionDuration prometheus.Histogram

	mu sync.RWMutex

	// Internal state.
	isRunning bool
	stopChan  chan struct{}

	// Authentication.
	basicAuthPassword string
	jwtSecret         []byte

	// TLS
	tlsConfig *tls.Config
}

// SetUsernameResolver configures the shared UID-to-username resolver.
func (exp *PrometheusExporter) SetUsernameResolver(resolver func(int) string) {
	if exp == nil || resolver == nil {
		return
	}
	exp.usernameResolver.Store(resolver)
}

// NewPrometheusExporter crea un nuovo esportatore Prometheus.
func NewPrometheusExporter(cfg *config.Config) (*PrometheusExporter, error) {
	logger := logging.GetLogger()

	if !cfg.EnablePrometheus {
		logger.Debug("Prometheus exporter disabled by configuration")
		return nil, nil
	}

	logger.Info("Creating Prometheus exporter",
		"host", cfg.PrometheusMetricsBindHost,
		"port", cfg.PrometheusMetricsBindPort,
	)

	// Verifica che la porta sia valida
	if cfg.PrometheusMetricsBindPort <= 0 || cfg.PrometheusMetricsBindPort > 65535 {
		return nil, fmt.Errorf("invalid Prometheus metrics bind port %d (must be 1-65535)", cfg.PrometheusMetricsBindPort)
	}

	// Ottieni hostname e server_role
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	serverRole := cfg.ServerRole
	if serverRole == "" {
		serverRole = "unspecified"
	}

	exp := &PrometheusExporter{
		cfg:                  cfg,
		logger:               logger,
		registry:             prometheus.NewRegistry(),
		hostname:             hostname,
		serverRole:           serverRole,
		stopChan:             make(chan struct{}, 1),
		activeUserMetrics:    make(map[string]bool),
		prevMemoryHighEvents: make(map[string]uint64),
		prevIOStats:          make(map[string]ioStatsSnapshot),
		prevUserPatterns:     make(map[string]string),
	}

	logger.Info("Prometheus exporter created",
		"hostname", exp.hostname,
		"server_role", exp.serverRole,
	)

	// Carica credenziali di autenticazione e certificati TLS
	if err := exp.loadCredentials(); err != nil {
		return nil, fmt.Errorf("failed to load Prometheus security credentials: %w", err)
	}

	// Registra metriche
	if err := exp.registerMetrics(); err != nil {
		return nil, fmt.Errorf("failed to register metrics: %w", err)
	}

	// Registra metriche standard di Go
	exp.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	logger.Info("Prometheus exporter created successfully",
		"auth_type", cfg.PrometheusAuthType,
	)
	return exp, nil
}

// loadCredentials loads authentication credentials and TLS certificates.
func (exp *PrometheusExporter) loadCredentials() error {
	authType := exp.cfg.PrometheusAuthType
	switch authType {
	case "", "none", "basic", "jwt", "both":
	default:
		return fmt.Errorf("unsupported Prometheus authentication type %q", authType)
	}

	// Load the Basic Auth password.
	if authType == "basic" || authType == "both" {
		if strings.TrimSpace(exp.cfg.PrometheusAuthUsername) == "" {
			return fmt.Errorf("prometheus basic authentication username is empty")
		}
		if exp.cfg.PrometheusAuthPasswordFile == "" {
			return fmt.Errorf("prometheus basic authentication password file is not configured")
		}
		password, err := os.ReadFile(exp.cfg.PrometheusAuthPasswordFile)
		if err != nil {
			return fmt.Errorf("failed to read password file: %w", err)
		}
		exp.basicAuthPassword = strings.TrimSpace(string(password))
		if exp.basicAuthPassword == "" {
			return fmt.Errorf("prometheus basic authentication password is empty")
		}
		exp.logger.Info("Basic authentication password loaded")
	}

	// Load the JWT secret.
	if authType == "jwt" || authType == "both" {
		if exp.cfg.PrometheusJWTSecretFile == "" {
			return fmt.Errorf("prometheus JWT secret file is not configured")
		}
		secret, err := os.ReadFile(exp.cfg.PrometheusJWTSecretFile)
		if err != nil {
			return fmt.Errorf("failed to read JWT secret file: %w", err)
		}
		exp.jwtSecret = []byte(strings.TrimSpace(string(secret)))
		if len(exp.jwtSecret) == 0 {
			return fmt.Errorf("prometheus JWT secret is empty")
		}
		exp.logger.Info("JWT secret loaded",
			"issuer", exp.cfg.PrometheusJWTIssuer,
			"audience", exp.cfg.PrometheusJWTAudience,
		)
	}

	// Load TLS certificates.
	if exp.cfg.PrometheusTLSEnabled {
		tlsConfig, err := tlsconfig.BuildServer(tlsconfig.ServerOptions{
			CertFile:   exp.cfg.PrometheusTLSCertFile,
			KeyFile:    exp.cfg.PrometheusTLSKeyFile,
			CAFile:     exp.cfg.PrometheusTLSCAFile,
			MinVersion: exp.cfg.PrometheusTLSMinVersion,
		})
		if err != nil {
			return fmt.Errorf("loading Prometheus TLS configuration: %w", err)
		}
		exp.tlsConfig = tlsConfig
		exp.logger.Info("TLS certificate and key loaded",
			"cert_file", exp.cfg.PrometheusTLSCertFile,
			"key_file", exp.cfg.PrometheusTLSKeyFile,
			"ca_file", exp.cfg.PrometheusTLSCAFile,
		)
	}

	return nil
}

// registerMetrics registers every Prometheus metric exposed by resman.
func (exp *PrometheusExporter) registerMetrics() error {
	// Use one namespace for all application metrics.
	namespace := "resman"

	// Apply the same bounded identity labels to every application metric.
	staticLabels := prometheus.Labels{
		"hostname":    exp.hostname,
		"server_role": exp.serverRole,
	}

	// === Gauges (current values) ===

	exp.cpuTotalUsage = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "cpu_total_usage_percent",
		Help:        "Total CPU usage percentage across all cores",
		ConstLabels: staticLabels,
	})

	exp.memoryUsage = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "memory_usage_megabytes",
		Help:        "Total memory usage in megabytes",
		ConstLabels: staticLabels,
	})

	exp.totalMemoryMB = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "memory_total_megabytes",
		Help:        "Total physical memory in megabytes",
		ConstLabels: staticLabels,
	})

	exp.cachedMemoryMB = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "memory_cached_megabytes",
		Help:        "Cached memory in megabytes (reclaimable by kernel)",
		ConstLabels: staticLabels,
	})

	exp.limitedUsers = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "limited_users_count",
		Help:        "Number of users with CPU limits currently applied",
		ConstLabels: staticLabels,
	})

	// === ALL USERS metrics (all non-system users, UID >= SYSTEM_UID_MIN) ===

	exp.allUsersCPUUsage = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "all_users_cpu_usage_percent",
		Help:        "Total CPU usage percentage by ALL non-system users (UID >= SYSTEM_UID_MIN), regardless of filters",
		ConstLabels: staticLabels,
	})

	exp.allUsersMemoryUsage = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "all_users_memory_usage_bytes",
		Help:        "Total memory usage in bytes by ALL non-system users (UID >= SYSTEM_UID_MIN)",
		ConstLabels: staticLabels,
	})

	exp.allUsersCount = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "all_users_count",
		Help:        "Number of ALL active non-system users (UID >= SYSTEM_UID_MIN), regardless of filters",
		ConstLabels: staticLabels,
	})

	// === LIMITED USERS metrics (only users passing filters) ===

	exp.limitedUsersCPUUsage = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "limited_users_cpu_usage_percent",
		Help:        "Total CPU usage percentage by users passing filters (USER_INCLUDE_LIST && !USER_EXCLUDE_LIST)",
		ConstLabels: staticLabels,
	})

	exp.limitedUsersMemoryUsage = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "limited_users_memory_usage_bytes",
		Help:        "Total memory usage in bytes by users passing filters",
		ConstLabels: staticLabels,
	})

	exp.limitedUsersCount = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "limited_users_count_filtered",
		Help:        "Number of users passing filters (can be limited)",
		ConstLabels: staticLabels,
	})

	exp.limitsActive = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "limits_active",
		Help:        "Whether CPU limits are currently active (1) or not (0)",
		ConstLabels: staticLabels,
	})

	exp.systemLoad = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "system_load_average",
		Help:        "System load average (1 minute)",
		ConstLabels: staticLabels,
	})

	exp.totalCores = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "cpu_total_cores",
		Help:        "Total number of CPU cores",
		ConstLabels: staticLabels,
	})

	exp.actionCores = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "cpu_action_cores",
		Help:        "Number of CPU cores resman uses for actions (total - min_system_cores)",
		ConstLabels: staticLabels,
	})

	exp.procFSUnavailableProcesses = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "procfs_unavailable_processes",
			Help:        "Current number of observed processes without a required procfs decision input",
			ConstLabels: staticLabels,
		},
		[]string{"access"},
	)

	// === Metrics with dynamic labels ===

	exp.userCPUUsage = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "user_cpu_usage_percent",
			Help:        "CPU usage percentage per user (instantaneous, last cycle)",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "username"},
	)

	exp.userCPUUsageAverage = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "user_cpu_usage_average_percent",
			Help:        "CPU usage percentage per user (average since process start)",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "username"},
	)

	exp.userCPUUsageEMA = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "user_cpu_usage_ema_percent",
			Help:        "CPU usage percentage per user (exponential moving average, α=0.3)",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "username"},
	)

	// Per-user memory usage.
	exp.userMemoryUsage = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "user_memory_usage_bytes",
			Help:        "Memory usage in bytes per user",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "username"},
	)

	// Per-user process count.
	exp.userProcessCount = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "user_process_count",
			Help:        "Number of processes per user",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "username"},
	)

	exp.userLimited = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "user_cpu_limited",
			Help:        "Whether CPU limit is applied for user (1) or not (0)",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "username"},
	)

	exp.userMemoryHighEvents = promauto.With(exp.registry).NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "user_memory_high_breaches_total",
			Help:        "Total number of times user exceeded memory.high soft limit",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "username"},
	)

	exp.userIOReadBytes = promauto.With(exp.registry).NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "user_io_read_bytes_total",
			Help:        "Total bytes read from block devices by user",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "username"},
	)

	exp.userIOWriteBytes = promauto.With(exp.registry).NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "user_io_write_bytes_total",
			Help:        "Total bytes written to block devices by user",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "username"},
	)

	exp.userIOReadOps = promauto.With(exp.registry).NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "user_io_read_ops_total",
			Help:        "Total read-family syscalls reported by /proc/PID/io syscr per user",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "username"},
	)

	exp.userIOWriteOps = promauto.With(exp.registry).NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "user_io_write_ops_total",
			Help:        "Total write-family syscalls reported by /proc/PID/io syscw per user",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "username"},
	)

	exp.userWorkloadPattern = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "user_workload_pattern_confidence",
			Help:        "Detected workload pattern confidence per user (AUTODETECT_PATTERNS)",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "username", "pattern"},
	)

	exp.cgroupCPUQuota = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "cgroup_cpu_quota_microseconds",
			Help:        "CPU quota in microseconds per period (max = unlimited)",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "cgroup_path"},
	)

	exp.cgroupCPUPeriod = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "cgroup_cpu_period_microseconds",
			Help:        "CPU period in microseconds",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "cgroup_path"},
	)

	// Per-user cgroup memory usage.
	exp.cgroupMemoryUsage = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "cgroup_memory_usage_bytes",
			Help:        "Memory usage in bytes per cgroup (user)",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "cgroup_path"},
	)

	// === Counters ===

	exp.limitsActivatedTotal = promauto.With(exp.registry).NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Name:        "limits_activated_total",
		Help:        "Total confirmed transitions from inactive to active CPU limits",
		ConstLabels: staticLabels,
	})

	exp.limitsDeactivatedTotal = promauto.With(exp.registry).NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Name:        "limits_deactivated_total",
		Help:        "Total confirmed transitions from active to inactive CPU limits",
		ConstLabels: staticLabels,
	})

	exp.controlCyclesTotal = promauto.With(exp.registry).NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Name:        "control_cycles_total",
		Help:        "Total number of control cycles started",
		ConstLabels: staticLabels,
	})

	exp.controlCycleTriggers = promauto.With(exp.registry).NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "control_cycle_triggers_total",
			Help:        "Total number of control cycles by trigger source",
			ConstLabels: staticLabels,
		},
		[]string{"trigger"},
	)

	exp.psiEventsTotal = promauto.With(exp.registry).NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "psi_events_total",
			Help:        "Total number of PSI pressure events received from the kernel",
			ConstLabels: staticLabels,
		},
		[]string{"type", "scope"},
	)

	exp.psiLastEventTimestamp = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "psi_last_event_timestamp_seconds",
			Help:        "Unix timestamp of the last PSI pressure event received from the kernel",
			ConstLabels: staticLabels,
		},
		[]string{"type", "scope"},
	)

	exp.errorsTotal = promauto.With(exp.registry).NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "errors_total",
			Help:        "Total number of operational errors by component and bounded error type",
			ConstLabels: staticLabels,
		},
		[]string{"component", "error_type"},
	)

	// === Execution-time histograms ===

	exp.controlCycleDuration = promauto.With(exp.registry).NewHistogram(prometheus.HistogramOpts{
		Namespace:   namespace,
		Name:        "control_cycle_duration_seconds",
		Help:        "Duration of control cycles, including failed and suspended cycles, in seconds",
		ConstLabels: staticLabels,
		Buckets:     prometheus.DefBuckets,
	})

	exp.metricsCollectionDuration = promauto.With(exp.registry).NewHistogram(prometheus.HistogramOpts{
		Namespace:   namespace,
		Name:        "metrics_collection_duration_seconds",
		Help:        "Duration of system metrics collection for control cycles and metrics-only refreshes in seconds",
		ConstLabels: staticLabels,
		Buckets:     []float64{.001, .005, .01, .025, .05, .1, .25, .5},
	})

	return nil
}

// ExporterMetrics contains one typed update for system-wide Prometheus gauges.
type ExporterMetrics struct {
	TotalCPUUsage                                float64
	TotalCores                                   int
	ObservedUsersCPUUsage                        float64
	ObservedUsersCount                           int
	ObservedUsersMemoryUsage                     uint64
	CPUEligibleUsersCPUUsage                     float64
	CPUEligibleUsersCount                        int
	CPUEligibleUsersMemoryUsage                  uint64
	CPUActivelyLimitedUsersCount                 int
	CPULimitsActive                              bool
	MemoryUsageMB                                float64
	TotalMemoryMB                                float64
	CachedMemoryMB                               float64
	SystemLoad                                   float64
	ProcFSExecutableIdentityUnavailableProcesses int
	ProcFSIOUnavailableProcesses                 int
}

// UpdateSystemSnapshot publishes one typed system-wide gauge snapshot.
func (exp *PrometheusExporter) UpdateSystemSnapshot(metrics ExporterMetrics) {
	if exp == nil {
		return
	}

	exp.mu.Lock()
	defer exp.mu.Unlock()

	exp.cpuTotalUsage.Set(metrics.TotalCPUUsage)
	exp.totalCores.Set(float64(metrics.TotalCores))
	exp.allUsersCPUUsage.Set(metrics.ObservedUsersCPUUsage)
	exp.allUsersCount.Set(float64(metrics.ObservedUsersCount))
	exp.allUsersMemoryUsage.Set(float64(metrics.ObservedUsersMemoryUsage))
	exp.limitedUsersCPUUsage.Set(metrics.CPUEligibleUsersCPUUsage)
	exp.limitedUsersCount.Set(float64(metrics.CPUEligibleUsersCount))
	exp.limitedUsersMemoryUsage.Set(float64(metrics.CPUEligibleUsersMemoryUsage))
	exp.limitedUsers.Set(float64(metrics.CPUActivelyLimitedUsersCount))
	exp.limitsActive.Set(boolMetricValue(metrics.CPULimitsActive))
	exp.memoryUsage.Set(metrics.MemoryUsageMB)
	exp.totalMemoryMB.Set(metrics.TotalMemoryMB)
	exp.cachedMemoryMB.Set(metrics.CachedMemoryMB)
	exp.systemLoad.Set(metrics.SystemLoad)
	exp.procFSUnavailableProcesses.WithLabelValues(procFSAccessExecutableIdentity).Set(
		float64(metrics.ProcFSExecutableIdentityUnavailableProcesses),
	)
	exp.procFSUnavailableProcesses.WithLabelValues(procFSAccessIODecision).Set(
		float64(metrics.ProcFSIOUnavailableProcesses),
	)
}

func boolMetricValue(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

// UpdateUserMetrics updates per-user metrics using observed CPU enforcement state.
func (exp *PrometheusExporter) UpdateUserMetrics(uid int, username string, cpuUsage float64, cpuUsageAverage float64, cpuUsageEMA float64, memoryUsage uint64, processCount int, cpuLimitActive bool, cgroupPath, cpuQuota string, memoryHighEvents uint64, ioReadBytes, ioWriteBytes, ioReadOps, ioWriteOps uint64) {
	if exp == nil || exp.registry == nil {
		return
	}

	uidStr := strconv.Itoa(uid)

	// Resolve an empty or numeric username before taking the exporter lock.
	if username == "" || username == uidStr {
		username = exp.getUsernameFromUID(uidStr)
	}

	// Read cgroup memory before acquiring lock (fix #4: avoid file I/O under lock)
	cgroupMemory := uint64(0)
	if cgroupPath != "" {
		cgroupMemory = uint64(exp.getCgroupMemoryUsage(cgroupPath))
	}

	exp.mu.Lock()
	defer exp.mu.Unlock()

	// Track the user as present in the current metrics set.
	userKey := fmt.Sprintf("%s_%s", uidStr, username)
	exp.activeUserMetrics[userKey] = true

	// Update per-user CPU usage.
	exp.userCPUUsage.WithLabelValues(uidStr, username).Set(cpuUsage)
	exp.userCPUUsageAverage.WithLabelValues(uidStr, username).Set(cpuUsageAverage)
	exp.userCPUUsageEMA.WithLabelValues(uidStr, username).Set(cpuUsageEMA)

	// Update per-user memory usage in bytes.
	exp.userMemoryUsage.WithLabelValues(uidStr, username).Set(float64(memoryUsage))

	// Update the per-user process count.
	exp.userProcessCount.WithLabelValues(uidStr, username).Set(float64(processCount))

	// Publish observed CPU enforcement state.
	limitedValue := 0.0
	if cpuLimitActive {
		limitedValue = 1.0
	}
	exp.userLimited.WithLabelValues(uidStr, username).Set(limitedValue)

	// Update memory.high breach events by delta.
	memoryHighKey := fmt.Sprintf("%s_%s", uidStr, username)
	prev := exp.prevMemoryHighEvents[memoryHighKey]
	if memoryHighEvents > prev {
		delta := memoryHighEvents - prev
		exp.userMemoryHighEvents.WithLabelValues(uidStr, username).Add(float64(delta))
	}
	exp.prevMemoryHighEvents[memoryHighKey] = memoryHighEvents

	// Update IO statistics (counters with delta)
	ioKey := fmt.Sprintf("%d_%s", uid, username)
	prevIO := exp.prevIOStats[ioKey]
	if ioReadBytes >= prevIO.ReadBytes {
		exp.userIOReadBytes.WithLabelValues(uidStr, username).Add(float64(ioReadBytes - prevIO.ReadBytes))
	}
	if ioWriteBytes >= prevIO.WriteBytes {
		exp.userIOWriteBytes.WithLabelValues(uidStr, username).Add(float64(ioWriteBytes - prevIO.WriteBytes))
	}
	if ioReadOps >= prevIO.ReadOps {
		exp.userIOReadOps.WithLabelValues(uidStr, username).Add(float64(ioReadOps - prevIO.ReadOps))
	}
	if ioWriteOps >= prevIO.WriteOps {
		exp.userIOWriteOps.WithLabelValues(uidStr, username).Add(float64(ioWriteOps - prevIO.WriteOps))
	}
	exp.prevIOStats[ioKey] = ioStatsSnapshot{
		ReadBytes:  ioReadBytes,
		WriteBytes: ioWriteBytes,
		ReadOps:    ioReadOps,
		WriteOps:   ioWriteOps,
	}

	// Update cgroup metrics when a path is available.
	if cgroupPath != "" {
		// Update the CPU quota.
		if cpuQuota != "" {
			quota, period := parseCPUQuota(cpuQuota)
			if quota >= 0 {
				exp.cgroupCPUQuota.WithLabelValues(uidStr, cgroupPath).Set(float64(quota))
			}
			if period > 0 {
				exp.cgroupCPUPeriod.WithLabelValues(uidStr, cgroupPath).Set(float64(period))
			}
		}

		// Use the value read before locking to avoid redundant cgroup file I/O.
		exp.cgroupMemoryUsage.WithLabelValues(uidStr, cgroupPath).Set(float64(cgroupMemory))
	}
}

// CleanupUserMetrics rimuove le metriche per gli utenti non più attivi.
func (exp *PrometheusExporter) CleanupUserMetrics(activeUids map[int]bool) {
	if exp == nil {
		return
	}

	exp.mu.Lock()
	defer exp.mu.Unlock()

	// Itera su tutti gli utenti tracciati
	for userKey := range exp.activeUserMetrics {
		// Controlla se l'utente è ancora attivo
		parts := strings.SplitN(userKey, "_", 2)
		if len(parts) != 2 {
			continue
		}

		uidStr := parts[0]
		username := parts[1]

		uid, err := strconv.Atoi(uidStr)
		if err != nil {
			continue
		}

		// Se l'utente non è più attivo, rimuovi le metriche
		if !activeUids[uid] {
			// Rimuovi dalle metriche
			exp.userCPUUsage.DeleteLabelValues(uidStr, username)
			exp.userCPUUsageAverage.DeleteLabelValues(uidStr, username)
			exp.userCPUUsageEMA.DeleteLabelValues(uidStr, username)
			exp.userMemoryUsage.DeleteLabelValues(uidStr, username)
			exp.userProcessCount.DeleteLabelValues(uidStr, username)
			exp.userLimited.DeleteLabelValues(uidStr, username)
			exp.userMemoryHighEvents.DeleteLabelValues(uidStr, username)
			exp.userIOReadBytes.DeleteLabelValues(uidStr, username)
			exp.userIOWriteBytes.DeleteLabelValues(uidStr, username)
			exp.userIOReadOps.DeleteLabelValues(uidStr, username)
			exp.userIOWriteOps.DeleteLabelValues(uidStr, username)
			if prevPattern, ok := exp.prevUserPatterns[userKey]; ok {
				exp.userWorkloadPattern.DeleteLabelValues(uidStr, username, prevPattern)
			}

			// Rimuovi dalla mappa dei valori precedenti
			memoryHighKey := fmt.Sprintf("%s_%s", uidStr, username)
			delete(exp.prevMemoryHighEvents, memoryHighKey)
			delete(exp.prevIOStats, memoryHighKey)
			delete(exp.prevUserPatterns, userKey)

			// Rimuovi dal tracking
			delete(exp.activeUserMetrics, userKey)

			exp.logger.Debug("Removed metrics for inactive user",
				"uid", uid,
				"username", username,
			)
		}
	}
}

// getCgroupMemoryUsage legge l'uso memoria da un cgroup specifico
func (exp *PrometheusExporter) getCgroupMemoryUsage(cgroupPath string) int64 {
	memoryCurrentFile := filepath.Join(cgroupPath, "memory.current")

	if data, err := os.ReadFile(memoryCurrentFile); err == nil {
		if usage, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
			return usage
		}
	}

	return 0
}

// UpdateSystemMetrics aggiorna le metriche di sistema.
func (exp *PrometheusExporter) UpdateSystemMetrics(totalCores int, actionCores int, systemLoad float64) {
	if exp == nil {
		return
	}

	exp.mu.Lock()
	defer exp.mu.Unlock()

	exp.totalCores.Set(float64(totalCores))
	exp.actionCores.Set(float64(actionCores))
	exp.systemLoad.Set(systemLoad)
}

// UpdateUserWorkloadPattern aggiorna il pattern rilevato per un utente.
func (exp *PrometheusExporter) UpdateUserWorkloadPattern(uid int, username string, pattern string, confidence float64) {
	if exp == nil || exp.registry == nil || exp.userWorkloadPattern == nil {
		return
	}

	uidStr := strconv.Itoa(uid)
	if username == "" || username == uidStr {
		username = exp.getUsernameFromUID(uidStr)
	}
	userKey := fmt.Sprintf("%s_%s", uidStr, username)

	exp.mu.Lock()
	defer exp.mu.Unlock()

	if prevPattern, ok := exp.prevUserPatterns[userKey]; ok && prevPattern != pattern {
		exp.userWorkloadPattern.DeleteLabelValues(uidStr, username, prevPattern)
	}

	exp.userWorkloadPattern.WithLabelValues(uidStr, username, pattern).Set(confidence)
	exp.prevUserPatterns[userKey] = pattern
}

// parseCPUQuota estrae quota e period da una stringa "quota period".
func parseCPUQuota(quotaStr string) (quota int64, period int64) {
	parts := strings.Fields(quotaStr)
	if len(parts) != 2 {
		return -1, -1
	}

	if parts[0] == "max" {
		quota = -1 // Indica "max" (illimitato)
	} else {
		if val, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
			quota = val
		}
	}

	if val, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
		period = val
	}

	return quota, period
}

// getUsernameFromUID converte un UID in username.
func (exp *PrometheusExporter) getUsernameFromUID(uidStr string) string {
	uid, err := strconv.Atoi(uidStr)
	if err != nil {
		return "unknown"
	}
	if resolver, ok := exp.usernameResolver.Load().(func(int) string); ok {
		return resolver(uid)
	}

	// Prova a leggere da /etc/passwd
	file, err := os.Open("/etc/passwd")
	if err != nil {
		return uidStr
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Split(line, ":")
		if len(fields) >= 3 {
			if strconv.Itoa(uid) == fields[2] {
				return fields[0] // Username
			}
		}
	}

	return uidStr
}

// IncrementLimitsActivated records a confirmed inactive-to-active transition.
func (exp *PrometheusExporter) IncrementLimitsActivated() {
	if exp == nil {
		return
	}
	exp.limitsActivatedTotal.Inc()
}

// IncrementLimitsDeactivated records a confirmed active-to-inactive transition.
func (exp *PrometheusExporter) IncrementLimitsDeactivated() {
	if exp == nil {
		return
	}
	exp.limitsDeactivatedTotal.Inc()
}

// RecordControlCycleTrigger records the source that started a control cycle.
func (exp *PrometheusExporter) RecordControlCycleTrigger(trigger string) {
	if exp == nil || exp.controlCycleTriggers == nil {
		return
	}
	if trigger == "" {
		trigger = "unknown"
	}
	exp.controlCyclesTotal.Inc()
	exp.controlCycleTriggers.WithLabelValues(trigger).Inc()
}

// RecordPSIEvent registra un evento PSI ricevuto dal kernel.
func (exp *PrometheusExporter) RecordPSIEvent(typ, scope string, timestamp time.Time) {
	if exp == nil || exp.psiEventsTotal == nil || exp.psiLastEventTimestamp == nil {
		return
	}
	if typ == "" {
		typ = "unknown"
	}
	if scope == "" {
		scope = "unknown"
	}
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	exp.psiEventsTotal.WithLabelValues(typ, scope).Inc()
	exp.psiLastEventTimestamp.WithLabelValues(typ, scope).Set(float64(timestamp.Unix()))
}

// RecordControlCycleDuration records the duration of one control cycle.
func (exp *PrometheusExporter) RecordControlCycleDuration(duration time.Duration) {
	if exp == nil {
		return
	}
	exp.controlCycleDuration.Observe(duration.Seconds())
}

// RecordMetricsCollectionDuration records one system metrics collection duration.
func (exp *PrometheusExporter) RecordMetricsCollectionDuration(duration time.Duration) {
	if exp == nil {
		return
	}
	exp.metricsCollectionDuration.Observe(duration.Seconds())
}

// RecordError records one operational error using a bounded label pair.
func (exp *PrometheusExporter) RecordError(component, errorType string) {
	if exp == nil {
		return
	}
	exp.errorsTotal.WithLabelValues(component, errorType).Inc()
}

// authMiddleware gestisce l'autenticazione per Basic Auth e JWT
func (exp *PrometheusExporter) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Se l'autenticazione è disabilitata, passa direttamente
		if exp.cfg.PrometheusAuthType == "none" || exp.cfg.PrometheusAuthType == "" {
			next.ServeHTTP(w, r)
			return
		}

		authenticated := false

		// Try Basic Auth
		if exp.cfg.PrometheusAuthType == "basic" || exp.cfg.PrometheusAuthType == "both" {
			if exp.checkBasicAuth(r) {
				authenticated = true
			}
		}

		// Try JWT Auth
		if !authenticated && (exp.cfg.PrometheusAuthType == "jwt" || exp.cfg.PrometheusAuthType == "both") {
			if exp.checkJWTAuth(r) {
				authenticated = true
			}
		}

		if !authenticated {
			exp.logger.Debug("Authentication failed",
				"remote_addr", r.RemoteAddr,
				"path", r.URL.Path,
			)
			w.Header().Set("WWW-Authenticate", `Basic realm="Resource Manager Metrics", Bearer`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		exp.logger.Debug("Authentication successful",
			"remote_addr", r.RemoteAddr,
			"path", r.URL.Path,
		)
		next.ServeHTTP(w, r)
	})
}

// checkBasicAuth verifica le credenziali Basic Auth
func (exp *PrometheusExporter) checkBasicAuth(r *http.Request) bool {
	if exp.cfg.PrometheusAuthUsername == "" || exp.basicAuthPassword == "" {
		return false
	}
	username, password, ok := r.BasicAuth()
	if !ok {
		return false
	}

	// Verifica username
	if subtle.ConstantTimeCompare([]byte(username), []byte(exp.cfg.PrometheusAuthUsername)) != 1 {
		return false
	}

	// Verifica password
	if subtle.ConstantTimeCompare([]byte(password), []byte(exp.basicAuthPassword)) != 1 {
		return false
	}

	return true
}

// checkJWTAuth verifica il token JWT
func (exp *PrometheusExporter) checkJWTAuth(r *http.Request) bool {
	if len(exp.jwtSecret) == 0 {
		return false
	}
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return false
	}

	// Estrai il token Bearer
	parts := strings.Split(authHeader, " ")
	if len(parts) != 2 || parts[0] != "Bearer" {
		return false
	}

	tokenString := parts[1]

	// Parse e valida il token
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// Verifica l'algoritmo
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return exp.jwtSecret, nil
	}, jwt.WithExpirationRequired())

	if err != nil {
		exp.logger.Debug("JWT parse error", "error", err)
		return false
	}

	if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
		// Verifica issuer
		if exp.cfg.PrometheusJWTIssuer != "" {
			if issuer, ok := claims["iss"].(string); !ok || issuer != exp.cfg.PrometheusJWTIssuer {
				return false
			}
		}

		// Verifica audience
		if exp.cfg.PrometheusJWTAudience != "" {
			if audience, ok := claims["aud"].(string); !ok || audience != exp.cfg.PrometheusJWTAudience {
				return false
			}
		}

		return true
	}

	return false
}

// healthHandler gestisce l'endpoint /health
func (exp *PrometheusExporter) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"status": "healthy", "timestamp": "%s", "auth_enabled": "%s"}`,
		time.Now().Format(time.RFC3339),
		exp.cfg.PrometheusAuthType,
	)
}

// rootHandler gestisce l'endpoint root
func (exp *PrometheusExporter) rootHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	authInfo := ""
	if exp.cfg.PrometheusAuthType != "none" && exp.cfg.PrometheusAuthType != "" {
		authInfo = " (Authentication: " + exp.cfg.PrometheusAuthType + ")"
	}
	_, _ = fmt.Fprintf(w, `<html><body><h1>Resource Manager Metrics%s</h1><p><a href="/metrics">Metrics</a></p><p><a href="/health">Health</a></p></body></html>`, authInfo)
}

// Start starts the Prometheus HTTP or HTTPS server.
func (exp *PrometheusExporter) Start(ctx context.Context) error {
	if exp == nil {
		return nil
	}

	exp.mu.Lock()
	if exp.isRunning {
		exp.mu.Unlock()
		return fmt.Errorf("exporter already running")
	}
	exp.isRunning = true
	exp.mu.Unlock()

	mux := http.NewServeMux()

	// Metrics handler with authentication.
	mux.Handle("/metrics", exp.authMiddleware(promhttp.HandlerFor(
		exp.registry,
		promhttp.HandlerOpts{
			Registry:          exp.registry,
			EnableOpenMetrics: true,
		},
	)))

	// Health endpoint without authentication for monitoring.
	mux.HandleFunc("/health", exp.healthHandler)

	// Root endpoint
	mux.HandleFunc("/", exp.rootHandler)

	addr := fmt.Sprintf("%s:%d", exp.cfg.PrometheusMetricsBindHost, exp.cfg.PrometheusMetricsBindPort)
	exp.server = &http.Server{
		Addr:      addr,
		Handler:   mux,
		TLSConfig: exp.tlsConfig,
	}

	// Configure TLS when enabled.
	if exp.cfg.PrometheusTLSEnabled {
		if exp.tlsConfig == nil || len(exp.tlsConfig.Certificates) == 0 {
			exp.mu.Lock()
			exp.isRunning = false
			exp.mu.Unlock()
			return fmt.Errorf("TLS enabled but server TLS configuration is not loaded")
		}
		exp.logger.Info("Starting Prometheus HTTPS server",
			"address", addr,
			"auth_type", exp.cfg.PrometheusAuthType,
			"tls_enabled", exp.cfg.PrometheusTLSEnabled,
			"tls_min_version", exp.cfg.PrometheusTLSMinVersion,
			"mtls_enabled", exp.cfg.PrometheusTLSCAFile != "",
		)
	} else {
		exp.logger.Info("Starting Prometheus HTTP server",
			"address", addr,
			"auth_type", exp.cfg.PrometheusAuthType,
			"tls_enabled", false,
		)
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		exp.mu.Lock()
		exp.isRunning = false
		exp.mu.Unlock()
		return fmt.Errorf("failed to listen for Prometheus metrics on %s: %w", addr, err)
	}

	// Start the server in a goroutine.
	listenErr := make(chan error, 1)
	go func() {
		var err error
		if exp.cfg.PrometheusTLSEnabled {
			err = exp.server.ServeTLS(listener, "", "")
		} else {
			err = exp.server.Serve(listener)
		}
		if err != nil && err != http.ErrServerClosed {
			exp.logger.Error("Prometheus server error", "error", err)
			listenErr <- err
		}
	}()
	exp.logger.Info("Prometheus server verified as listening", "address", listener.Addr().String())

	// Handle shutdown.
	go func() {
		select {
		case <-ctx.Done():
			exp.logger.Info("Context cancelled, shutting down Prometheus server")
			exp.shutdown()
		case err := <-listenErr:
			exp.logger.Error("Server listen error", "error", err)
			exp.shutdown()
		case <-exp.stopChan:
			exp.logger.Info("Stop signal received")
			exp.shutdown()
		}
	}()

	return nil
}

// shutdown esegue lo shutdown graceful del server.
func (exp *PrometheusExporter) shutdown() {
	exp.mu.Lock()
	defer exp.mu.Unlock()

	if !exp.isRunning || exp.server == nil {
		return
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	exp.logger.Info("Shutting down Prometheus HTTP server")
	if err := exp.server.Shutdown(shutdownCtx); err != nil {
		exp.logger.Error("Error during Prometheus server shutdown", "error", err)
		// Forza la chiusura se lo shutdown graceful fallisce
		_ = exp.server.Close()
	}

	exp.isRunning = false
	exp.logger.Info("Prometheus HTTP server stopped")
}

// Stop ferma il server Prometheus.
func (exp *PrometheusExporter) Stop() error {
	if exp == nil {
		return nil
	}

	select {
	case exp.stopChan <- struct{}{}:
		return nil
	default:
		return fmt.Errorf("stop already in progress")
	}
}

// IsRunning restituisce true se l'esportatore è in esecuzione.
func (exp *PrometheusExporter) IsRunning() bool {
	if exp == nil {
		return false
	}

	exp.mu.RLock()
	defer exp.mu.RUnlock()
	return exp.isRunning
}

// GetMetricsEndpoint restituisce l'endpoint delle metriche.
func (exp *PrometheusExporter) GetMetricsEndpoint() string {
	if exp == nil {
		return ""
	}
	scheme := "http"
	if exp.cfg.PrometheusTLSEnabled {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%d/metrics", scheme, exp.cfg.PrometheusMetricsBindHost, exp.cfg.PrometheusMetricsBindPort)
}
