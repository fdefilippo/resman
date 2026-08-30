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
	"errors"
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

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/operationgate"
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

type prometheusRun struct {
	server    *http.Server
	stop      chan struct{}
	stopOnce  sync.Once
	serveDone chan error
	done      chan struct{}
	err       error
}

// LimitHookType identifies one bounded limit-hook delivery mechanism.
type LimitHookType string

const (
	LimitHookTypeScript LimitHookType = "script"
	LimitHookTypeHTTP   LimitHookType = "http"
)

// LimitHookOutcome identifies one bounded terminal hook execution outcome.
type LimitHookOutcome string

const (
	LimitHookOutcomeSuccess   LimitHookOutcome = "success"
	LimitHookOutcomeFailure   LimitHookOutcome = "failure"
	LimitHookOutcomeTimeout   LimitHookOutcome = "timeout"
	LimitHookOutcomeCancelled LimitHookOutcome = "cancelled"
)

// PrometheusExporter exports metrics in Prometheus format.
type PrometheusExporter struct {
	cfg      *config.Config
	logger   *logging.Logger
	registry *prometheus.Registry

	// Static labels for every metric (from configuration).
	hostname   string
	serverRole string

	// Base metrics with hostname and server_role labels.
	cpuTotalUsage           prometheus.Gauge
	memoryUsage             prometheus.Gauge
	totalMemoryMB           prometheus.Gauge
	cachedMemoryMB          prometheus.Gauge
	cpuActivelyLimitedUsers prometheus.Gauge

	// ALL USERS metrics include every non-system user (UID >= SYSTEM_UID_MIN).
	allUsersCPUUsage    prometheus.Gauge
	allUsersMemoryUsage prometheus.Gauge
	allUsersCount       prometheus.Gauge

	// Per-resource eligibility metrics use the matching policy lists.
	cpuEligibleUsersCPUUsage      prometheus.Gauge
	cpuEligibleUsersMemoryUsage   prometheus.Gauge
	cpuEligibleUsersCount         prometheus.Gauge
	ramEligibleUsersMemoryUsage   prometheus.Gauge
	ramEligibleUsersCount         prometheus.Gauge
	ioEligibleUsersCount          prometheus.Gauge
	ioEligibleUsersReadBPS        prometheus.Gauge
	ioEligibleUsersWriteBPS       prometheus.Gauge
	ioEligibleUsersReadBlockIOPS  prometheus.Gauge
	ioEligibleUsersWriteBlockIOPS prometheus.Gauge

	activelyLimitedUsers       prometheus.Gauge
	cpuLimitsActive            prometheus.Gauge
	resourceLimitsActive       prometheus.Gauge
	anyLimitsActive            prometheus.Gauge
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
	userCPULimitActive   *prometheus.GaugeVec
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
	activeCgroupPaths    map[string]string // UID -> last published cgroup path
	prevMemoryHighEvents map[string]uint64 // "uid_username" -> last known value
	prevIOStats          map[string]ioStatsSnapshot
	prevUserPatterns     map[string]string // "uid_username" -> previous pattern label
	usernameResolver     atomic.Value      // func(int) string

	// Counters are increment-only metrics.
	cpuLimitsActivatedTotal   prometheus.Counter
	cpuLimitsDeactivatedTotal prometheus.Counter
	controlCyclesTotal        prometheus.Counter
	controlCycleTriggers      *prometheus.CounterVec
	psiEventsTotal            *prometheus.CounterVec
	psiLastEventTimestamp     *prometheus.GaugeVec
	errorsTotal               *prometheus.CounterVec
	cgroupIngressSkipped      *prometheus.CounterVec
	limitHookExecutions       *prometheus.CounterVec

	// Histograms record operation durations.
	controlCycleDuration      prometheus.Histogram
	metricsCollectionDuration prometheus.Histogram

	mu sync.RWMutex
	// metricsGate serializes delta accounting and label cleanup without
	// coupling Prometheus writes to the lifecycle state mutex.
	metricsGate operationgate.Gate

	// Internal state.
	isRunning bool
	starting  bool
	startDone chan struct{}
	run       *prometheusRun

	shutdownHTTPServer func(*http.Server, context.Context) error
	closeHTTPServer    func(*http.Server) error

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

// NewPrometheusExporter creates a Prometheus exporter.
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

	// Validate the port.
	if cfg.PrometheusMetricsBindPort <= 0 || cfg.PrometheusMetricsBindPort > 65535 {
		return nil, fmt.Errorf("invalid Prometheus metrics bind port %d (must be 1-65535)", cfg.PrometheusMetricsBindPort)
	}

	// Resolve the hostname and server role.
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
		activeUserMetrics:    make(map[string]bool),
		activeCgroupPaths:    make(map[string]string),
		prevMemoryHighEvents: make(map[string]uint64),
		prevIOStats:          make(map[string]ioStatsSnapshot),
		prevUserPatterns:     make(map[string]string),
		shutdownHTTPServer: func(server *http.Server, ctx context.Context) error {
			return server.Shutdown(ctx)
		},
		closeHTTPServer: func(server *http.Server) error {
			return server.Close()
		},
	}

	logger.Info("Prometheus exporter created",
		"hostname", exp.hostname,
		"server_role", exp.serverRole,
	)

	// Load authentication credentials and TLS certificates.
	if err := exp.loadCredentials(); err != nil {
		return nil, fmt.Errorf("failed to load Prometheus security credentials: %w", err)
	}

	// Register application metrics.
	if err := exp.registerMetrics(); err != nil {
		return nil, fmt.Errorf("failed to register metrics: %w", err)
	}

	// Register standard Go metrics.
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

	exp.cpuActivelyLimitedUsers = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "cpu_actively_limited_users_count",
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

	// === PER-RESOURCE ELIGIBILITY metrics ===

	exp.cpuEligibleUsersCPUUsage = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "cpu_eligible_users_cpu_usage_percent",
		Help:        "Total enforceable CPU usage percentage by users eligible under CPU policy",
		ConstLabels: staticLabels,
	})

	exp.cpuEligibleUsersMemoryUsage = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "cpu_eligible_users_memory_usage_bytes",
		Help:        "Total enforceable memory usage in bytes by users eligible under CPU policy",
		ConstLabels: staticLabels,
	})

	exp.cpuEligibleUsersCount = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "cpu_eligible_users_count",
		Help:        "Number of users eligible under CPU policy",
		ConstLabels: staticLabels,
	})

	exp.ramEligibleUsersMemoryUsage = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "ram_eligible_users_memory_usage_bytes",
		Help:        "Total enforceable memory usage in bytes by users eligible under RAM policy",
		ConstLabels: staticLabels,
	})

	exp.ramEligibleUsersCount = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "ram_eligible_users_count",
		Help:        "Number of users eligible under RAM policy",
		ConstLabels: staticLabels,
	})

	exp.ioEligibleUsersCount = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "io_eligible_users_count",
		Help:        "Number of users eligible under I/O policy",
		ConstLabels: staticLabels,
	})

	exp.ioEligibleUsersReadBPS = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "io_eligible_users_read_bytes_per_second",
		Help:        "Total enforceable read byte rate by users eligible under I/O policy",
		ConstLabels: staticLabels,
	})

	exp.ioEligibleUsersWriteBPS = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "io_eligible_users_write_bytes_per_second",
		Help:        "Total enforceable write byte rate by users eligible under I/O policy",
		ConstLabels: staticLabels,
	})

	exp.ioEligibleUsersReadBlockIOPS = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "io_eligible_users_read_block_operations_per_second",
		Help:        "Total block-device read operation rate by users eligible under I/O policy",
		ConstLabels: staticLabels,
	})

	exp.ioEligibleUsersWriteBlockIOPS = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "io_eligible_users_write_block_operations_per_second",
		Help:        "Total block-device write operation rate by users eligible under I/O policy",
		ConstLabels: staticLabels,
	})

	exp.activelyLimitedUsers = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "actively_limited_users_count",
		Help:        "Number of distinct users with observed CPU, RAM, or I/O enforcement",
		ConstLabels: staticLabels,
	})

	exp.cpuLimitsActive = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "cpu_limits_active",
		Help:        "Whether CPU limits are currently active (1) or not (0)",
		ConstLabels: staticLabels,
	})

	exp.resourceLimitsActive = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "resource_limits_active",
		Help:        "Whether RAM or I/O limits are currently active (1) or not (0)",
		ConstLabels: staticLabels,
	})

	exp.anyLimitsActive = promauto.With(exp.registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "any_limits_active",
		Help:        "Whether CPU, RAM, or I/O limits are currently active (1) or not (0)",
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

	exp.userCPULimitActive = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "user_cpu_limit_active",
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
			Help:        "Finite CPU quota in microseconds per period; absent when cpu.max is unlimited or unavailable",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "cgroup_path"},
	)

	exp.cgroupCPUPeriod = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "cgroup_cpu_period_microseconds",
			Help:        "CPU period in microseconds; absent when cpu.max is unavailable",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "cgroup_path"},
	)

	// Per-user cgroup memory usage.
	exp.cgroupMemoryUsage = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "cgroup_memory_usage_bytes",
			Help:        "Observed memory.current usage in bytes per user cgroup; absent when unavailable",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "cgroup_path"},
	)

	// === Counters ===

	exp.cpuLimitsActivatedTotal = promauto.With(exp.registry).NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Name:        "cpu_limits_activated_total",
		Help:        "Total confirmed transitions from inactive to active CPU limits",
		ConstLabels: staticLabels,
	})

	exp.cpuLimitsDeactivatedTotal = promauto.With(exp.registry).NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Name:        "cpu_limits_deactivated_total",
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

	exp.cgroupIngressSkipped = promauto.With(exp.registry).NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "cgroup_ingress_skipped_total",
			Help:        "Total process ingress attempts skipped at the ResMan PID namespace boundary",
			ConstLabels: staticLabels,
		},
		[]string{"reason"},
	)

	exp.limitHookExecutions = promauto.With(exp.registry).NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "limit_hook_executions_total",
			Help:        "Total completed limit-hook executions by delivery mechanism and terminal outcome",
			ConstLabels: staticLabels,
		},
		[]string{"hook_type", "outcome"},
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

