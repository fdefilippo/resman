package state

import (
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

type ioRemediationTestDeps struct {
	psi         cgroup.PSIStats
	removeCalls int
}

func (d *ioRemediationTestDeps) GetPSIStats(int) (cgroup.PSIStats, error) {
	return d.psi, nil
}

func (d *ioRemediationTestDeps) GetIOStats(int) (uint64, uint64, uint64, uint64, error) {
	return 0, 0, 0, 0, nil
}

func (d *ioRemediationTestDeps) ApplyTemporaryIOLimit(int, string, string, int, int, string, float64) error {
	return nil
}

func (d *ioRemediationTestDeps) RemoveIOLimit(int) error {
	d.removeCalls++
	return nil
}

func TestIORemediationCleanupPreservesStarvationState(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.IORemediationEnabled = true
	cfg.IOStarvationCheckInterval = 0
	cfg.IOStarvationThreshold = 300
	cfg.IOPSIThreshold = 25

	deps := &ioRemediationTestDeps{
		psi: cgroup.PSIStats{SomeAvg10: 90},
	}
	remediation := NewIORemediation(logging.GetLogger())

	remediation.CheckAndRemediate(deps, cfg, []int{1000})
	state := remediation.boostStates[1000]
	if state == nil || state.StarvationStart.IsZero() {
		t.Fatal("first starvation sample was not retained")
	}

	remediation.Cleanup(24 * time.Hour)
	state = remediation.boostStates[1000]
	if state == nil {
		t.Fatal("cleanup removed a state that has never been boosted")
	}

	state.StarvationStart = time.Now().Add(-time.Duration(cfg.IOStarvationThreshold) * time.Second)
	remediation.CheckAndRemediate(deps, cfg, []int{1000})
	if deps.removeCalls != 1 {
		t.Fatalf("IO boost calls = %d, want 1 after continuous starvation", deps.removeCalls)
	}
	if !state.IsActive {
		t.Fatal("IO boost state should be active after remediation")
	}
}

func TestIORemediationCleanupRemovesUsersNoLongerSeen(t *testing.T) {
	remediation := NewIORemediation(logging.GetLogger())
	remediation.boostStates[1000] = &IOBoostState{
		LastSeen: time.Now().Add(-25 * time.Hour),
	}

	remediation.Cleanup(24 * time.Hour)
	if _, exists := remediation.boostStates[1000]; exists {
		t.Fatal("cleanup retained a user that was no longer observed")
	}
}
