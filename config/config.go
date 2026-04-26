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
// config/config.go
package config

import (
	"fmt"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Timeframe rappresenta un intervallo di tempo per i blackout
type Timeframe struct {
	DaysOfWeek []int // Giorni della settimana (0-6, 0=Domenica)
	HourStart  int   // Ora inizio (0-23)
	HourEnd    int   // Ora fine (0-23)
}

// Config contiene tutti i parametri configurabili dell'applicazione.
type Config struct {
	mu sync.RWMutex

	// Regex cache for pre-compiled patterns (performance optimization)
	regexCache sync.Map // map[string]*regexp.Regexp

	// Paths
	CgroupRoot         string `config:"CGROUP_ROOT"`
	CgroupBase         string `config:"CGROUP_BASE"`
	ConfigFile         string `config:"CONFIG_FILE"` // Ricorsivo, usato all'avvio
	LogFile            string `config:"LOG_FILE"`
	CreatedCgroupsFile string `config:"CREATED_CGROUPS_FILE"`
	MetricsCacheFile   string `config:"METRICS_CACHE_FILE"`
	PrometheusFile     string `config:"PROMETHEUS_FILE"`

	// Timing
	PollingInterval int `config:"POLLING_INTERVAL"`
	MinActiveTime   int `config:"MIN_ACTIVE_TIME"`
	MetricsCacheTTL int `config:"METRICS_CACHE_TTL"`

	// Timeout (seconds/milliseconds)
	CgroupOperationTimeout int `config:"CGROUP_OPERATION_TIMEOUT"` // Timeout for cgroup operations (seconds)
	CgroupRetryDelayMs     int `config:"CGROUP_RETRY_DELAY_MS"`    // Delay between cgroup retry attempts (milliseconds)
	MCPShutdownTimeout     int `config:"MCP_SHUTDOWN_TIMEOUT"`     // Timeout for MCP server shutdown (seconds)

	// Thresholds (percentages)
	CPUThreshold        int `config:"CPU_THRESHOLD"`
	CPUReleaseThreshold int `config:"CPU_RELEASE_THRESHOLD"`

	// Threshold Time Window (seconds)
	CPUThresholdDuration int `config:"CPU_THRESHOLD_DURATION"` // Seconds to wait before activating limits (0 = immediate)

	// CPU limits (cpu.max format: "quota period")
	CPUQuotaNormal  string `config:"CPU_QUOTA_NORMAL"`
	CPUQuotaLimited string `config:"CPU_QUOTA_LIMITED"`

	// RAM limits
	RAMEnabled          bool    `config:"RAM_LIMIT_ENABLED"`
	RAMThreshold        int     `config:"RAM_THRESHOLD"`
	RAMReleaseThreshold int     `config:"RAM_RELEASE_THRESHOLD"`
	RAMQuotaLimited     string  `config:"RAM_QUOTA_LIMITED"`
	RAMQuotaPerUser     string  `config:"RAM_QUOTA_PER_USER"`
	DisableSwap         bool    `config:"DISABLE_SWAP"`
	RAMHighRatio        float64 `config:"RAM_HIGH_RATIO"` // Ratio for memory.high (0.0-1.0, default 0.8)

	// RAM User Include List (regex support)
	RAMUserIncludeList []string `config:"RAM_USER_INCLUDE_LIST"`

	// RAM User Exclude List (regex support)
	RAMUserExcludeList []string `config:"RAM_USER_EXCLUDE_LIST"`

	// IO limits (block I/O via cgroups v2 io controller)
	IOEnabled           bool   `config:"IO_LIMIT_ENABLED"`
	IOThreshold         int    `config:"IO_THRESHOLD"`
	IOReleaseThreshold  int    `config:"IO_RELEASE_THRESHOLD"`
	IOReadBPS           string `config:"IO_READ_BPS"`           // Read bandwidth limit (e.g., "100M", "1G")
	IOWriteBPS          string `config:"IO_WRITE_BPS"`          // Write bandwidth limit (e.g., "50M", "500M")
	IOReadIOPS          int    `config:"IO_READ_IOPS"`          // Read IOPS limit (0 = unlimited)
	IOWriteIOPS         int    `config:"IO_WRITE_IOPS"`         // Write IOPS limit (0 = unlimited)
	IODeviceFilter      string `config:"IO_DEVICE_FILTER"`      // "all" or "major:minor" (default "all")
	IOThresholdDuration int    `config:"IO_THRESHOLD_DURATION"` // Seconds to wait before activating IO limits (0 = immediate)

	// IO Starvation Auto-Remediation
	IORemediationEnabled      bool    `config:"IO_REMEDIATION_ENABLED"`
	IOStarvationThreshold     int     `config:"IO_STARVATION_THRESHOLD"`      // Seconds of continuous throttling before remediation
	IOStarvationCheckInterval int     `config:"IO_STARVATION_CHECK_INTERVAL"` // Check frequency in seconds
	IOBoostMultiplier         float64 `config:"IO_BOOST_MULTIPLIER"`          // Multiplier for temporary limits
	IOBoostDuration           int     `config:"IO_BOOST_DURATION"`            // Duration of boost in seconds
	IOBoostMaxPerHour         int     `config:"IO_BOOST_MAX_PER_HOUR"`        // Max boosts per user per hour
	IOPSIThreshold            float64 `config:"IO_PSI_THRESHOLD"`             // PSI some avg10 % threshold
	IORevertOnNormal          bool    `config:"IO_REVERT_ON_NORMAL"`          // Revert limits when IO returns to normal

	// IO User Include/Exclude Lists (regex support)
	IOUserIncludeList []string `config:"IO_USER_INCLUDE_LIST"`
	IOUserExcludeList []string `config:"IO_USER_EXCLUDE_LIST"`

	// Workload Pattern Detection (auto-detect user patterns)
	AutodetectPatterns         bool    `config:"AUTODETECT_PATTERNS"`
	PatternHistoryHours        int     `config:"PATTERN_HISTORY_HOURS"`        // Finestra storica (ore)
	PatternMinSamples          int     `config:"PATTERN_MIN_SAMPLES"`          // Minimo campioni per decidere
	PatternConfidenceThreshold float64 `config:"PATTERN_CONFIDENCE_THRESHOLD"` // Soglia confidenza (0.0-1.0)
	// Policy per pattern
	BatchNightCPUQuota  int    `config:"BATCH_NIGHT_CPU_QUOTA"` // CPU quota per batch (microseconds)
	BatchNightRAMQuota  string `config:"BATCH_NIGHT_RAM_QUOTA"` // RAM quota per batch
	InteractiveCPUQuota int    `config:"INTERACTIVE_CPU_QUOTA"` // CPU quota per interattivo
	InteractiveRAMQuota string `config:"INTERACTIVE_RAM_QUOTA"` // RAM quota per interattivo

	// Hooks
	LimitHookEnabled bool   `config:"LIMIT_HOOK_ENABLED"`
	LimitHookScript  string `config:"LIMIT_HOOK_SCRIPT"`
	LimitHookURL     string `config:"LIMIT_HOOK_URL"`
	LimitHookTimeout int    `config:"LIMIT_HOOK_TIMEOUT"` // seconds

	// Prometheus
	EnablePrometheus          bool   `config:"ENABLE_PROMETHEUS"`
	PrometheusMetricsBindHost string `config:"PROMETHEUS_METRICS_BIND_HOST"` // Default: 127.0.0.1 (secure)
	PrometheusMetricsBindPort int    `config:"PROMETHEUS_METRICS_BIND_PORT"`

	// Prometheus TLS/HTTPS (optional)
	PrometheusTLSEnabled    bool   `config:"PROMETHEUS_TLS_ENABLED"`
	PrometheusTLSCertFile   string `config:"PROMETHEUS_TLS_CERT_FILE"`
	PrometheusTLSKeyFile    string `config:"PROMETHEUS_TLS_KEY_FILE"`
	PrometheusTLSCAFile     string `config:"PROMETHEUS_TLS_CA_FILE"`
	PrometheusTLSMinVersion string `config:"PROMETHEUS_TLS_MIN_VERSION"` // 1.0, 1.1, 1.2, 1.3

	// Prometheus Authentication
	PrometheusAuthType         string `config:"PROMETHEUS_AUTH_TYPE"` // none, basic, jwt, both
	PrometheusAuthUsername     string `config:"PROMETHEUS_AUTH_USERNAME"`
	PrometheusAuthPasswordFile string `config:"PROMETHEUS_AUTH_PASSWORD_FILE"`
	PrometheusJWTSecretFile    string `config:"PROMETHEUS_JWT_SECRET_FILE"`
	PrometheusJWTIssuer        string `config:"PROMETHEUS_JWT_ISSUER"`
	PrometheusJWTAudience      string `config:"PROMETHEUS_JWT_AUDIENCE"`
	PrometheusJWTExpiry        int    `config:"PROMETHEUS_JWT_EXPIRY"` // seconds

	// Logging
	LogLevel   string `config:"LOG_LEVEL"`
	LogMaxSize int    `config:"LOG_MAX_SIZE"` // in bytes
	UseSyslog  bool   `config:"USE_SYSLOG"`

	// System
	MinSystemCores int `config:"MIN_SYSTEM_CORES"`
	SystemUIDMin   int `config:"SYSTEM_UID_MIN"`
	SystemUIDMax   int `config:"SYSTEM_UID_MAX"`

	// User Include List (users to INCLUDE in limiting, regex support)
	UserIncludeList []string `config:"USER_INCLUDE_LIST"` // Comma-separated regex patterns

	// User Exclude List (users to EXCLUDE from limits, regex support)
	UserExcludeList []string `config:"USER_EXCLUDE_LIST"` // Comma-separated regex patterns

	// Process Exclude List (process names to EXCLUDE from limits, comma-separated)
	// These processes are never limited, even if the user is in the include list
	ProcessExcludeList []string `config:"PROCESS_EXCLUDE_LIST"`

	// Blackout Timeframes (when CPU Manager should NOT apply limits)
	BlackoutTimeframes []Timeframe `config:"-"` // Parsed from BLACKOUT_SPEC

	// Blackout specification string (crontab-like format)
	BlackoutSpec string `config:"BLACKOUT"` // e.g., "1-5 08-18;0,6 00-23"

	// Load checking
	IgnoreSystemLoad bool `config:"IGNORE_SYSTEM_LOAD"`

	// Server Role
	ServerRole string `config:"SERVER_ROLE"` // e.g., database, web-frontend, batch, application, etc.

	// MCP Server
	MCPEnabled       bool   `config:"MCP_ENABLED"`
	MCPTransport     string `config:"MCP_TRANSPORT"`
	MCPHTTPPort      int    `config:"MCP_HTTP_PORT"`
	MCPHTTPHost      string `config:"MCP_HTTP_HOST"`
	MCPLogLevel      string `config:"MCP_LOG_LEVEL"`
	MCPAuthToken     string `config:"MCP_AUTH_TOKEN"`
	MCPAllowWriteOps bool   `config:"MCP_ALLOW_WRITE_OPS"`

	// Metrics Database (SQLite)
	MetricsDBEnabled       bool   `config:"METRICS_DB_ENABLED"`
	MetricsDBPath          string `config:"METRICS_DB_PATH"`
	MetricsDBRetentionDays int    `config:"METRICS_DB_RETENTION_DAYS"`
	MetricsDBWriteInterval int    `config:"METRICS_DB_WRITE_INTERVAL"` // seconds

	// Username Cache TTL (minutes)
	UsernameCacheTTL int `config:"USERNAME_CACHE_TTL"` // minutes, default 60

	// PSI Event-Driven mode (usa poll() sui pressure file invece del solo ticker)
	PSIEventDriven       bool   `config:"PSI_EVENT_DRIVEN"`       // Enable PSI event-driven control cycles
	PSICPUStallThreshold int    `config:"PSI_CPU_STALL_THRESHOLD"` // CPU stall threshold in microseconds (default 50000 = 5% su window 1s)
	PSIOStallThreshold   int    `config:"PSI_IO_STALL_THRESHOLD"` // IO stall threshold in microseconds (default 50000)
	PSIWindowUs          int    `config:"PSI_WINDOW_US"`          // PSI tracking window in microseconds (default 1000000 = 1s)
	PSIFallbackInterval  int    `config:"PSI_FALLBACK_INTERVAL"`  // Fallback polling interval in seconds when event-driven (default 300 = 5min)
}

// DefaultConfig restituisce la configurazione predefinita (come nel tuo script Bash).
func DefaultConfig() *Config {
	// Lettura dinamica del pid_max per il default di SYSTEM_UID_MAX
	pidMax := 60000 // valore di fallback
	if data, err := os.ReadFile("/proc/sys/kernel/pid_max"); err == nil {
		if val, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			pidMax = val
		}
	}

	return &Config{
		CgroupRoot:         "/sys/fs/cgroup",
		CgroupBase:         "resman",
		ConfigFile:         "/etc/resman.conf",
		LogFile:            "/var/log/resman.log",
		CreatedCgroupsFile: "/var/run/resman-cgroups.txt",
		MetricsCacheFile:   "/var/run/resman-metrics.cache",
		PrometheusFile:     "/var/run/resman-metrics.prom",

		PollingInterval: 30,
		MinActiveTime:   60,
		MetricsCacheTTL: 15,

		// Timeout defaults
		CgroupOperationTimeout: 5,   // 5 seconds for cgroup operations
		CgroupRetryDelayMs:     100, // 100ms between retries
		MCPShutdownTimeout:     10,  // 10 seconds for MCP shutdown

		CPUThreshold:         75,
		CPUReleaseThreshold:  40,
		CPUThresholdDuration: 90, // Default: wait 90 seconds before activating limits

		CPUQuotaNormal:  "max 100000",
		CPUQuotaLimited: "50000 100000", // 0.5 core

		RAMEnabled:          false,
		RAMThreshold:        75,
		RAMReleaseThreshold: 40,
		RAMQuotaLimited:     "2G",
		RAMQuotaPerUser:     "512M",
		DisableSwap:         false,
		RAMHighRatio:        0.8, // Default: memory.high = 80% of memory.max
		RAMUserIncludeList:  nil,
		RAMUserExcludeList:  nil,

		// IO limits
		IOEnabled:           false,
		IOThreshold:         75,
		IOReleaseThreshold:  40,
		IOReadBPS:           "100M", // 100 MB/s
		IOWriteBPS:          "50M",  // 50 MB/s
		IOReadIOPS:          1000,
		IOWriteIOPS:         500,
		IODeviceFilter:      "all",
		IOThresholdDuration: 0, // 0 = immediate (no duration check)

		// IO Starvation Auto-Remediation
		IORemediationEnabled:      false,
		IOStarvationThreshold:     300, // 5 minutes
		IOStarvationCheckInterval: 30,  // 30 seconds
		IOBoostMultiplier:         2.0, // 2x limits
		IOBoostDuration:           600, // 10 minutes
		IOBoostMaxPerHour:         3,
		IOPSIThreshold:            50.0, // 50%
		IORevertOnNormal:          true,

		IOUserIncludeList: nil,
		IOUserExcludeList: nil,

		// Workload Pattern Detection
		AutodetectPatterns:         false,
		PatternHistoryHours:        168, // 7 days
		PatternMinSamples:          24,  // 24 hours minimum
		PatternConfidenceThreshold: 0.7,
		BatchNightCPUQuota:         200000, // 200% (2 cores)
		BatchNightRAMQuota:         "4G",
		InteractiveCPUQuota:        50000, // 50%
		InteractiveRAMQuota:        "1G",

		// Limit hook
		LimitHookEnabled: false,
		LimitHookScript:  "",
		LimitHookURL:     "",
		LimitHookTimeout: 10,

		EnablePrometheus:          false,
		PrometheusMetricsBindHost: "127.0.0.1", // Default: localhost only (secure)
		PrometheusMetricsBindPort: 1974,

		// Prometheus TLS (disabled by default)
		PrometheusTLSEnabled:    false,
		PrometheusTLSCertFile:   "/etc/resman/tls/server.crt",
		PrometheusTLSKeyFile:    "/etc/resman/tls/server.key",
		PrometheusTLSCAFile:     "",
		PrometheusTLSMinVersion: "1.2", // TLS 1.2 minimum recommended

		// Prometheus Authentication (disabled by default)
		PrometheusAuthType:         "none",
		PrometheusAuthUsername:     "",
		PrometheusAuthPasswordFile: "",
		PrometheusJWTSecretFile:    "",
		PrometheusJWTIssuer:        "resman",
		PrometheusJWTAudience:      "prometheus",
		PrometheusJWTExpiry:        3600,

		LogLevel:   "INFO",
		LogMaxSize: 10 * 1024 * 1024, // 10MB
		UseSyslog:  false,

		MinSystemCores:   1,
		SystemUIDMin:     1000,
		SystemUIDMax:     pidMax,
		IgnoreSystemLoad: false,
		ServerRole:       "",  // Empty by default
		UserIncludeList:  nil, // nil = all users included (no filter)
		UserExcludeList:  nil, // nil = no users excluded (all users can be limited)
		ProcessExcludeList: []string{ // Default processes to never limit (regex patterns)
			"^systemd$", "^dbus-daemon$", "^dbus-broker$", "^polkitd$",
		},
		BlackoutSpec:       "", // Empty = no blackout (always active)
		BlackoutTimeframes: nil,

		// MCP Server
		MCPEnabled:       false,
		MCPTransport:     "stdio",
		MCPHTTPPort:      1969,
		MCPHTTPHost:      "", // Empty = use default 0.0.0.0
		MCPLogLevel:      "INFO",
		MCPAuthToken:     "",
		MCPAllowWriteOps: false,

		// Metrics Database (SQLite)
		MetricsDBEnabled:       false,
		MetricsDBPath:          "/etc/resman/metrics.db",
		MetricsDBRetentionDays: 30,
		MetricsDBWriteInterval: 30, // Same as polling interval by default

		// Username Cache TTL (minutes)
		UsernameCacheTTL: 60, // Default 60 minutes

		// PSI Event-Driven mode defaults
		PSIEventDriven:       false,
		PSICPUStallThreshold: 50000,
		PSIOStallThreshold:   50000,
		PSIWindowUs:          1000000,
		PSIFallbackInterval:  300,
	}
}

