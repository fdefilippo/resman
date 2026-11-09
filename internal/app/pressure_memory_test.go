package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/readiness"
	"github.com/fdefilippo/resman/logging"
	"github.com/fdefilippo/resman/state"
)

func TestMemoryWakeupReloadOwnsAndRetiresThirdMonitor(t *testing.T) {
	root := t.TempDir()
	for _, kind := range []string{"cpu", "io", "memory"} {
		if err := os.WriteFile(filepath.Join(root, kind+".pressure"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	initial := config.DefaultConfig()
	initial.PSIEventDriven = true
	initial.CgroupRoot = root
	manager, err := state.NewManager(initial, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: initial, stateManager: manager, logger: logging.GetLogger(), configReloaded: make(chan struct{}, 1)}
	a.startPSIWatcher()
	defer a.stopPSIWatcher()
	if a.psiWatcher == nil || a.psiWatcher.ActiveSystemMonitors() != 2 {
		t.Fatal("default wake sources changed")
	}
	body, err := os.ReadFile(filepath.Join(root, "memory.pressure"))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Fatal("default settings registered a memory wakeup")
	}
	old := a.psiWatcher
	enabled := config.DefaultConfig()
	enabled.PSIEventDriven = true
	enabled.CgroupRoot = root
	enabled.PSIMemoryEventDriven = true
	enabled.PSIMemoryStallThreshold = 75000
	manager.UpdateConfig(enabled)
	if err := a.applyReloadedConfig(enabled); err != nil {
		t.Fatal(err)
	}
	if a.psiWatcher == nil || a.psiWatcher == old || a.psiWatcher.ActiveSystemMonitors() != 3 {
		t.Fatal("memory opt-in did not replace the owned watcher")
	}
	body, err = os.ReadFile(filepath.Join(root, "memory.pressure"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "some 75000 1000000") {
		t.Fatalf("memory trigger=%q", body)
	}
	disabled := config.DefaultConfig()
	disabled.PSIEventDriven = true
	disabled.CgroupRoot = root
	manager.UpdateConfig(disabled)
	if err := a.applyReloadedConfig(disabled); err != nil {
		t.Fatal(err)
	}
	if a.psiWatcher == nil || a.psiWatcher.ActiveSystemMonitors() != 2 {
		t.Fatal("memory monitor remained after disabling opt-in")
	}
}

func TestMissingOptionalMemoryWakeSourceDegradesReadiness(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.PSIEventDriven = true
	cfg.PSIMemoryEventDriven = true
	cfg.BlackoutTimeframes, _ = config.ParseTimeframe("* 00-24")
	manager, err := state.NewManager(cfg, &blackoutCollector{}, &blackoutCgroups{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RunControlCycle(t.Context()); err != nil {
		t.Fatal(err)
	}
	watcher := cgroup.NewPSIWatcher(1000000)
	if err := watcher.Start(); err != nil {
		t.Fatal(err)
	}
	defer watcher.Stop()
	for _, kind := range []string{"cpu", "io"} {
		path := filepath.Join(t.TempDir(), kind+".pressure")
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		watcher.SetThreshold(kind, 50000)
		if err := watcher.AddMonitor(0, kind, path); err != nil {
			t.Fatal(err)
		}
	}
	a := &App{cfg: cfg, ctx: context.Background(), stateManager: manager, psiWatcher: watcher, psiEventDriven: true}
	a.phase.Store(phaseRunning)
	if got := a.ReadinessSnapshot(); got.PSI != readiness.Degraded || got.Status == readiness.NotReady {
		t.Fatalf("missing optional memory=%+v", got)
	}
}

func TestMemoryWakeRunsNormalSuspendedCycleWithNoAppliedIntent(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.PSIEventDriven = true
	cfg.PSIMemoryEventDriven = true
	cfg.BlackoutTimeframes, _ = config.ParseTimeframe("* 00-24")
	manager, err := state.NewManager(cfg, &blackoutCollector{}, &blackoutCgroups{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: cfg, ctx: t.Context(), stateManager: manager, logger: logging.GetLogger()}
	complete := make(chan struct{})
	close(complete)
	a.handlePSIEvent(cgroup.PSIEvent{Type: "memory", Timestamp: time.Now()}, &complete)
	select {
	case <-complete:
	default:
		t.Fatal("wake cycle not drained")
	}
	history := manager.GetControlHistory(1)
	if len(history) != 1 || history[0].Trigger != "psi_system_memory" || !history[0].Suspended || history[0].ExecutionAttempted || history[0].SampleAvailable {
		t.Fatalf("memory wake history=%+v", history)
	}
}
