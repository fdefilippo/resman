package app

import (
	"context"
	"testing"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/readiness"
	"github.com/fdefilippo/resman/state"
)

func TestReadinessLifecycleAndOptionalFailure(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.BlackoutTimeframes, _ = config.ParseTimeframe("* 00-24")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager, err := state.NewManager(cfg, &blackoutCollector{}, &blackoutCgroups{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: cfg, ctx: ctx, stateManager: manager, configPath: "requested.conf", databaseRequested: true}
	if got := a.ReadinessSnapshot(); got.Status != readiness.NotReady {
		t.Fatalf("startup=%+v", got)
	}
	a.phase.Store(phaseRunning)
	if got := a.ReadinessSnapshot(); got.Status != readiness.NotReady {
		t.Fatalf("no initial decision=%+v", got)
	}
	if err := manager.RunControlCycle(ctx); err != nil {
		t.Fatal(err)
	}
	got := a.ReadinessSnapshot()
	if got.Status != readiness.Degraded || got.Database != readiness.Degraded || got.ConfigWatcher != readiness.Degraded || got.Systemd != readiness.Disabled || !got.DecisionsSuspended {
		t.Fatalf("supported blackout/observation degradation=%+v", got)
	}
	a.phase.Store(phaseStopping)
	if got := a.ReadinessSnapshot(); got.Status != readiness.NotReady {
		t.Fatalf("shutdown=%+v", got)
	}
	a.phase.Store(phaseRunning)
	cancel()
	if got := a.ReadinessSnapshot(); got.Status != readiness.NotReady {
		t.Fatalf("cancelled=%+v", got)
	}
}
