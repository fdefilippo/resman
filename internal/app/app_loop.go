package app

import (
	"fmt"
	"os"
	"time"

	"github.com/fdefilippo/resman/state"
)

func (a *App) Run() error {
	if a.err != nil {
		return a.err
	}

	a.startSignalHandler()
	a.startPSIWatcher()
	return a.runControlLoop()
}
func (a *App) runControlLoop() error {
	pollingInterval := a.controlCycleInterval()
	a.logger.Info("Entering main control loop",
		"polling_interval_seconds", pollingInterval,
		"psi_event_driven", a.cfg.GetPSIEventDriven(),
	)

	ticker := time.NewTicker(time.Duration(pollingInterval) * time.Second)
	defer func() {
		ticker.Stop()
	}()

	if err := a.stateManager.RunControlCycleWithTrigger(a.ctx, state.ControlCycleTriggerInitial); err != nil {
		a.logger.Error("Error in initial control cycle",
			"cycle_id", "initial",
			"error", err,
		)
		fmt.Fprintf(os.Stderr, "\nWarning: Error in initial control cycle: %v\n", err)
		fmt.Fprintf(os.Stderr, "This may indicate:\n")
		fmt.Fprintf(os.Stderr, "  1. Cgroup setup issues\n")
		fmt.Fprintf(os.Stderr, "  2. Permission problems\n")
		fmt.Fprintf(os.Stderr, "  3. Invalid configuration\n")
		fmt.Fprintf(os.Stderr, "Check logs for details: tail -f %s\n", a.cfg.LogFile)
	}

	cycleComplete := make(chan struct{})
	close(cycleComplete)

	for {
		select {
		case <-a.ctx.Done():
			a.shutdown()
			return nil
		case <-ticker.C:
			ticker = a.handleTickerCycle(ticker, &pollingInterval, &cycleComplete)
		case psiEvent, ok := <-a.psiEvents:
			if ok {
				a.handlePSIEvent(psiEvent, &cycleComplete)
			}
		}
	}
}

func (a *App) handleTickerCycle(ticker *time.Ticker, pollingInterval *int, cycleComplete *chan struct{}) *time.Ticker {
	currentPollingInterval := a.controlCycleInterval()
	if currentPollingInterval != *pollingInterval {
		ticker.Stop()
		*pollingInterval = currentPollingInterval
		ticker = time.NewTicker(time.Duration(*pollingInterval) * time.Second)
		a.logger.Info("Control loop interval updated",
			"polling_interval_seconds", *pollingInterval,
			"psi_event_driven", a.cfg.GetPSIEventDriven(),
		)
	}

	startTime := time.Now()
	if !a.acquireCycleSlot(*cycleComplete, "ticker", *pollingInterval) {
		return ticker
	}

	*cycleComplete = make(chan struct{})
	if err := a.stateManager.RunControlCycleWithTrigger(a.ctx, state.ControlCycleTriggerTicker); err != nil {
		a.logger.Error("Error in control cycle", "error", err)
	}

	duration := time.Since(startTime)
	close(*cycleComplete)

	if duration > time.Duration(*pollingInterval/2)*time.Second {
		a.logger.Warn("Control cycle took longer than expected",
			"duration_ms", duration.Milliseconds(),
			"polling_interval_ms", *pollingInterval*1000,
		)
	} else {
		a.logger.Debug("Control cycle completed",
			"duration_ms", duration.Milliseconds(),
		)
	}

	return ticker
}

func (a *App) controlCycleInterval() int {
	cfg := a.stateManager.GetConfig()
	if cfg.GetPSIEventDriven() {
		return cfg.GetPSIFallbackInterval()
	}
	return cfg.GetPollingInterval()
}

func (a *App) acquireCycleSlot(cycleComplete chan struct{}, source string, pollingInterval int) bool {
	select {
	case <-cycleComplete:
		return true
	default:
		if source == "psi" {
			a.logger.Debug("Skipping PSI-triggered cycle - previous still running")
		} else {
			a.logger.Warn("Skipping control cycle - previous cycle still running",
				"reason", "backpressure",
				"polling_interval_ms", pollingInterval*1000,
			)
		}
		return false
	}
}
