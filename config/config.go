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
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/fdefilippo/resman/internal/operationgate"
)

// Timeframe represents a blackout time interval.
type Timeframe struct {
	DaysOfWeek []int // Days of the week (0-6, where 0 is Sunday)
	HourStart  int   // Ora inizio (0-23)
	HourEnd    int   // Ora fine esclusiva (0-24)
}

// Config contains every public and derived runtime setting.
type Config struct {
	mu sync.RWMutex

	// saveGate is shared across reload epochs and serializes config-file transactions.
	saveGate *operationgate.Gate
	// saveState is protected by saveGate and shared across reload epochs.
	saveState *configPersistenceState

	// Regex cache for pre-compiled patterns (performance optimization)
	regexCache sync.Map // map[string]*regexp.Regexp

	// Paths
	CgroupRoot         string `config:"CGROUP_ROOT"`
	CgroupBase         string `config:"CGROUP_BASE"`
	ConfigFile         string `config:"-"` // Runtime path selected by the --config flag
	LogFile            string `config:"LOG_FILE"`
	CreatedCgroupsFile string `config:"CREATED_CGROUPS_FILE"`

	// Timing
	PollingInterval int `config:"POLLING_INTERVAL"`
	MinActiveTime   int `config:"MIN_ACTIVE_TIME"`
	MetricsCacheTTL int `config:"METRICS_CACHE_TTL"`
	// MetricsRefreshInterval updates Prometheus and Grafana without making decisions.
	MetricsRefreshInterval int `config:"METRICS_REFRESH_INTERVAL"`
	// ProcessMinAgeSeconds prevents newly started processes from distorting the CPU delta.
	ProcessMinAgeSeconds int `config:"PROCESS_MIN_AGE_SECONDS"`

	// Timeouts (seconds)
	CgroupOperationTimeout int `config:"CGROUP_OPERATION_TIMEOUT"` // Timeout for cgroup operations (seconds)
	MCPShutdownTimeout     int `config:"MCP_SHUTDOWN_TIMEOUT"`     // Timeout for MCP server shutdown (seconds)

	// Thresholds (percentages)
	CPUThreshold        int `config:"CPU_THRESHOLD"`
	CPUReleaseThreshold int `config:"CPU_RELEASE_THRESHOLD"`

	// Threshold Time Window (seconds)
	CPUThresholdDuration int `config:"CPU_THRESHOLD_DURATION"` // Seconds to wait before activating limits (0 = immediate)

	// CPU limits (cpu.max format: "quota period")
	CPUQuotaNormal string `config:"CPU_QUOTA_NORMAL"`

	// RAM limits
	RAMEnabled          bool    `config:"RAM_LIMIT_ENABLED"`
	RAMThreshold        int     `config:"RAM_THRESHOLD"`
	RAMReleaseThreshold int     `config:"RAM_RELEASE_THRESHOLD"`
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
	PatternHistoryHours        int     `config:"PATTERN_HISTORY_HOURS"`        // History window in hours
	PatternMinSamples          int     `config:"PATTERN_MIN_SAMPLES"`          // Minimum distinct hourly buckets
	PatternConfidenceThreshold float64 `config:"PATTERN_CONFIDENCE_THRESHOLD"` // Confidence threshold (0.0-1.0)
	// Per-pattern policies
	BatchNightCPUQuota  int    `config:"BATCH_NIGHT_CPU_QUOTA"` // CPU quota per batch (microseconds)
	BatchNightRAMQuota  string `config:"BATCH_NIGHT_RAM_QUOTA"` // RAM quota per batch
	InteractiveCPUQuota int    `config:"INTERACTIVE_CPU_QUOTA"` // CPU quota for interactive workloads
	InteractiveRAMQuota string `config:"INTERACTIVE_RAM_QUOTA"` // RAM quota for interactive workloads

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

	// Blackout timeframes during which ResMan must not apply limits.
	BlackoutTimeframes []Timeframe `config:"-"` // Parsed from BLACKOUT

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
	MCPTLSEnabled    bool   `config:"MCP_TLS_ENABLED"`
	MCPTLSCertFile   string `config:"MCP_TLS_CERT_FILE"`
	MCPTLSKeyFile    string `config:"MCP_TLS_KEY_FILE"`
	MCPTLSCAFile     string `config:"MCP_TLS_CA_FILE"`
	MCPTLSMinVersion string `config:"MCP_TLS_MIN_VERSION"`
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

	// PSI event-driven mode uses poll() on pressure files instead of only a ticker.
	PSIEventDriven       bool `config:"PSI_EVENT_DRIVEN"`        // Enable PSI event-driven control cycles
	PSICPUStallThreshold int  `config:"PSI_CPU_STALL_THRESHOLD"` // CPU stall threshold in microseconds (default 50000 = 5% su window 1s)
	PSIOStallThreshold   int  `config:"PSI_IO_STALL_THRESHOLD"`  // IO stall threshold in microseconds (default 50000)
	PSIWindowUs          int  `config:"PSI_WINDOW_US"`           // PSI tracking window in microseconds (default 1000000 = 1s)
	PSIFallbackInterval  int  `config:"PSI_FALLBACK_INTERVAL"`   // Fallback polling interval in seconds when event-driven (default 300 = 5min)
	PSIBoostWeight       int  `config:"PSI_BOOST_WEIGHT"`        // CPU weight boost on PSI event (default 300, normal weight is 100)
	PSIBoostDuration     int  `config:"PSI_BOOST_DURATION"`      // Seconds before reverting PSI boost (default 120)
}