// LoadAndValidate carica la configurazione da file e variabili d'ambiente,
// sovrascrivendo i default, e poi la valida.
func LoadAndValidate(configPath string) (*Config, error) {
	cfg := DefaultConfig()

	// 1. Carica dal file di configurazione (se esiste)
	if err := loadFromFile(configPath, cfg); err != nil {
		return nil, fmt.Errorf("loading config file %s: %w", configPath, err)
	}

	// 2. Sovrascrivi con le variabili d'ambiente
	warnings := loadFromEnvironment(cfg)
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "WARNING: %s\n", w)
	}

	// 3. Valida
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	// 4. Warning se USER_INCLUDE_LIST è vuota (nessun utente sarà limitato)
	if cfg.UserIncludeList == nil || len(cfg.UserIncludeList) == 0 {
		fmt.Fprintf(os.Stderr, "WARNING: USER_INCLUDE_LIST is empty - no users will be CPU limited. "+
			"Set USER_INCLUDE_LIST=.* to limit all users, or specify patterns (e.g., USER_INCLUDE_LIST=^www.*,^app.*).\n")
	}

	return cfg, nil
}

// loadFromFile legge un file di configurazione in formato chiave=valore.
func loadFromFile(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// File non esistente è ok, useremo default/env
			return nil
		}
		return err
	}

	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		// Salta commenti e righe vuote
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("malformed config line %d: %s", i+1, line)
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// Rimuovi commenti inline (tutto dopo #)
		if commentIdx := strings.Index(value, "#"); commentIdx != -1 {
			value = value[:commentIdx]
		}

		// Rimuovi eventuali virgolette e spazi extra
		value = strings.TrimSpace(strings.Trim(value, `"'`))

		if err := setConfigField(cfg, key, value); err != nil {
			return fmt.Errorf("setting key %s on line %d: %w", key, i+1, err)
		}
	}
	return nil
}

