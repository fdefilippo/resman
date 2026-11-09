package reloader

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/cpupoints"
)

type rejectingBlockIOPSStateManager struct {
	testCPUPointsStateManager
	failure error
}

func (m *rejectingBlockIOPSStateManager) PrepareBlockIOPSConfig(*config.Config) (func(), error) {
	return nil, m.failure
}

func TestBlockIOPSPreflightRejectsBothReloadPathsBeforeMutation(t *testing.T) {
	for _, composite := range []bool{false, true} {
		name := "configuration"
		if composite {
			name = "composite candidate"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cpu-points.map")
			policy := loadReloaderPolicy(t, path, cpupoints.PolicyMapMarker+"\nalice=300\n")
			current := config.DefaultConfig()
			current.CPUPointsFile = path
			candidate := config.DefaultConfig()
			candidate.CPUPointsFile = path
			candidate.IOEnabled = true
			failure := errors.New("mandatory IOPS source unavailable")
			manager := &rejectingBlockIOPSStateManager{testCPUPointsStateManager: testCPUPointsStateManager{testStateConfigManager: testStateConfigManager{cfg: current}, policy: policy}, failure: failure}
			cgroups := &testCgroupConfigManager{}
			collector := &testMetricsConfigCollector{}
			hooked := false
			reloader := NewReloader(manager, cgroups, collector, func(*config.Config) error { hooked = true; return nil })
			reloader.identityResolver = reloaderResolver{"alice": 1000}
			var err error
			if composite {
				outcome := reloader.OnConfigCandidate(candidate, func() error { return nil })
				err = outcome.Err
				if outcome.Published || outcome.Processed {
					t.Fatal("failed source candidate was published or processed")
				}
			} else {
				err = reloader.OnConfigChange(candidate)
			}
			if !errors.Is(err, failure) {
				t.Fatalf("source preflight error lost: %v", err)
			}
			if manager.cfg != current || manager.updates != 0 || len(manager.reconciled) != 0 || manager.published != 0 || cgroups.cfg != nil || collector.cfg != nil || hooked {
				t.Fatal("source failure mutated enforcement or a configuration consumer")
			}
		})
	}
}
