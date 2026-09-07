package state

import (
	"context"
	"testing"

	"github.com/fdefilippo/resman/config"
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