// loadFromEnvironment sovrascrive i valori con le variabili d'ambiente.
// Restituisce una lista di warning per valori non parsabili.
func loadFromEnvironment(cfg *Config) []string {
	cfgType := reflect.TypeOf(cfg).Elem()
	cfgValue := reflect.ValueOf(cfg).Elem()
	var warnings []string

	for i := 0; i < cfgType.NumField(); i++ {
		field := cfgType.Field(i)

		// Ottieni il tag 'config' per il nome della variabile d'ambiente
		envKey := field.Tag.Get("config")
		if envKey == "" {
			continue
		}

		// Cerca la variabile d'ambiente
		envValue := os.Getenv(envKey)
		if envValue == "" {
			continue
		}

		// Imposta il valore in base al tipo
		fieldValue := cfgValue.Field(i)
		if !fieldValue.CanSet() {
			continue
		}

		switch field.Type.Kind() {
		case reflect.String:
			fieldValue.SetString(envValue)
		case reflect.Int:
			if intVal, err := strconv.Atoi(envValue); err == nil {
				fieldValue.SetInt(int64(intVal))
			} else {
				warnings = append(warnings, fmt.Sprintf("Invalid integer for %s=%q, using default value %d", envKey, envValue, fieldValue.Int()))
			}
		case reflect.Bool:
			lowerVal := strings.ToLower(envValue)
			boolVal := false
			switch lowerVal {
			case "true", "1", "yes", "on":
				boolVal = true
			case "false", "0", "no", "off":
				boolVal = false
			default:
				warnings = append(warnings, fmt.Sprintf("Invalid boolean for %s=%q, using default value %v", envKey, envValue, fieldValue.Bool()))
			}
			fieldValue.SetBool(boolVal)
		case reflect.Slice:
			if field.Type.Elem().Kind() == reflect.String {
				parts := strings.Split(envValue, ",")
				sliceVal := reflect.MakeSlice(field.Type, 0, len(parts))
				for _, part := range parts {
					part = strings.TrimSpace(part)
					if part != "" {
						sliceVal = reflect.Append(sliceVal, reflect.ValueOf(part))
					}
				}
				fieldValue.Set(sliceVal)
			} else {
				warnings = append(warnings, fmt.Sprintf("Unsupported slice type for %s=%q", envKey, envValue))
			}
		case reflect.Float64:
			if fVal, err := strconv.ParseFloat(envValue, 64); err == nil {
				fieldValue.SetFloat(fVal)
			} else {
				warnings = append(warnings, fmt.Sprintf("Invalid float for %s=%q, using default value %f", envKey, envValue, fieldValue.Float()))
			}
		}
	}

	return warnings
}

