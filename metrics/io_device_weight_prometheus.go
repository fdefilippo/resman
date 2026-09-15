package metrics

import (
	"strconv"
	"time"

	"github.com/fdefilippo/resman/internal/ioweights"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// IODeviceWeightExporterMetrics is the bounded weighted-I/O lifecycle view.
type IODeviceWeightExporterMetrics struct {
	State                string
	Reason               string
	Mechanism            string
	Programmed           bool
	ProgrammedState      string
	ReadBack             bool
	ReadBackState        string
	FunctionallyAccepted bool
	EffectQualified      bool
	AuthorityCoverage    string
	CompleteUsers        int
	PartialUsers         int
	UnavailableUsers     int
	SiblingSlices        int
	TotalPoints          uint64
	RequestedAt          time.Time
	NextRetryAt          time.Time
	Values               []IODeviceWeightValueMetrics
	ObservedDelivery     string
}

// IODeviceWeightValueMetrics is one bounded per-slice, per-device value path.
type IODeviceWeightValueMetrics struct {
	UID            int
	Class          string
	Device         string
	Mechanism      string
	Coverage       string
	RequestedValue uint64
	SystemdValue   uint64
	KernelValue    uint64
	NominalShare   float64
	Programmed     bool
	ReadBack       bool
}

type ioDeviceWeightPrometheusMetrics struct {
	state                  *prometheus.GaugeVec
	reason                 *prometheus.GaugeVec
	mechanism              *prometheus.GaugeVec
	programmedState        *prometheus.GaugeVec
	readBackState          *prometheus.GaugeVec
	authorityCoverage      *prometheus.GaugeVec
	programmed             prometheus.Gauge
	readBack               prometheus.Gauge
	functionallyAccepted   prometheus.Gauge
	effectQualified        prometheus.Gauge
	partialUsers           prometheus.Gauge
	completeUsers          prometheus.Gauge
	unavailableUsers       prometheus.Gauge
	siblingSlices          prometheus.Gauge
	totalPoints            prometheus.Gauge
	requestedSeconds       prometheus.Gauge
	nextRetrySeconds       prometheus.Gauge
	values                 *prometheus.GaugeVec
	nominalShare           *prometheus.GaugeVec
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
	m.mechanism = promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "io_device_weight_mechanism", Help: "Selected kernel weighted-I/O mechanism: none, bfq, io_cost, or mixed", ConstLabels: labels}, []string{"mechanism"})
	m.programmedState = promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "io_device_weight_programming_state", Help: "Bounded mutation state: not_attempted, confirmed, failed, or released", ConstLabels: labels}, []string{"state"})
	m.readBackState = promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "io_device_weight_readback_state", Help: "Bounded readback state: not_attempted, confirmed, failed, or released", ConstLabels: labels}, []string{"state"})
	m.authorityCoverage = promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "io_device_weight_authority_coverage", Help: "Atomic authority coverage: complete, partial, or unavailable", ConstLabels: labels}, []string{"coverage"})
	m.programmed = gauge("io_device_weight_programmed", "Whether at least one owned user-slice weight is programmed")
	m.readBack = gauge("io_device_weight_read_back", "Whether the complete programmed weighted-I/O plan was read back from systemd and the active kernel mechanism")
	m.functionallyAccepted = gauge("io_device_weight_functionally_accepted", "Whether the owned live probe accepted the configured device set")
	m.effectQualified = gauge("io_device_weight_effect_qualified", "Whether retained controlled-contention evidence qualifies this exact representative and mechanism")
	m.partialUsers = gauge("io_device_weight_partial_users", "Number of programmed user slices whose observed UID workload coverage is partial")
	m.completeUsers = gauge("io_device_weight_complete_users", "Number of sibling slices with complete authority coverage")
	m.unavailableUsers = gauge("io_device_weight_unavailable_users", "Number of sibling slices unavailable in the captured authority inventory")
	m.siblingSlices = gauge("io_device_weight_sibling_slices", "Number of user.slice sibling slices in the current weighted-I/O denominator")
	m.totalPoints = gauge("io_device_weight_total_points", "Sum of requested weighted-I/O points across current sibling slices")
	m.requestedSeconds = gauge("io_device_weight_requested_seconds", "Seconds since the current weighted-I/O request was published; zero while disabled")
	m.nextRetrySeconds = gauge("io_device_weight_next_retry_seconds", "Seconds until the next post-READY weighted-I/O classification or confirmation; zero when none is scheduled")
	m.values = promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "io_device_weight_value", Help: "Exact requested and systemd-derived weights plus the expected kernel-domain weight confirmed present by sibling and device", ConstLabels: labels}, []string{"uid", "class", "device", "mechanism", "coverage", "stage"})
	m.nominalShare = promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "io_device_weight_nominal_share", Help: "Nominal requested share across all discovered sibling slices, including unavailable authority; this is not delivered throughput", ConstLabels: labels}, []string{"uid", "class", "device", "mechanism", "coverage"})
	m.observedDelivery = promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "io_device_weight_observed_delivery", Help: "Bounded observation of delivered weighted-I/O effect; production policy currently reports not_measured", ConstLabels: labels}, []string{"state"})
	m.classificationAttempts = promauto.With(registry).NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: "io_device_weight_classification_attempts_total", Help: "Total post-READY weighted-I/O classifier Classify and Confirm calls", ConstLabels: labels})
	m.probeAttempts = promauto.With(registry).NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: "io_device_weight_probe_attempts_total", Help: "Total owned transient weighted-I/O probe attempts", ConstLabels: labels})
}

