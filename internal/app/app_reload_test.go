package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
	"github.com/fdefilippo/resman/state"
)

type recordingCPUSamplingCadence struct {
	intervals []time.Duration
}

func (r *recordingCPUSamplingCadence) SetFallbackCPUSamplingInterval(interval time.Duration) {
	r.intervals = append(r.intervals, interval)
}

func TestWithMetricsCollectorWiresCPUSamplingCadence(t *testing.T) {
	cfg := config.DefaultConfig()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	application := NewApp(cfg, "", ctx, cancel, nil, logging.GetLogger()).WithMetricsCollector()
	if application.err != nil {
		t.Fatalf("WithMetricsCollector() error: %v", application.err)
	}
	if application.metricsCollector == nil {
		t.Fatal("WithMetricsCollector() did not create a collector")
	}
	t.Cleanup(application.metricsCollector.Stop)
	if application.cpuSamplingCadence != application.metricsCollector {
		t.Fatal("WithMetricsCollector() did not wire the runtime cadence sink to the collector")
	}
}

func TestApplyReloadedConfigPublishesEffectiveCPUSamplingCadence(t *testing.T) {
	tests := []struct {
		name             string
		psiConfigured    bool
		psiActive        bool
		pollingInterval  int
		fallbackInterval int
		want             time.Duration
	}{
		{
			name:             "normal polling",
			pollingInterval:  45,
			fallbackInterval: 10,
			want:             45 * time.Second,
		},
		{
			name:             "PSI configured but inactive",
			psiConfigured:    true,
			pollingInterval:  30,
			fallbackInterval: 10,
			want:             30 * time.Second,
		},
		{
			name:             "PSI configured and active",
			psiConfigured:    true,
			psiActive:        true,
			pollingInterval:  30,
			fallbackInterval: 10,
			want:             10 * time.Second,
		},
		{
			name:             "stale runtime flag cannot enable unconfigured PSI",
			psiActive:        true,
			pollingInterval:  30,
			fallbackInterval: 10,
			want:             30 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.PSIEventDriven = tt.psiConfigured
			cfg.PollingInterval = tt.pollingInterval
			cfg.PSIFallbackInterval = tt.fallbackInterval
			stateManager, err := state.NewManager(cfg, nil, nil, nil)
			if err != nil {
				t.Fatalf("NewManager() error: %v", err)
			}
			recorder := &recordingCPUSamplingCadence{}
			application := &App{
				cfg:                cfg,
				logger:             logging.GetLogger(),
				stateManager:       stateManager,
				psiEventDriven:     tt.psiActive,
				cpuSamplingCadence: recorder,
				configReloaded:     make(chan struct{}, 1),
			}

			if err := application.applyReloadedConfig(cfg); err != nil {
				t.Fatalf("applyReloadedConfig() error: %v", err)
			}
			if len(recorder.intervals) != 1 {
				t.Fatalf("published intervals = %v, want one interval", recorder.intervals)
			}
			if got := recorder.intervals[0]; got != tt.want {
				t.Fatalf("published interval = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestApplyReloadedConfigReconfiguresPSIWatcher(t *testing.T) {
	cgroupRoot := t.TempDir()
	cpuPressurePath := filepath.Join(cgroupRoot, "cpu.pressure")
	ioPressurePath := filepath.Join(cgroupRoot, "io.pressure")
	for _, path := range []string{cpuPressurePath, ioPressurePath} {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatalf("WriteFile(%s) error: %v", path, err)
		}
	}

	initial := config.DefaultConfig()
	initial.CgroupRoot = cgroupRoot
	initial.PSIEventDriven = true
	stateManager, err := state.NewManager(initial, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	application := &App{
		cfg:            initial,
		logger:         logging.GetLogger(),
		stateManager:   stateManager,
		configReloaded: make(chan struct{}, 1),
	}
	application.startPSIWatcher()
	t.Cleanup(application.stopPSIWatcher)

	application.psiMu.RLock()
	initialWatcher := application.psiWatcher
	application.psiMu.RUnlock()
	if initialWatcher == nil || !application.isPSIEventDrivenActive() {
		t.Fatal("initial PSI watcher did not start")
	}

	runtimeOnly := config.DefaultConfig()
	runtimeOnly.CgroupRoot = cgroupRoot
	runtimeOnly.PSIEventDriven = true
	runtimeOnly.PollingInterval = initial.PollingInterval + 1
	stateManager.UpdateConfig(runtimeOnly)
	if err := application.applyReloadedConfig(runtimeOnly); err != nil {
		t.Fatalf("applyReloadedConfig(runtimeOnly) error: %v", err)
	}
	application.psiMu.RLock()
	watcherAfterRuntimeChange := application.psiWatcher
	application.psiMu.RUnlock()
	if watcherAfterRuntimeChange != initialWatcher {
		t.Fatal("non-PSI config change restarted the PSI watcher")
	}
	if application.currentConfig() != runtimeOnly {
		t.Fatal("App config pointer was not updated after runtime-only reload")
	}
	select {
	case <-application.configReloaded:
	default:
		t.Fatal("runtime config reload did not wake the control loop")
	}

	reconfigured := config.DefaultConfig()
	reconfigured.CgroupRoot = cgroupRoot
	reconfigured.PSIEventDriven = true
	reconfigured.PSICPUStallThreshold = 75000
	reconfigured.PSIWindowUs = 2000000
	stateManager.UpdateConfig(reconfigured)
	if err := application.applyReloadedConfig(reconfigured); err != nil {
		t.Fatalf("applyReloadedConfig(reconfigured) error: %v", err)
	}
	application.psiMu.RLock()
	reconfiguredWatcher := application.psiWatcher
	application.psiMu.RUnlock()
	if reconfiguredWatcher == nil || reconfiguredWatcher == initialWatcher {
		t.Fatal("PSI threshold change did not replace the watcher")
	}
	cpuTrigger, err := os.ReadFile(cpuPressurePath)
	if err != nil {
		t.Fatalf("ReadFile(cpu.pressure) error: %v", err)
	}
	if !strings.HasPrefix(string(cpuTrigger), "some 75000 2000000") {
		t.Fatalf("cpu.pressure trigger = %q, want updated threshold and window", cpuTrigger)
	}

	disabled := config.DefaultConfig()
	disabled.CgroupRoot = cgroupRoot
	disabled.PSIEventDriven = false
	stateManager.UpdateConfig(disabled)
	if err := application.applyReloadedConfig(disabled); err != nil {
		t.Fatalf("applyReloadedConfig(disabled) error: %v", err)
	}
	application.psiMu.RLock()
	disabledWatcher := application.psiWatcher
	application.psiMu.RUnlock()
	if disabledWatcher != nil || application.isPSIEventDrivenActive() {
		t.Fatal("PSI watcher remained active after PSI_EVENT_DRIVEN=false")
	}
}