// setConfigField imposta il valore di un campo nella struct Config basandosi sul tag `config`.
func setConfigField(cfg *Config, key, value string) error {
	switch key {
	// Paths
	case "CGROUP_ROOT":
		cfg.CgroupRoot = value
	case "CGROUP_BASE":
		// SECURITY: Validate path does not contain traversal sequences
		if strings.Contains(value, "..") || strings.HasPrefix(value, "/") {
			return fmt.Errorf("invalid CGROUP_BASE: must be a relative path without '..'")
		}
		cfg.CgroupBase = value
	case "CONFIG_FILE":
		cfg.ConfigFile = value
	case "LOG_FILE":
		cfg.LogFile = value
	case "CREATED_CGROUPS_FILE":
		cfg.CreatedCgroupsFile = value
	case "METRICS_CACHE_FILE":
		cfg.MetricsCacheFile = value
	case "PROMETHEUS_FILE":
		cfg.PrometheusFile = value

	// Timing
	case "POLLING_INTERVAL":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.PollingInterval = i
		}
	case "MIN_ACTIVE_TIME":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.MinActiveTime = i
		}
	case "METRICS_CACHE_TTL":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.MetricsCacheTTL = i
		}

	// Thresholds
	case "CPU_THRESHOLD":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.CPUThreshold = i
		}
	case "CPU_RELEASE_THRESHOLD":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.CPUReleaseThreshold = i
		}
	case "CPU_THRESHOLD_DURATION":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.CPUThresholdDuration = i
		}

	// CPU limits
	case "CPU_QUOTA_NORMAL":
		cfg.CPUQuotaNormal = value
	case "CPU_QUOTA_LIMITED":
		cfg.CPUQuotaLimited = value

	// Limit hook
	case "LIMIT_HOOK_ENABLED":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.LimitHookEnabled = true
		case "false", "0", "no", "off":
			cfg.LimitHookEnabled = false
		default:
			cfg.LimitHookEnabled = false
		}
	case "LIMIT_HOOK_SCRIPT":
		cfg.LimitHookScript = value
	case "LIMIT_HOOK_URL":
		cfg.LimitHookURL = value
	case "LIMIT_HOOK_TIMEOUT":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.LimitHookTimeout = i
		}

	// Prometheus
	case "ENABLE_PROMETHEUS":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.EnablePrometheus = true
		case "false", "0", "no", "off":
			cfg.EnablePrometheus = false
		default:
			cfg.EnablePrometheus = false
		}
	case "PROMETHEUS_METRICS_BIND_HOST":
		cfg.PrometheusMetricsBindHost = value
	case "PROMETHEUS_METRICS_BIND_PORT":
		if i, err := strconv.Atoi(value); err == nil {
			if i < 1 || i > 65535 {
				return fmt.Errorf("invalid PROMETHEUS_METRICS_BIND_PORT: %d (must be 1-65535)", i)
			}
			cfg.PrometheusMetricsBindPort = i
		}
	// Backward compatibility: old variable names
	case "PROMETHEUS_HOST":
		cfg.PrometheusMetricsBindHost = value
	case "PROMETHEUS_PORT":
		if i, err := strconv.Atoi(value); err == nil {
			if i < 1 || i > 65535 {
				return fmt.Errorf("invalid PROMETHEUS_PORT: %d (must be 1-65535)", i)
			}
			cfg.PrometheusMetricsBindPort = i
		}

	// Prometheus TLS
	case "PROMETHEUS_TLS_ENABLED":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.PrometheusTLSEnabled = true
		case "false", "0", "no", "off":
			cfg.PrometheusTLSEnabled = false
		default:
			cfg.PrometheusTLSEnabled = false
		}
	case "PROMETHEUS_TLS_CERT_FILE":
		cfg.PrometheusTLSCertFile = value
	case "PROMETHEUS_TLS_KEY_FILE":
		cfg.PrometheusTLSKeyFile = value
	case "PROMETHEUS_TLS_CA_FILE":
		cfg.PrometheusTLSCAFile = value
	case "PROMETHEUS_TLS_MIN_VERSION":
		cfg.PrometheusTLSMinVersion = strings.ToUpper(value)

	// Prometheus Authentication
	case "PROMETHEUS_AUTH_TYPE":
		cfg.PrometheusAuthType = strings.ToLower(value)
	case "PROMETHEUS_AUTH_USERNAME":
		cfg.PrometheusAuthUsername = value
	case "PROMETHEUS_AUTH_PASSWORD_FILE":
		cfg.PrometheusAuthPasswordFile = value
	case "PROMETHEUS_JWT_SECRET_FILE":
		cfg.PrometheusJWTSecretFile = value
	case "PROMETHEUS_JWT_ISSUER":
		cfg.PrometheusJWTIssuer = value
	case "PROMETHEUS_JWT_AUDIENCE":
		cfg.PrometheusJWTAudience = value
	case "PROMETHEUS_JWT_EXPIRY":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.PrometheusJWTExpiry = i
		}

	// Logging
	case "LOG_LEVEL":
		cfg.LogLevel = strings.ToUpper(value)
	case "LOG_MAX_SIZE":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.LogMaxSize = i
		}
	case "USE_SYSLOG":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.UseSyslog = true
		case "false", "0", "no", "off":
			cfg.UseSyslog = false
		default:
			cfg.UseSyslog = false
		}

	// System
	case "MIN_SYSTEM_CORES":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.MinSystemCores = i
		}
	case "SYSTEM_UID_MIN":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.SystemUIDMin = i
		}
	case "SYSTEM_UID_MAX":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.SystemUIDMax = i
		}

	// User Include List
	case "USER_INCLUDE_LIST":
		// Parse comma-separated list of regex patterns
		value = strings.TrimSpace(value)
		if value == "" {
			cfg.UserIncludeList = nil // Empty = all users included
		} else {
			patterns := strings.Split(value, ",")
			cfg.UserIncludeList = make([]string, 0, len(patterns))
			for _, pattern := range patterns {
				pattern = strings.TrimSpace(pattern)
				if pattern != "" {
					// Validate regex pattern
					if _, err := regexp.Compile(pattern); err != nil {
						return fmt.Errorf("invalid regex pattern '%s': %w", pattern, err)
					}
					cfg.UserIncludeList = append(cfg.UserIncludeList, pattern)
				}
			}
			if len(cfg.UserIncludeList) == 0 {
				cfg.UserIncludeList = nil
			}
		}

	// User Exclude List
	case "USER_EXCLUDE_LIST":
		// Parse comma-separated list of regex patterns
		value = strings.TrimSpace(value)
		if value == "" {
			cfg.UserExcludeList = nil // Empty = no users excluded
		} else {
			patterns := strings.Split(value, ",")
			cfg.UserExcludeList = make([]string, 0, len(patterns))
			for _, pattern := range patterns {
				pattern = strings.TrimSpace(pattern)
				if pattern != "" {
					// Validate regex pattern
					if _, err := regexp.Compile(pattern); err != nil {
						return fmt.Errorf("invalid regex pattern '%s': %w", pattern, err)
					}
					cfg.UserExcludeList = append(cfg.UserExcludeList, pattern)
				}
			}
			if len(cfg.UserExcludeList) == 0 {
				cfg.UserExcludeList = nil
			}
		}

	// Process Exclude List
	case "PROCESS_EXCLUDE_LIST":
		// Parse comma-separated list of process names (regex support)
		value = strings.TrimSpace(value)
		if value == "" {
			cfg.ProcessExcludeList = nil // Empty = no processes excluded
		} else {
			patterns := strings.Split(value, ",")
			cfg.ProcessExcludeList = make([]string, 0, len(patterns))
			for _, pattern := range patterns {
				pattern = strings.TrimSpace(pattern)
				if pattern != "" {
					// Validate regex pattern
					if _, err := regexp.Compile(pattern); err != nil {
						return fmt.Errorf("invalid regex pattern '%s' in PROCESS_EXCLUDE_LIST: %w", pattern, err)
					}
					cfg.ProcessExcludeList = append(cfg.ProcessExcludeList, pattern)
				}
			}
			if len(cfg.ProcessExcludeList) == 0 {
				cfg.ProcessExcludeList = nil
			}
		}

	// Blackout Timeframes
	case "BLACKOUT":
		cfg.BlackoutSpec = strings.TrimSpace(value)
		if cfg.BlackoutSpec != "" {
			timeframes, err := ParseTimeframe(cfg.BlackoutSpec)
			if err != nil {
				return fmt.Errorf("invalid blackout specification '%s': %w", cfg.BlackoutSpec, err)
			}
			cfg.BlackoutTimeframes = timeframes
		} else {
			cfg.BlackoutTimeframes = nil
		}

	// Backward compatibility: old variable name
	case "USER_WHITELIST":
		// Treat old USER_WHITELIST as USER_EXCLUDE_LIST for compatibility
		value = strings.TrimSpace(value)
		if value == "" {
			cfg.UserExcludeList = nil
		} else {
			usernames := strings.Split(value, ",")
			cfg.UserExcludeList = make([]string, 0, len(usernames))
			for _, username := range usernames {
				username = strings.TrimSpace(username)
				if username != "" {
					cfg.UserExcludeList = append(cfg.UserExcludeList, username)
				}
			}
			if len(cfg.UserExcludeList) == 0 {
				cfg.UserExcludeList = nil
			}
		}

	// Load checking
	case "IGNORE_SYSTEM_LOAD":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.IgnoreSystemLoad = true
		case "false", "0", "no", "off":
			cfg.IgnoreSystemLoad = false
		default:
			cfg.IgnoreSystemLoad = false
		}

	// Server Role
	case "SERVER_ROLE":
		cfg.ServerRole = value

	// MCP Server
	case "MCP_ENABLED":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.MCPEnabled = true
		case "false", "0", "no", "off":
			cfg.MCPEnabled = false
		default:
			cfg.MCPEnabled = false
		}
	case "MCP_TRANSPORT":
		cfg.MCPTransport = strings.ToLower(value)
	case "MCP_HTTP_PORT":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.MCPHTTPPort = i
		}
	case "MCP_HTTP_HOST":
		cfg.MCPHTTPHost = value
	case "MCP_LOG_LEVEL":
		cfg.MCPLogLevel = strings.ToUpper(value)
	case "MCP_AUTH_TOKEN":
		cfg.MCPAuthToken = value
	case "MCP_ALLOW_WRITE_OPS":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.MCPAllowWriteOps = true
		case "false", "0", "no", "off":
			cfg.MCPAllowWriteOps = false
		default:
			cfg.MCPAllowWriteOps = false
		}

	// Metrics Database
	case "METRICS_DB_ENABLED":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.MetricsDBEnabled = true
		case "false", "0", "no", "off":
			cfg.MetricsDBEnabled = false
		default:
			cfg.MetricsDBEnabled = false
		}
	case "METRICS_DB_PATH":
		cfg.MetricsDBPath = value
	case "METRICS_DB_RETENTION_DAYS":
		if i, err := strconv.Atoi(value); err == nil && i > 0 {
			cfg.MetricsDBRetentionDays = i
		}
	case "METRICS_DB_WRITE_INTERVAL":
		if i, err := strconv.Atoi(value); err == nil && i > 0 {
			cfg.MetricsDBWriteInterval = i
		}
	case "USERNAME_CACHE_TTL":
		if i, err := strconv.Atoi(value); err == nil && i > 0 {
			cfg.UsernameCacheTTL = i
		}

	// Timeouts
	case "CGROUP_OPERATION_TIMEOUT":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.CgroupOperationTimeout = i
		}
	case "CGROUP_RETRY_DELAY_MS":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.CgroupRetryDelayMs = i
		}
	case "MCP_SHUTDOWN_TIMEOUT":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.MCPShutdownTimeout = i
		}

	// RAM limits
	case "RAM_LIMIT_ENABLED":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.RAMEnabled = true
		case "false", "0", "no", "off":
			cfg.RAMEnabled = false
		default:
			cfg.RAMEnabled = false
		}
	case "RAM_THRESHOLD":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.RAMThreshold = i
		}
	case "RAM_RELEASE_THRESHOLD":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.RAMReleaseThreshold = i
		}
	case "RAM_QUOTA_LIMITED":
		cfg.RAMQuotaLimited = value
	case "RAM_QUOTA_PER_USER":
		cfg.RAMQuotaPerUser = value
	case "DISABLE_SWAP":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.DisableSwap = true
		case "false", "0", "no", "off":
			cfg.DisableSwap = false
		default:
			cfg.DisableSwap = false
		}
	case "RAM_HIGH_RATIO":
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			cfg.RAMHighRatio = f
		}
	case "RAM_USER_INCLUDE_LIST":
		value = strings.TrimSpace(value)
		if value == "" {
			cfg.RAMUserIncludeList = nil
		} else {
			patterns := strings.Split(value, ",")
			cfg.RAMUserIncludeList = make([]string, 0, len(patterns))
			for _, pattern := range patterns {
				pattern = strings.TrimSpace(pattern)
				if pattern != "" {
					if _, err := regexp.Compile(pattern); err != nil {
						return fmt.Errorf("invalid regex pattern '%s' in RAM_USER_INCLUDE_LIST: %w", pattern, err)
					}
					cfg.RAMUserIncludeList = append(cfg.RAMUserIncludeList, pattern)
				}
			}
			if len(cfg.RAMUserIncludeList) == 0 {
				cfg.RAMUserIncludeList = nil
			}
		}
	case "RAM_USER_EXCLUDE_LIST":
		value = strings.TrimSpace(value)
		if value == "" {
			cfg.RAMUserExcludeList = nil
		} else {
			patterns := strings.Split(value, ",")
			cfg.RAMUserExcludeList = make([]string, 0, len(patterns))
			for _, pattern := range patterns {
				pattern = strings.TrimSpace(pattern)
				if pattern != "" {
					if _, err := regexp.Compile(pattern); err != nil {
						return fmt.Errorf("invalid regex pattern '%s' in RAM_USER_EXCLUDE_LIST: %w", pattern, err)
					}
					cfg.RAMUserExcludeList = append(cfg.RAMUserExcludeList, pattern)
				}
			}
			if len(cfg.RAMUserExcludeList) == 0 {
				cfg.RAMUserExcludeList = nil
			}
		}

	// IO limits
	case "IO_LIMIT_ENABLED":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.IOEnabled = true
		case "false", "0", "no", "off":
			cfg.IOEnabled = false
		default:
			cfg.IOEnabled = false
		}
	case "IO_THRESHOLD":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.IOThreshold = i
		}
	case "IO_RELEASE_THRESHOLD":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.IOReleaseThreshold = i
		}
	case "IO_READ_BPS":
		cfg.IOReadBPS = value
	case "IO_WRITE_BPS":
		cfg.IOWriteBPS = value
	case "IO_READ_IOPS":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.IOReadIOPS = i
		}
	case "IO_WRITE_IOPS":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.IOWriteIOPS = i
		}
	case "IO_DEVICE_FILTER":
		cfg.IODeviceFilter = value
	case "IO_THRESHOLD_DURATION":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.IOThresholdDuration = i
		}
	case "IO_USER_INCLUDE_LIST":
		value = strings.TrimSpace(value)
		if value == "" {
			cfg.IOUserIncludeList = nil
		} else {
			patterns := strings.Split(value, ",")
			cfg.IOUserIncludeList = make([]string, 0, len(patterns))
			for _, pattern := range patterns {
				pattern = strings.TrimSpace(pattern)
				if pattern != "" {
					if _, err := regexp.Compile(pattern); err != nil {
						return fmt.Errorf("invalid regex pattern '%s' in IO_USER_INCLUDE_LIST: %w", pattern, err)
					}
					cfg.IOUserIncludeList = append(cfg.IOUserIncludeList, pattern)
				}
			}
			if len(cfg.IOUserIncludeList) == 0 {
				cfg.IOUserIncludeList = nil
			}
		}
	case "IO_USER_EXCLUDE_LIST":
		value = strings.TrimSpace(value)
		if value == "" {
			cfg.IOUserExcludeList = nil
		} else {
			patterns := strings.Split(value, ",")
			cfg.IOUserExcludeList = make([]string, 0, len(patterns))
			for _, pattern := range patterns {
				pattern = strings.TrimSpace(pattern)
				if pattern != "" {
					if _, err := regexp.Compile(pattern); err != nil {
						return fmt.Errorf("invalid regex pattern '%s' in IO_USER_EXCLUDE_LIST: %w", pattern, err)
					}
					cfg.IOUserExcludeList = append(cfg.IOUserExcludeList, pattern)
				}
			}
			if len(cfg.IOUserExcludeList) == 0 {
				cfg.IOUserExcludeList = nil
			}
		}

	// IO Starvation Auto-Remediation
	case "IO_REMEDIATION_ENABLED":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.IORemediationEnabled = true
		case "false", "0", "no", "off":
			cfg.IORemediationEnabled = false
		default:
			cfg.IORemediationEnabled = false
		}
	case "IO_STARVATION_THRESHOLD":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.IOStarvationThreshold = i
		}
	case "IO_STARVATION_CHECK_INTERVAL":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.IOStarvationCheckInterval = i
		}
	case "IO_BOOST_MULTIPLIER":
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			cfg.IOBoostMultiplier = f
		}
	case "IO_BOOST_DURATION":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.IOBoostDuration = i
		}
	case "IO_BOOST_MAX_PER_HOUR":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.IOBoostMaxPerHour = i
		}
	case "IO_PSI_THRESHOLD":
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			cfg.IOPSIThreshold = f
		}
	case "IO_REVERT_ON_NORMAL":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.IORevertOnNormal = true
		case "false", "0", "no", "off":
			cfg.IORevertOnNormal = false
		default:
			cfg.IORevertOnNormal = true
		}

	// Workload Pattern Detection
	case "AUTODETECT_PATTERNS":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.AutodetectPatterns = true
		case "false", "0", "no", "off":
			cfg.AutodetectPatterns = false
		default:
			cfg.AutodetectPatterns = false
		}
	case "PATTERN_HISTORY_HOURS":
		if i, err := strconv.Atoi(value); err == nil && i > 0 {
			cfg.PatternHistoryHours = i
		}
	case "PATTERN_MIN_SAMPLES":
		if i, err := strconv.Atoi(value); err == nil && i > 0 {
			cfg.PatternMinSamples = i
		}
	case "PATTERN_CONFIDENCE_THRESHOLD":
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			cfg.PatternConfidenceThreshold = f
		}
	case "BATCH_NIGHT_CPU_QUOTA":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.BatchNightCPUQuota = i
		}
	case "BATCH_NIGHT_RAM_QUOTA":
		cfg.BatchNightRAMQuota = value
	case "INTERACTIVE_CPU_QUOTA":
		if i, err := strconv.Atoi(value); err == nil {
			cfg.InteractiveCPUQuota = i
		}
	case "INTERACTIVE_RAM_QUOTA":
		cfg.InteractiveRAMQuota = value

	// PSI Event-Driven mode
	case "PSI_EVENT_DRIVEN":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.PSIEventDriven = true
		case "false", "0", "no", "off":
			cfg.PSIEventDriven = false
		default:
			cfg.PSIEventDriven = false
		}
	case "PSI_CPU_STALL_THRESHOLD":
		if i, err := strconv.Atoi(value); err == nil && i > 0 {
			cfg.PSICPUStallThreshold = i
		}
	case "PSI_IO_STALL_THRESHOLD":
		if i, err := strconv.Atoi(value); err == nil && i > 0 {
			cfg.PSIOStallThreshold = i
		}
	case "PSI_WINDOW_US":
		if i, err := strconv.Atoi(value); err == nil && i > 0 {
			cfg.PSIWindowUs = i
		}
	case "PSI_FALLBACK_INTERVAL":
		if i, err := strconv.Atoi(value); err == nil && i > 0 {
			cfg.PSIFallbackInterval = i
		}

	default:
		return nil
	}

	return nil
}

