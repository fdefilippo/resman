package metrics

import (
	"github.com/fdefilippo/resman/config"
	"testing"
)

func TestLimitHookAttemptMetricCountsCallsWithBoundedLabels(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	exp, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	exp.RecordLimitHookAttempt(LimitHookTypeHTTP, LimitHookOutcomeFailure)
	exp.RecordLimitHookAttempt(LimitHookTypeHTTP, LimitHookOutcomeSuccess)
	exp.RecordLimitHookAttempt("unbounded", LimitHookOutcomeFailure)
	exp.RecordLimitHookAttempt(LimitHookTypeScript, "raw error")
	families, err := exp.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "resman_limit_hook_attempts_total" {
			if len(family.Metric) != 2 {
				t.Fatalf("attempt series=%d", len(family.Metric))
			}
			for _, series := range family.Metric {
				if series.GetCounter().GetValue() != 1 {
					t.Fatalf("counter=%v", series)
				}
			}
			return
		}
	}
	t.Fatal("attempt metric missing")
}
