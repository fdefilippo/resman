package state

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/systemdunit"
)

func TestControlCycleCancellationNeverHidesAnIndependentFailure(t *testing.T) {
	for _, tt := range []struct {
		name   string
		cancel bool
		err    error
		want   bool
	}{
		{"owned cancellation", true, fmt.Errorf("wrapped: %w", context.Canceled), true},
		{"joined cancellations", true, errors.Join(context.Canceled, fmt.Errorf("nested: %w", context.Canceled)), true},
		{"live caller", false, context.Canceled, false},
		{"timeout", true, context.DeadlineExceeded, false},
		{"other failure", true, errors.New("conflict"), false},
		{"joined real failure", true, errors.Join(context.Canceled, errors.New("readback failure")), false},
		{"success", true, nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			if got := IsControlCycleCancellation(ctx, tt.err); got != tt.want {
				t.Fatalf("classification=%v, want %v", got, tt.want)
			}
			exporter := &mockPrometheusExporter{}
			manager := &Manager{prometheusExporter: exporter}
			if tt.err != nil {
				err := manager.deferSystemdCPUPointsError(ctx, "readback", tt.err)
				if !errors.Is(err, tt.err) {
					t.Fatalf("lost failure: %v", err)
				}
				wantErrors := 1
				if tt.want {
					wantErrors = 0
				}
				if got := len(exporter.recordedErrors()); got != wantErrors {
					t.Fatalf("error counter=%d, want %d", got, wantErrors)
				}
			}
		})
	}
}

type cancelingTopologyAdapter struct {
	*fakeSystemdCPUUnitAdapter
	cancel context.CancelFunc
}

func (a *cancelingTopologyAdapter) Discover(ctx context.Context) (systemdunit.TopologySnapshot, error) {
	a.cancel()
	return systemdunit.TopologySnapshot{}, fmt.Errorf("read_slice: %w", ctx.Err())
}

func TestNativeInFlightCancellationDoesNotCountAnAdapterFailure(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct{ uid, points int }{"alice": {1000, 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.systemdUnits = &cancelingTopologyAdapter{fakeSystemdCPUUnitAdapter: adapter, cancel: cancel}
	manager.systemdCPURequested = true
	err := manager.RunControlCycle(ctx)
	if !IsControlCycleCancellation(ctx, err) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if records := manager.prometheusExporter.(*mockPrometheusExporter).recordedErrors(); len(records) != 0 {
		t.Fatalf("cancellation counted as failure: %+v", records)
	}
	if manager.metricsCollector.(*mockMetricsCollector).decisionCalls != 0 {
		t.Fatal("canceled reconciliation advanced decision baselines")
	}
}

func TestCanceledNativeCycleStopsBeforeDecisionAndPreservesPriorErrors(t *testing.T) {
	for _, prior := range []error{nil, errors.New("external conflict")} {
		ctx, cancel := context.WithCancel(context.Background())
		run := &controlCycleContext{ctx: ctx, cfg: config.DefaultConfig()}
		err := runControlCyclePipeline(&Manager{}, run, []controlCycleStage{
			{name: "in_flight", continueAfterError: true, run: func(*Manager, *controlCycleContext) error { cancel(); return errors.Join(prior, context.Canceled) }},
			{name: "decision", run: func(*Manager, *controlCycleContext) error {
				t.Fatal("canceled cycle advanced decision state")
				return nil
			}},
		})
		if !errors.Is(err, context.Canceled) || (prior != nil && !errors.Is(err, prior)) {
			t.Fatalf("lost cycle result: %v", err)
		}
	}
}