// IODecisionPolicy is an atomic snapshot of the configuration used to decide
// whether I/O enforcement should be activated, maintained, or released.
type IODecisionPolicy struct {
	Enabled           bool
	Threshold         int
	ReleaseThreshold  int
	ThresholdDuration int
	ReadBPS           string
	WriteBPS          string
	ReadIOPS          int
	WriteIOPS         int
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	// Read pid_max dynamically for the SYSTEM_UID_MAX default.
	pidMax := 60000 // Fallback value.
	if data, err := os.ReadFile("/proc/sys/kernel/pid_max"); err == nil {
		if val, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			pidMax = val
		}
	}

	return &Config{
		saveGate:           &operationgate.Gate{},
		saveState:          &configPersistenceState{},
		CgroupRoot:         "/sys/fs/cgroup",
		CgroupBase:         "resman",
		ConfigFile:         DefaultConfigPath,
		LogFile:            "/var/log/resman.log",
		CreatedCgroupsFile: DefaultCreatedCgroupsPath,

		PollingInterval: 30,
		MinActiveTime:   60,
		MetricsCacheTTL: 15,
		// Refresh Prometheus/Grafana independently from the decision control loop.
		MetricsRefreshInterval: 30,
		// Younger processes do not contribute to user CPU usage.
		ProcessMinAgeSeconds: 60,

		// Timeout defaults
		CgroupOperationTimeout: 5,  // 5 seconds for cgroup operations
		MCPShutdownTimeout:     10, // 10 seconds for MCP shutdown

		CPUThreshold:         75,
		CPUReleaseThreshold:  40,
		CPUThresholdDuration: 90, // Default: wait 90 seconds before activating limits

		CPUQuotaNormal: "max 100000",

		RAMEnabled:          false,
		RAMThreshold:        75,
		RAMReleaseThreshold: 40,
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
		PatternMinSamples:          24,  // 24 distinct hourly buckets minimum
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

		LogLevel:   "INFO",
		LogMaxSize: 10 * 1024 * 1024, // 10MB
		UseSyslog:  false,

		MinSystemCores:   1,
		SystemUIDMin:     1000,
		SystemUIDMax:     pidMax,
		IgnoreSystemLoad: false,
		ServerRole:       "",  // Empty by default
		UserIncludeList:  nil, // nil = no users eligible for CPU limits
		UserExcludeList:  nil, // nil = no additional users excluded
		ProcessExcludeList: []string{ // Default processes to never limit (regex patterns)
			"^systemd$", "^dbus-daemon$", "^dbus-broker$", "^polkitd$",
		},
		BlackoutSpec:       "", // Empty = no blackout (always active)
		BlackoutTimeframes: nil,

		// MCP Server
		MCPEnabled:       false,
		MCPTransport:     "stdio",
		MCPHTTPPort:      1969,
		MCPHTTPHost:      "127.0.0.1",
		MCPTLSEnabled:    true,
		MCPTLSCertFile:   "/etc/resman/tls/server.crt",
		MCPTLSKeyFile:    "/etc/resman/tls/server.key",
		MCPTLSCAFile:     "",
		MCPTLSMinVersion: "1.3",
		MCPLogLevel:      "INFO",
		MCPAuthToken:     "",
		MCPAllowWriteOps: false,

		// Metrics Database (SQLite)
		MetricsDBEnabled:       false,
		MetricsDBPath:          DefaultMetricsDBPath,
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
		PSIBoostWeight:       300,
		PSIBoostDuration:     120,
	}
}

// LoadAndValidate loads file and environment overrides over the defaults, then
// validates the resulting configuration.
func LoadAndValidate(configPath string) (*Config, error) {
	return loadAndValidateWithLayout(configPath, defaultDiskLayout)
}

func loadAndValidateWithLayout(configPath string, layout diskLayout) (*Config, error) {
	cfg := DefaultConfig()
	resolvedConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("resolving config file path %s: %w", configPath, err)
	}
	resolvedConfigPath = filepath.Clean(resolvedConfigPath)
	if err := rejectLegacyConfigAtDefault(resolvedConfigPath, layout); err != nil {
		return nil, err
	}

	// 1. Load the configuration file when it exists.
	if err := loadFromFile(resolvedConfigPath, cfg); err != nil {
		return nil, fmt.Errorf("loading config file %s: %w", resolvedConfigPath, err)
	}

	// 2. Apply environment overrides.
	if err := loadFromEnvironment(cfg); err != nil {
		return nil, fmt.Errorf("loading environment overrides: %w", err)
	}

	// The command-line path is authoritative for reloads and MCP writes.
	cfg.ConfigFile = resolvedConfigPath

	// 3. Validate the complete result.
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}
	if err := rejectLegacyMetricsDBAtDefault(cfg, layout); err != nil {
		return nil, err
	}

	// Warn when CPU limiting is disabled by an empty include list.
	if len(cfg.GetUserIncludeList()) == 0 {
		fmt.Fprintf(os.Stderr, "WARNING: USER_INCLUDE_LIST is empty - no users will be CPU limited. "+
			"Set USER_INCLUDE_LIST=.* to make all non-excluded users CPU-eligible, "+
			"or specify patterns (e.g., USER_INCLUDE_LIST=^www.*,^app.*).\n")
	}

	return cfg, nil
}

// loadFromFile reads a key=value configuration file.
func loadFromFile(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// A missing file is allowed; defaults and environment still apply.
			return nil
		}
		return err
	}

	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		// Skip comments and blank lines.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("malformed config line %d: %s", i+1, line)
		}

		key := strings.TrimSpace(parts[0])
		value := stripInlineComment(parts[1])

		// Remove surrounding quotes and whitespace.
		value = strings.TrimSpace(value)
		value = strings.TrimSpace(strings.Trim(value, `"'`))

		if err := setConfigField(cfg, key, value); err != nil {
			return fmt.Errorf("configuration file %s line %d key %s: %w", path, i+1, key, err)
		}
	}
	return nil
}

func stripInlineComment(value string) string {
	var quote rune
	escaped := false
	previousWhitespace := false

	for index, current := range value {
		if escaped {
			escaped = false
			previousWhitespace = unicode.IsSpace(current)
			continue
		}
		if quote != 0 {
			switch current {
			case '\\':
				escaped = true
			case quote:
				quote = 0
			}
			previousWhitespace = unicode.IsSpace(current)
			continue
		}
		if current == '"' || current == '\'' {
			quote = current
			previousWhitespace = false
			continue
		}
		if current == '#' && previousWhitespace {
			return value[:index]
		}
		previousWhitespace = unicode.IsSpace(current)
	}

	return value
}

// loadFromEnvironment applies supported environment overrides and explicitly
// rejects environment variables for removed public keys.
func loadFromEnvironment(cfg *Config) error {
	cfgType := reflect.TypeOf(cfg).Elem()
	var errs []error

	removedKeys := make([]string, 0, len(removedConfigKeys))
	for key := range removedConfigKeys {
		removedKeys = append(removedKeys, key)
	}
	sort.Strings(removedKeys)
	for _, key := range removedKeys {
		if value, ok := os.LookupEnv(key); ok {
			errs = append(errs, fmt.Errorf("%s=%q: %w", key, value, removedConfigKeyError(key)))
		}
	}

	for i := 0; i < cfgType.NumField(); i++ {
		field := cfgType.Field(i)

		envKey := field.Tag.Get("config")
		if envKey == "" || envKey == "-" {
			continue
		}

		envValue, ok := os.LookupEnv(envKey)
		if !ok {
			continue
		}

		if err := setConfigField(cfg, envKey, envValue); err != nil {
			errs = append(errs, fmt.Errorf("%s=%q: %w", envKey, envValue, err))
		}
	}

	return errors.Join(errs...)
}

// setConfigField applies one public key through its validated handler.
func setConfigField(cfg *Config, key, value string) error {
	handler, ok := configFieldHandlers[key]
	if !ok {
		if _, removed := removedConfigKeys[key]; removed {
			return removedConfigKeyError(key)
		}
		return fmt.Errorf("unknown configuration key %q", key)
	}
	return handler(cfg, value)
}

var removedConfigKeys = map[string]string{
	"CONFIG_FILE":           "select the active file with the --config command-line option",
	"CPU_QUOTA_LIMITED":     "CPU enforcement is proportional and has no global limited quota",
	"METRICS_CACHE_FILE":    "resman has no file-backed metrics cache",
	"PROMETHEUS_FILE":       "resman exports metrics over HTTP and does not write a Prometheus textfile",
	"PROMETHEUS_HOST":       "use PROMETHEUS_METRICS_BIND_HOST",
	"PROMETHEUS_JWT_EXPIRY": "token lifetime is set by the signed exp claim at token issuance",
	"PROMETHEUS_PORT":       "use PROMETHEUS_METRICS_BIND_PORT",
	"RAM_QUOTA_LIMITED":     "RAM enforcement uses RAM_QUOTA_PER_USER",
	"USER_WHITELIST":        "use USER_EXCLUDE_LIST",
}

