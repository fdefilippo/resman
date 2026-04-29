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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ioStatsSnapshot tiene traccia dei valori precedenti per calcolare i delta dei counter IO.
type ioStatsSnapshot struct {
	ReadBytes  uint64
	WriteBytes uint64
	ReadOps    uint64
	WriteOps   uint64
}

// PrometheusExporter esporta metriche in formato Prometheus.
type PrometheusExporter struct {
	cfg      *config.Config
	logger   *logging.Logger
	registry *prometheus.Registry
	server   *http.Server

	// Label fisse per tutte le metriche (da configurazione)
	hostname   string
	serverRole string

	// Metriche base (con label hostname e server_role)
	cpuTotalUsage  prometheus.Gauge
	memoryUsage    prometheus.Gauge
	totalMemoryMB  prometheus.Gauge
	cachedMemoryMB prometheus.Gauge
	limitedUsers   prometheus.Gauge

	// ALL USERS metrics (tutti gli utenti non-system, UID >= SYSTEM_UID_MIN)
	allUsersCPUUsage    prometheus.Gauge
	allUsersMemoryUsage prometheus.Gauge
	allUsersCount       prometheus.Gauge

	// LIMITED USERS metrics (solo utenti che passano i filtri)
	limitedUsersCPUUsage    prometheus.Gauge
	limitedUsersMemoryUsage prometheus.Gauge
	limitedUsersCount       prometheus.Gauge

	limitsActive prometheus.Gauge
	systemLoad   prometheus.Gauge
	totalCores   prometheus.Gauge
	actionCores  prometheus.Gauge

	// Metriche con label aggiuntive
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

	// Track utenti attivi per cleanup metriche
	activeUserMetrics    map[string]bool   // "uid_username" -> true
	prevMemoryHighEvents map[string]uint64 // "uid_username" -> last known value
	prevIOStats          map[string]ioStatsSnapshot
	prevUserPatterns     map[string]string // "uid_username" -> previous pattern label

	// Metriche counter (solo incremento)
	limitsActivatedTotal   prometheus.Counter
	limitsDeactivatedTotal prometheus.Counter
	controlCyclesTotal     prometheus.Counter
	controlCycleTriggers   *prometheus.CounterVec
	psiEventsTotal         *prometheus.CounterVec
	psiLastEventTimestamp  *prometheus.GaugeVec
	errorsTotal            *prometheus.CounterVec

	// Metriche histogram per tempi di esecuzione
	controlCycleDuration      prometheus.Histogram
	metricsCollectionDuration prometheus.Histogram

	// Cache per evitare aggiornamenti troppo frequenti
	lastUpdate     time.Time
	updateInterval time.Duration
	mu             sync.RWMutex

	// Stato interno
	isRunning bool
	stopChan  chan struct{}

	// Autenticazione
	basicAuthPassword string
	jwtSecret         []byte

	// TLS
	tlsCertFile string
	tlsKeyFile  string
	tlsCAFile   string
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
		updateInterval:       15 * time.Second,
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
		logger.Warn("Failed to load authentication credentials", "error", err)
	}

	// Registra metriche
	if err := exp.registerMetrics(); err != nil {
		return nil, fmt.Errorf("failed to register metrics: %w", err)
	}

	// Registra metriche standard di Go
	exp.registry.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	logger.Info("Prometheus exporter created successfully",
		"auth_type", cfg.PrometheusAuthType,
	)
	return exp, nil
}