// SystemExporterMetrics contains one typed update for system-wide Prometheus gauges.
type SystemExporterMetrics struct {
	TotalCPUUsage                                float64
	TotalCores                                   int
	ActionCores                                  int
	ObservedUsersCPUUsage                        float64
	ObservedUsersCount                           int
	ObservedUsersMemoryUsage                     uint64
	CPUEligibleUsersCPUUsage                     float64
	CPUEligibleUsersCount                        int
	CPUEligibleUsersMemoryUsage                  uint64
	RAMEligibleUsersCount                        int
	RAMEligibleUsersMemoryUsage                  uint64
	IOEligibleUsersCount                         int
	IOEligibleUsersReadBytesPerSecond            float64
	IOEligibleUsersWriteBytesPerSecond           float64
	IOEligibleUsersReadBlockOperationsPerSecond  float64
	IOEligibleUsersWriteBlockOperationsPerSecond float64
	CPUActivelyLimitedUsersCount                 int
	ActivelyLimitedUsersCount                    int
	CPULimitsActive                              bool
	ResourceLimitsActive                         bool
	AnyLimitsActive                              bool
	MemoryUsageMB                                float64
	TotalMemoryMB                                float64
	CachedMemoryMB                               float64
	SystemLoad                                   float64
	ProcFSExecutableIdentityUnavailableProcesses int
	ProcFSIOUnavailableProcesses                 int
}