func removedConfigKeyError(key string) error {
	return fmt.Errorf("configuration key %s was removed: %s", key, removedConfigKeys[key])
}

type configFieldHandler func(*Config, string) error

var configFieldHandlers = map[string]configFieldHandler{
	"CGROUP_ROOT":          setString(func(cfg *Config, value string) { cfg.CgroupRoot = value }),
	"CGROUP_BASE":          setCgroupBase,
	"LOG_FILE":             setString(func(cfg *Config, value string) { cfg.LogFile = value }),
	"CREATED_CGROUPS_FILE": setString(func(cfg *Config, value string) { cfg.CreatedCgroupsFile = value }),
	"POLLING_INTERVAL":     setInt(func(cfg *Config, value int) { cfg.PollingInterval = value }),
	"MIN_ACTIVE_TIME":      setInt(func(cfg *Config, value int) { cfg.MinActiveTime = value }),
	"METRICS_CACHE_TTL":    setInt(func(cfg *Config, value int) { cfg.MetricsCacheTTL = value }),
	"METRICS_REFRESH_INTERVAL": setPositiveInt(func(cfg *Config, value int) {
		cfg.MetricsRefreshInterval = value
	}),
	"PROCESS_MIN_AGE_SECONDS": setInt(func(cfg *Config, value int) {
		cfg.ProcessMinAgeSeconds = value
	}),
	"CPU_THRESHOLD":          setInt(func(cfg *Config, value int) { cfg.CPUThreshold = value }),
	"CPU_RELEASE_THRESHOLD":  setInt(func(cfg *Config, value int) { cfg.CPUReleaseThreshold = value }),
	"CPU_THRESHOLD_DURATION": setInt(func(cfg *Config, value int) { cfg.CPUThresholdDuration = value }),
	"CPU_QUOTA_NORMAL":       setString(func(cfg *Config, value string) { cfg.CPUQuotaNormal = value }),
	"LIMIT_HOOK_ENABLED":     setBool(func(cfg *Config, value bool) { cfg.LimitHookEnabled = value }),
	"LIMIT_HOOK_SCRIPT":      setString(func(cfg *Config, value string) { cfg.LimitHookScript = value }),
	"LIMIT_HOOK_URL":         setString(func(cfg *Config, value string) { cfg.LimitHookURL = value }),
	"LIMIT_HOOK_TIMEOUT":     setInt(func(cfg *Config, value int) { cfg.LimitHookTimeout = value }),
	"ENABLE_PROMETHEUS":      setBool(func(cfg *Config, value bool) { cfg.EnablePrometheus = value }),
	"PROMETHEUS_METRICS_BIND_HOST": setString(func(cfg *Config, value string) {
		cfg.PrometheusMetricsBindHost = value
	}),
	"PROMETHEUS_METRICS_BIND_PORT": setPort("PROMETHEUS_METRICS_BIND_PORT", func(cfg *Config, value int) {
		cfg.PrometheusMetricsBindPort = value
	}),
	"PROMETHEUS_TLS_ENABLED":   setBool(func(cfg *Config, value bool) { cfg.PrometheusTLSEnabled = value }),
	"PROMETHEUS_TLS_CERT_FILE": setString(func(cfg *Config, value string) { cfg.PrometheusTLSCertFile = value }),
	"PROMETHEUS_TLS_KEY_FILE":  setString(func(cfg *Config, value string) { cfg.PrometheusTLSKeyFile = value }),
	"PROMETHEUS_TLS_CA_FILE":   setString(func(cfg *Config, value string) { cfg.PrometheusTLSCAFile = value }),
	"PROMETHEUS_TLS_MIN_VERSION": setStringTransform(strings.ToUpper, func(cfg *Config, value string) {
		cfg.PrometheusTLSMinVersion = value
	}),
	"PROMETHEUS_AUTH_TYPE": setStringTransform(strings.ToLower, func(cfg *Config, value string) {
		cfg.PrometheusAuthType = value
	}),
	"PROMETHEUS_AUTH_USERNAME":      setString(func(cfg *Config, value string) { cfg.PrometheusAuthUsername = value }),
	"PROMETHEUS_AUTH_PASSWORD_FILE": setString(func(cfg *Config, value string) { cfg.PrometheusAuthPasswordFile = value }),
	"PROMETHEUS_JWT_SECRET_FILE":    setString(func(cfg *Config, value string) { cfg.PrometheusJWTSecretFile = value }),
	"PROMETHEUS_JWT_ISSUER":         setString(func(cfg *Config, value string) { cfg.PrometheusJWTIssuer = value }),
	"PROMETHEUS_JWT_AUDIENCE":       setString(func(cfg *Config, value string) { cfg.PrometheusJWTAudience = value }),
	"LOG_LEVEL":                     setStringTransform(strings.ToUpper, func(cfg *Config, value string) { cfg.LogLevel = value }),
	"LOG_MAX_SIZE":                  setInt(func(cfg *Config, value int) { cfg.LogMaxSize = value }),
	"USE_SYSLOG":                    setBool(func(cfg *Config, value bool) { cfg.UseSyslog = value }),
	"MIN_SYSTEM_CORES":              setInt(func(cfg *Config, value int) { cfg.MinSystemCores = value }),
	"SYSTEM_UID_MIN":                setInt(func(cfg *Config, value int) { cfg.SystemUIDMin = value }),
	"SYSTEM_UID_MAX":                setInt(func(cfg *Config, value int) { cfg.SystemUIDMax = value }),
	"USER_INCLUDE_LIST":             setRegexList("", func(cfg *Config, value []string) { cfg.UserIncludeList = value }),
	"USER_EXCLUDE_LIST":             setRegexList("", func(cfg *Config, value []string) { cfg.UserExcludeList = value }),
	"PROCESS_EXCLUDE_LIST":          setRegexList(" in PROCESS_EXCLUDE_LIST", func(cfg *Config, value []string) { cfg.ProcessExcludeList = value }),
	"BLACKOUT":                      setBlackout,
	"IGNORE_SYSTEM_LOAD":            setBool(func(cfg *Config, value bool) { cfg.IgnoreSystemLoad = value }),
	"SERVER_ROLE":                   setString(func(cfg *Config, value string) { cfg.ServerRole = value }),
	"MCP_ENABLED":                   setBool(func(cfg *Config, value bool) { cfg.MCPEnabled = value }),
	"MCP_TRANSPORT":                 setStringTransform(strings.ToLower, func(cfg *Config, value string) { cfg.MCPTransport = value }),
	"MCP_HTTP_PORT":                 setInt(func(cfg *Config, value int) { cfg.MCPHTTPPort = value }),
	"MCP_HTTP_HOST":                 setString(func(cfg *Config, value string) { cfg.MCPHTTPHost = value }),
	"MCP_TLS_ENABLED":               setBool(func(cfg *Config, value bool) { cfg.MCPTLSEnabled = value }),
	"MCP_TLS_CERT_FILE":             setString(func(cfg *Config, value string) { cfg.MCPTLSCertFile = value }),
	"MCP_TLS_KEY_FILE":              setString(func(cfg *Config, value string) { cfg.MCPTLSKeyFile = value }),
	"MCP_TLS_CA_FILE":               setString(func(cfg *Config, value string) { cfg.MCPTLSCAFile = value }),
	"MCP_TLS_MIN_VERSION":           setStringTransform(strings.ToUpper, func(cfg *Config, value string) { cfg.MCPTLSMinVersion = value }),
	"MCP_LOG_LEVEL":                 setStringTransform(strings.ToUpper, func(cfg *Config, value string) { cfg.MCPLogLevel = value }),
	"MCP_AUTH_TOKEN":                setString(func(cfg *Config, value string) { cfg.MCPAuthToken = value }),
	"MCP_ALLOW_WRITE_OPS":           setBool(func(cfg *Config, value bool) { cfg.MCPAllowWriteOps = value }),
	"METRICS_DB_ENABLED":            setBool(func(cfg *Config, value bool) { cfg.MetricsDBEnabled = value }),
	"METRICS_DB_PATH":               setString(func(cfg *Config, value string) { cfg.MetricsDBPath = value }),
	"METRICS_DB_RETENTION_DAYS":     setPositiveInt(func(cfg *Config, value int) { cfg.MetricsDBRetentionDays = value }),
	"METRICS_DB_WRITE_INTERVAL":     setPositiveInt(func(cfg *Config, value int) { cfg.MetricsDBWriteInterval = value }),
	"USERNAME_CACHE_TTL":            setPositiveInt(func(cfg *Config, value int) { cfg.UsernameCacheTTL = value }),
	"CGROUP_OPERATION_TIMEOUT":      setInt(func(cfg *Config, value int) { cfg.CgroupOperationTimeout = value }),
	"MCP_SHUTDOWN_TIMEOUT":          setInt(func(cfg *Config, value int) { cfg.MCPShutdownTimeout = value }),
	"RAM_LIMIT_ENABLED":             setBool(func(cfg *Config, value bool) { cfg.RAMEnabled = value }),
	"RAM_THRESHOLD":                 setInt(func(cfg *Config, value int) { cfg.RAMThreshold = value }),
	"RAM_RELEASE_THRESHOLD":         setInt(func(cfg *Config, value int) { cfg.RAMReleaseThreshold = value }),
	"RAM_QUOTA_PER_USER":            setString(func(cfg *Config, value string) { cfg.RAMQuotaPerUser = value }),
	"DISABLE_SWAP":                  setBool(func(cfg *Config, value bool) { cfg.DisableSwap = value }),
	"RAM_HIGH_RATIO":                setFloat(func(cfg *Config, value float64) { cfg.RAMHighRatio = value }),
	"RAM_USER_INCLUDE_LIST":         setRegexList(" in RAM_USER_INCLUDE_LIST", func(cfg *Config, value []string) { cfg.RAMUserIncludeList = value }),
	"RAM_USER_EXCLUDE_LIST":         setRegexList(" in RAM_USER_EXCLUDE_LIST", func(cfg *Config, value []string) { cfg.RAMUserExcludeList = value }),
	"IO_LIMIT_ENABLED":              setBool(func(cfg *Config, value bool) { cfg.IOEnabled = value }),
	"IO_THRESHOLD":                  setInt(func(cfg *Config, value int) { cfg.IOThreshold = value }),
	"IO_RELEASE_THRESHOLD":          setInt(func(cfg *Config, value int) { cfg.IOReleaseThreshold = value }),
	"IO_READ_BPS":                   setString(func(cfg *Config, value string) { cfg.IOReadBPS = value }),
	"IO_WRITE_BPS":                  setString(func(cfg *Config, value string) { cfg.IOWriteBPS = value }),
	"IO_READ_IOPS":                  setInt(func(cfg *Config, value int) { cfg.IOReadIOPS = value }),
	"IO_WRITE_IOPS":                 setInt(func(cfg *Config, value int) { cfg.IOWriteIOPS = value }),
	"IO_DEVICE_FILTER":              setString(func(cfg *Config, value string) { cfg.IODeviceFilter = value }),
	"IO_THRESHOLD_DURATION":         setInt(func(cfg *Config, value int) { cfg.IOThresholdDuration = value }),
	"IO_USER_INCLUDE_LIST":          setRegexList(" in IO_USER_INCLUDE_LIST", func(cfg *Config, value []string) { cfg.IOUserIncludeList = value }),
	"IO_USER_EXCLUDE_LIST":          setRegexList(" in IO_USER_EXCLUDE_LIST", func(cfg *Config, value []string) { cfg.IOUserExcludeList = value }),
	"IO_REMEDIATION_ENABLED":        setBool(func(cfg *Config, value bool) { cfg.IORemediationEnabled = value }),
	"IO_STARVATION_THRESHOLD":       setInt(func(cfg *Config, value int) { cfg.IOStarvationThreshold = value }),
	"IO_STARVATION_CHECK_INTERVAL":  setInt(func(cfg *Config, value int) { cfg.IOStarvationCheckInterval = value }),
	"IO_BOOST_MULTIPLIER":           setFloat(func(cfg *Config, value float64) { cfg.IOBoostMultiplier = value }),
	"IO_BOOST_DURATION":             setInt(func(cfg *Config, value int) { cfg.IOBoostDuration = value }),
	"IO_BOOST_MAX_PER_HOUR":         setInt(func(cfg *Config, value int) { cfg.IOBoostMaxPerHour = value }),
	"IO_PSI_THRESHOLD":              setFloat(func(cfg *Config, value float64) { cfg.IOPSIThreshold = value }),
	"IO_REVERT_ON_NORMAL":           setBool(func(cfg *Config, value bool) { cfg.IORevertOnNormal = value }),
	"AUTODETECT_PATTERNS":           setBool(func(cfg *Config, value bool) { cfg.AutodetectPatterns = value }),
	"PATTERN_HISTORY_HOURS":         setPositiveInt(func(cfg *Config, value int) { cfg.PatternHistoryHours = value }),
	"PATTERN_MIN_SAMPLES":           setPositiveInt(func(cfg *Config, value int) { cfg.PatternMinSamples = value }),
	"PATTERN_CONFIDENCE_THRESHOLD":  setFloat(func(cfg *Config, value float64) { cfg.PatternConfidenceThreshold = value }),
	"BATCH_NIGHT_CPU_QUOTA":         setInt(func(cfg *Config, value int) { cfg.BatchNightCPUQuota = value }),
	"BATCH_NIGHT_RAM_QUOTA":         setString(func(cfg *Config, value string) { cfg.BatchNightRAMQuota = value }),
	"INTERACTIVE_CPU_QUOTA":         setInt(func(cfg *Config, value int) { cfg.InteractiveCPUQuota = value }),
	"INTERACTIVE_RAM_QUOTA":         setString(func(cfg *Config, value string) { cfg.InteractiveRAMQuota = value }),
	"PSI_EVENT_DRIVEN":              setBool(func(cfg *Config, value bool) { cfg.PSIEventDriven = value }),
	"PSI_CPU_STALL_THRESHOLD":       setPositiveInt(func(cfg *Config, value int) { cfg.PSICPUStallThreshold = value }),
	"PSI_IO_STALL_THRESHOLD":        setPositiveInt(func(cfg *Config, value int) { cfg.PSIOStallThreshold = value }),
	"PSI_WINDOW_US":                 setPositiveInt(func(cfg *Config, value int) { cfg.PSIWindowUs = value }),
	"PSI_FALLBACK_INTERVAL":         setPositiveInt(func(cfg *Config, value int) { cfg.PSIFallbackInterval = value }),
	"PSI_BOOST_WEIGHT":              setPositiveInt(func(cfg *Config, value int) { cfg.PSIBoostWeight = value }),
	"PSI_BOOST_DURATION":            setPositiveInt(func(cfg *Config, value int) { cfg.PSIBoostDuration = value }),
}

