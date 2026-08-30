/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 */

package config

import (
	"fmt"
	"reflect"
	"sort"
)

// FieldLifecycle defines whether a configuration field may change while the
// process is running or requires reconstruction of one or more components.
type FieldLifecycle string

const (
	LifecycleDynamic         FieldLifecycle = "dynamic"
	LifecycleRestartRequired FieldLifecycle = "restart-required"
)

var dynamicConfigFields = []string{
	"POLLING_INTERVAL", "MIN_ACTIVE_TIME", "METRICS_CACHE_TTL",
	"METRICS_REFRESH_INTERVAL", "PROCESS_MIN_AGE_SECONDS",
	"CGROUP_OPERATION_TIMEOUT", "MCP_SHUTDOWN_TIMEOUT",
	"CPU_THRESHOLD", "CPU_RELEASE_THRESHOLD", "CPU_THRESHOLD_DURATION",
	"CPU_QUOTA_NORMAL",
	"RAM_LIMIT_ENABLED", "RAM_THRESHOLD", "RAM_RELEASE_THRESHOLD",
	"RAM_QUOTA_PER_USER", "DISABLE_SWAP", "RAM_HIGH_RATIO",
	"RAM_USER_INCLUDE_LIST", "RAM_USER_EXCLUDE_LIST",
	"IO_LIMIT_ENABLED", "IO_THRESHOLD", "IO_RELEASE_THRESHOLD",
	"IO_READ_BPS", "IO_WRITE_BPS", "IO_READ_IOPS", "IO_WRITE_IOPS",
	"IO_DEVICE_FILTER", "IO_THRESHOLD_DURATION",
	"IO_REMEDIATION_ENABLED", "IO_STARVATION_THRESHOLD",
	"IO_STARVATION_CHECK_INTERVAL", "IO_BOOST_MULTIPLIER", "IO_BOOST_DURATION",
	"IO_BOOST_MAX_PER_HOUR", "IO_PSI_THRESHOLD", "IO_REVERT_ON_NORMAL",
	"IO_USER_INCLUDE_LIST", "IO_USER_EXCLUDE_LIST",
	"AUTODETECT_PATTERNS", "PATTERN_HISTORY_HOURS", "PATTERN_MIN_SAMPLES",
	"PATTERN_CONFIDENCE_THRESHOLD", "BATCH_NIGHT_CPU_QUOTA",
	"BATCH_NIGHT_RAM_QUOTA", "INTERACTIVE_CPU_QUOTA", "INTERACTIVE_RAM_QUOTA",
	"LIMIT_HOOK_ENABLED", "LIMIT_HOOK_SCRIPT", "LIMIT_HOOK_URL", "LIMIT_HOOK_TIMEOUT",
	"LOG_LEVEL", "MIN_SYSTEM_CORES", "SYSTEM_UID_MIN", "SYSTEM_UID_MAX",
	"USER_INCLUDE_LIST", "USER_EXCLUDE_LIST", "PROCESS_EXCLUDE_LIST", "BLACKOUT",
	"IGNORE_SYSTEM_LOAD", "METRICS_DB_RETENTION_DAYS",
	"USERNAME_CACHE_TTL", "PSI_EVENT_DRIVEN", "PSI_CPU_STALL_THRESHOLD",
	"PSI_IO_STALL_THRESHOLD", "PSI_WINDOW_US", "PSI_FALLBACK_INTERVAL",
	"PSI_BOOST_WEIGHT", "PSI_BOOST_DURATION",
}

