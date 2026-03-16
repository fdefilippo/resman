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
    "os"
    "regexp"
    "strconv"
    "strings"
    "reflect"
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
    // Paths
    CgroupRoot         string `config:"CGROUP_ROOT"`
    ScriptCgroupBase   string `config:"SCRIPT_CGROUP_BASE"`
    ConfigFile         string `config:"CONFIG_FILE"` // Ricorsivo, usato all'avvio
    LogFile            string `config:"LOG_FILE"`
    CreatedCgroupsFile string `config:"CREATED_CGROUPS_FILE"`
    MetricsCacheFile   string `config:"METRICS_CACHE_FILE"`
    PrometheusFile     string `config:"PROMETHEUS_FILE"`

    // Timing
    PollingInterval  int `config:"POLLING_INTERVAL"`
    MinActiveTime    int `config:"MIN_ACTIVE_TIME"`
    MetricsCacheTTL  int `config:"METRICS_CACHE_TTL"`

    // Thresholds (percentages)
    CPUThreshold       int `config:"CPU_THRESHOLD"`
    CPUReleaseThreshold int `config:"CPU_RELEASE_THRESHOLD"`

    // CPU limits (cpu.max format: "quota period")
    CPUQuotaNormal   string `config:"CPU_QUOTA_NORMAL"`
    CPUQuotaLimited  string `config:"CPU_QUOTA_LIMITED"`

    // Prometheus
    EnablePrometheus        bool   `config:"ENABLE_PROMETHEUS"`
    PrometheusMetricsBindHost string `config:"PROMETHEUS_METRICS_BIND_HOST"`
    PrometheusMetricsBindPort int    `config:"PROMETHEUS_METRICS_BIND_PORT"`

    // Prometheus TLS/HTTPS (optional)
    PrometheusTLSEnabled     bool   `config:"PROMETHEUS_TLS_ENABLED"`
    PrometheusTLSCertFile    string `config:"PROMETHEUS_TLS_CERT_FILE"`
    PrometheusTLSKeyFile     string `config:"PROMETHEUS_TLS_KEY_FILE"`
    PrometheusTLSCAFile      string `config:"PROMETHEUS_TLS_CA_FILE"`
    PrometheusTLSMinVersion  string `config:"PROMETHEUS_TLS_MIN_VERSION"`  // 1.0, 1.1, 1.2, 1.3

    // Prometheus Authentication
    PrometheusAuthType     string `config:"PROMETHEUS_AUTH_TYPE"`     // none, basic, jwt, both
    PrometheusAuthUsername string `config:"PROMETHEUS_AUTH_USERNAME"`
    PrometheusAuthPasswordFile string `config:"PROMETHEUS_AUTH_PASSWORD_FILE"`
    PrometheusJWTSecretFile    string `config:"PROMETHEUS_JWT_SECRET_FILE"`
    PrometheusJWTIssuer        string `config:"PROMETHEUS_JWT_ISSUER"`
    PrometheusJWTAudience      string `config:"PROMETHEUS_JWT_AUDIENCE"`
    PrometheusJWTExpiry        int    `config:"PROMETHEUS_JWT_EXPIRY"`  // seconds

    // Logging
    LogLevel   string `config:"LOG_LEVEL"`
    LogMaxSize int    `config:"LOG_MAX_SIZE"` // in bytes
    UseSyslog  bool   `config:"USE_SYSLOG"`

    // System
    MinSystemCores int `config:"MIN_SYSTEM_CORES"`
    SystemUIDMin   int `config:"SYSTEM_UID_MIN"`
    SystemUIDMax   int `config:"SYSTEM_UID_MAX"`

    // User Include List (users to INCLUDE in monitoring, regex support)
    UserIncludeList []string `config:"USER_INCLUDE_LIST"`  // Comma-separated regex patterns

    // User Exclude List (users to EXCLUDE from limits, regex support)
    UserExcludeList []string `config:"USER_EXCLUDE_LIST"`  // Comma-separated regex patterns

    // Blackout Timeframes (when CPU Manager should NOT apply limits)
    BlackoutTimeframes []Timeframe `config:"-"`  // Parsed from BLACKOUT_SPEC

    // Blackout specification string (crontab-like format)
    BlackoutSpec string `config:"CPU_MANAGER_BLACKOUT"`  // e.g., "1-5 08-18;0,6 00-23"

    // Load checking
    IgnoreSystemLoad bool `config:"IGNORE_SYSTEM_LOAD"`

    // Server Role
    ServerRole string `config:"SERVER_ROLE"`  // e.g., database, web-frontend, batch, application, etc.

    // MCP Server
    MCPEnabled       bool   `config:"MCP_ENABLED"`
    MCPTransport     string `config:"MCP_TRANSPORT"`
    MCPHTTPPort      int    `config:"MCP_HTTP_PORT"`
    MCPHTTPHost      string `config:"MCP_HTTP_HOST"`
    MCPLogLevel      string `config:"MCP_LOG_LEVEL"`
    MCPAllowWriteOps bool   `config:"MCP_ALLOW_WRITE_OPS"`
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
        ScriptCgroupBase:   "cpu_manager",
        ConfigFile:         "/etc/cpu-manager.conf",
        LogFile:            "/var/log/cpu-manager.log",
        CreatedCgroupsFile: "/var/run/cpu-manager-cgroups.txt",
        MetricsCacheFile:   "/var/run/cpu-manager-metrics.cache",
        PrometheusFile:     "/var/run/cpu-manager-metrics.prom",

        PollingInterval:  30,
        MinActiveTime:    60,
        MetricsCacheTTL:  15,

        CPUThreshold:       75,
        CPUReleaseThreshold: 40,

        CPUQuotaNormal:  "max 100000",
        CPUQuotaLimited: "50000 100000", // 0.5 core

        EnablePrometheus:        false,
        PrometheusMetricsBindHost: "",  // Empty = use default 0.0.0.0
        PrometheusMetricsBindPort: 1974,

        // Prometheus TLS (disabled by default)
        PrometheusTLSEnabled:     false,
        PrometheusTLSCertFile:    "/etc/cpu-manager/tls/server.crt",
        PrometheusTLSKeyFile:     "/etc/cpu-manager/tls/server.key",
        PrometheusTLSCAFile:      "",
        PrometheusTLSMinVersion:  "1.2",  // TLS 1.2 minimum recommended

        // Prometheus Authentication (disabled by default)
        PrometheusAuthType:     "none",
        PrometheusAuthUsername: "",
        PrometheusAuthPasswordFile: "",
        PrometheusJWTSecretFile:    "",
        PrometheusJWTIssuer:        "cpu-manager",
        PrometheusJWTAudience:      "prometheus",
        PrometheusJWTExpiry:        3600,

        LogLevel:   "INFO",
        LogMaxSize: 10 * 1024 * 1024, // 10MB
        UseSyslog:  false,

        MinSystemCores: 1,
        SystemUIDMin:   1000,
        SystemUIDMax:   pidMax,
        IgnoreSystemLoad: false,
        ServerRole:       "",  // Empty by default
        UserIncludeList:   nil, // nil = all users included (no filter)
        UserExcludeList:   nil, // nil = no users excluded (all users can be limited)
        BlackoutSpec:      "",  // Empty = no blackout (always active)
        BlackoutTimeframes: nil,

        // MCP Server
        MCPEnabled:       false,
        MCPTransport:     "stdio",
        MCPHTTPPort:      1969,
        MCPHTTPHost:      "",  // Empty = use default 0.0.0.0
        MCPLogLevel:      "INFO",
        MCPAllowWriteOps: false,
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
    loadFromEnvironment(cfg)

    // 3. Valida
    if err := validateConfig(cfg); err != nil {
        return nil, fmt.Errorf("validation failed: %w", err)
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
func loadFromEnvironment(cfg *Config) {
    cfgType := reflect.TypeOf(*cfg)
    cfgValue := reflect.ValueOf(cfg).Elem()

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
            }
        case reflect.Bool:
            lowerVal := strings.ToLower(envValue)
            boolVal := false
            switch lowerVal {
            case "true", "1", "yes", "on":
                boolVal = true
            case "false", "0", "no", "off":
                boolVal = false
            }
            fieldValue.SetBool(boolVal)
        }
    }
}