func setString(assign func(*Config, string)) configFieldHandler {
	return func(cfg *Config, value string) error {
		assign(cfg, value)
		return nil
	}
}

func setStringTransform(transform func(string) string, assign func(*Config, string)) configFieldHandler {
	return func(cfg *Config, value string) error {
		assign(cfg, transform(value))
		return nil
	}
}

func setInt(assign func(*Config, int)) configFieldHandler {
	return func(cfg *Config, value string) error {
		i, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer %q: %w", value, err)
		}
		assign(cfg, i)
		return nil
	}
}

func setPositiveInt(assign func(*Config, int)) configFieldHandler {
	return func(cfg *Config, value string) error {
		i, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid positive integer %q: %w", value, err)
		}
		if i <= 0 {
			return fmt.Errorf("value must be greater than 0, got %d", i)
		}
		assign(cfg, i)
		return nil
	}
}

func setFloat(assign func(*Config, float64)) configFieldHandler {
	return func(cfg *Config, value string) error {
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("invalid floating-point value %q: %w", value, err)
		}
		assign(cfg, f)
		return nil
	}
}

func setBool(assign func(*Config, bool)) configFieldHandler {
	return func(cfg *Config, value string) error {
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			assign(cfg, true)
		case "false", "0", "no", "off":
			assign(cfg, false)
		default:
			return fmt.Errorf("invalid boolean %q (expected true, false, 1, 0, yes, no, on, or off)", value)
		}
		return nil
	}
}