func (m *ioDeviceWeightPrometheusMetrics) update(snapshot IODeviceWeightExporterMetrics) {
	m.state.Reset()
	state := snapshot.State
	if !ioweights.ValidActivationState(state) {
		state = string(ioweights.ReasonUnknown)
	}
	m.state.WithLabelValues(state).Set(1)
	m.reason.Reset()
	if snapshot.Reason != "" {
		reason := snapshot.Reason
		if !ioweights.ValidReason(reason) {
			reason = ioweights.ReasonUnknown
		}
		m.reason.WithLabelValues(reason).Set(1)
	}
	m.mechanism.Reset()
	mechanism := snapshot.Mechanism
	if !ioweights.ValidMechanismState(mechanism) {
		mechanism = "unknown"
	}
	m.mechanism.WithLabelValues(mechanism).Set(1)
	updateVerificationState := func(metric *prometheus.GaugeVec, value string) {
		metric.Reset()
		if !ioweights.ValidVerificationState(value) {
			value = "unknown"
		}
		metric.WithLabelValues(value).Set(1)
	}
	updateVerificationState(m.programmedState, snapshot.ProgrammedState)
	updateVerificationState(m.readBackState, snapshot.ReadBackState)
	m.authorityCoverage.Reset()
	coverage := snapshot.AuthorityCoverage
	if !ioweights.ValidAuthorityCoverage(coverage) {
		coverage = "unknown"
	}
	m.authorityCoverage.WithLabelValues(coverage).Set(1)
	m.programmed.Set(boolMetricValue(snapshot.Programmed))
	m.readBack.Set(boolMetricValue(snapshot.ReadBack))
	m.functionallyAccepted.Set(boolMetricValue(snapshot.FunctionallyAccepted))
	m.effectQualified.Set(boolMetricValue(snapshot.EffectQualified))
	m.partialUsers.Set(float64(snapshot.PartialUsers))
	m.completeUsers.Set(float64(snapshot.CompleteUsers))
	m.unavailableUsers.Set(float64(snapshot.UnavailableUsers))
	m.siblingSlices.Set(float64(snapshot.SiblingSlices))
	m.totalPoints.Set(float64(snapshot.TotalPoints))
	now := time.Now()
	requestedSeconds := 0.0
	if !snapshot.RequestedAt.IsZero() {
		requestedSeconds = max(0, now.Sub(snapshot.RequestedAt).Seconds())
	}
	m.requestedSeconds.Set(requestedSeconds)
	nextRetrySeconds := 0.0
	if !snapshot.NextRetryAt.IsZero() {
		nextRetrySeconds = max(0, snapshot.NextRetryAt.Sub(now).Seconds())
	}
	m.nextRetrySeconds.Set(nextRetrySeconds)
	m.values.Reset()
	m.nominalShare.Reset()
	for _, value := range snapshot.Values {
		labels := []string{strconv.Itoa(value.UID), value.Class, value.Device, value.Mechanism, value.Coverage}
		m.nominalShare.WithLabelValues(labels...).Set(value.NominalShare)
		m.values.WithLabelValues(append(labels, "requested")...).Set(float64(value.RequestedValue))
		if value.Programmed {
			m.values.WithLabelValues(append(labels, "systemd")...).Set(float64(value.SystemdValue))
		}
		if value.ReadBack {
			m.values.WithLabelValues(append(labels, "kernel")...).Set(float64(value.KernelValue))
		}
	}
	m.observedDelivery.Reset()
	delivery := snapshot.ObservedDelivery
	if !ioweights.ValidDeliveryState(delivery) {
		delivery = ioweights.ReasonUnknown
	}
	m.observedDelivery.WithLabelValues(delivery).Set(1)
}
