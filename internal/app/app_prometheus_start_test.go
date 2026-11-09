package app

import (
	"context"
	"net"
	"testing"

	"github.com/fdefilippo/resman/config"
)

func TestPrometheusBindFailurePreservesConfigurationIntentAndApp(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "occupied port"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			cfg := config.DefaultConfig()
			cfg.EnablePrometheus = enabled
			cfg.PrometheusMetricsBindPort = listener.Addr().(*net.TCPAddr).Port
			logger := &startupCaptureLogger{}
			app := &App{cfg: cfg, ctx: context.Background(), logger: logger}
			if app.WithPrometheus() != app || app.err != nil || app.prometheusExporter != nil {
				t.Fatalf("unexpected app state after unavailable exporter: %v", app.err)
			}
			if cfg.EnablePrometheus != enabled {
				t.Fatal("listener failure changed configuration intent")
			}
			if enabled && len(logger.errors) != 1 {
				t.Fatalf("bind failure logs = %v", logger.errors)
			}
		})
	}
}