func setPort(key string, assign func(*Config, int)) configFieldHandler {
	return func(cfg *Config, value string) error {
		i, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid %s %q: %w", key, value, err)
		}
		if i < 1 || i > 65535 {
			return fmt.Errorf("invalid %s: %d (must be 1-65535)", key, i)
		}
		assign(cfg, i)
		return nil
	}
}

func setCgroupBase(cfg *Config, value string) error {
	if strings.Contains(value, "..") || strings.HasPrefix(value, "/") {
		return fmt.Errorf("invalid CGROUP_BASE: must be a relative path without '..'")
	}
	cfg.CgroupBase = value
	return nil
}

func setBlackout(cfg *Config, value string) error {
	cfg.BlackoutSpec = strings.TrimSpace(value)
	if cfg.BlackoutSpec == "" {
		cfg.BlackoutTimeframes = nil
		return nil
	}
	timeframes, err := ParseTimeframe(cfg.BlackoutSpec)
	if err != nil {
		return fmt.Errorf("invalid blackout specification '%s': %w", cfg.BlackoutSpec, err)
	}
	cfg.BlackoutTimeframes = timeframes
	return nil
}

func setRegexList(errorContext string, assign func(*Config, []string)) configFieldHandler {
	return func(cfg *Config, value string) error {
		patterns, err := parseRegexList(value, errorContext)
		if err != nil {
			return err
		}
		assign(cfg, patterns)
		return nil
	}
}

func parseRegexList(value, errorContext string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}

	rawPatterns := strings.Split(value, ",")
	patterns := make([]string, 0, len(rawPatterns))
	for _, pattern := range rawPatterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return nil, fmt.Errorf("invalid regex pattern '%s'%s: %w", pattern, errorContext, err)
		}
		patterns = append(patterns, pattern)
	}
	if len(patterns) == 0 {
		return nil, nil
	}
	return patterns, nil
}

// validateConfig validates the complete runtime configuration.
func validateConfig(cfg *Config) error {
	var errors []string

	if err := setBlackout(cfg, cfg.BlackoutSpec); err != nil {
		errors = append(errors, err.Error())
	}

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
	if cfg.MetricsCacheTTL < 1 {
		errors = append(errors, "METRICS_CACHE_TTL must be at least 1 second")
	}
	if cfg.MetricsRefreshInterval < 5 {
		errors = append(errors, "METRICS_REFRESH_INTERVAL must be at least 5 seconds")
	}
	if cfg.ProcessMinAgeSeconds < 0 {
		errors = append(errors, "PROCESS_MIN_AGE_SECONDS cannot be negative")
	}
	if cfg.LogMaxSize <= 0 {
		errors = append(errors, "LOG_MAX_SIZE must be greater than 0")
	}
	if cfg.CgroupOperationTimeout <= 0 {
		errors = append(errors, "CGROUP_OPERATION_TIMEOUT must be greater than 0")
	}
	if cfg.MCPShutdownTimeout <= 0 {
		errors = append(errors, "MCP_SHUTDOWN_TIMEOUT must be greater than 0")
	}
	if cfg.PrometheusTLSMinVersion == "" {
		if cfg.PrometheusTLSEnabled {
			errors = append(errors, "PROMETHEUS_TLS_MIN_VERSION is required when Prometheus TLS is enabled")
		}
	} else if !isValidTLSVersion(cfg.PrometheusTLSMinVersion) {
		errors = append(errors, "PROMETHEUS_TLS_MIN_VERSION must be one of: 1.0, 1.1, 1.2, 1.3")
	}

	// Validate PSI event-driven configuration
	if cfg.PSICPUStallThreshold < 0 || (cfg.PSIEventDriven && cfg.PSICPUStallThreshold == 0) {
		errors = append(errors, "PSI_CPU_STALL_THRESHOLD must be greater than 0")
	}
	if cfg.PSIOStallThreshold < 0 || (cfg.PSIEventDriven && cfg.PSIOStallThreshold == 0) {
		errors = append(errors, "PSI_IO_STALL_THRESHOLD must be greater than 0")
	}
	if cfg.PSIWindowUs < 0 || (cfg.PSIEventDriven && cfg.PSIWindowUs == 0) {
		errors = append(errors, "PSI_WINDOW_US must be greater than 0")
	}
	if cfg.PSIFallbackInterval < 0 || (cfg.PSIEventDriven && cfg.PSIFallbackInterval == 0) {
		errors = append(errors, "PSI_FALLBACK_INTERVAL must be greater than 0")
	}
	if cfg.PSIBoostWeight < 0 || cfg.PSIBoostWeight > 10000 || (cfg.PSIEventDriven && cfg.PSIBoostWeight == 0) {
		errors = append(errors, "PSI_BOOST_WEIGHT must be between 1 and 10000")
	}
	if cfg.PSIBoostDuration < 0 || (cfg.PSIEventDriven && cfg.PSIBoostDuration == 0) {
		errors = append(errors, "PSI_BOOST_DURATION must be greater than 0")
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

	if err := cfg.MCPServerConfig().Validate(); err != nil {
		errors = append(errors, err.Error())
	}

	// Validate CPU quota format.
	if !isValidCPUQuota(cfg.CPUQuotaNormal) {
		errors = append(errors, "CPU_QUOTA_NORMAL must be 'max period' or 'quota period' with quota >= 1000 and period > 0")
	}

	// Validate RAM limits configuration.
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
		if !isValidByteQuota(cfg.RAMQuotaPerUser) {
			errors = append(errors, "RAM_QUOTA_PER_USER must be a valid byte value (e.g., '536870912', '512M', '1G')")
		}
		if cfg.RAMHighRatio < 0 || cfg.RAMHighRatio > 1 {
			errors = append(errors, "RAM_HIGH_RATIO must be between 0.0 and 1.0 (e.g., 0.8 for 80%, 0 to disable)")
		}
	}
	if !isValidByteQuota(cfg.BatchNightRAMQuota) {
		errors = append(errors, "BATCH_NIGHT_RAM_QUOTA must be a valid byte value (e.g., '8589934592', '8G')")
	}
	if !isValidByteQuota(cfg.InteractiveRAMQuota) {
		errors = append(errors, "INTERACTIVE_RAM_QUOTA must be a valid byte value (e.g., '536870912', '512M')")
	}

	// Validate IO limits
	if !isValidIODeviceFilter(cfg.IODeviceFilter) {
		errors = append(errors, "IO_DEVICE_FILTER must be 'all' or a 'major:minor' device number")
	}
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

func isValidTLSVersion(version string) bool {
	switch strings.TrimSpace(version) {
	case "1.0", "1.1", "1.2", "1.3":
		return true
	default:
		return false
	}
}

func isValidIODeviceFilter(filter string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" || filter == "all" {
		return true
	}

	parts := strings.Split(filter, ":")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		if _, err := strconv.ParseUint(part, 10, 32); err != nil {
			return false
		}
	}
	return true
}

