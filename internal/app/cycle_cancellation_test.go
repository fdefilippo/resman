package app

import (
	"context"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/state"
)

type cycleCancellationLogger struct {
	shutdownLogger
	cancel             context.CancelFunc
	canceled, failures int
}

func (l *cycleCancellationLogger) Info(message string, _ ...interface{}) {
	if message == "Entering main control loop" {
		l.cancel()
	}
	if message == "Control cycle canceled by shutdown" {
		l.canceled++
	}
}
func (l *cycleCancellationLogger) Error(string, ...interface{}) { l.failures++ }

func TestApplicationReportsShutdownCancellationForEveryCycleTrigger(t *testing.T) {
	for _, trigger := range []string{"initial", "ticker", "psi"} {
		t.Run(trigger, func(t *testing.T) {
			cfg := config.DefaultConfig()
			manager, err := state.NewManager(cfg, nil, &shutdownCgroupManager{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logger := &cycleCancellationLogger{cancel: cancel}
			a := &App{cfg: cfg, ctx: ctx, stateManager: manager, logger: logger}
			done := make(chan struct{})
			close(done)
			switch trigger {
			case "initial":
				// Cancel after the loop's initial context check, before its first cycle.
				if err := a.runControlLoop(); err != nil {
					t.Fatal(err)
				}
			case "ticker":
				cancel()
				ticker := time.NewTicker(time.Hour)
				interval := cfg.GetPollingInterval()
				ticker = a.handleTickerCycle(ticker, &interval, &done)
				ticker.Stop()
			case "psi":
				cancel()
				a.handlePSIEvent(cgroup.PSIEvent{Type: "cpu"}, &done)
			}
			if logger.canceled != 1 || logger.failures != 0 {
				t.Fatalf("canceled INFO=%d, ERROR=%d", logger.canceled, logger.failures)
			}
			select {
			case <-done:
			default:
				t.Fatal("canceled cycle retained execution slot")
			}
		})
	}
}
