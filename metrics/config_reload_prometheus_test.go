package metrics

import (
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
)

func TestConfigReloadPrometheusStateDistinguishesPendingRefusalFromAcknowledgedEpoch(t *testing.T) {
	exporter := newCPUPointsTestExporter(t)
	assertGaugeLabelValue(t, exporter, "resman_config_reload_state", "state", "never", 1)

	refusedAt := time.Date(2026, 9, 1, 20, 0, 0, 0, time.UTC)
	exporter.ObserveConfigReload(config.ReloadObservation{
		State: config.ReloadStateRefused, Source: "automatic", Timestamp: refusedAt,
	})
	assertGaugeLabelValue(t, exporter, "resman_config_reload_state", "state", "never", 0)
	assertGaugeLabelValue(t, exporter, "resman_config_reload_state", "state", "refused", 1)
	if got := gatheredMetricValue(t, exporter, "resman_config_reload_last_attempt_timestamp_seconds"); got != float64(refusedAt.Unix()) {
		t.Fatalf("last attempt timestamp = %v, want %d", got, refusedAt.Unix())
	}
	if got := gatheredMetricValue(t, exporter, "resman_config_reload_last_success_timestamp_seconds"); got != 0 {
		t.Fatalf("last success advanced after refusal: %v", got)
	}

	failedAt := refusedAt.Add(30 * time.Second)
	exporter.ObserveConfigReload(config.ReloadObservation{
		State: config.ReloadStateFailed, Source: "automatic", Timestamp: failedAt,
	})
	assertGaugeLabelValue(t, exporter, "resman_config_reload_state", "state", "refused", 0)
	assertGaugeLabelValue(t, exporter, "resman_config_reload_state", "state", "failed", 1)
	if got := gatheredMetricValue(t, exporter, "resman_config_reload_last_attempt_timestamp_seconds"); got != float64(failedAt.Unix()) {
		t.Fatalf("failed attempt timestamp = %v, want %d", got, failedAt.Unix())
	}

	appliedAt := refusedAt.Add(time.Minute)
	exporter.ObserveConfigReload(config.ReloadObservation{
		State: config.ReloadStateApplied, Source: "forced", Processed: true, Timestamp: appliedAt,
	})
	assertGaugeLabelValue(t, exporter, "resman_config_reload_state", "state", "refused", 0)
	assertGaugeLabelValue(t, exporter, "resman_config_reload_state", "state", "failed", 0)
	assertGaugeLabelValue(t, exporter, "resman_config_reload_state", "state", "applied", 1)
	if got := gatheredMetricValue(t, exporter, "resman_config_reload_last_success_timestamp_seconds"); got != float64(appliedAt.Unix()) {
		t.Fatalf("last success timestamp = %v, want %d", got, appliedAt.Unix())
	}
}