// isValidCPUQuota validates the "quota period" or "max period" format.
func isValidCPUQuota(quota string) bool {
	parts := strings.Fields(quota)
	if len(parts) != 2 {
		return false
	}
	period, err := strconv.Atoi(parts[1])
	if err != nil || period <= 0 {
		return false
	}
	if parts[0] == "max" {
		return true
	}
	numericQuota, err := strconv.Atoi(parts[0])
	return err == nil && numericQuota >= 1000
}

// isValidByteQuota validates a byte quota format.
// Valid formats are bytes (for example, "1073741824") or case-insensitive K/M/G/T suffixes.
func isValidByteQuota(quota string) bool {
	if quota == "" {
		return false
	}
	_, err := ParseByteQuota(quota)
	return err == nil
}

// ParseByteQuota converts a byte quota with an optional case-insensitive K/M/G/T suffix.
func ParseByteQuota(quota string) (uint64, error) {
	if quota == "" {
		return 0, fmt.Errorf("empty byte quota")
	}

	multipliers := map[string]uint64{
		"K": 1024,
		"M": 1024 * 1024,
		"G": 1024 * 1024 * 1024,
		"T": 1024 * 1024 * 1024 * 1024,
	}
	suffix := strings.ToUpper(quota[len(quota)-1:])
	multiplier, hasSuffix := multipliers[suffix]
	if hasSuffix {
		number := quota[:len(quota)-1]
		value, err := strconv.ParseUint(number, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid byte quota number %q: %w", number, err)
		}
		if value > ^uint64(0)/multiplier {
			return 0, fmt.Errorf("byte quota %q overflows uint64", quota)
		}
		return value * multiplier, nil
	}

	// Plain bytes
	val, err := strconv.ParseUint(quota, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte quota format %q: %w", quota, err)
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

// IsUserIncluded reports whether a username matches the CPU include list.
// An empty list disables CPU limiting for every user.
func (c *Config) IsUserIncluded(username string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isUserIncludedLocked(username)
}

func (c *Config) isUserIncludedLocked(username string) bool {
	if len(c.UserIncludeList) == 0 {
		return false
	}

	for _, pattern := range c.UserIncludeList {
		if c.matchPattern(pattern, username) {
			return true
		}
	}
	return false
}

// IsUserExcluded reports whether a username matches the CPU exclude list.
// An empty list does not exclude any user that is otherwise CPU-eligible.
func (c *Config) IsUserExcluded(username string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isUserExcludedLocked(username)
}

func (c *Config) isUserExcludedLocked(username string) bool {
	// An unset or empty exclude list excludes no users.
	if len(c.UserExcludeList) == 0 {
		return false // No exclude list = no users excluded
	}

	// Otherwise, check whether the username matches a configured regular expression.
	for _, pattern := range c.UserExcludeList {
		if c.matchPattern(pattern, username) {
			return true // User matches exclude pattern
		}
	}
	return false
}

// IsUserWhitelisted reports whether a user is eligible for CPU limiting.
// The user must match a non-empty include list and must not match the exclude list.
func (c *Config) IsUserWhitelisted(username string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isUserIncludedLocked(username) && !c.isUserExcludedLocked(username)
}

// UserEligibility describes whether one user may be limited for each resource.
type UserEligibility struct {
	EligibleForCPU bool
	EligibleForRAM bool
	EligibleForIO  bool
}

// EvaluateUserEligibility evaluates every resource policy from one config snapshot.
func (c *Config) EvaluateUserEligibility(username string) UserEligibility {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return UserEligibility{
		EligibleForCPU: c.isUserIncludedLocked(username) && !c.isUserExcludedLocked(username),
		EligibleForRAM: c.isUserIncludedForRAMLocked(username) && !c.isUserExcludedForRAMLocked(username),
		EligibleForIO:  c.isUserIncludedForIOLocked(username) && !c.isUserExcludedForIOLocked(username),
	}
}

// IsProcessExcluded reports whether a canonical process name matches the
// process exclusion policy.
func (c *Config) IsProcessExcluded(processName string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isProcessExcludedLocked(processName)
}

func (c *Config) isProcessExcludedLocked(processName string) bool {
	if len(c.ProcessExcludeList) == 0 {
		return false
	}
	for _, pattern := range c.ProcessExcludeList {
		if c.matchPattern(pattern, processName) {
			return true
		}
	}
	return false
}

// IsUserIncludedForRAM reports whether a user matches the RAM include policy.
func (c *Config) IsUserIncludedForRAM(username string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isUserIncludedForRAMLocked(username)
}

func (c *Config) isUserIncludedForRAMLocked(username string) bool {
	if len(c.RAMUserIncludeList) == 0 {
		return true
	}
	for _, pattern := range c.RAMUserIncludeList {
		if c.matchPattern(pattern, username) {
			return true
		}
	}
	return false
}

// IsUserExcludedForRAM reports whether a user matches the RAM exclude policy.
func (c *Config) IsUserExcludedForRAM(username string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isUserExcludedForRAMLocked(username)
}

func (c *Config) isUserExcludedForRAMLocked(username string) bool {
	if len(c.RAMUserExcludeList) == 0 {
		return false
	}
	for _, pattern := range c.RAMUserExcludeList {
		if c.matchPattern(pattern, username) {
			return true
		}
	}
	return false
}

// IsUserWhitelistedForRAM reports whether a user is eligible for RAM limiting.
func (c *Config) IsUserWhitelistedForRAM(username string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isUserIncludedForRAMLocked(username) && !c.isUserExcludedForRAMLocked(username)
}

// IsUserIncludedForIO reports whether a user matches the I/O include policy.
// An empty include list includes every user.
func (c *Config) IsUserIncludedForIO(username string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isUserIncludedForIOLocked(username)
}

func (c *Config) isUserIncludedForIOLocked(username string) bool {
	if len(c.IOUserIncludeList) == 0 {
		return true
	}
	for _, pattern := range c.IOUserIncludeList {
		if c.matchPattern(pattern, username) {
			return true
		}
	}
	return false
}

// IsUserExcludedForIO reports whether a user matches the I/O exclude policy.
// An empty exclude list excludes no user.
func (c *Config) IsUserExcludedForIO(username string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isUserExcludedForIOLocked(username)
}

func (c *Config) isUserExcludedForIOLocked(username string) bool {
	if len(c.IOUserExcludeList) == 0 {
		return false
	}
	for _, pattern := range c.IOUserExcludeList {
		if c.matchPattern(pattern, username) {
			return true
		}
	}
	return false
}

// IsUserWhitelistedForIO reports whether a user is eligible for I/O limiting.
func (c *Config) IsUserWhitelistedForIO(username string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isUserIncludedForIOLocked(username) && !c.isUserExcludedForIOLocked(username)
}

// PersistUserExcludeList writes a detached exclude-policy snapshot without
// publishing it to live consumers. A watcher acknowledgement owns publication.
func (c *Config) PersistUserExcludeList(patterns []string, configPath string) (UserFilterPersistenceResult, error) {
	return c.persistUserFilterWithWriter(patterns, configPath, userFilterExclude, writeFileAtomically)
}

// PersistUserIncludeList writes a detached include-policy snapshot without
// publishing it to live consumers. A watcher acknowledgement owns publication.
func (c *Config) PersistUserIncludeList(patterns []string, configPath string) (UserFilterPersistenceResult, error) {
	return c.persistUserFilterWithWriter(patterns, configPath, userFilterInclude, writeFileAtomically)
}

type userFilterField uint8

const (
	userFilterInclude userFilterField = iota
	userFilterExclude
)

type userFilterPersistenceSnapshot struct {
	include      []string
	exclude      []string
	writeInclude bool
	writeExclude bool
}

// UserFilterPersistenceResult reports the previous live value and any legacy
// filesystem artifacts removed while persisting the detached filter update.
type UserFilterPersistenceResult struct {
	PreviousValue []string
	PersistenceResult
}

func (c *Config) persistenceCoordinator() (*operationgate.Gate, *configPersistenceState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.saveGate == nil {
		c.saveGate = &operationgate.Gate{}
	}
	if c.saveState == nil {
		c.saveState = &configPersistenceState{}
	}
	return c.saveGate, c.saveState
}

func (c *Config) userFilterSnapshot() userFilterPersistenceSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return userFilterPersistenceSnapshot{
		include: append([]string(nil), c.UserIncludeList...),
		exclude: append([]string(nil), c.UserExcludeList...),
	}
}

func (c *Config) persistUserFilterWithWriter(
	patterns []string,
	configPath string,
	field userFilterField,
	writer atomicFileWriter,
) (UserFilterPersistenceResult, error) {
	if err := validateUserFilterPatterns(patterns); err != nil {
		snapshot := c.userFilterSnapshot()
		switch field {
		case userFilterInclude:
			return UserFilterPersistenceResult{PreviousValue: snapshot.include}, err
		case userFilterExclude:
			return UserFilterPersistenceResult{PreviousValue: snapshot.exclude}, err
		default:
			return UserFilterPersistenceResult{}, fmt.Errorf("unknown user filter field %d: %w", field, err)
		}
	}

	saveGate, saveState := c.persistenceCoordinator()
	leavePersistence := saveGate.Enter()
	defer leavePersistence()

	snapshot := c.userFilterSnapshot()
	var previous []string
	switch field {
	case userFilterInclude:
		previous = append([]string(nil), snapshot.include...)
		snapshot.include = append([]string(nil), patterns...)
		snapshot.writeInclude = true
	case userFilterExclude:
		previous = append([]string(nil), snapshot.exclude...)
		snapshot.exclude = append([]string(nil), patterns...)
		snapshot.writeExclude = true
	default:
		return UserFilterPersistenceResult{}, fmt.Errorf("unknown user filter field %d", field)
	}
	result := UserFilterPersistenceResult{PreviousValue: previous}
	if saveState.unusableErr != nil {
		return result, fmt.Errorf(
			"configuration persistence is unavailable until resman restarts after operator recovery: %w",
			saveState.unusableErr,
		)
	}

	persistenceResult, err := saveUserFilterSnapshotWithWriter(configPath, snapshot, writer)
	result.PersistenceResult = persistenceResult
	if err != nil {
		var unusableErr *configPersistenceUnusableError
		if errors.As(err, &unusableErr) {
			saveState.unusableErr = err
		}
		return result, err
	}
	return result, nil
}

func validateUserFilterPatterns(patterns []string) error {
	for _, pattern := range patterns {
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("invalid regex pattern %q: %w", pattern, err)
		}
	}
	return nil
}