// loadAuthCredentials carica le credenziali di autenticazione e i certificati TLS
func (exp *PrometheusExporter) loadCredentials() error {
	// Carica password per Basic Auth
	if exp.cfg.PrometheusAuthType == "basic" || exp.cfg.PrometheusAuthType == "both" {
		if exp.cfg.PrometheusAuthPasswordFile != "" {
			password, err := os.ReadFile(exp.cfg.PrometheusAuthPasswordFile)
			if err != nil {
				return fmt.Errorf("failed to read password file: %w", err)
			}
			exp.basicAuthPassword = strings.TrimSpace(string(password))
			exp.logger.Info("Basic authentication password loaded")
		}
	}

	// Carica secret per JWT
	if exp.cfg.PrometheusAuthType == "jwt" || exp.cfg.PrometheusAuthType == "both" {
		if exp.cfg.PrometheusJWTSecretFile != "" {
			secret, err := os.ReadFile(exp.cfg.PrometheusJWTSecretFile)
			if err != nil {
				return fmt.Errorf("failed to read JWT secret file: %w", err)
			}
			exp.jwtSecret = []byte(strings.TrimSpace(string(secret)))
			exp.logger.Info("JWT secret loaded",
				"issuer", exp.cfg.PrometheusJWTIssuer,
				"audience", exp.cfg.PrometheusJWTAudience,
				"expiry_seconds", exp.cfg.PrometheusJWTExpiry,
			)
		}
	}

	// Carica certificati TLS
	if exp.cfg.PrometheusTLSEnabled {
		if exp.cfg.PrometheusTLSCertFile != "" {
			if _, err := os.Stat(exp.cfg.PrometheusTLSCertFile); err != nil {
				return fmt.Errorf("TLS certificate file not found: %s", exp.cfg.PrometheusTLSCertFile)
			}
			exp.tlsCertFile = exp.cfg.PrometheusTLSCertFile
			exp.logger.Info("TLS certificate file loaded",
				"cert_file", exp.cfg.PrometheusTLSCertFile,
			)
		}

		if exp.cfg.PrometheusTLSKeyFile != "" {
			if _, err := os.Stat(exp.cfg.PrometheusTLSKeyFile); err != nil {
				return fmt.Errorf("TLS key file not found: %s", exp.cfg.PrometheusTLSKeyFile)
			}
			exp.tlsKeyFile = exp.cfg.PrometheusTLSKeyFile
			exp.logger.Info("TLS key file loaded",
				"key_file", exp.cfg.PrometheusTLSKeyFile,
			)
		}

		if exp.cfg.PrometheusTLSCAFile != "" {
			if _, err := os.Stat(exp.cfg.PrometheusTLSCAFile); err != nil {
				return fmt.Errorf("TLS CA file not found: %s", exp.cfg.PrometheusTLSCAFile)
			}
			exp.tlsCAFile = exp.cfg.PrometheusTLSCAFile
			exp.logger.Info("TLS CA file loaded",
				"ca_file", exp.cfg.PrometheusTLSCAFile,
			)
		}
	}

	return nil
}

// registerMetrics registra tutte le metriche Prometheus.
func (exp *PrometheusExporter) registerMetrics() error {
	// Namespace per tutte le metriche
	namespace := "resman"

	// Label fisse per tutte le metriche
	staticLabels := prometheus.Labels{
		"hostname":    exp.hostname,
		"server_role": exp.serverRole,
	}

	// === Metriche Gauge (valori correnti) ===

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

	// === Metriche con label ===

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

	// NUOVA METRICA: Memoria per utente
	exp.userMemoryUsage = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "user_memory_usage_bytes",
			Help:        "Memory usage in bytes per user",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "username"},
	)

	// NUOVA METRICA: Numero processi per utente
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
			Help:        "Total read operations on block devices by user",
			ConstLabels: staticLabels,
		},
		[]string{"uid", "username"},
	)

	exp.userIOWriteOps = promauto.With(exp.registry).NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "user_io_write_ops_total",
			Help:        "Total write operations on block devices by user",
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
			Namespace: namespace,
			Name:      "cgroup_cpu_quota_microseconds",
			Help:      "CPU quota in microseconds per period (max = unlimited)",
		},
		[]string{"uid", "cgroup_path"},
	)

	exp.cgroupCPUPeriod = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "cgroup_cpu_period_microseconds",
			Help:      "CPU period in microseconds",
		},
		[]string{"uid", "cgroup_path"},
	)

	// NUOVA METRICA: Memoria cgroup per utente
	exp.cgroupMemoryUsage = promauto.With(exp.registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "cgroup_memory_usage_bytes",
			Help:      "Memory usage in bytes per cgroup (user)",
		},
		[]string{"uid", "cgroup_path"},
	)

	// === Metriche Counter (solo incremento) ===

	exp.limitsActivatedTotal = promauto.With(exp.registry).NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "limits_activated_total",
		Help:      "Total number of times CPU limits were activated",
	})

	exp.limitsDeactivatedTotal = promauto.With(exp.registry).NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "limits_deactivated_total",
		Help:      "Total number of times CPU limits were deactivated",
	})

	exp.controlCyclesTotal = promauto.With(exp.registry).NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "control_cycles_total",
		Help:      "Total number of control cycles executed",
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
			Namespace: namespace,
			Name:      "errors_total",
			Help:      "Total number of errors by type",
		},
		[]string{"component", "error_type"},
	)

	// === Metriche Histogram (distribuzione) ===

	exp.controlCycleDuration = promauto.With(exp.registry).NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "control_cycle_duration_seconds",
		Help:      "Duration of control cycles in seconds",
		Buckets:   prometheus.DefBuckets,
	})

	exp.metricsCollectionDuration = promauto.With(exp.registry).NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "metrics_collection_duration_seconds",
		Help:      "Duration of metrics collection in seconds",
		Buckets:   []float64{.001, .005, .01, .025, .05, .1, .25, .5},
	})

	return nil
}

