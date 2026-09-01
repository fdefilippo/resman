package metrics

import (
	"github.com/fdefilippo/resman/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var configReloadStates = []config.ReloadState{
	config.ReloadState("never"),
	config.ReloadStateApplied,
	config.ReloadStateRefused,
	config.ReloadStateFailed,
}

type configReloadPrometheusMetrics struct {
	state                *prometheus.GaugeVec
	lastAttemptTimestamp prometheus.Gauge
	lastSuccessTimestamp prometheus.Gauge
}

func (m *configReloadPrometheusMetrics) register(registry prometheus.Registerer, namespace string, labels prometheus.Labels) {
	m.state = promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "config_reload_state",
		Help:        "Current bounded reload state: never, applied, refused (disk candidate pending while the prior epoch remains active), or failed",
		ConstLabels: labels,
	}, []string{"state"})
	m.lastAttemptTimestamp = promauto.With(registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "config_reload_last_attempt_timestamp_seconds",
		Help:        "Unix timestamp of the latest terminal configuration reload attempt",
		ConstLabels: labels,
	})
	m.lastSuccessTimestamp = promauto.With(registry).NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "config_reload_last_success_timestamp_seconds",
		Help:        "Unix timestamp of the latest acknowledged configuration epoch",
		ConstLabels: labels,
	})
	m.setState(config.ReloadState("never"))
}

func (m *configReloadPrometheusMetrics) observe(observation config.ReloadObservation) {
	m.setState(observation.State)
	m.lastAttemptTimestamp.Set(float64(observation.Timestamp.Unix()))
	if observation.State == config.ReloadStateApplied {
		m.lastSuccessTimestamp.Set(float64(observation.Timestamp.Unix()))
	}
}

func (m *configReloadPrometheusMetrics) setState(current config.ReloadState) {
	for _, state := range configReloadStates {
		value := float64(0)
		if state == current {
			value = 1
		}
		m.state.WithLabelValues(string(state)).Set(value)
	}
}