// UpdateSystemSnapshot publishes one typed system-wide gauge snapshot.
func (exp *PrometheusExporter) UpdateSystemSnapshot(metrics SystemExporterMetrics) {
	if exp == nil {
		return
	}

	exp.cpuTotalUsage.Set(metrics.TotalCPUUsage)
	exp.totalCores.Set(float64(metrics.TotalCores))
	exp.actionCores.Set(float64(metrics.ActionCores))
	exp.allUsersCPUUsage.Set(metrics.ObservedUsersCPUUsage)
	exp.allUsersCount.Set(float64(metrics.ObservedUsersCount))
	exp.allUsersMemoryUsage.Set(float64(metrics.ObservedUsersMemoryUsage))
	exp.cpuEligibleUsersCPUUsage.Set(metrics.CPUEligibleUsersCPUUsage)
	exp.cpuEligibleUsersCount.Set(float64(metrics.CPUEligibleUsersCount))
	exp.cpuEligibleUsersMemoryUsage.Set(float64(metrics.CPUEligibleUsersMemoryUsage))
	exp.ramEligibleUsersCount.Set(float64(metrics.RAMEligibleUsersCount))
	exp.ramEligibleUsersMemoryUsage.Set(float64(metrics.RAMEligibleUsersMemoryUsage))
	exp.ioEligibleUsersCount.Set(float64(metrics.IOEligibleUsersCount))
	exp.ioEligibleUsersReadBPS.Set(metrics.IOEligibleUsersReadBytesPerSecond)
	exp.ioEligibleUsersWriteBPS.Set(metrics.IOEligibleUsersWriteBytesPerSecond)
	exp.ioEligibleUsersReadBlockIOPS.Set(metrics.IOEligibleUsersReadBlockOperationsPerSecond)
	exp.ioEligibleUsersWriteBlockIOPS.Set(metrics.IOEligibleUsersWriteBlockOperationsPerSecond)
	exp.cpuActivelyLimitedUsers.Set(float64(metrics.CPUActivelyLimitedUsersCount))
	exp.activelyLimitedUsers.Set(float64(metrics.ActivelyLimitedUsersCount))
	exp.cpuLimitsActive.Set(boolMetricValue(metrics.CPULimitsActive))
	exp.resourceLimitsActive.Set(boolMetricValue(metrics.ResourceLimitsActive))
	exp.anyLimitsActive.Set(boolMetricValue(metrics.AnyLimitsActive))
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

// UserExporterMetrics contains one typed per-user observation and enforcement snapshot.
type UserExporterMetrics struct {
	UID                  int
	Username             string
	CPUUsagePercent      float64
	CPUUsageAverage      float64
	CPUUsageEMA          float64
	MemoryUsageBytes     uint64
	ProcessCount         int
	CPULimitActive       bool
	CgroupPath           string
	CPUQuota             string
	MemoryHighEvents     uint64
	ObservedIOReadBytes  uint64
	ObservedIOWriteBytes uint64
	ObservedIOReadOps    uint64
	ObservedIOWriteOps   uint64
}

// UpdateUserSnapshot updates per-user metrics from one typed snapshot.
func (exp *PrometheusExporter) UpdateUserSnapshot(metrics UserExporterMetrics) {
	if exp == nil || exp.registry == nil {
		return
	}

	uidStr := strconv.Itoa(metrics.UID)
	username := metrics.Username

	// Resolve an empty or numeric username before taking the exporter lock.
	if username == "" || username == uidStr {
		username = exp.getUsernameFromUID(uidStr)
	}

	// Read cgroup memory before acquiring the metrics gate: filesystem I/O must
	// not delay other metric updates or cleanup.
	cgroupMemory, cgroupMemoryAvailable := uint64(0), false
	if metrics.CgroupPath != "" {
		cgroupMemory, cgroupMemoryAvailable = exp.getCgroupMemoryUsage(metrics.CgroupPath)
	}

	leaveMetrics := exp.metricsGate.Enter()
	defer leaveMetrics()

	// Track the user as present in the current metrics set.
	userKey := fmt.Sprintf("%s_%s", uidStr, username)
	exp.activeUserMetrics[userKey] = true

	// Update per-user CPU usage.
	exp.userCPUUsage.WithLabelValues(uidStr, username).Set(metrics.CPUUsagePercent)
	exp.userCPUUsageAverage.WithLabelValues(uidStr, username).Set(metrics.CPUUsageAverage)
	exp.userCPUUsageEMA.WithLabelValues(uidStr, username).Set(metrics.CPUUsageEMA)

	// Update per-user memory usage in bytes.
	exp.userMemoryUsage.WithLabelValues(uidStr, username).Set(float64(metrics.MemoryUsageBytes))

	// Update the per-user process count.
	exp.userProcessCount.WithLabelValues(uidStr, username).Set(float64(metrics.ProcessCount))

	// Publish observed CPU enforcement state.
	limitedValue := 0.0
	if metrics.CPULimitActive {
		limitedValue = 1.0
	}
	exp.userCPULimitActive.WithLabelValues(uidStr, username).Set(limitedValue)

	// Update memory.high breach events by delta.
	memoryHighKey := fmt.Sprintf("%s_%s", uidStr, username)
	prev := exp.prevMemoryHighEvents[memoryHighKey]
	if metrics.MemoryHighEvents > prev {
		delta := metrics.MemoryHighEvents - prev
		exp.userMemoryHighEvents.WithLabelValues(uidStr, username).Add(float64(delta))
	}
	exp.prevMemoryHighEvents[memoryHighKey] = metrics.MemoryHighEvents

	// Update IO statistics (counters with delta)
	ioKey := fmt.Sprintf("%d_%s", metrics.UID, username)
	prevIO := exp.prevIOStats[ioKey]
	if metrics.ObservedIOReadBytes >= prevIO.ReadBytes {
		exp.userIOReadBytes.WithLabelValues(uidStr, username).Add(float64(metrics.ObservedIOReadBytes - prevIO.ReadBytes))
	}
	if metrics.ObservedIOWriteBytes >= prevIO.WriteBytes {
		exp.userIOWriteBytes.WithLabelValues(uidStr, username).Add(float64(metrics.ObservedIOWriteBytes - prevIO.WriteBytes))
	}
	if metrics.ObservedIOReadOps >= prevIO.ReadOps {
		exp.userIOReadOps.WithLabelValues(uidStr, username).Add(float64(metrics.ObservedIOReadOps - prevIO.ReadOps))
	}
	if metrics.ObservedIOWriteOps >= prevIO.WriteOps {
		exp.userIOWriteOps.WithLabelValues(uidStr, username).Add(float64(metrics.ObservedIOWriteOps - prevIO.WriteOps))
	}
	exp.prevIOStats[ioKey] = ioStatsSnapshot{
		ReadBytes:  metrics.ObservedIOReadBytes,
		WriteBytes: metrics.ObservedIOWriteBytes,
		ReadOps:    metrics.ObservedIOReadOps,
		WriteOps:   metrics.ObservedIOWriteOps,
	}

	previousCgroupPath := exp.activeCgroupPaths[uidStr]
	if previousCgroupPath != "" && previousCgroupPath != metrics.CgroupPath {
		exp.deleteCgroupMetricSeries(uidStr, previousCgroupPath)
	}
	if metrics.CgroupPath == "" {
		delete(exp.activeCgroupPaths, uidStr)
		return
	}
	exp.activeCgroupPaths[uidStr] = metrics.CgroupPath

	quota, period := parseCPUQuota(metrics.CPUQuota)
	if period <= 0 {
		exp.cgroupCPUQuota.DeleteLabelValues(uidStr, metrics.CgroupPath)
		exp.cgroupCPUPeriod.DeleteLabelValues(uidStr, metrics.CgroupPath)
	} else {
		if quota >= 0 {
			exp.cgroupCPUQuota.WithLabelValues(uidStr, metrics.CgroupPath).Set(float64(quota))
		} else {
			// A valid max record has a period but no finite quota sample.
			exp.cgroupCPUQuota.DeleteLabelValues(uidStr, metrics.CgroupPath)
		}
		exp.cgroupCPUPeriod.WithLabelValues(uidStr, metrics.CgroupPath).Set(float64(period))
	}

	if cgroupMemoryAvailable {
		exp.cgroupMemoryUsage.WithLabelValues(uidStr, metrics.CgroupPath).Set(float64(cgroupMemory))
	} else {
		exp.cgroupMemoryUsage.DeleteLabelValues(uidStr, metrics.CgroupPath)
	}
}

// CleanupUserMetrics removes series for users absent from the current snapshot.
func (exp *PrometheusExporter) CleanupUserMetrics(activeUids map[int]bool) {
	if exp == nil {
		return
	}

	leaveMetrics := exp.metricsGate.Enter()
	defer leaveMetrics()

	// Iterate over every tracked user.
	for userKey := range exp.activeUserMetrics {
		// Validate whether the user remains active.
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

		// Remove every series and local baseline for inactive users.
		if !activeUids[uid] {
			// Remove exported series.
			exp.userCPUUsage.DeleteLabelValues(uidStr, username)
			exp.userCPUUsageAverage.DeleteLabelValues(uidStr, username)
			exp.userCPUUsageEMA.DeleteLabelValues(uidStr, username)
			exp.userMemoryUsage.DeleteLabelValues(uidStr, username)
			exp.userProcessCount.DeleteLabelValues(uidStr, username)
			exp.userCPULimitActive.DeleteLabelValues(uidStr, username)
			exp.userMemoryHighEvents.DeleteLabelValues(uidStr, username)
			exp.userIOReadBytes.DeleteLabelValues(uidStr, username)
			exp.userIOWriteBytes.DeleteLabelValues(uidStr, username)
			exp.userIOReadOps.DeleteLabelValues(uidStr, username)
			exp.userIOWriteOps.DeleteLabelValues(uidStr, username)
			if prevPattern, ok := exp.prevUserPatterns[userKey]; ok {
				exp.userWorkloadPattern.DeleteLabelValues(uidStr, username, prevPattern)
			}
			if cgroupPath := exp.activeCgroupPaths[uidStr]; cgroupPath != "" {
				exp.deleteCgroupMetricSeries(uidStr, cgroupPath)
				delete(exp.activeCgroupPaths, uidStr)
			}

			// Remove previous-value baselines.
			memoryHighKey := fmt.Sprintf("%s_%s", uidStr, username)
			delete(exp.prevMemoryHighEvents, memoryHighKey)
			delete(exp.prevIOStats, memoryHighKey)
			delete(exp.prevUserPatterns, userKey)

			// Remove presence tracking.
			delete(exp.activeUserMetrics, userKey)

			exp.logger.Debug("Removed metrics for inactive user",
				"uid", uid,
				"username", username,
			)
		}
	}
}

// getCgroupMemoryUsage reads memory.current for one cgroup.
func (exp *PrometheusExporter) getCgroupMemoryUsage(cgroupPath string) (uint64, bool) {
	memoryCurrentFile := filepath.Join(cgroupPath, "memory.current")
	data, err := os.ReadFile(memoryCurrentFile)
	if err != nil {
		return 0, false
	}
	usage, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return usage, true
}

// deleteCgroupMetricSeries removes every cgroup gauge for one UID/path tuple.
// The caller must hold metricsGate.
func (exp *PrometheusExporter) deleteCgroupMetricSeries(uidStr, cgroupPath string) {
	if cgroupPath == "" {
		return
	}
	exp.cgroupCPUQuota.DeleteLabelValues(uidStr, cgroupPath)
	exp.cgroupCPUPeriod.DeleteLabelValues(uidStr, cgroupPath)
	exp.cgroupMemoryUsage.DeleteLabelValues(uidStr, cgroupPath)
}

// UpdateUserWorkloadPattern publishes the detected pattern for one user.
func (exp *PrometheusExporter) UpdateUserWorkloadPattern(uid int, username string, pattern string, confidence float64) {
	if exp == nil || exp.registry == nil || exp.userWorkloadPattern == nil {
		return
	}

	uidStr := strconv.Itoa(uid)
	if username == "" || username == uidStr {
		username = exp.getUsernameFromUID(uidStr)
	}
	userKey := fmt.Sprintf("%s_%s", uidStr, username)

	leaveMetrics := exp.metricsGate.Enter()
	defer leaveMetrics()

	if prevPattern, ok := exp.prevUserPatterns[userKey]; ok && prevPattern != pattern {
		exp.userWorkloadPattern.DeleteLabelValues(uidStr, username, prevPattern)
	}

	exp.userWorkloadPattern.WithLabelValues(uidStr, username, pattern).Set(confidence)
	exp.prevUserPatterns[userKey] = pattern
}

// parseCPUQuota extracts quota and period from a "quota period" value.
func parseCPUQuota(quotaStr string) (quota int64, period int64) {
	parts := strings.Fields(quotaStr)
	if len(parts) != 2 {
		return -1, -1
	}
	parsedPeriod, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || parsedPeriod <= 0 {
		return -1, -1
	}

	if parts[0] == "max" {
		return -1, parsedPeriod
	}
	parsedQuota, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || parsedQuota <= 0 {
		return -1, -1
	}
	return parsedQuota, parsedPeriod
}

// getUsernameFromUID converts a UID into a username.
func (exp *PrometheusExporter) getUsernameFromUID(uidStr string) string {
	uid, err := strconv.Atoi(uidStr)
	if err != nil {
		return "unknown"
	}
	if resolver, ok := exp.usernameResolver.Load().(func(int) string); ok {
		return resolver(uid)
	}

	// Fall back to /etc/passwd.
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

// IncrementCPULimitsActivated records a confirmed inactive-to-active CPU transition.
func (exp *PrometheusExporter) IncrementCPULimitsActivated() {
	if exp == nil {
		return
	}
	exp.cpuLimitsActivatedTotal.Inc()
}

// IncrementCPULimitsDeactivated records a confirmed active-to-inactive CPU transition.
func (exp *PrometheusExporter) IncrementCPULimitsDeactivated() {
	if exp == nil {
		return
	}
	exp.cpuLimitsDeactivatedTotal.Inc()
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

// RecordPSIEvent records a PSI event received from the kernel.
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

// RecordCgroupIngressSkips records bounded PID namespace guard outcomes.
func (exp *PrometheusExporter) RecordCgroupIngressSkips(result cgroup.ProcessMoveResult) {
	if exp == nil {
		return
	}
	if result.PIDNamespaceMismatches > 0 {
		exp.cgroupIngressSkipped.WithLabelValues(string(cgroup.PIDNamespaceMismatch)).Add(float64(result.PIDNamespaceMismatches))
	}
	if result.PIDNamespaceUnavailable > 0 {
		exp.cgroupIngressSkipped.WithLabelValues(string(cgroup.PIDNamespaceUnavailable)).Add(float64(result.PIDNamespaceUnavailable))
	}
}

// RecordLimitHookExecution records one terminal hook outcome using bounded labels.
func (exp *PrometheusExporter) RecordLimitHookExecution(hookType LimitHookType, outcome LimitHookOutcome) {
	if exp == nil {
		return
	}
	if !validLimitHookType(hookType) || !validLimitHookOutcome(outcome) {
		exp.logger.Error("Rejected invalid limit hook metric labels",
			"hook_type", hookType,
			"outcome", outcome,
		)
		return
	}
	exp.limitHookExecutions.WithLabelValues(string(hookType), string(outcome)).Inc()
}

func validLimitHookType(hookType LimitHookType) bool {
	return hookType == LimitHookTypeScript || hookType == LimitHookTypeHTTP
}

func validLimitHookOutcome(outcome LimitHookOutcome) bool {
	switch outcome {
	case LimitHookOutcomeSuccess, LimitHookOutcomeFailure, LimitHookOutcomeTimeout, LimitHookOutcomeCancelled:
		return true
	default:
		return false
	}
}

// authMiddleware handles Basic Auth and JWT authentication.
func (exp *PrometheusExporter) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pass the request through when authentication is disabled.
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

// checkBasicAuth validates Basic Auth credentials.
func (exp *PrometheusExporter) checkBasicAuth(r *http.Request) bool {
	if exp.cfg.PrometheusAuthUsername == "" || exp.basicAuthPassword == "" {
		return false
	}
	username, password, ok := r.BasicAuth()
	if !ok {
		return false
	}

	// Verify the username.
	if subtle.ConstantTimeCompare([]byte(username), []byte(exp.cfg.PrometheusAuthUsername)) != 1 {
		return false
	}

	// Verify the password.
	if subtle.ConstantTimeCompare([]byte(password), []byte(exp.basicAuthPassword)) != 1 {
		return false
	}

	return true
}

// checkJWTAuth validates a JWT.
func (exp *PrometheusExporter) checkJWTAuth(r *http.Request) bool {
	if len(exp.jwtSecret) == 0 {
		return false
	}
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return false
	}

	// Extract the bearer token.
	parts := strings.Split(authHeader, " ")
	if len(parts) != 2 || parts[0] != "Bearer" {
		return false
	}

	tokenString := parts[1]

	// Parse and validate the token.
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// Verify the signing algorithm.
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
		// Verify the issuer.
		if exp.cfg.PrometheusJWTIssuer != "" {
			if issuer, ok := claims["iss"].(string); !ok || issuer != exp.cfg.PrometheusJWTIssuer {
				return false
			}
		}

		// Verify the audience.
		if exp.cfg.PrometheusJWTAudience != "" {
			if audience, ok := claims["aud"].(string); !ok || audience != exp.cfg.PrometheusJWTAudience {
				return false
			}
		}

		return true
	}

	return false
}

// healthHandler serves the /health endpoint.
func (exp *PrometheusExporter) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"status": "healthy", "timestamp": "%s", "auth_enabled": "%s"}`,
		time.Now().Format(time.RFC3339),
		exp.cfg.PrometheusAuthType,
	)
}

// rootHandler serves the root endpoint.
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
	if exp.isRunning || exp.starting {
		exp.mu.Unlock()
		return fmt.Errorf("exporter already running")
	}
	exp.starting = true
	exp.startDone = make(chan struct{})
	// A failed restart must not expose the terminal result of an earlier run.
	exp.run = nil
	exp.mu.Unlock()
	defer exp.finishStartAttempt()

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
	server := &http.Server{
		Addr:      addr,
		Handler:   mux,
		TLSConfig: exp.tlsConfig,
	}

	// Configure TLS when enabled.
	if exp.cfg.PrometheusTLSEnabled {
		if exp.tlsConfig == nil || len(exp.tlsConfig.Certificates) == 0 {
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
		return fmt.Errorf("failed to listen for Prometheus metrics on %s: %w", addr, err)
	}

	run := &prometheusRun{
		server:    server,
		stop:      make(chan struct{}),
		serveDone: make(chan error, 1),
		done:      make(chan struct{}),
	}
	exp.mu.Lock()
	exp.run = run
	exp.isRunning = true
	exp.mu.Unlock()

	// Start the server and retain its terminal result for Stop.
	go func() {
		var err error
		if exp.cfg.PrometheusTLSEnabled {
			err = server.ServeTLS(listener, "", "")
		} else {
			err = server.Serve(listener)
		}
		run.serveDone <- err
	}()
	exp.logger.Info("Prometheus server verified as listening", "address", listener.Addr().String())

	go exp.monitorRun(ctx, run)

	return nil
}

func (exp *PrometheusExporter) finishStartAttempt() {
	exp.mu.Lock()
	exp.starting = false
	if exp.startDone != nil {
		close(exp.startDone)
	}
	exp.mu.Unlock()
}

func (exp *PrometheusExporter) monitorRun(ctx context.Context, run *prometheusRun) {
	var shutdownErr error
	var serveErr error
	select {
	case serveErr = <-run.serveDone:
	case <-ctx.Done():
		exp.logger.Info("Context cancelled, shutting down Prometheus server")
		shutdownErr = exp.shutdownRun(run)
		serveErr = <-run.serveDone
	case <-run.stop:
		exp.logger.Info("Stop signal received")
		shutdownErr = exp.shutdownRun(run)
		serveErr = <-run.serveDone
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	if serveErr != nil {
		exp.logger.Error("Prometheus server error", "error", serveErr)
	}

	run.err = errors.Join(shutdownErr, serveErr)
	exp.mu.Lock()
	if exp.run == run {
		exp.isRunning = false
	}
	exp.mu.Unlock()
	close(run.done)
}

func (exp *PrometheusExporter) shutdownRun(run *prometheusRun) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	exp.logger.Info("Shutting down Prometheus HTTP server")
	if err := exp.shutdownHTTPServer(run.server, shutdownCtx); err != nil {
		exp.logger.Error("Error during Prometheus server shutdown", "error", err)
		closeErr := exp.closeHTTPServer(run.server)
		if closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
			return errors.Join(err, fmt.Errorf("force close Prometheus server: %w", closeErr))
		}
		return err
	}
	exp.logger.Info("Prometheus HTTP server stopped")
	return nil
}

// Stop waits for the Prometheus server goroutines and returns their terminal error.
func (exp *PrometheusExporter) Stop() error {
	if exp == nil {
		return nil
	}

	for {
		exp.mu.RLock()
		starting := exp.starting
		startDone := exp.startDone
		run := exp.run
		exp.mu.RUnlock()

		if starting {
			<-startDone
			continue
		}
		if run == nil {
			return nil
		}
		run.stopOnce.Do(func() { close(run.stop) })
		<-run.done
		return run.err
	}
}

// IsRunning reports whether the exporter is running.
func (exp *PrometheusExporter) IsRunning() bool {
	if exp == nil {
		return false
	}

	exp.mu.RLock()
	defer exp.mu.RUnlock()
	return exp.isRunning
}

// GetMetricsEndpoint returns the metrics endpoint.
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
