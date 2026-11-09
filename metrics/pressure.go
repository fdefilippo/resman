package metrics

import (
	"github.com/fdefilippo/resman/internal/pressure"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// pressureMetrics publishes a bounded fixed resource/stall/window inventory.
type pressureMetrics struct {
	percent      *prometheus.GaugeVec
	available    *prometheus.GaugeVec
	sampledAt    *prometheus.GaugeVec
	sourceStatus *prometheus.GaugeVec
}

func newPressureMetrics(registry *prometheus.Registry, namespace string, labels prometheus.Labels) *pressureMetrics {
	factory := promauto.With(registry)
	return &pressureMetrics{
		percent:      factory.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "psi_pressure_percent", Help: "Kernel system PSI averages in percent; only available resource and stall modes are present", ConstLabels: labels}, []string{"type", "scope", "stall", "window_seconds"}),
		available:    factory.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "psi_pressure_available", Help: "Whether the decision-owned system PSI observation is available for the resource and stall mode", ConstLabels: labels}, []string{"type", "scope", "stall"}),
		sampledAt:    factory.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "psi_pressure_sample_timestamp_seconds", Help: "Timestamp of the last decision-owned system PSI sampling attempt; observation refreshes do not renew it", ConstLabels: labels}, []string{"type", "scope"}),
		sourceStatus: factory.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "psi_pressure_source_status", Help: "One-hot bounded status of the last system PSI sampling attempt", ConstLabels: labels}, []string{"type", "scope", "reason"}),
	}
}

func (p *pressureMetrics) update(snapshot pressure.Snapshot) {
	for _, resource := range []struct {
		kind   pressure.Resource
		sample pressure.Measurement
	}{
		{pressure.CPU, snapshot.CPU}, {pressure.IO, snapshot.IO}, {pressure.Memory, snapshot.Memory},
	} {
		kind := string(resource.kind)
		sample := resource.sample
		reason := sample.UnavailableReason
		if sample.Available {
			reason = pressure.ReasonNone
		} else if reason != pressure.ReasonRead && reason != pressure.ReasonInvalid {
			reason = pressure.ReasonInvalid
		}
		for _, status := range []pressure.UnavailableReason{pressure.ReasonNone, pressure.ReasonRead, pressure.ReasonInvalid} {
			p.sourceStatus.WithLabelValues(kind, "system", string(status)).Set(boolMetricValue(status == reason))
		}
		at := float64(0)
		if !snapshot.At.IsZero() {
			at = float64(snapshot.At.UnixNano()) / 1e9
		}
		p.sampledAt.WithLabelValues(kind, "system").Set(at)
		fullAvailable := sample.Available && sample.Stats.FullAvailable && resource.kind != pressure.CPU
		for _, mode := range []struct {
			name      string
			available bool
			values    [3]float64
		}{
			{"some", sample.Available, [3]float64{sample.Stats.SomeAvg10, sample.Stats.SomeAvg60, sample.Stats.SomeAvg300}},
			{"full", fullAvailable, [3]float64{sample.Stats.FullAvg10, sample.Stats.FullAvg60, sample.Stats.FullAvg300}},
		} {
			p.available.WithLabelValues(kind, "system", mode.name).Set(boolMetricValue(mode.available))
			for i, window := range []string{"10", "60", "300"} {
				if mode.available {
					p.percent.WithLabelValues(kind, "system", mode.name, window).Set(mode.values[i])
				} else {
					p.percent.DeleteLabelValues(kind, "system", mode.name, window)
				}
			}
		}
	}
}
