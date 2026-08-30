package app

import (
	"fmt"
	"os"
	"time"

	"github.com/fdefilippo/resman/state"
)

const databaseRetentionInterval = 24 * time.Hour

type cpuSamplingCadenceSink interface {
	SetFallbackCPUSamplingInterval(time.Duration)
}

func (a *App) Run() error {
	if a.err != nil {
		return a.err
	}

	a.startSignalHandler()
	a.startPSIWatcher()
	if err := a.notifyReady(); err != nil {
		return fmt.Errorf("notify service readiness: %w", err)
	}
	return a.runControlLoop()
}
func (a *App) runControlLoop() error {
	if a.ctx.Err() != nil {
		return a.shutdown()
	}

	cfg := a.currentConfig()
	pollingInterval := a.controlCycleInterval()
	a.publishFallbackCPUSamplingInterval(pollingInterval)
	metricsRefreshInterval := a.metricsRefreshInterval()
	a.logger.Info("Entering main control loop",
		"polling_interval_seconds", pollingInterval,
		"metrics_refresh_interval_seconds", metricsRefreshInterval,
		"psi_event_driven_configured", cfg.GetPSIEventDriven(),
		"psi_event_driven_active", a.isPSIEventDrivenActive(),
	)

	ticker := time.NewTicker(time.Duration(pollingInterval) * time.Second)
	defer func() {
		ticker.Stop()
	}()

	metricsTicker, metricsRefreshC := newOptionalTicker(metricsRefreshInterval)
	defer func() {
		if metricsTicker != nil {
			metricsTicker.Stop()
		}
	}()

	retentionTicker, retentionC := newDatabaseRetentionTicker(a.dbManager != nil)
	defer func() {
		if retentionTicker != nil {
			retentionTicker.Stop()
		}
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
		fmt.Fprintf(os.Stderr, "Check logs for details: tail -f %s\n", cfg.LogFile)
	}

	cycleComplete := make(chan struct{})
	close(cycleComplete)

	for {
		select {
		case <-a.ctx.Done():
			return a.shutdown()
		case <-ticker.C:
			ticker = a.handleTickerCycle(ticker, &pollingInterval, &cycleComplete)
			metricsTicker, metricsRefreshC = a.refreshMetricsTicker(metricsTicker, metricsRefreshC, &metricsRefreshInterval)
		case <-metricsRefreshC:
			a.handleMetricsRefreshCycle()
			metricsTicker, metricsRefreshC = a.refreshMetricsTicker(metricsTicker, metricsRefreshC, &metricsRefreshInterval)
		case <-retentionC:
			a.handleDatabaseRetention()
		case <-a.configReloaded:
			ticker = a.refreshControlTicker(ticker, &pollingInterval)
			metricsTicker, metricsRefreshC = a.refreshMetricsTicker(metricsTicker, metricsRefreshC, &metricsRefreshInterval)
		case psiEvent, ok := <-a.psiEventChannel():
			if ok {
				a.handlePSIEvent(psiEvent, &cycleComplete)
			}
		}
	}
}

func newDatabaseRetentionTicker(enabled bool) (*time.Ticker, <-chan time.Time) {
	if !enabled {
		return nil, nil
	}
	ticker := time.NewTicker(databaseRetentionInterval)
	return ticker, ticker.C
}

func (a *App) handleDatabaseRetention() {
	if a.dbManager == nil {
		return
	}
	cfg := a.stateManager.GetConfig()
	deleted, err := a.dbManager.CleanupOldData(cfg.MetricsDBRetentionDays)
	if err != nil {
		a.logger.Warn("Periodic metrics database retention failed",
			"retention_days", cfg.MetricsDBRetentionDays,
			"error", err,
		)
		return
	}
	a.logger.Info("Periodic metrics database retention completed",
		"retention_days", cfg.MetricsDBRetentionDays,
		"records_deleted", deleted,
	)
}

func (a *App) handleTickerCycle(ticker *time.Ticker, pollingInterval *int, cycleComplete *chan struct{}) *time.Ticker {
	ticker = a.refreshControlTicker(ticker, pollingInterval)

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

func (a *App) refreshControlTicker(ticker *time.Ticker, pollingInterval *int) *time.Ticker {
	currentPollingInterval := a.controlCycleInterval()
	if currentPollingInterval != *pollingInterval {
		cfg := a.currentConfig()
		ticker.Stop()
		*pollingInterval = currentPollingInterval
		a.publishFallbackCPUSamplingInterval(currentPollingInterval)
		ticker = time.NewTicker(time.Duration(*pollingInterval) * time.Second)
		a.logger.Info("Control loop interval updated",
			"polling_interval_seconds", *pollingInterval,
			"psi_event_driven_configured", cfg.GetPSIEventDriven(),
			"psi_event_driven_active", a.isPSIEventDrivenActive(),
		)
	}
	return ticker
}

func (a *App) publishFallbackCPUSamplingInterval(intervalSeconds int) {
	if a.cpuSamplingCadence == nil {
		return
	}
	a.cpuSamplingCadence.SetFallbackCPUSamplingInterval(time.Duration(intervalSeconds) * time.Second)
}

func (a *App) handleMetricsRefreshCycle() {
	if err := a.stateManager.RunMetricsRefresh(a.ctx, "metrics_refresh"); err != nil {
		a.logger.Warn("Error in metrics refresh", "error", err)
	}
}

func (a *App) controlCycleInterval() int {
	cfg := a.stateManager.GetConfig()
	if a.isPSIEventDrivenActive() {
		return cfg.GetPSIFallbackInterval()
	}
	return cfg.GetPollingInterval()
}

func (a *App) metricsRefreshInterval() int {
	cfg := a.stateManager.GetConfig()
	if !a.isPSIEventDrivenActive() || a.prometheusExporter == nil {
		return 0
	}
	return cfg.GetMetricsRefreshInterval()
}

func newOptionalTicker(interval int) (*time.Ticker, <-chan time.Time) {
	if interval <= 0 {
		return nil, nil
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	return ticker, ticker.C
}

func (a *App) refreshMetricsTicker(current *time.Ticker, currentC <-chan time.Time, currentInterval *int) (*time.Ticker, <-chan time.Time) {
	nextInterval := a.metricsRefreshInterval()
	if nextInterval == *currentInterval {
		return current, currentC
	}

	if current != nil {
		current.Stop()
	}
	*currentInterval = nextInterval

	if nextInterval <= 0 {
		cfg := a.currentConfig()
		a.logger.Info("Metrics refresh loop disabled",
			"psi_event_driven_configured", cfg.GetPSIEventDriven(),
			"psi_event_driven_active", a.isPSIEventDrivenActive(),
		)
		return nil, nil
	}

	cfg := a.currentConfig()
	a.logger.Info("Metrics refresh interval updated",
		"metrics_refresh_interval_seconds", nextInterval,
		"psi_event_driven_configured", cfg.GetPSIEventDriven(),
		"psi_event_driven_active", a.isPSIEventDrivenActive(),
	)
	return newOptionalTicker(nextInterval)
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