func saveUserFilterSnapshotWithWriter(
	path string,
	snapshot userFilterPersistenceSnapshot,
	writer atomicFileWriter,
) (PersistenceResult, error) {
	return saveUserFilterSnapshot(path, snapshot, writer, removeCommittedFile)
}

func saveUserFilterSnapshot(
	path string,
	snapshot userFilterPersistenceSnapshot,
	writer atomicFileWriter,
	remover committedFileRemover,
) (PersistenceResult, error) {
	result := PersistenceResult{}
	metadata, original, exists, err := readConfigFile(path)
	if err != nil {
		return result, err
	}

	backupPath := path + configBackupSuffix
	if exists {
		if _, err := writer(backupPath, original, metadata); err != nil {
			return result, fmt.Errorf("failed to create secure configuration backup: %w", err)
		}
	}

	result, err = removeLegacyConfigArtifactsBeside(path)
	if err != nil {
		return result, err
	}

	// Preserve comments and unrelated settings from the exact version backed up
	// above, avoiding a second read with different contents or metadata.
	lines := snapshot.updateConfigLines(original, exists)
	content := strings.Join(lines, "\n")
	committed, err := writer(path, []byte(content), metadata)
	if err == nil {
		return result, nil
	}

	writeErr := fmt.Errorf("failed to persist configuration: %w", err)
	if !committed {
		return result, writeErr
	}

	// A parent-directory sync failure happens after rename. Restore the previous
	// readable contents; live configuration publication remains a separate step.
	if exists {
		restored, restoreErr := writer(path, original, metadata)
		if restoreErr != nil {
			if restored {
				return result, errors.Join(writeErr, fmt.Errorf(
					"restored previous readable configuration at %s but failed to confirm rollback durability: %w",
					path,
					restoreErr,
				))
			}
			return result, errors.Join(writeErr, &configPersistenceUnusableError{
				detail:   fmt.Sprintf("failed to restore configuration at %s", path),
				recovery: fmt.Sprintf("stop resman, restore %s, and restart before accepting further configuration writes", backupPath),
				cause:    restoreErr,
			})
		}
	} else {
		removed, removeErr := remover(path)
		if removeErr != nil {
			if removed {
				return result, errors.Join(writeErr, fmt.Errorf(
					"removed newly created readable configuration at %s but failed to confirm removal durability: %w",
					path,
					removeErr,
				))
			}
			return result, errors.Join(writeErr, &configPersistenceUnusableError{
				detail:   fmt.Sprintf("failed to remove newly created configuration at %s", path),
				recovery: fmt.Sprintf("stop resman, remove %s, and restart before accepting further configuration writes", path),
				cause:    removeErr,
			})
		}
	}
	return result, writeErr
}

