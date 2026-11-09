package metrics

import (
	"fmt"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/pressure"
)

func pressureSeries(t *testing.T, exp *PrometheusExporter, name string) map[string]float64 {
	t.Helper()
	families, err := exp.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]float64{}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			key := fmt.Sprintf("%s/%s/%s/%s/%s", labels["type"], labels["scope"], labels["stall"], labels["window_seconds"], labels["reason"])
			result[key] = metric.GetGauge().GetValue()
		}
	}
	return result
}

func TestPressureGaugesDeleteUnavailableInsteadOfPublishingZeros(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.EnablePrometheus = true
	exp, err := NewPrometheusExporter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1700000000, 0)
	available := pressure.Measurement{Available: true, Stats: pressure.Stats{SomeAvg10: 5, SomeAvg60: 2, SomeAvg300: 1, FullAvailable: true, FullAvg10: 3, FullAvg60: 1, FullAvg300: 0}}
	snapshot := pressure.Snapshot{At: at, CPU: available, IO: available, Memory: available}
	exp.UpdateSystemSnapshot(SystemExporterMetrics{Pressure: &snapshot})
	values := pressureSeries(t, exp, "resman_psi_pressure_percent")
	if len(values) != 15 || values["memory/system/full/10/"] != 3 {
		t.Fatalf("pressure values=%v", values)
	}
	if _, found := values["cpu/system/full/10/"]; found {
		t.Fatal("undefined host CPU full published")
	}
	// Host-only refreshes must not erase or renew the decision sample.
	exp.UpdateSystemSnapshot(SystemExporterMetrics{})
	if got := pressureSeries(t, exp, "resman_psi_pressure_sample_timestamp_seconds")["memory/system///"]; got != float64(at.Unix()) {
		t.Fatalf("refresh changed pressure age: %v", got)
	}
	snapshot.At = at.Add(time.Second)
	snapshot.Memory = pressure.Measurement{UnavailableReason: pressure.ReasonRead}
	exp.UpdateSystemSnapshot(SystemExporterMetrics{Pressure: &snapshot})
	values = pressureSeries(t, exp, "resman_psi_pressure_percent")
	if len(values) != 9 {
		t.Fatalf("stale unavailable series remain: %v", values)
	}
	if _, found := values["memory/system/some/10/"]; found {
		t.Fatal("unavailable memory became a measured zero")
	}
	if got := pressureSeries(t, exp, "resman_psi_pressure_available")["memory/system/some//"]; got != 0 {
		t.Fatalf("available=%v", got)
	}
	if got := pressureSeries(t, exp, "resman_psi_pressure_source_status")["memory/system///read_error"]; got != 1 {
		t.Fatalf("source failure status=%v", got)
	}
}
