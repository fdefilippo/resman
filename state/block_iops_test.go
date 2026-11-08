package state

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/blockiops"
	"github.com/fdefilippo/resman/internal/systemdunit"
)

type fakeBlockIOPSSource struct {
	sample  blockiops.Sample
	failure error
	closed  bool
}

func (s *fakeBlockIOPSSource) Register(uid uint32, invocation string, cgroup uint64) (blockiops.Identity, error) {
	return blockiops.Identity{SourceGeneration: 1, PolicyGeneration: 1, WatchGeneration: 1, UID: uid, InvocationID: invocation, CgroupID: cgroup}, s.failure
}
func (s *fakeBlockIOPSSource) Read(id blockiops.Identity, _ []blockiops.Device) (blockiops.Sample, error) {
	sample := s.sample
	sample.Devices = make(map[blockiops.Device]blockiops.Counters, len(s.sample.Devices))
	for d, c := range s.sample.Devices {
		sample.Devices[d] = c
	}
	sample.Identity = id
	return sample, s.failure
}
func (s *fakeBlockIOPSSource) Retain([]blockiops.Identity) error { return s.failure }
func (s *fakeBlockIOPSSource) Health() error                     { return s.failure }
func (s *fakeBlockIOPSSource) Close() error                      { s.closed = true; return nil }

func TestBlockIOPSCollectorFeedsActivationAndCertifiedRelease(t *testing.T) {
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(1000)}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &mockCgroupManager{}, 2)
	cfg := manager.cfg
	cfg.IOEnabled = true
	cfg.IOReadBPS = "max"
	cfg.IOWriteBPS = "max"
	cfg.IOReadIOPS = 100
	cfg.IOWriteIOPS = 100
	cfg.IOThreshold = 50
	cfg.IOReleaseThreshold = 25
	cfg.IOThresholdDuration = 0
	d := blockiops.Device{Major: 8}
	source := &fakeBlockIOPSSource{sample: blockiops.Sample{Started: time.Now().Add(-20 * time.Second), Devices: map[blockiops.Device]blockiops.Counters{d: {}}}}
	source.sample.Finished = source.sample.Started
	manager.blockIOPS = &blockIOPSRuntime{source: source, previous: make(map[int]blockiops.Sample)}
	manager.resolveBlockIOPSDevices = func(string) ([]blockiops.Device, error) { return []blockiops.Device{d}, nil }
	collect := func() *SystemMetrics {
		t.Helper()
		topology, _ := adapter.Discover(context.Background())
		id := topology.Users[0].Unit.Identity
		inventory := systemdunit.NewProcessAuthorityInventory(1, "test", true, []systemdunit.ProcessAuthorityObservation{{UID: 1000, Identity: id, CPUCoverage: true, ResourceAuthority: systemdunit.ResourceAuthority{State: systemdunit.ResourceCoverageComplete}}})
		sample := &SystemMetrics{IOEligibleUsers: []int{1000}, IOEligibleUsersCount: 1, TotalCores: 2, TotalMemoryMB: 1000, systemdAuthorityInventory: &inventory}
		if err := manager.collectEligibleBlockIOPS(sample, cfg.GetIODecisionPolicy()); err != nil {
			t.Fatal(err)
		}
		return sample
	}
	first := collect()
	if first.IOBlockIOPSUnavailableUsers != 1 {
		t.Fatal("first read was fabricated as idle")
	}
	source.sample.Started = source.sample.Started.Add(10 * time.Second)
	source.sample.Finished = source.sample.Started
	source.sample.Devices[d] = blockiops.Counters{Write: blockiops.Partition{Selected: 1000, Unresolved: 100}}
	busy := collect()
	if busy.IOBlockIOPSUnavailableUsers != 0 {
		t.Fatal("healthy source stayed unavailable")
	}
	if decision, _ := manager.makeDecision(busy); decision != decisionActivate {
		t.Fatalf("IOPS-only activation=%s", decision)
	}
	if !busy.IOBlockIOPSDevices[d].Uncertain {
		t.Fatal("uncertainty lost")
	}
	// The same total counters in the next interval prove idle even after mixed traffic.
	source.sample.Started = source.sample.Started.Add(10 * time.Second)
	source.sample.Finished = source.sample.Started
	quiet := collect()
	manager.limitsActive = true
	manager.limitsAppliedTime = time.Now().Add(-time.Hour)
	if decision, reason := manager.makeDecision(quiet); decision != decisionDeactivate {
		t.Fatalf("quiet CPU episode pinned by IOPS: %s: %s", decision, reason)
	}
	manager.resourceLimitsActive = true
	manager.limitsActive = false
	if decision, reason := manager.makeDecision(quiet); decision != decisionDeactivate {
		t.Fatalf("quiet RAM episode pinned by IOPS: %s: %s", decision, reason)
	}
	source.failure = errors.New("injected source loss")
	if err := manager.collectEligibleBlockIOPS(quiet, cfg.GetIODecisionPolicy()); err == nil {
		t.Fatal("source failure hidden")
	}
	if manager.limitsActive || manager.resourceLimitsActive {
		t.Fatal("source failure pinned enforcement")
	}
}