// UpdateMetrics aggiorna i valori delle metriche.
func (exp *PrometheusExporter) UpdateMetrics(metrics map[string]float64) {
	if exp == nil {
		return
	}

	exp.mu.Lock()
	defer exp.mu.Unlock()

	// Aggiorna solo se è passato abbastanza tempo dall'ultimo aggiornamento
	now := time.Now()
	if now.Sub(exp.lastUpdate) < exp.updateInterval {
		return
	}
	exp.lastUpdate = now

	// Incrementa il contatore dei cicli
	exp.controlCyclesTotal.Inc()

	// Aggiorna le metriche base
	for key, value := range metrics {
		switch {
		case key == "cpu_total_usage":
			exp.cpuTotalUsage.Set(value)

		// ALL USERS metrics
		case key == "all_users_cpu_usage":
			exp.allUsersCPUUsage.Set(value)
		case key == "all_users_count":
			exp.allUsersCount.Set(value)

		// LIMITED USERS metrics
		case key == "limited_users":
			exp.limitedUsers.Set(value)
		case key == "limited_users_cpu_usage":
			exp.limitedUsersCPUUsage.Set(value)
		case key == "limited_users_count":
			exp.limitedUsersCount.Set(value)

		case key == "memory_usage_mb":
			exp.memoryUsage.Set(value)
		case key == "total_memory_mb":
			exp.totalMemoryMB.Set(value)
		case key == "cached_memory_mb":
			exp.cachedMemoryMB.Set(value)
		case key == "all_users_memory_usage":
			exp.allUsersMemoryUsage.Set(value)
		case key == "limited_users_memory_usage":
			exp.limitedUsersMemoryUsage.Set(value)
		case key == "limits_active":
			exp.limitsActive.Set(value)
		case key == "system_load":
			exp.systemLoad.Set(value)
		case key == "total_cores":
			exp.totalCores.Set(value)
		case strings.HasPrefix(key, "user_cpu_usage_"):
			// Formato: user_cpu_usage_1000 (dove 1000 è l'UID)
			parts := strings.Split(key, "_")
			if len(parts) >= 4 {
				uid := parts[3]
				username := exp.getUsernameFromUID(uid)
				exp.userCPUUsage.WithLabelValues(uid, username).Set(value)
			}
		case strings.HasPrefix(key, "user_memory_usage_"):
			// Formato: user_memory_usage_1000 (dove 1000 è l'UID)
			parts := strings.Split(key, "_")
			if len(parts) >= 4 {
				uid := parts[3]
				username := exp.getUsernameFromUID(uid)
				// Converti MB in bytes se necessario
				bytesValue := value
				if strings.HasSuffix(key, "_mb") {
					bytesValue = value * 1024 * 1024
				}
				exp.userMemoryUsage.WithLabelValues(uid, username).Set(bytesValue)
			}
		case strings.HasPrefix(key, "user_limited_"):
			// Formato: user_limited_1000
			parts := strings.Split(key, "_")
			if len(parts) >= 3 {
				uid := parts[2]
				username := exp.getUsernameFromUID(uid)
				exp.userLimited.WithLabelValues(uid, username).Set(value)
			}
		case strings.HasPrefix(key, "cgroup_cpu_quota_"):
			// Formato: cgroup_cpu_quota_1000:/sys/fs/cgroup/...
			exp.updateCgroupMetric(key, value, exp.cgroupCPUQuota)
		case strings.HasPrefix(key, "cgroup_cpu_period_"):
			// Formato: cgroup_cpu_period_1000:/sys/fs/cgroup/...
			exp.updateCgroupMetric(key, value, exp.cgroupCPUPeriod)
		case strings.HasPrefix(key, "cgroup_memory_usage_"):
			// Formato: cgroup_memory_usage_1000:/sys/fs/cgroup/...
			exp.updateCgroupMetric(key, value, exp.cgroupMemoryUsage)
		case key == "control_cycle_duration":
			exp.controlCycleDuration.Observe(value)
		case key == "metrics_collection_duration":
			exp.metricsCollectionDuration.Observe(value)
		}
	}
}