// setConfigField imposta il valore di un campo nella struct Config basandosi sul tag `config`.
func setConfigField(cfg *Config, key, value string) error {
    switch key {
    // Paths
    case "CGROUP_ROOT":
        cfg.CgroupRoot = value
    case "SCRIPT_CGROUP_BASE":
        cfg.ScriptCgroupBase = value
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

    // CPU limits
    case "CPU_QUOTA_NORMAL":
        cfg.CPUQuotaNormal = value
    case "CPU_QUOTA_LIMITED":
        cfg.CPUQuotaLimited = value

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
            cfg.PrometheusMetricsBindPort = i
        }
    // Backward compatibility: old variable names
    case "PROMETHEUS_HOST":
        cfg.PrometheusMetricsBindHost = value
    case "PROMETHEUS_PORT":
        if i, err := strconv.Atoi(value); err == nil {
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

    // Blackout Timeframes
    case "CPU_MANAGER_BLACKOUT":
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
    case "MCP_ALLOW_WRITE_OPS":
        switch strings.ToLower(value) {
        case "true", "1", "yes", "on":
            cfg.MCPAllowWriteOps = true
        case "false", "0", "no", "off":
            cfg.MCPAllowWriteOps = false
        default:
            cfg.MCPAllowWriteOps = false
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

    // Validate polling interval
    if cfg.PollingInterval < 5 {
        errors = append(errors, "POLLING_INTERVAL must be at least 5 seconds")
    }

    // Validate CPU quota format
    if !isValidCPUQuota(cfg.CPUQuotaLimited) {
        errors = append(errors, "CPU_QUOTA_LIMITED must be in format 'quota period' or 'max period'")
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

// IsUserIncluded verifica se un username corrisponde ai pattern della include list
// Se la include list è nil o vuota, tutti gli utenti sono inclusi
func (c *Config) IsUserIncluded(username string) bool {
    // Se la include list non è configurata o è vuota, tutti gli utenti sono inclusi
    if c.UserIncludeList == nil || len(c.UserIncludeList) == 0 {
        return true // No include list = all users included
    }
    
    // Altrimenti, controlla se lo username corrisponde a uno dei pattern regex
    for _, pattern := range c.UserIncludeList {
        if matched, _ := regexp.MatchString(pattern, username); matched {
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
        if matched, _ := regexp.MatchString(pattern, username); matched {
            return true // User matches exclude pattern
        }
    }
    return false
}

// IsUserWhitelisted è un alias per IsUserExcluded per retrocompatibilità
// Il nome è fuorviante ma mantenuto per compatibilità con il codice esistente
func (c *Config) IsUserWhitelisted(username string) bool {
    return !c.IsUserExcluded(username)
}

// SetUserExcludeList imposta la lista di utenti da escludere e salva su file
func (c *Config) SetUserExcludeList(patterns []string, configPath string, reload bool) ([]string, error) {
    // Valida tutti i pattern regex
    for _, pattern := range patterns {
        if _, err := regexp.Compile(pattern); err != nil {
            return nil, fmt.Errorf("invalid regex pattern '%s': %w", pattern, err)
        }
    }
    
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

// IsProcessExcluded verifica se un processo dovrebbe essere escluso dai limiti
// Controlla il nome del comando (comm) contro la blacklist di processi di sistema
func (c *Config) IsProcessExcluded(processName string) bool {
    // Processi di sistema che non dovrebbero mai essere limitati
    excludedProcesses := []string{
        "systemd", "dbus-daemon", "dbus-broker", "polkitd", "udisks2d",
        "NetworkManager", "nm-dispatcher", "wpa_supplicant",
        "sshd", "sshd-session", "cron", "crond", "anacron",
        "rsyslogd", "rsyslog", "syslogd", "syslog-ng",
        "dockerd", "docker", "containerd", "kubelet", "kube-proxy",
        "nginx", "apache2", "httpd", "php-fpm",
        "mysqld", "mariadbd", "postgres", "mongod", "redis-server",
        "postfix", "master", "pickup", "qmgr",
        "chronyd", "ntpd", "systemd-timesyncd",
        "firewalld", "iptables", "nft",
        "auditd", "audit",
        "irqbalance", "mcelog", "smartd",
        "cupsd", "avahi-daemon", "bluetoothd",
        "gdm", "gdm-wayland-session", "gnome-shell",
        "lightdm", "sddm", "xdm",
        "vmtoolsd", "vmware-user", "VBoxService", "VBoxClient",
        "qemu-ga", "qemu-system", "libvirtd",
        "lxcfs", "lxc-monitord",
        "zabbix_agentd", "zabbix_sender",
        "prometheus", "node_exporter", "grafana-server",
        "telegraf", "collectd", "datadog-agent",
    }
    
    processName = strings.ToLower(processName)
    for _, excluded := range excludedProcesses {
        if processName == excluded || strings.HasPrefix(processName, excluded+"-") {
            return true
        }
    }
    return false
}
