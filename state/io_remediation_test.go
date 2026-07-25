package state

import (
	"errors"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

type ioRemediationTestDeps struct {
	psi            cgroup.PSIStats
	temporaryCalls []temporaryIOLimitCall
	temporaryError error
}

type temporaryIOLimitCall struct {
	uid          int
	readBPS      string
	writeBPS     string
	readIOPS     int
	writeIOPS    int
	deviceFilter string
	multiplier   float64
}

func (d *ioRemediationTestDeps) GetPSIStats(int) (cgroup.PSIStats, error) {
	return d.psi, nil
}

func (d *ioRemediationTestDeps) ApplyTemporaryIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string, multiplier float64) error {
	d.temporaryCalls = append(d.temporaryCalls, temporaryIOLimitCall{
		uid:          uid,
		readBPS:      readBPS,
		writeBPS:     writeBPS,
		readIOPS:     readIOPS,
		writeIOPS:    writeIOPS,
		deviceFilter: deviceFilter,
		multiplier:   multiplier,
	})
	return d.temporaryError
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
	if len(deps.temporaryCalls) != 1 {
		t.Fatalf("IO boost calls = %d, want 1 after continuous starvation", len(deps.temporaryCalls))
	}
	call := deps.temporaryCalls[0]
	if call.multiplier != cfg.IOBoostMultiplier ||
		call.readBPS != cfg.IOReadBPS ||
		call.writeBPS != cfg.IOWriteBPS ||
		call.readIOPS != cfg.IOReadIOPS ||
		call.writeIOPS != cfg.IOWriteIOPS ||
		call.deviceFilter != cfg.IODeviceFilter {
		t.Fatalf("temporary IO limit call = %+v, want configured limits with multiplier %v", call, cfg.IOBoostMultiplier)
	}
	if !state.IsActive {
		t.Fatal("IO boost state should be active after remediation")
	}
}

func TestIORemediationExpiresBoostWhenPressureIsNormal(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.IORemediationEnabled = true
	cfg.IOStarvationCheckInterval = 0
	cfg.IOBoostDuration = 60
	cfg.IORevertOnNormal = false

	deps := &ioRemediationTestDeps{psi: cgroup.PSIStats{SomeAvg10: 0}}
	remediation := NewIORemediation(logging.GetLogger())
	remediation.boostStates[1000] = &IOBoostState{
		IsActive:             true,
		StartTime:            time.Now().Add(-time.Minute),
		OriginalReadBPS:      "100M",
		OriginalWriteBPS:     "50M",
		OriginalReadIOPS:     100,
		OriginalWriteIOPS:    50,
		OriginalDeviceFilter: "8:0",
	}

	remediation.CheckAndRemediate(deps, cfg, []int{1000})

	if len(deps.temporaryCalls) != 1 {
		t.Fatalf("restore calls = %d, want 1", len(deps.temporaryCalls))
	}
	call := deps.temporaryCalls[0]
	if call.uid != 1000 ||
		call.readBPS != "100M" ||
		call.writeBPS != "50M" ||
		call.readIOPS != 100 ||
		call.writeIOPS != 50 ||
		call.deviceFilter != "8:0" ||
		call.multiplier != 1 {
		t.Fatalf("restore calls = %+v, want one call with multiplier 1", deps.temporaryCalls)
	}
	if remediation.boostStates[1000].IsActive {
		t.Fatal("expired boost remained active while pressure was normal")
	}
}

func TestIORemediationRetriesFailedRestore(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.IORemediationEnabled = true
	cfg.IOStarvationCheckInterval = 0
	cfg.IOBoostDuration = 60

	deps := &ioRemediationTestDeps{
		psi:            cgroup.PSIStats{SomeAvg10: 0},
		temporaryError: errors.New("write failed"),
	}
	remediation := NewIORemediation(logging.GetLogger())
	remediation.boostStates[1000] = &IOBoostState{
		IsActive:  true,
		StartTime: time.Now().Add(-time.Minute),
	}

	remediation.CheckAndRemediate(deps, cfg, []int{1000})

	if !remediation.boostStates[1000].IsActive {
		t.Fatal("failed restore cleared active boost state instead of leaving it for retry")
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