// updateConfigLines preserves existing content while updating selected managed fields.
func (s userFilterPersistenceSnapshot) updateConfigLines(content []byte, exists bool) []string {
	if !exists {
		return s.generateConfigLines()
	}

	lines := strings.Split(string(content), "\n")
	updated := make([]string, 0, len(lines))

	includeListWritten := false
	excludeListWritten := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Preserve comments and blank lines.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			updated = append(updated, line)
			continue
		}

		// Replace managed filter settings with their current values.
		if strings.HasPrefix(trimmed, "USER_INCLUDE_LIST=") {
			if s.writeInclude {
				updated = append(updated, fmt.Sprintf("USER_INCLUDE_LIST=%s", strings.Join(s.include, ",")))
			} else {
				updated = append(updated, line)
			}
			includeListWritten = true
			continue
		}

		if strings.HasPrefix(trimmed, "USER_EXCLUDE_LIST=") {
			if s.writeExclude {
				updated = append(updated, fmt.Sprintf("USER_EXCLUDE_LIST=%s", strings.Join(s.exclude, ",")))
			} else {
				updated = append(updated, line)
			}
			excludeListWritten = true
			continue
		}

		// Preserve all other settings verbatim.
		updated = append(updated, line)
	}

	// Append managed settings that were absent from the source.
	if !includeListWritten {
		updated = append(updated, fmt.Sprintf("USER_INCLUDE_LIST=%s", strings.Join(s.include, ",")))
	}
	if !excludeListWritten {
		updated = append(updated, fmt.Sprintf("USER_EXCLUDE_LIST=%s", strings.Join(s.exclude, ",")))
	}

	return updated
}

// generateConfigLines creates a minimal configuration for a new file.
func (s userFilterPersistenceSnapshot) generateConfigLines() []string {
	includeList := ""
	if len(s.include) > 0 {
		includeList = strings.Join(s.include, ",")
	}
	excludeList := ""
	if len(s.exclude) > 0 {
		excludeList = strings.Join(s.exclude, ",")
	}

	return []string{
		"# ResMan Configuration",
		fmt.Sprintf("USER_INCLUDE_LIST=%s", includeList),
		fmt.Sprintf("USER_EXCLUDE_LIST=%s", excludeList),
		"",
	}
}

// ParseTimeframe parses a string such as "1-5 08-18" or multiple ranges such as "1-5 08-18;0,6 00-24".
func ParseTimeframe(spec string) ([]Timeframe, error) {
	var timeframes []Timeframe

	// Accept multiple timeframes separated by semicolons.
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

		// Parse days.
		days, err := parseDays(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid days spec '%s': %w", parts[0], err)
		}

		// Parse hours.
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

// parseDays handles formats such as 1-5, 0,6, *, and 1.
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
			if start > end {
				return nil, fmt.Errorf("day range start must not be greater than end: %s", part)
			}

			for i := start; i <= end; i++ {
				days = append(days, i)
			}
		} else {
			// Single day: 1.
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

// parseHours handles formats such as 08-18, 00-24, and 22-06.
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

	if start < 0 || start > 23 || end < 0 || end > 24 {
		return 0, 0, fmt.Errorf("start hour must be 0-23 and end hour must be 0-24")
	}

	if start == end {
		return 0, 0, fmt.Errorf("start and end hour must differ")
	}

	return start, end, nil
}

// IsInBlackout reports whether the current time is within a blackout timeframe.
func (c *Config) IsInBlackout() bool {
	_, active := c.blackoutEndAt(time.Now())
	return active
}

// GetNextBlackoutEnd returns the next blackout end when a blackout is active.
func (c *Config) GetNextBlackoutEnd() *time.Time {
	end, active := c.blackoutEndAt(time.Now())
	if !active {
		return nil
	}
	return &end
}

func (c *Config) blackoutEndAt(now time.Time) (time.Time, bool) {
	currentDay := int(now.Weekday())
	currentHour := now.Hour()

	for _, tf := range c.BlackoutTimeframes {
		if tf.HourStart < tf.HourEnd {
			if timeframeIncludesDay(tf, currentDay) &&
				currentHour >= tf.HourStart && currentHour < tf.HourEnd {
				end := time.Date(now.Year(), now.Month(), now.Day(), tf.HourEnd, 0, 0, 0, now.Location())
				return end, true
			}
			continue
		}

		if timeframeIncludesDay(tf, currentDay) && currentHour >= tf.HourStart {
			end := time.Date(now.Year(), now.Month(), now.Day()+1, tf.HourEnd, 0, 0, 0, now.Location())
			return end, true
		}
		previousDay := (currentDay + 6) % 7
		if timeframeIncludesDay(tf, previousDay) && currentHour < tf.HourEnd {
			end := time.Date(now.Year(), now.Month(), now.Day(), tf.HourEnd, 0, 0, 0, now.Location())
			return end, true
		}
	}

	return time.Time{}, false
}

func timeframeIncludesDay(tf Timeframe, day int) bool {
	for _, configuredDay := range tf.DaysOfWeek {
		if configuredDay == day {
			return true
		}
	}
	return false
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

// GetMetricsRefreshInterval returns the Prometheus/Grafana refresh interval in seconds.
func (c *Config) GetMetricsRefreshInterval() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.MetricsRefreshInterval
}

// GetProcessMinAgeSeconds returns the minimum age for lifetime-average CPU metrics.
func (c *Config) GetProcessMinAgeSeconds() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ProcessMinAgeSeconds
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

// GetIODecisionPolicy returns a consistent snapshot of every I/O decision knob.
func (c *Config) GetIODecisionPolicy() IODecisionPolicy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return IODecisionPolicy{
		Enabled:           c.IOEnabled,
		Threshold:         c.IOThreshold,
		ReleaseThreshold:  c.IOReleaseThreshold,
		ThresholdDuration: c.IOThresholdDuration,
		ReadBPS:           c.IOReadBPS,
		WriteBPS:          c.IOWriteBPS,
		ReadIOPS:          c.IOReadIOPS,
		WriteIOPS:         c.IOWriteIOPS,
	}
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

// GetPSIBoostWeight returns the CPU weight boost on PSI event.
func (c *Config) GetPSIBoostWeight() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PSIBoostWeight
}

// GetPSIBoostDuration returns the seconds before reverting a PSI boost.
func (c *Config) GetPSIBoostDuration() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.PSIBoostDuration
}
