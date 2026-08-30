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
	"reflect"
	"slices"
	"testing"
)

func TestEveryPublicConfigFieldHasExactlyOneLifecycle(t *testing.T) {
	configType := reflect.TypeOf((*Config)(nil)).Elem()
	want := make(map[string]bool)
	for i := 0; i < configType.NumField(); i++ {
		field := configType.Field(i)
		key := field.Tag.Get("config")
		if key != "" && key != "-" {
			want[key] = true
			continue
		}
		if reason := nonPublicConfigFields[field.Name]; reason == "" {
			t.Errorf("non-public Config field %s has no lifecycle exclusion reason", field.Name)
		}
	}

	if len(configFieldLifecycles) != len(want) {
		t.Errorf("lifecycle table has %d entries, Config has %d public keys", len(configFieldLifecycles), len(want))
	}
	for key := range want {
		lifecycle, ok := LifecycleForField(key)
		if !ok {
			t.Errorf("configuration key %s has no lifecycle", key)
			continue
		}
		if lifecycle != LifecycleDynamic && lifecycle != LifecycleRestartRequired {
			t.Errorf("configuration key %s has invalid lifecycle %q", key, lifecycle)
		}
	}
	for key := range configFieldLifecycles {
		if !want[key] {
			t.Errorf("lifecycle table contains unknown configuration key %s", key)
		}
	}
	if len(nonPublicConfigFields)+len(want) != configType.NumField() {
		t.Errorf("lifecycle metadata covers %d Config fields, want %d",
			len(nonPublicConfigFields)+len(want), configType.NumField())
	}
}

func TestApplyReloadLifecycle(t *testing.T) {
	effective := DefaultConfig()
	effective.MCPAuthToken = "old-secret"
	requested := DefaultConfig()
	requested.CPUThreshold = 91
	requested.UsernameCacheTTL = 17
	requested.MetricsDBRetentionDays = 12
	requested.CreatedCgroupsFile = "/other/created-cgroups"
	requested.MetricsDBPath = "/other/metrics.db"
	requested.MCPTLSCertFile = "/other/server.crt"
	requested.MCPAuthToken = "new-secret"
	requested.ServerRole = "other-role"

	rejected, err := ApplyReloadLifecycle(effective, requested)
	if err != nil {
		t.Fatalf("ApplyReloadLifecycle() error: %v", err)
	}
	wantRejected := []string{
		"CREATED_CGROUPS_FILE", "MCP_AUTH_TOKEN", "MCP_TLS_CERT_FILE",
		"METRICS_DB_PATH", "SERVER_ROLE",
	}
	if !slices.Equal(rejected, wantRejected) {
		t.Fatalf("rejected fields = %v, want %v", rejected, wantRejected)
	}
	if requested.CPUThreshold != 91 || requested.UsernameCacheTTL != 17 || requested.MetricsDBRetentionDays != 12 {
		t.Fatal("dynamic fields were not preserved from the requested configuration")
	}
	if requested.CreatedCgroupsFile != effective.CreatedCgroupsFile ||
		requested.MetricsDBPath != effective.MetricsDBPath ||
		requested.MCPTLSCertFile != effective.MCPTLSCertFile ||
		requested.MCPAuthToken != effective.MCPAuthToken ||
		requested.ServerRole != effective.ServerRole {
		t.Fatal("restart-required fields were not restored from the effective configuration")
	}
	for _, field := range rejected {
		if field == "old-secret" || field == "new-secret" {
			t.Fatal("secret value leaked in deferred field list")
		}
	}
}

func TestRepresentativeFieldLifecycles(t *testing.T) {
	tests := map[string]FieldLifecycle{
		"PROCESS_EXCLUDE_LIST":      LifecycleDynamic,
		"USERNAME_CACHE_TTL":        LifecycleDynamic,
		"METRICS_DB_RETENTION_DAYS": LifecycleDynamic,
		"CREATED_CGROUPS_FILE":      LifecycleRestartRequired,
		"METRICS_DB_PATH":           LifecycleRestartRequired,
		"MCP_TLS_KEY_FILE":          LifecycleRestartRequired,
		"SERVER_ROLE":               LifecycleRestartRequired,
	}
	for key, want := range tests {
		got, ok := LifecycleForField(key)
		if !ok || got != want {
			t.Errorf("LifecycleForField(%q) = %q, %v; want %q, true", key, got, ok, want)
		}
	}
}