// validateConfig esegue tutte le validazioni come nello script Bash.
func validateConfig(cfg *Config) error {
	var errors []string

	// Validate CPU thresholds
	if cfg.CPUThreshold < 1 || cfg.CPUThreshold > 100 {
		errors = append(errors, "CPU_THRESHOLD must be between 1 and 100")
	}
	if cfg.CPUReleaseThreshold < 1 || cfg.CPUReleaseThreshold > 100 {
		errors = append(errors, "CPU_RELEASE_THRESHOLD must be between 1 and 100")
	}
	if cfg.CPUThreshold <= cfg.CPUReleaseThreshold {
		errors = append(errors, "CPU_THRESHOLD must be greater than CPU_RELEASE_THRESHOLD")
	}

	// Validate threshold duration
	if cfg.CPUThresholdDuration < 0 {
		errors = append(errors, "CPU_THRESHOLD_DURATION cannot be negative")
	}

	// Validate metrics database configuration
	if cfg.MetricsDBRetentionDays < 1 {
		errors = append(errors, "METRICS_DB_RETENTION_DAYS must be at least 1")
	}
	if cfg.MetricsDBWriteInterval < 5 {
		errors = append(errors, "METRICS_DB_WRITE_INTERVAL must be at least 5 seconds")
	}
	if cfg.UsernameCacheTTL < 1 {
		errors = append(errors, "USERNAME_CACHE_TTL must be at least 1 minute")
	}

	// Validate polling interval
	if cfg.PollingInterval < 5 {
		errors = append(errors, "POLLING_INTERVAL must be at least 5 seconds")
	}

	// Validate limit hook configuration
	if cfg.LimitHookEnabled {
		if cfg.LimitHookTimeout < 1 {
			errors = append(errors, "LIMIT_HOOK_TIMEOUT must be at least 1 second")
		}
		if cfg.LimitHookScript == "" && cfg.LimitHookURL == "" {
			errors = append(errors, "LIMIT_HOOK_SCRIPT or LIMIT_HOOK_URL must be set when LIMIT_HOOK_ENABLED=true")
		}
		if cfg.LimitHookURL != "" {
			parsedURL, err := url.Parse(cfg.LimitHookURL)
			if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
				errors = append(errors, "LIMIT_HOOK_URL must be a valid http or https URL")
			}
		}
	}

	// Validate CPU quota format
	if !isValidCPUQuota(cfg.CPUQuotaLimited) {
		errors = append(errors, "CPU_QUOTA_LIMITED must be in format 'quota period' or 'max period'")
	}

	// Validate RAM limits configuration
	if cfg.RAMEnabled {
		if cfg.RAMThreshold < 1 || cfg.RAMThreshold > 100 {
			errors = append(errors, "RAM_THRESHOLD must be between 1 and 100")
		}
		if cfg.RAMReleaseThreshold < 1 || cfg.RAMReleaseThreshold > 100 {
			errors = append(errors, "RAM_RELEASE_THRESHOLD must be between 1 and 100")
		}
		if cfg.RAMThreshold <= cfg.RAMReleaseThreshold {
			errors = append(errors, "RAM_THRESHOLD must be greater than RAM_RELEASE_THRESHOLD")
		}
		if !isValidByteQuota(cfg.RAMQuotaLimited) {
			errors = append(errors, "RAM_QUOTA_LIMITED must be a valid byte value (e.g., '1073741824', '512M', '1G')")
		}
		if !isValidByteQuota(cfg.RAMQuotaPerUser) {
			errors = append(errors, "RAM_QUOTA_PER_USER must be a valid byte value (e.g., '536870912', '512M', '1G')")
		}
		if cfg.RAMHighRatio < 0 || cfg.RAMHighRatio > 1 {
			errors = append(errors, "RAM_HIGH_RATIO must be between 0.0 and 1.0 (e.g., 0.8 for 80%, 0 to disable)")
		}
	}

	// Validate IO limits
	if cfg.IOEnabled {
		if cfg.IOThreshold < 1 || cfg.IOThreshold > 100 {
			errors = append(errors, "IO_THRESHOLD must be between 1 and 100")
		}
		if cfg.IOReleaseThreshold < 1 || cfg.IOReleaseThreshold > 100 {
			errors = append(errors, "IO_RELEASE_THRESHOLD must be between 1 and 100")
		}
		if cfg.IOThreshold <= cfg.IOReleaseThreshold {
			errors = append(errors, "IO_THRESHOLD must be greater than IO_RELEASE_THRESHOLD")
		}
		if cfg.IOReadBPS != "" && cfg.IOReadBPS != "max" {
			if !isValidByteQuota(cfg.IOReadBPS) {
				errors = append(errors, "IO_READ_BPS must be a valid byte value (e.g., '104857600', '100M', '1G')")
			}
		}
		if cfg.IOWriteBPS != "" && cfg.IOWriteBPS != "max" {
			if !isValidByteQuota(cfg.IOWriteBPS) {
				errors = append(errors, "IO_WRITE_BPS must be a valid byte value (e.g., '52428800', '50M', '500M')")
			}
		}
		if cfg.IOReadIOPS < 0 {
			errors = append(errors, "IO_READ_IOPS must be >= 0 (0 = unlimited)")
		}
		if cfg.IOWriteIOPS < 0 {
			errors = append(errors, "IO_WRITE_IOPS must be >= 0 (0 = unlimited)")
		}
	}

	// Validate log level
	validLogLevels := map[string]bool{"DEBUG": true, "INFO": true, "WARN": true, "ERROR": true}
	if !validLogLevels[cfg.LogLevel] {
		errors = append(errors, "LOG_LEVEL must be one of: DEBUG, INFO, WARN, ERROR")
	}

	// Validate UID ranges
	if cfg.SystemUIDMin < 0 {
		errors = append(errors, "SYSTEM_UID_MIN cannot be negative")
	}
	if cfg.SystemUIDMax < cfg.SystemUIDMin {
		errors = append(errors, "SYSTEM_UID_MAX must be greater than SYSTEM_UID_MIN")
	}

	if len(errors) > 0 {
		return fmt.Errorf("%s", strings.Join(errors, "; "))
	}
	return nil
}