var restartRequiredConfigFields = []string{
	"CGROUP_ROOT", "CGROUP_BASE", "LOG_FILE", "CREATED_CGROUPS_FILE",
	"ENABLE_PROMETHEUS", "PROMETHEUS_METRICS_BIND_HOST", "PROMETHEUS_METRICS_BIND_PORT",
	"PROMETHEUS_TLS_ENABLED", "PROMETHEUS_TLS_CERT_FILE", "PROMETHEUS_TLS_KEY_FILE",
	"PROMETHEUS_TLS_CA_FILE", "PROMETHEUS_TLS_MIN_VERSION",
	"PROMETHEUS_AUTH_TYPE", "PROMETHEUS_AUTH_USERNAME", "PROMETHEUS_AUTH_PASSWORD_FILE",
	"PROMETHEUS_JWT_SECRET_FILE", "PROMETHEUS_JWT_ISSUER", "PROMETHEUS_JWT_AUDIENCE",
	"LOG_MAX_SIZE", "USE_SYSLOG", "SERVER_ROLE",
	"MCP_ENABLED", "MCP_TRANSPORT", "MCP_HTTP_PORT", "MCP_HTTP_HOST",
	"MCP_TLS_ENABLED", "MCP_TLS_CERT_FILE", "MCP_TLS_KEY_FILE", "MCP_TLS_CA_FILE",
	"MCP_TLS_MIN_VERSION", "MCP_LOG_LEVEL", "MCP_AUTH_TOKEN", "MCP_ALLOW_WRITE_OPS",
	"METRICS_DB_ENABLED", "METRICS_DB_PATH", "METRICS_DB_WRITE_INTERVAL",
}

var configFieldLifecycles = buildConfigFieldLifecycles()

// nonPublicConfigFields documents why Config members without public keys do
// not participate independently in reload diffing.
var nonPublicConfigFields = map[string]string{
	"mu":                 "internal synchronization",
	"saveGate":           "shared configuration persistence coordinator",
	"saveState":          "shared configuration persistence health",
	"regexCache":         "derived cache rebuilt from public policy keys",
	"ConfigFile":         "runtime path selected by the command line",
	"BlackoutTimeframes": "derived from BLACKOUT",
}

func buildConfigFieldLifecycles() map[string]FieldLifecycle {
	result := make(map[string]FieldLifecycle, len(dynamicConfigFields)+len(restartRequiredConfigFields))
	add := func(fields []string, lifecycle FieldLifecycle) {
		for _, field := range fields {
			if previous, exists := result[field]; exists {
				panic(fmt.Sprintf("configuration field %s has duplicate lifecycle %s and %s", field, previous, lifecycle))
			}
			result[field] = lifecycle
		}
	}
	add(dynamicConfigFields, LifecycleDynamic)
	add(restartRequiredConfigFields, LifecycleRestartRequired)
	return result
}

// LifecycleForField returns the authoritative lifecycle for a public
// configuration key.
func LifecycleForField(field string) (FieldLifecycle, bool) {
	lifecycle, ok := configFieldLifecycles[field]
	return lifecycle, ok
}

// ApplyReloadLifecycle restores restart-required values from the effective
// configuration and returns the rejected key names. Values are never returned,
// which keeps credentials out of errors and logs.
func ApplyReloadLifecycle(effective, requested *Config) ([]string, error) {
	if effective == nil || requested == nil {
		return nil, fmt.Errorf("effective and requested config must not be nil")
	}
	if effective == requested {
		return nil, nil
	}

	saveGate, saveState := effective.persistenceCoordinator()
	effective.mu.RLock()
	requested.mu.Lock()
	requested.saveGate = saveGate
	requested.saveState = saveState
	rejected, err := applyReloadLifecycleLocked(effective, requested)
	requested.mu.Unlock()
	effective.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	sort.Strings(rejected)
	return rejected, nil
}

func applyReloadLifecycleLocked(effective, requested *Config) ([]string, error) {
	typeOfConfig := reflect.TypeOf(effective).Elem()
	effectiveValue := reflect.ValueOf(effective).Elem()
	requestedValue := reflect.ValueOf(requested).Elem()
	var rejected []string

	for i := 0; i < typeOfConfig.NumField(); i++ {
		structField := typeOfConfig.Field(i)
		key := structField.Tag.Get("config")
		if key == "" || key == "-" {
			continue
		}
		lifecycle, ok := configFieldLifecycles[key]
		if !ok {
			return nil, fmt.Errorf("configuration field %s has no lifecycle classification", key)
		}
		if lifecycle != LifecycleRestartRequired {
			continue
		}
		currentField := effectiveValue.Field(i)
		requestedField := requestedValue.Field(i)
		if reflect.DeepEqual(currentField.Interface(), requestedField.Interface()) {
			continue
		}
		requestedField.Set(currentField)
		rejected = append(rejected, key)
	}
	return rejected, nil
}
