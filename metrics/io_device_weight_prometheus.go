package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// IODeviceWeightExporterMetrics is the bounded weighted-I/O lifecycle view.
type IODeviceWeightExporterMetrics struct {
	State                string
	Reason               string
	Programmed           bool
	ReadBack             bool
	FunctionallyAccepted bool
	EffectQualified      bool
	PartialUsers         int
	ObservedDelivery     string
}

type ioDeviceWeightPrometheusMetrics struct {
	state                  *prometheus.GaugeVec
	reason                 *prometheus.GaugeVec
	programmed             prometheus.Gauge
	readBack               prometheus.Gauge
	functionallyAccepted   prometheus.Gauge
	effectQualified        prometheus.Gauge
	partialUsers           prometheus.Gauge
	observedDelivery       *prometheus.GaugeVec
	classificationAttempts prometheus.Counter
	probeAttempts          prometheus.Counter
}

func (m *ioDeviceWeightPrometheusMetrics) register(registry prometheus.Registerer, namespace string, labels prometheus.Labels) {
	gauge := func(name, help string) prometheus.Gauge {
		return promauto.With(registry).NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help, ConstLabels: labels})
	}
	m.state = promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "io_device_weight_state", Help: "Bounded weighted-I/O lifecycle state", ConstLabels: labels}, []string{"state"})
	m.reason = promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "io_device_weight_reason", Help: "Bounded reason for the current weighted-I/O lifecycle state", ConstLabels: labels}, []string{"reason"})
	m.programmed = gauge("io_device_weight_programmed", "Whether at least one owned user-slice weight is programmed")
	m.readBack = gauge("io_device_weight_read_back", "Whether the complete programmed weighted-I/O plan was read back from systemd and the active kernel mechanism")
	m.functionallyAccepted = gauge("io_device_weight_functionally_accepted", "Whether the owned live probe accepted the configured device set")
	m.effectQualified = gauge("io_device_weight_effect_qualified", "Whether retained controlled-contention evidence qualifies this exact representative and mechanism")
	m.partialUsers = gauge("io_device_weight_partial_users", "Number of programmed user slices whose observed UID workload coverage is partial")
	m.observedDelivery = promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "io_device_weight_observed_delivery", Help: "Bounded observation of delivered weighted-I/O effect; production policy currently reports not_measured", ConstLabels: labels}, []string{"state"})
	m.classificationAttempts = promauto.With(registry).NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: "io_device_weight_classification_attempts_total", Help: "Total read-only weighted-I/O capability classifications and confirmations", ConstLabels: labels})
	m.probeAttempts = promauto.With(registry).NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: "io_device_weight_probe_attempts_total", Help: "Total owned transient weighted-I/O probe attempts", ConstLabels: labels})
}

func (m *ioDeviceWeightPrometheusMetrics) update(snapshot IODeviceWeightExporterMetrics) {
	m.state.Reset()
	if validIODeviceWeightState(snapshot.State) {
		m.state.WithLabelValues(snapshot.State).Set(1)
	}
	m.reason.Reset()
	if snapshot.Reason != "" && validIODeviceWeightReason(snapshot.Reason) {
		m.reason.WithLabelValues(snapshot.Reason).Set(1)
	}
	m.programmed.Set(boolMetricValue(snapshot.Programmed))
	m.readBack.Set(boolMetricValue(snapshot.ReadBack))
	m.functionallyAccepted.Set(boolMetricValue(snapshot.FunctionallyAccepted))
	m.effectQualified.Set(boolMetricValue(snapshot.EffectQualified))
	m.partialUsers.Set(float64(snapshot.PartialUsers))
	m.observedDelivery.Reset()
	if snapshot.ObservedDelivery == "not_measured" {
		m.observedDelivery.WithLabelValues(snapshot.ObservedDelivery).Set(1)
	}
}

func validIODeviceWeightState(value string) bool {
	switch value {
	case "disabled", "requested_pending", "refused_observation", "refused_intervention", "probe_candidate", "functionally_accepted":
		return true
	default:
		return false
	}
}

func validIODeviceWeightReason(value string) bool {
	switch value {
	case "adapter_unavailable", "ambiguous_topology", "authority_unavailable", "cancelled_generation", "configuration_changed",
		"apply_unavailable", "capability_changed", "device_identity_changed", "device_missing", "duplicate_device",
		"empty_enabled_plan", "evidence_unavailable", "invalid_classifier_input", "invalid_selector",
		"io_cost_changed", "mechanism_ambiguous", "mechanism_unsupported", "no_active_mechanism",
		"probe_failed", "probe_unavailable", "readback_unavailable", "scheduler_changed", "topology_changed",
		"topology_unavailable", "unsafe_restore", "adapter_closed", "authorization_denied",
		"bus_unavailable", "capability_probe_failed", "external_property_conflict", "invalid_value",
		"kernel_verification_failed", "lease_store_failed", "malformed_reply", "property_not_allowed",
		"readback_mismatch", "required_capability_unavailable", "timeout", "unit_file_verification_failed",
		"unit_missing", "unit_recreated":
		return true
	default:
		return false
	}
}