// isValidCPUQuota verifica il formato "quota period" o "max period".
func isValidCPUQuota(quota string) bool {
	parts := strings.Split(quota, " ")
	if len(parts) != 2 {
		return false
	}
	if parts[0] == "max" {
		_, err := strconv.Atoi(parts[1])
		return err == nil
	}
	// Entrambi devono essere numeri
	_, err1 := strconv.Atoi(parts[0])
	_, err2 := strconv.Atoi(parts[1])
	return err1 == nil && err2 == nil
}

// isValidByteQuota verifica il formato di una quota in byte.
// Formati validi: bytes (es. "1073741824"), K/M/G/T (es. "512M", "1G")
func isValidByteQuota(quota string) bool {
	if quota == "" {
		return false
	}
	_, err := ParseRAMQuota(quota)
	return err == nil
}

// ParseRAMQuota converte una stringa di quota RAM in bytes.
// Formati supportati: bytes, K, M, G, T (es. "1073741824", "512M", "1G")
func ParseRAMQuota(quota string) (uint64, error) {
	if quota == "" {
		return 0, fmt.Errorf("empty RAM quota")
	}

	// Check for suffix
	suffixes := map[string]uint64{
		"K": 1024,
		"M": 1024 * 1024,
		"G": 1024 * 1024 * 1024,
		"T": 1024 * 1024 * 1024 * 1024,
	}

	for suffix, multiplier := range suffixes {
		if strings.HasSuffix(quota, suffix) {
			numStr := strings.TrimSuffix(quota, suffix)
			val, err := strconv.ParseUint(numStr, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid number: %s", numStr)
			}
			return val * multiplier, nil
		}
	}

	// Plain bytes
	val, err := strconv.ParseUint(quota, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid RAM quota format: %s", quota)
	}
	return val, nil
}

// matchPattern checks if a string matches a regex pattern, using a cache
// to avoid recompiling the same pattern repeatedly.
func (c *Config) matchPattern(pattern, s string) bool {
	// Try to load from cache
	if val, ok := c.regexCache.Load(pattern); ok {
		if re, ok := val.(*regexp.Regexp); ok {
			return re.MatchString(s)
		}
	}

	// Not in cache or invalid type, compile regex
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false // Invalid pattern, no match
	}

	// Store in cache (if another goroutine stored concurrently, LoadOrStore returns existing)
	stored, _ := c.regexCache.LoadOrStore(pattern, re)
	return stored.(*regexp.Regexp).MatchString(s)
}

// IsUserIncluded verifica se un username corrisponde ai pattern della include list
// Se la include list è nil o vuota, tutti gli utenti sono inclusi
func (c *Config) IsUserIncluded(username string) bool {
	// Se la include list non è configurata o è vuota, tutti gli utenti sono inclusi
	if c.UserIncludeList == nil || len(c.UserIncludeList) == 0 {
		return true // No include list = all users included
	}

	// Altrimenti, controlla se lo username corrisponde a uno dei pattern regex
	for _, pattern := range c.UserIncludeList {
		if c.matchPattern(pattern, username) {
			return true // User matches include pattern
		}
	}
	return false // User does not match any include pattern
}

// IsUserExcluded verifica se un username corrisponde ai pattern della exclude list
// Se la exclude list è nil o vuota, nessun utente è escluso (tutti possono essere limitati)
func (c *Config) IsUserExcluded(username string) bool {
	// Se la exclude list non è configurata o è vuota, nessun utente è escluso
	if c.UserExcludeList == nil || len(c.UserExcludeList) == 0 {
		return false // No exclude list = no users excluded
	}

	// Altrimenti, controlla se lo username corrisponde a uno dei pattern regex
	for _, pattern := range c.UserExcludeList {
		if c.matchPattern(pattern, username) {
			return true // User matches exclude pattern
		}
	}
	return false
}

// IsUserWhitelisted verifica se un utente può essere limitato
// Un utente può essere limitato se:
// 1. È incluso nella include list (se configurata)
// 2. NON è escluso dalla exclude list
func (c *Config) IsUserWhitelisted(username string) bool {
	return c.IsUserIncluded(username) && !c.IsUserExcluded(username)
}

// IsProcessExcluded verifica se un processo deve essere escluso dai limiti
// I processi nella PROCESS_EXCLUDE_LIST non sono mai limitati (regex support)
func (c *Config) IsProcessExcluded(processName string) bool {
	if c.ProcessExcludeList == nil || len(c.ProcessExcludeList) == 0 {
		return false // No processes excluded
	}
	for _, pattern := range c.ProcessExcludeList {
		if c.matchPattern(pattern, processName) {
			return true
		}
	}
	return false
}

// IsUserIncludedForRAM verifica se un utente è incluso per i limiti RAM (regex support)
func (c *Config) IsUserIncludedForRAM(username string) bool {
	if c.RAMUserIncludeList == nil || len(c.RAMUserIncludeList) == 0 {
		return true
	}
	for _, pattern := range c.RAMUserIncludeList {
		if c.matchPattern(pattern, username) {
			return true
		}
	}
	return false
}

// IsUserExcludedForRAM verifica se un utente è escluso dai limiti RAM (regex support)
func (c *Config) IsUserExcludedForRAM(username string) bool {
	if c.RAMUserExcludeList == nil || len(c.RAMUserExcludeList) == 0 {
		return false
	}
	for _, pattern := range c.RAMUserExcludeList {
		if c.matchPattern(pattern, username) {
			return true
		}
	}
	return false
}

// IsUserWhitelistedForRAM verifica se un utente può essere limitato per RAM
func (c *Config) IsUserWhitelistedForRAM(username string) bool {
	return c.IsUserIncludedForRAM(username) && !c.IsUserExcludedForRAM(username)
}

