package metrics

import (
	"testing"
	"time"
)

func TestIODeviceWeightPrometheusPublishesIndependentLifecycleDimensions(t *testing.T) {
	exporter := newCPUPointsTestExporter(t)
	exporter.UpdateSystemSnapshot(SystemExporterMetrics{IODeviceWeight: &IODeviceWeightExporterMetrics{
		State: "refused_observation", Reason: "ambiguous_topology", Mechanism: "bfq", Programmed: true,
		ProgrammedState: "confirmed", ReadBack: false, ReadBackState: "failed",
		FunctionallyAccepted: false, EffectQualified: false, AuthorityCoverage: "partial",
		CompleteUsers: 3, PartialUsers: 2, UnavailableUsers: 1, SiblingSlices: 6, TotalPoints: 1500,
		RequestedAt: time.Now().Add(-time.Minute), NextRetryAt: time.Now().Add(time.Minute),
		Values:           []IODeviceWeightValueMetrics{{UID: 1000, Class: "mapped", Device: "8:0", Mechanism: "bfq", Coverage: "partial", RequestedValue: 700, SystemdValue: 6700, NominalShare: 0.5, Programmed: true}},
		ObservedDelivery: "not_measured",
	}})

	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_state", "state", "refused_observation", 1)
	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_reason", "reason", "ambiguous_topology", 1)
	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_observed_delivery", "state", "not_measured", 1)
	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_mechanism", "mechanism", "bfq", 1)
	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_programming_state", "state", "confirmed", 1)
	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_readback_state", "state", "failed", 1)
	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_authority_coverage", "coverage", "partial", 1)
	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_value", "stage", "requested", 700)
	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_value", "stage", "systemd", 6700)
	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_nominal_share", "uid", "1000", 0.5)
	for name, want := range map[string]float64{
		"resman_io_device_weight_programmed":            1,
		"resman_io_device_weight_read_back":             0,
		"resman_io_device_weight_functionally_accepted": 0,
		"resman_io_device_weight_effect_qualified":      0,
		"resman_io_device_weight_partial_users":         2,
		"resman_io_device_weight_complete_users":        3,
		"resman_io_device_weight_unavailable_users":     1,
		"resman_io_device_weight_sibling_slices":        6,
		"resman_io_device_weight_total_points":          1500,
	} {
		if got := gatheredMetricValue(t, exporter, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}

	exporter.IncrementIODeviceWeightClassification()
	exporter.IncrementIODeviceWeightClassification()
	exporter.IncrementIODeviceWeightProbe()
	if got := gatheredMetricValue(t, exporter, "resman_io_device_weight_classification_attempts_total"); got != 2 {
		t.Fatalf("classification attempts = %v, want 2", got)
	}
	if got := gatheredMetricValue(t, exporter, "resman_io_device_weight_probe_attempts_total"); got != 1 {
		t.Fatalf("probe attempts = %v, want 1", got)
	}
}

func TestIODeviceWeightPrometheusBoundsUnknownLabels(t *testing.T) {
	exporter := newCPUPointsTestExporter(t)
	exporter.UpdateSystemSnapshot(SystemExporterMetrics{IODeviceWeight: &IODeviceWeightExporterMetrics{
		State: "invented", Reason: "device-/tmp/operator-value", ObservedDelivery: "invented",
	}})
	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_state", "state", "unknown", 1)
	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_reason", "reason", "unknown", 1)
	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_observed_delivery", "state", "unknown", 1)
}