// UpdateUserMetrics aggiorna le metriche specifiche per utente.
func (exp *PrometheusExporter) UpdateUserMetrics(uid int, username string, cpuUsage float64, cpuUsageAverage float64, cpuUsageEMA float64, memoryUsage uint64, processCount int, isLimited bool, cgroupPath, cpuQuota string, memoryHighEvents uint64, ioReadBytes, ioWriteBytes, ioReadOps, ioWriteOps uint64) {
	if exp == nil || exp.registry == nil {
		return
	}

	uidStr := strconv.Itoa(uid)

	// Se username è vuoto, cerca di ottenerlo (before lock to minimize hold time)
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

	// Marca utente come attivo
	userKey := fmt.Sprintf("%s_%s", uidStr, username)
	exp.activeUserMetrics[userKey] = true

	// Aggiorna uso CPU dell'utente
	exp.userCPUUsage.WithLabelValues(uidStr, username).Set(cpuUsage)
	exp.userCPUUsageAverage.WithLabelValues(uidStr, username).Set(cpuUsageAverage)
	exp.userCPUUsageEMA.WithLabelValues(uidStr, username).Set(cpuUsageEMA)

	// Aggiorna uso memoria dell'utente (in bytes)
	exp.userMemoryUsage.WithLabelValues(uidStr, username).Set(float64(memoryUsage))

	// Aggiorna numero processi dell'utente
	exp.userProcessCount.WithLabelValues(uidStr, username).Set(float64(processCount))

	// Aggiorna stato limite
	limitedValue := 0.0
	if isLimited {
		limitedValue = 1.0
	}
	exp.userLimited.WithLabelValues(uidStr, username).Set(limitedValue)

	// Aggiorna eventi memory.high breach (counter con delta)
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

	// Se disponibile, aggiorna le metriche cgroup
	if cgroupPath != "" {
		// Aggiorna quota CPU
		if cpuQuota != "" {
			quota, period := parseCPUQuota(cpuQuota)
			if quota >= 0 {
				exp.cgroupCPUQuota.WithLabelValues(uidStr, cgroupPath).Set(float64(quota))
			}
			if period > 0 {
				exp.cgroupCPUPeriod.WithLabelValues(uidStr, cgroupPath).Set(float64(period))
			}
		}

		// Aggiorna uso memoria del cgroup (fix #6: use pre-read value, no redundant file read)
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

	// Prova a leggere da /etc/passwd
	file, err := os.Open("/etc/passwd")
	if err != nil {
		return uidStr
	}
	defer file.Close()

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

// updateCgroupMetric aggiorna una metrica cgroup con parsing delle label.
func (exp *PrometheusExporter) updateCgroupMetric(key string, value float64, metric *prometheus.GaugeVec) {
	// Formato: cgroup_cpu_quota_1000:/sys/fs/cgroup/resman/user_1000
	if !strings.Contains(key, ":") {
		return
	}

	// Rimuove il prefisso (es: "cgroup_cpu_quota_")
	prefixEnd := strings.Index(key, "_")
	if prefixEnd == -1 {
		return
	}

	// Estrae UID e path
	remaining := key[prefixEnd+1:]
	colonIndex := strings.Index(remaining, ":")
	if colonIndex == -1 {
		return
	}

	uid := remaining[:colonIndex]
	cgroupPath := remaining[colonIndex+1:]

	metric.WithLabelValues(uid, cgroupPath).Set(value)
}

// IncrementLimitsActivated incrementa il contatore di attivazioni limiti.
func (exp *PrometheusExporter) IncrementLimitsActivated() {
	if exp == nil {
		return
	}
	exp.limitsActivatedTotal.Inc()
}

// IncrementLimitsDeactivated incrementa il contatore di disattivazioni limiti.
func (exp *PrometheusExporter) IncrementLimitsDeactivated() {
	if exp == nil {
		return
	}
	exp.limitsDeactivatedTotal.Inc()
}

// RecordControlCycleTrigger registra la causa che ha avviato un ciclo di controllo.
func (exp *PrometheusExporter) RecordControlCycleTrigger(trigger string) {
	if exp == nil || exp.controlCycleTriggers == nil {
		return
	}
	if trigger == "" {
		trigger = "unknown"
	}
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

// RecordControlCycleDuration registra la durata di un ciclo di controllo.
func (exp *PrometheusExporter) RecordControlCycleDuration(duration time.Duration) {
	if exp == nil {
		return
	}
	exp.controlCycleDuration.Observe(duration.Seconds())
}

// RecordMetricsCollectionDuration registra la durata della raccolta metriche.
func (exp *PrometheusExporter) RecordMetricsCollectionDuration(duration time.Duration) {
	if exp == nil {
		return
	}
	exp.metricsCollectionDuration.Observe(duration.Seconds())
}

// RecordError incrementa il contatore errori per un componente specifico.
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
	})

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
	fmt.Fprintf(w, `{"status": "healthy", "timestamp": "%s", "auth_enabled": "%s"}`,
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
	fmt.Fprintf(w, `<html><body><h1>Resource Manager Metrics%s</h1><p><a href="/metrics">Metrics</a></p><p><a href="/health">Health</a></p></body></html>`, authInfo)
}

// Start avvia il server HTTP per Prometheus.
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

	// Handler per le metriche con autenticazione
	mux.Handle("/metrics", exp.authMiddleware(promhttp.HandlerFor(
		exp.registry,
		promhttp.HandlerOpts{
			Registry:          exp.registry,
			EnableOpenMetrics: true,
		},
	)))

	// Health check endpoint (senza autenticazione per monitoring)
	mux.HandleFunc("/health", exp.healthHandler)

	// Root endpoint
	mux.HandleFunc("/", exp.rootHandler)

	addr := fmt.Sprintf("%s:%d", exp.cfg.PrometheusMetricsBindHost, exp.cfg.PrometheusMetricsBindPort)
	exp.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Configura TLS se abilitato
	if exp.cfg.PrometheusTLSEnabled {
		exp.logger.Info("Starting Prometheus HTTPS server",
			"address", addr,
			"auth_type", exp.cfg.PrometheusAuthType,
			"tls_enabled", exp.cfg.PrometheusTLSEnabled,
			"tls_min_version", exp.cfg.PrometheusTLSMinVersion,
		)
	} else {
		exp.logger.Info("Starting Prometheus HTTP server",
			"address", addr,
			"auth_type", exp.cfg.PrometheusAuthType,
			"tls_enabled", false,
		)
	}

	// Avvia il server in una goroutine
	listenErr := make(chan error, 1)
	go func() {
		var err error
		if exp.cfg.PrometheusTLSEnabled {
			// HTTPS con TLS
			if exp.tlsCertFile == "" || exp.tlsKeyFile == "" {
				listenErr <- fmt.Errorf("TLS enabled but certificate or key file not configured")
				return
			}
			err = exp.server.ListenAndServeTLS(exp.tlsCertFile, exp.tlsKeyFile)
		} else {
			// HTTP semplice
			err = exp.server.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			exp.logger.Error("Prometheus server error", "error", err)
			listenErr <- err
		}
	}()

	// Verifica che il server sia effettivamente in ascolto
	go func() {
		time.Sleep(500 * time.Millisecond)
		resp, err := http.Get(fmt.Sprintf("http://%s/health", addr))
		if err == nil && resp.StatusCode == 200 {
			exp.logger.Info("Prometheus server verified as running")
			resp.Body.Close()
		} else {
			exp.logger.Warn("Could not verify Prometheus server", "error", err)
		}
	}()

	// Gestione shutdown
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
		exp.server.Close()
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
	return fmt.Sprintf("http://%s:%d/metrics", exp.cfg.PrometheusMetricsBindHost, exp.cfg.PrometheusMetricsBindPort)
}