// IsUserIncludedForIO verifica se l'utente è nella IO include list.
// Se la lista è vuota/nil, tutti sono inclusi.
func (c *Config) IsUserIncludedForIO(username string) bool {
	if c.IOUserIncludeList == nil || len(c.IOUserIncludeList) == 0 {
		return true
	}
	for _, pattern := range c.IOUserIncludeList {
		if c.matchPattern(pattern, username) {
			return true
		}
	}
	return false
}

// IsUserExcludedForIO verifica se l'utente è nella IO exclude list.
// Se la lista è vuota/nil, nessuno è escluso.
func (c *Config) IsUserExcludedForIO(username string) bool {
	if c.IOUserExcludeList == nil || len(c.IOUserExcludeList) == 0 {
		return false
	}
	for _, pattern := range c.IOUserExcludeList {
		if c.matchPattern(pattern, username) {
			return true
		}
	}
	return false
}

// IsUserWhitelistedForIO verifica se un utente può essere limitato per IO
func (c *Config) IsUserWhitelistedForIO(username string) bool {
	return c.IsUserIncludedForIO(username) && !c.IsUserExcludedForIO(username)
}

// SetUserExcludeList imposta la lista di utenti da escludere e salva su file
func (c *Config) SetUserExcludeList(patterns []string, configPath string, reload bool) ([]string, error) {
	// Valida tutti i pattern regex
	for _, pattern := range patterns {
		if _, err := regexp.Compile(pattern); err != nil {
			return nil, fmt.Errorf("invalid regex pattern '%s': %w", pattern, err)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Salva valore precedente
	previousValue := make([]string, len(c.UserExcludeList))
	copy(previousValue, c.UserExcludeList)

	// Aggiorna configurazione in memoria
	c.UserExcludeList = patterns

	// Salva su file
	if err := c.SaveToFile(configPath); err != nil {
		// Ripristina valore precedente se salvataggio fallisce
		c.UserExcludeList = previousValue
		return nil, err
	}

	return previousValue, nil
}

// SetUserIncludeList imposta la lista di pattern include e salva su file
func (c *Config) SetUserIncludeList(patterns []string, configPath string, reload bool) ([]string, error) {
	// Valida tutti i pattern regex
	for _, pattern := range patterns {
		if _, err := regexp.Compile(pattern); err != nil {
			return nil, fmt.Errorf("invalid regex pattern '%s': %w", pattern, err)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Salva valore precedente
	previousValue := make([]string, len(c.UserIncludeList))
	copy(previousValue, c.UserIncludeList)

	// Aggiorna configurazione in memoria
	c.UserIncludeList = patterns

	// Salva su file
	if err := c.SaveToFile(configPath); err != nil {
		// Ripristina valore precedente se salvataggio fallisce
		c.UserIncludeList = previousValue
		return nil, err
	}

	return previousValue, nil
}

// SaveToFile salva la configurazione su file, creando backup automatico
func (c *Config) SaveToFile(path string) error {
	// 1. Crea backup del file esistente
	if _, err := os.Stat(path); err == nil {
		timestamp := time.Now().Format("20060102_150405")
		backupPath := fmt.Sprintf("%s.backup_%s", path, timestamp)

		// Leggi contenuto originale
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read config file for backup: %w", err)
		}

		// Scrivi backup
		if err := os.WriteFile(backupPath, content, 0644); err != nil {
			return fmt.Errorf("failed to create backup: %w", err)
		}
	}

	// 2. Leggi il file esistente e aggiorna le righe
	lines, err := c.updateConfigLines(path)
	if err != nil {
		return err
	}

	// 3. Scrivi su file temporaneo
	tmpPath := path + ".tmp"
	content := strings.Join(lines, "\n")
	if err := os.WriteFile(tmpPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write temp config file: %w", err)
	}

	// 4. Rinomina atomico
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // Cleanup se rename fallisce
		return fmt.Errorf("failed to rename config file: %w", err)
	}

	return nil
}

// updateConfigLines legge e aggiorna le righe della configurazione
func (c *Config) updateConfigLines(path string) ([]string, error) {
	// Leggi file esistente
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// File non esiste, crea nuovo
			return c.generateConfigLines(), nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	lines := strings.Split(string(content), "\n")
	updated := make([]string, 0, len(lines))

	includeListWritten := false
	excludeListWritten := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Salta commenti e righe vuote
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			updated = append(updated, line)
			continue
		}

		// Controlla se è USER_INCLUDE_LIST o USER_EXCLUDE_LIST
		if strings.HasPrefix(trimmed, "USER_INCLUDE_LIST=") {
			value := strings.Join(c.UserIncludeList, ",")
			updated = append(updated, fmt.Sprintf("USER_INCLUDE_LIST=%s", value))
			includeListWritten = true
			continue
		}

		if strings.HasPrefix(trimmed, "USER_EXCLUDE_LIST=") {
			value := strings.Join(c.UserExcludeList, ",")
			updated = append(updated, fmt.Sprintf("USER_EXCLUDE_LIST=%s", value))
			excludeListWritten = true
			continue
		}

		// Altre righe lasciate invariate
		updated = append(updated, line)
	}

	// Aggiungi righe mancanti
	if !includeListWritten {
		value := strings.Join(c.UserIncludeList, ",")
		updated = append(updated, fmt.Sprintf("USER_INCLUDE_LIST=%s", value))
	}
	if !excludeListWritten {
		value := strings.Join(c.UserExcludeList, ",")
		updated = append(updated, fmt.Sprintf("USER_EXCLUDE_LIST=%s", value))
	}

	return updated, nil
}

// generateConfigLines genera linee di configurazione di base
func (c *Config) generateConfigLines() []string {
	includeList := ""
	if len(c.UserIncludeList) > 0 {
		includeList = strings.Join(c.UserIncludeList, ",")
	}
	excludeList := ""
	if len(c.UserExcludeList) > 0 {
		excludeList = strings.Join(c.UserExcludeList, ",")
	}

	return []string{
		"# CPU Manager Configuration",
		fmt.Sprintf("USER_INCLUDE_LIST=%s", includeList),
		fmt.Sprintf("USER_EXCLUDE_LIST=%s", excludeList),
		"",
	}
}

// ParseTimeframe parsea una stringa nel formato "1-5 08-18" o multipli "1-5 08-18;0,6 00-23"
func ParseTimeframe(spec string) ([]Timeframe, error) {
	var timeframes []Timeframe

	// Supporta multipli timeframe separati da ;
	specs := strings.Split(spec, ";")

	for _, s := range specs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}

		parts := strings.Fields(s)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid timeframe format: %s (expected: days hours)", s)
		}

		// Parse giorni
		days, err := parseDays(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid days spec '%s': %w", parts[0], err)
		}

		// Parse ore
		hourStart, hourEnd, err := parseHours(parts[1])
		if err != nil {
			return nil, fmt.Errorf("invalid hours spec '%s': %w", parts[1], err)
		}

		timeframes = append(timeframes, Timeframe{
			DaysOfWeek: days,
			HourStart:  hourStart,
			HourEnd:    hourEnd,
		})
	}

	if len(timeframes) == 0 {
		return nil, nil
	}

	return timeframes, nil
}

// parseDays gestisce formati: 1-5, 0,6, *, 1
func parseDays(spec string) ([]int, error) {
	if spec == "*" {
		return []int{0, 1, 2, 3, 4, 5, 6}, nil
	}

	var days []int
	parts := strings.Split(spec, ",")

	for _, part := range parts {
		if strings.Contains(part, "-") {
			// Range: 1-5
			rangeParts := strings.Split(part, "-")
			if len(rangeParts) != 2 {
				return nil, fmt.Errorf("invalid day range: %s", part)
			}
			start, err := strconv.Atoi(rangeParts[0])
			if err != nil {
				return nil, err
			}
			end, err := strconv.Atoi(rangeParts[1])
			if err != nil {
				return nil, err
			}

			if start < 0 || start > 6 || end < 0 || end > 6 {
				return nil, fmt.Errorf("days must be 0-6 (0=Sunday)")
			}

			for i := start; i <= end; i++ {
				days = append(days, i)
			}
		} else {
			// Singolo: 1
			day, err := strconv.Atoi(part)
			if err != nil {
				return nil, err
			}
			if day < 0 || day > 6 {
				return nil, fmt.Errorf("days must be 0-6 (0=Sunday)")
			}
			days = append(days, day)
		}
	}

	return days, nil
}

// parseHours gestisce formati: 08-18, 00-23
func parseHours(spec string) (int, int, error) {
	parts := strings.Split(spec, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid hour format: %s (expected: start-end)", spec)
	}

	start, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}

	end, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}

	if start < 0 || start > 23 || end < 0 || end > 23 {
		return 0, 0, fmt.Errorf("hours must be 0-23")
	}

	if start >= end {
		return 0, 0, fmt.Errorf("start hour must be before end hour")
	}

	return start, end, nil
}

