package state

import (
	"context"
	"testing"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/systemdunit"
	"github.com/fdefilippo/resman/metrics"
)

func TestSystemdNativeBlackoutSuppressesIntentWhileObservationRemainsIndependent(t *testing.T) {
	for _, blackout := range []string{"* 00-24", ""} {
		t.Run(blackout, func(t *testing.T) {
			policy := testCPUPointsPolicy(t, map[string]struct{ uid, points int }{"alice": {1000, 300}})
			adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
			manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
			cfg := manager.cfg
			cfg.UserIncludeList = []string{".*"}
			cfg.CPUThreshold = 2
			cfg.CPUReleaseThreshold = 1
			cfg.CPUThresholdDuration = 0
			cfg.IgnoreSystemLoad = true
			cfg.BlackoutSpec = blackout
			var err error
			cfg.BlackoutTimeframes, err = config.ParseTimeframe(blackout)
			if err != nil {
				t.Fatal(err)
			}
			collector := manager.metricsCollector.(*mockMetricsCollector)
			collector.allUserMetrics = map[int]*metrics.UserMetrics{1000: {
				UID: 1000, Username: "alice", CPUUsage: 300, CPUUsageEMA: 300, CPUUsageAverage: 300, ProcessCount: 3,
			}}
			if err := manager.RunMetricsRefresh(context.Background(), "blackout-observation"); err != nil {
				t.Fatal(err)
			}
			if collector.observationCalls == 0 || collector.decisionCalls != 0 || len(adapter.applies) != 0 {
				t.Fatal("observation did not remain independent of enforcement")
			}
			if err := manager.RunControlCycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if blackout != "" {
				if manager.systemdCPURequested || manager.limitsActive || len(adapter.applies) != 0 {
					t.Fatal("blackout allowed activation intent or native enforcement")
				}
			} else if !manager.systemdCPURequested || !manager.limitsActive || len(adapter.applies) == 0 {
				t.Fatal("same above-threshold fixture without blackout did not activate through systemd")
			}
		})
	}
}

func TestSystemdNativeBlackoutReleasesStandaloneIODeviceWeights(t *testing.T) {
	topology := testSystemdTopology(1000)
	adapter := &fakeSystemdCPUUnitAdapter{topology: topology}
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.IOWeightDevices = "8:0"
	classifier := &fakeIODeviceWeightClassifier{snapshot: testIODeviceWeightCandidate(t)}
	if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
		t.Fatal(err)
	}
	if result := manager.AttemptIODeviceWeightCapability(context.Background()); result.Status.State != IODeviceWeightFunctionallyAccepted {
		t.Fatalf("AttemptIODeviceWeightCapability() = %+v", result)
	}
	if err := manager.reconcileSystemdIODeviceWeights(context.Background(), completeIODeviceWeightSample(topology), manager.cfg); err != nil {
		t.Fatalf("reconcileSystemdIODeviceWeights() error = %v", err)
	}
	identity := topology.Users[0].Unit.Identity
	if !adapter.activeProperties[identity.Name][systemdunit.PropertyIODeviceWeight] {
		t.Fatal("weighted-I/O plan was not active before blackout")
	}
	manager.cfg.BlackoutSpec = "* 00-24"
	var err error
	manager.cfg.BlackoutTimeframes, err = config.ParseTimeframe(manager.cfg.BlackoutSpec)
	if err != nil {
		t.Fatal(err)
	}
	run := &controlCycleContext{ctx: context.Background(), cfg: manager.cfg, cycleID: 1, trigger: "test"}
	if err := manager.stageCheckBlackout(run); err != nil {
		t.Fatalf("stageCheckBlackout() error = %v", err)
	}
	status := manager.GetIODeviceWeightStatus()
	if status.State != IODeviceWeightFunctionallyAccepted || status.Programmed || status.ReadBack || adapter.activeProperties[identity.Name][systemdunit.PropertyIODeviceWeight] {
		t.Fatalf("blackout did not release standalone weights truthfully: status=%+v active=%+v", status, adapter.activeProperties)
	}
}
