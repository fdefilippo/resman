package metrics

import "testing"

func TestIODeviceWeightPrometheusPublishesIndependentLifecycleDimensions(t *testing.T) {
	exporter := newCPUPointsTestExporter(t)
	exporter.UpdateSystemSnapshot(SystemExporterMetrics{IODeviceWeight: &IODeviceWeightExporterMetrics{
		State: "refused_observation", Reason: "ambiguous_topology", Programmed: true,
		ReadBack: false, FunctionallyAccepted: false, EffectQualified: false,
		PartialUsers: 2, ObservedDelivery: "not_measured",
	}})

	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_state", "state", "refused_observation", 1)
	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_reason", "reason", "ambiguous_topology", 1)
	assertGaugeLabelValue(t, exporter, "resman_io_device_weight_observed_delivery", "state", "not_measured", 1)
	for name, want := range map[string]float64{
		"resman_io_device_weight_programmed":            1,
		"resman_io_device_weight_read_back":             0,
		"resman_io_device_weight_functionally_accepted": 0,
		"resman_io_device_weight_effect_qualified":      0,
		"resman_io_device_weight_partial_users":         2,
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

func TestIODeviceWeightPrometheusRejectsUnboundedLabels(t *testing.T) {
	exporter := newCPUPointsTestExporter(t)
	exporter.UpdateSystemSnapshot(SystemExporterMetrics{IODeviceWeight: &IODeviceWeightExporterMetrics{
		State: "invented", Reason: "device-/tmp/operator-value", ObservedDelivery: "invented",
	}})
	for _, name := range []string{
		"resman_io_device_weight_state",
		"resman_io_device_weight_reason",
		"resman_io_device_weight_observed_delivery",
	} {
		if hasMetricFamily(t, exporter, name) {
			t.Errorf("unbounded label created metric family %s", name)
		}
	}
}