// IsInBlackout verifica se l'orario corrente è in un blackout timeframe
// Restituisce true se CPU Manager NON deve applicare limiti
func (c *Config) IsInBlackout() bool {
	if len(c.BlackoutTimeframes) == 0 {
		return false
	}

	now := time.Now()
	currentDay := int(now.Weekday()) // 0=Domenica in Go
	currentHour := now.Hour()

	for _, tf := range c.BlackoutTimeframes {
		// Controlla giorno
		dayMatch := false
		for _, day := range tf.DaysOfWeek {
			if day == currentDay {
				dayMatch = true
				break
			}
		}

		if !dayMatch {
			continue
		}

		// Controlla ora
		if currentHour >= tf.HourStart && currentHour < tf.HourEnd {
			return true
		}
	}

	return false
}

// GetNextBlackoutEnd restituisce la prossima fine del blackout (se attivo)
func (c *Config) GetNextBlackoutEnd() *time.Time {
	if !c.IsInBlackout() {
		return nil
	}

	now := time.Now()
	currentDay := int(now.Weekday())
	currentHour := now.Hour()

	for _, tf := range c.BlackoutTimeframes {
		// Controlla se siamo in questo timeframe
		dayMatch := false
		for _, day := range tf.DaysOfWeek {
			if day == currentDay {
				dayMatch = true
				break
			}
		}

		if !dayMatch {
			continue
		}

		if currentHour >= tf.HourStart && currentHour < tf.HourEnd {
			// Siamo in questo blackout, calcola la fine
			end := time.Date(now.Year(), now.Month(), now.Day(), tf.HourEnd, 0, 0, 0, now.Location())
			return &end
		}
	}

	return nil
}

// ============================================================================
// THREAD-SAFE GETTERS
// ============================================================================
// These methods provide thread-safe access to configuration fields.
// Use these instead of direct field access to prevent race conditions
// during configuration reload.

// GetMetricsCacheTTL returns the metrics cache TTL in seconds.
func (c *Config) GetMetricsCacheTTL() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.MetricsCacheTTL
}

// GetSystemUIDMin returns the minimum UID to monitor.
func (c *Config) GetSystemUIDMin() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.SystemUIDMin
}

// GetSystemUIDMax returns the maximum UID to monitor.
func (c *Config) GetSystemUIDMax() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.SystemUIDMax
}

// GetCPUThreshold returns the CPU activation threshold percentage.
func (c *Config) GetCPUThreshold() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.CPUThreshold
}

// GetCPUReleaseThreshold returns the CPU deactivation threshold percentage.
func (c *Config) GetCPUReleaseThreshold() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.CPUReleaseThreshold
}

// GetCPUThresholdDuration returns the threshold duration in seconds.
func (c *Config) GetCPUThresholdDuration() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.CPUThresholdDuration
}

// GetMinActiveTime returns the minimum active time in seconds.
func (c *Config) GetMinActiveTime() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.MinActiveTime
}

// GetPollingInterval returns the polling interval in seconds.
func (c *Config) GetPollingInterval() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PollingInterval
}

// GetCgroupOperationTimeout returns the cgroup operation timeout in seconds.
func (c *Config) GetCgroupOperationTimeout() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.CgroupOperationTimeout
}

// GetCgroupRetryDelayMs returns the cgroup retry delay in milliseconds.
func (c *Config) GetCgroupRetryDelayMs() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.CgroupRetryDelayMs
}

// GetMCPShutdownTimeout returns the MCP shutdown timeout in seconds.
func (c *Config) GetMCPShutdownTimeout() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.MCPShutdownTimeout
}

// GetMinSystemCores returns the minimum system cores to keep available.
func (c *Config) GetMinSystemCores() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.MinSystemCores
}

// GetRAMHighRatio returns the ratio for memory.high (0.0-1.0).
// Default is 0.8 (80% of memory.max). Invalid values are clamped to 0.8.
func (c *Config) GetRAMHighRatio() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.RAMHighRatio <= 0 || c.RAMHighRatio > 1 {
		return 0.8
	}
	return c.RAMHighRatio
}

// GetIOEnabled returns whether IO limits are enabled.
func (c *Config) GetIOEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IOEnabled
}

// GetIOReadBPS returns the read bandwidth limit.
func (c *Config) GetIOReadBPS() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IOReadBPS
}

// GetIOWriteBPS returns the write bandwidth limit.
func (c *Config) GetIOWriteBPS() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IOWriteBPS
}

// GetIOReadIOPS returns the read IOPS limit.
func (c *Config) GetIOReadIOPS() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IOReadIOPS
}

// GetIOWriteIOPS returns the write IOPS limit.
func (c *Config) GetIOWriteIOPS() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IOWriteIOPS
}

// GetIODeviceFilter returns the device filter for IO limits.
func (c *Config) GetIODeviceFilter() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IODeviceFilter
}

// GetIOThresholdDuration returns the IO threshold duration in seconds.
func (c *Config) GetIOThresholdDuration() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IOThresholdDuration
}

// GetIORemediationEnabled returns whether IO starvation remediation is enabled.
func (c *Config) GetIORemediationEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IORemediationEnabled
}

// GetIOStarvationThreshold returns the starvation threshold in seconds.
func (c *Config) GetIOStarvationThreshold() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IOStarvationThreshold
}

// GetIOStarvationCheckInterval returns the check interval in seconds.
func (c *Config) GetIOStarvationCheckInterval() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IOStarvationCheckInterval
}

// GetIOBoostMultiplier returns the boost multiplier.
func (c *Config) GetIOBoostMultiplier() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IOBoostMultiplier
}

// GetIOBoostDuration returns the boost duration in seconds.
func (c *Config) GetIOBoostDuration() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IOBoostDuration
}

// GetIOBoostMaxPerHour returns the max boosts per user per hour.
func (c *Config) GetIOBoostMaxPerHour() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IOBoostMaxPerHour
}

// GetIOPSIThreshold returns the PSI threshold percentage.
func (c *Config) GetIOPSIThreshold() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IOPSIThreshold
}

// GetIORevertOnNormal returns whether to revert limits when IO returns to normal.
func (c *Config) GetIORevertOnNormal() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IORevertOnNormal
}

// GetAutodetectPatterns returns whether workload pattern detection is enabled.
func (c *Config) GetAutodetectPatterns() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AutodetectPatterns
}

// GetPatternHistoryHours returns the pattern history window in hours.
func (c *Config) GetPatternHistoryHours() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PatternHistoryHours
}

// GetPatternMinSamples returns the minimum samples required for pattern detection.
func (c *Config) GetPatternMinSamples() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PatternMinSamples
}

// GetPatternConfidenceThreshold returns the confidence threshold for pattern detection.
func (c *Config) GetPatternConfidenceThreshold() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PatternConfidenceThreshold
}

// GetBatchNightCPUQuota returns the CPU quota for batch night pattern.
func (c *Config) GetBatchNightCPUQuota() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.BatchNightCPUQuota
}

// GetBatchNightRAMQuota returns the RAM quota for batch night pattern.
func (c *Config) GetBatchNightRAMQuota() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.BatchNightRAMQuota
}

// GetInteractiveCPUQuota returns the CPU quota for interactive pattern.
func (c *Config) GetInteractiveCPUQuota() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.InteractiveCPUQuota
}

// GetInteractiveRAMQuota returns the RAM quota for interactive pattern.
func (c *Config) GetInteractiveRAMQuota() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.InteractiveRAMQuota
}

// GetIgnoreSystemLoad returns whether to ignore system load in decisions.
func (c *Config) GetIgnoreSystemLoad() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IgnoreSystemLoad
}

// GetUserIncludeList returns a copy of the user include list.
func (c *Config) GetUserIncludeList() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.UserIncludeList == nil {
		return nil
	}
	copy := make([]string, len(c.UserIncludeList))
	for i, v := range c.UserIncludeList {
		copy[i] = v
	}
	return copy
}

// GetUserExcludeList returns a copy of the user exclude list.
func (c *Config) GetUserExcludeList() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.UserExcludeList == nil {
		return nil
	}
	copy := make([]string, len(c.UserExcludeList))
	for i, v := range c.UserExcludeList {
		copy[i] = v
	}
	return copy
}

// GetProcessExcludeList returns a copy of the process exclude list.
func (c *Config) GetProcessExcludeList() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.ProcessExcludeList == nil {
		return nil
	}
	copy := make([]string, len(c.ProcessExcludeList))
	for i, v := range c.ProcessExcludeList {
		copy[i] = v
	}
	return copy
}

// GetPSIEventDriven returns whether PSI event-driven mode is enabled.
func (c *Config) GetPSIEventDriven() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PSIEventDriven
}

// GetPSICPUStallThreshold returns the CPU stall threshold in microseconds.
func (c *Config) GetPSICPUStallThreshold() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PSICPUStallThreshold
}

// GetPSIOStallThreshold returns the IO stall threshold in microseconds.
func (c *Config) GetPSIOStallThreshold() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PSIOStallThreshold
}

// GetPSIWindowUs returns the PSI tracking window in microseconds.
func (c *Config) GetPSIWindowUs() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PSIWindowUs
}

// GetPSIFallbackInterval returns the fallback polling interval when event-driven.
func (c *Config) GetPSIFallbackInterval() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PSIFallbackInterval
}
