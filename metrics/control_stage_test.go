package metrics

import (
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
)

func TestControlStageHistogramRejectsUnboundedLabels(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	exp, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []ControlStageObservation{
		{Stage: StageCollectMetrics, Outcome: StageSucceeded, Duration: time.Second},
		{Stage: StageExecuteDecision, Outcome: StageFailed, Duration: 2 * time.Second},
		{Stage: "uid_1000", Outcome: StageSucceeded},
		{Stage: StageCollectMetrics, Outcome: "raw error"},
	} {
		exp.ObserveControlStage(o)
	}
	families, err := exp.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "resman_control_stage_duration_seconds" {
			continue
		}
		if len(family.Metric) != 2 {
			t.Fatalf("series = %d, want 2", len(family.Metric))
		}
		for _, m := range family.Metric {
			if m.GetHistogram().GetSampleCount() != 1 {
				t.Fatal("wrong histogram count")
			}
		}
		return
	}
	t.Fatal("stage histogram missing")
}