func TestIOPSReleaseUsesUpperBoundAndKeepsDevicesIndependent(t *testing.T) {
	policy := config.IODecisionPolicy{Enabled: true, WriteIOPS: 100}
	d := blockiops.Device{Major: 8}
	sample := &SystemMetrics{IOEligibleUsersCount: 1, IOBlockIOPSDevices: map[blockiops.Device]blockIOPSDeviceBounds{d: {Write: blockiops.RateBounds{ActivationMinimum: 0, ReleaseMaximum: 90}, Uncertain: true}}}
	if evaluateIOPressure(policy, sample, 50).exceeded() {
		t.Fatal("uncertain operation activated limits")
	}
	if !evaluateIOReleasePressure(policy, sample, 25).exceeded() {
		t.Fatal("uncertainty authorized unsafe release")
	}
	sample.IOBlockIOPSDevices[d] = blockIOPSDeviceBounds{Write: blockiops.RateBounds{ActivationMinimum: 40, ReleaseMaximum: 40}}
	sample.IOBlockIOPSDevices[blockiops.Device{Major: 253}] = sample.IOBlockIOPSDevices[d]
	if evaluateIOPressure(policy, sample, 50).exceeded() {
		t.Fatal("layered devices were summed")
	}
}

func TestIOPSReloadPreparesBeforePublicationAndPreservesRollback(t *testing.T) {
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(1000)}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &mockCgroupManager{}, 2)
	manager.cfg.IOEnabled = true
	oldCfg := manager.cfg
	oldSource := &fakeBlockIOPSSource{}
	old := &blockIOPSRuntime{source: oldSource, previous: make(map[int]blockiops.Sample)}
	manager.blockIOPS = old
	calls := 0
	manager.blockIOPSFactory = func(*config.Config) (blockiops.Source, error) {
		calls++
		return nil, errors.New("unsupported candidate")
	}
	candidate := config.DefaultConfig()
	candidate.IOEnabled = true
	candidate.ProcessExcludeList = []string{"different"}
	if finish, err := manager.PrepareBlockIOPSConfig(candidate); err == nil || finish != nil {
		t.Fatal("invalid source candidate was accepted")
	}
	if manager.GetConfig() != oldCfg || manager.blockIOPS != old || oldSource.closed {
		t.Fatal("preflight changed the published source")
	}
	nextSource := &fakeBlockIOPSSource{}
	manager.blockIOPSFactory = func(*config.Config) (blockiops.Source, error) { calls++; return nextSource, nil }
	finish, err := manager.PrepareBlockIOPSConfig(candidate)
	if err != nil {
		t.Fatal(err)
	}
	manager.UpdateConfig(candidate)
	if manager.blockIOPS == old || oldSource.closed {
		t.Fatal("staging did not preserve rollback source")
	}
	manager.UpdateConfig(oldCfg)
	finish()
	if manager.blockIOPS != old || oldSource.closed || !nextSource.closed {
		t.Fatal("rollback did not close only the candidate")
	}
	unchanged := config.DefaultConfig()
	unchanged.IOEnabled = true
	unchanged.PollingInterval++
	finish, err = manager.PrepareBlockIOPSConfig(unchanged)
	if err != nil {
		t.Fatal(err)
	}
	manager.UpdateConfig(unchanged)
	finish()
	if calls != 2 || manager.blockIOPS != old {
		t.Fatal("unrelated reload restarted the source")
	}
}

func TestIOPSObservationProjectionDoesNotAdvanceDecisionBaseline(t *testing.T) {
	d := blockiops.Device{Major: 8}
	source := &fakeBlockIOPSSource{}
	manager := &Manager{blockIOPS: &blockIOPSRuntime{source: source, previous: map[int]blockiops.Sample{1000: {Identity: blockiops.Identity{SourceGeneration: 7}}}}}
	sample := &SystemMetrics{IOBlockIOPSSourceAvailable: true, IOBlockIOPSDevices: map[blockiops.Device]blockIOPSDeviceBounds{d: {Write: blockiops.RateBounds{ActivationMinimum: 1, ReleaseMaximum: 2}, Uncertain: true}}}
	manager.rememberBlockIOPSDecision(sample)
	observed := &SystemMetrics{}
	manager.projectBlockIOPSDecision(observed)
	observed.IOBlockIOPSDevices[d] = blockIOPSDeviceBounds{}
	if manager.blockIOPS.previous[1000].Identity.SourceGeneration != 7 || manager.blockIOPSLast.devices[d].Write.ReleaseMaximum != 2 {
		t.Fatal("observation mutated decision evidence")
	}
}

func TestCertifiedIOPSComparisonDoesNotRoundQuota(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("large quota requires a 64-bit config integer")
	}
	base := int64(1) << 53
	for _, tc := range []struct {
		name      string
		quota     int
		users     int
		threshold int
		rate      float64
		exceeded  bool
	}{
		{"quota rounds down", int(base + 1), 1, 100, float64(base), false},
		{"above unrepresentable quota", int(base + 1), 1, 100, float64(base + 2), true},
		{"exact equality", 100, 3, 75, 225, true},
		{"below equality", 100, 3, 75, 224, false},
		{"integer product overflow", int(base), int(base), 100, float64(base), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := config.IODecisionPolicy{Enabled: true, WriteIOPS: tc.quota}
			sample := &SystemMetrics{IOEligibleUsersCount: tc.users, IOBlockIOPSDevices: map[blockiops.Device]blockIOPSDeviceBounds{{Major: 8}: {Write: blockiops.RateBounds{ActivationMinimum: tc.rate, ReleaseMaximum: tc.rate}}}}
			for _, release := range []bool{false, true} {
				if got := evaluateIOBounds(policy, sample, tc.threshold, release).exceeded(); got != tc.exceeded {
					t.Fatalf("release=%v: exceeded=%v want %v", release, got, tc.exceeded)
				}
			}
		})
	}
}
