package state

import (
	"context"
	"errors"
	"testing"

	"github.com/fdefilippo/resman/metrics"
)

type stageRecorder struct {
	mockPrometheusExporter
	observations []metrics.ControlStageObservation
}

func (e *stageRecorder) ObserveControlStage(o metrics.ControlStageObservation) {
	e.observations = append(e.observations, o)
}

func TestControlStagesObserveOnlyExecutedWork(t *testing.T) {
	for _, tt := range []struct {
		name               string
		err                error
		suspend, continued bool
		outcome            metrics.ControlStageOutcome
		count              int
	}{
		{name: "success", outcome: metrics.StageSucceeded, count: 2},
		{name: "fatal", err: errors.New("failed"), outcome: metrics.StageFailed, count: 1},
		{name: "continued error", err: errors.New("failed"), continued: true, outcome: metrics.StageFailed, count: 2},
		{name: "suspended", suspend: true, outcome: metrics.StageSuspended, count: 1},
		{name: "cancelled", err: context.Canceled, outcome: metrics.StageCancelled, count: 1},
		{name: "cancelled with independent failure", err: errors.Join(context.Canceled, errors.New("failed")), outcome: metrics.StageFailed, count: 1},
		{name: "component deadline", err: context.DeadlineExceeded, outcome: metrics.StageFailed, count: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			exporter := &stageRecorder{}
			run := &controlCycleContext{ctx: context.Background()}
			stages := []controlCycleStage{
				{name: metrics.StageCheckBlackout, continueAfterError: tt.continued, run: func(_ *Manager, r *controlCycleContext) error { r.stopWithoutError = tt.suspend; return tt.err }},
				{name: metrics.StageCollectMetrics, run: func(*Manager, *controlCycleContext) error { return nil }},
			}
			err := runControlCyclePipeline(&Manager{prometheusExporter: exporter}, run, stages)
			if !errors.Is(err, tt.err) {
				t.Fatalf("error = %v, want %v", err, tt.err)
			}
			if len(exporter.observations) != tt.count {
				t.Fatalf("observations = %d, want %d", len(exporter.observations), tt.count)
			}
			o := exporter.observations[0]
			if o.Stage != metrics.StageCheckBlackout || o.Outcome != tt.outcome || o.Duration < 0 {
				t.Fatalf("observation = %+v", o)
			}
		})
	}
}
