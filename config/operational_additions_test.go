package config

import (
	"strings"
	"testing"
)

func TestOperationalAdditionsParseValidateAndReload(t *testing.T) {
	for _, tt := range []struct {
		key, value string
		invalid    bool
	}{
		{"PSI_MEMORY_EVENT_DRIVEN", "true", false},
		{"PSI_MEMORY_STALL_THRESHOLD", "100000", false},
		{"PSI_MEMORY_STALL_THRESHOLD", "0", true},
		{"PSI_MEMORY_STALL_THRESHOLD", "-1", true},
		{"LIMIT_HOOK_HTTP_MAX_ATTEMPTS", "1", false},
		{"LIMIT_HOOK_HTTP_MAX_ATTEMPTS", "5", false},
		{"LIMIT_HOOK_HTTP_MAX_ATTEMPTS", "0", true},
		{"LIMIT_HOOK_HTTP_MAX_ATTEMPTS", "6", true},
		{"METRICS_DB_ROLLUP_RETENTION_DAYS", "365", false},
		{"METRICS_DB_ROLLUP_RETENTION_DAYS", "0", true},
	} {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.PSIEventDriven = true
			err := setConfigField(cfg, tt.key, tt.value)
			if err == nil {
				err = validateConfig(cfg)
			}
			if (err != nil) != tt.invalid {
				t.Fatalf("error=%v invalid=%v", err, tt.invalid)
			}
		})
	}
	cfg := DefaultConfig()
	cfg.PSIMemoryEventDriven = true
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "requires PSI_EVENT_DRIVEN") {
		t.Fatalf("memory wake dependency=%v", err)
	}
	cfg.PSIEventDriven = true
	cfg.PSIMemoryStallThreshold = cfg.PSIWindowUs + 1
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "must not exceed PSI_WINDOW_US") {
		t.Fatalf("memory threshold=%v", err)
	}
	for _, key := range []string{"PSI_MEMORY_EVENT_DRIVEN", "PSI_MEMORY_STALL_THRESHOLD", "LIMIT_HOOK_HTTP_MAX_ATTEMPTS", "METRICS_DB_ROLLUP_RETENTION_DAYS"} {
		if lifecycle, _ := LifecycleForField(key); lifecycle != LifecycleDynamic {
			t.Fatalf("%s lifecycle=%s", key, lifecycle)
		}
	}
}

func TestRollupRetentionDoesNotRejectExistingLongRawRetention(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MetricsDBRetentionDays = 730
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("valid prior raw retention rejected: %v", err)
	}
	if cfg.MetricsDBRollupRetentionDays != 365 || cfg.GetMetricsDBRollupRetentionDays() != 730 {
		t.Fatalf("configured/effective retention=%d/%d", cfg.MetricsDBRollupRetentionDays, cfg.GetMetricsDBRollupRetentionDays())
	}
	cfg.MetricsDBRollupRetentionDays = 1000
	if cfg.GetMetricsDBRollupRetentionDays() != 1000 {
		t.Fatal("configured longer rollup retention ignored")
	}
}
