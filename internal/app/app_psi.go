package app

import (
	"os"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
)

func (a *App) startPSIWatcher() {
	a.startPSIWatcherWithConfig(a.currentConfig())
}

func (a *App) startPSIWatcherWithConfig(cfg *config.Config) {
	if cfg == nil || !cfg.GetPSIEventDriven() {
		return
	}

	// One owned descriptor per configured resource. Memory is an explicit opt-in
	// wake signal; all decisions still use the normal attributed resource sample.
	resources := []struct {
		kind      string
		threshold int
	}{{"cpu", cfg.GetPSICPUStallThreshold()}, {"io", cfg.GetPSIOStallThreshold()}}
	if cfg.GetPSIMemoryEventDriven() {
		resources = append(resources, struct {
			kind      string
			threshold int
		}{"memory", cfg.GetPSIMemoryStallThreshold()})
	}
	psiWatcher := cgroup.NewPSIWatcher(uint64(cfg.GetPSIWindowUs()))
	for _, resource := range resources {
		psiWatcher.SetThreshold(resource.kind, uint64(resource.threshold))
	}
	if err := psiWatcher.Start(); err != nil {
		a.logger.Warn("Failed to start PSI watcher, falling back to polling", "error", err)
		return
	}
	monitored := 0
	for _, resource := range resources {
		selected := selectPressurePath(cfg.CgroupRoot+"/"+resource.kind+".pressure", "/proc/pressure/"+resource.kind)
		if selected == "" {
			a.logger.Warn("Optional PSI wake source unavailable", "type", resource.kind)
			continue
		}
		if err := psiWatcher.AddMonitor(0, resource.kind, selected); err != nil {
			a.logger.Warn("Failed to monitor system pressure", "type", resource.kind, "path", selected, "error", err)
		} else {
			monitored++
		}
	}
	if monitored == 0 {
		psiWatcher.Stop()
		a.logger.Warn("No PSI pressure files could be monitored, falling back to polling",
			"polling_interval_seconds", cfg.GetPollingInterval(),
		)
		return
	}

	a.psiMu.Lock()
	a.psiWatcher = psiWatcher
	a.psiEvents = psiWatcher.Events()
	a.psiEventDriven = true
	a.psiMu.Unlock()
	a.logger.Info("PSI event-driven mode enabled",
		"cpu_threshold_us", cfg.GetPSICPUStallThreshold(),
		"io_threshold_us", cfg.GetPSIOStallThreshold(),
		"window_us", cfg.GetPSIWindowUs(),
		"memory_wakeup_enabled", cfg.GetPSIMemoryEventDriven(),
		"memory_threshold_us", cfg.GetPSIMemoryStallThreshold(),
		"system_monitors", monitored,
		"note", "PSI events trigger extra control cycles without changing resource controls",
	)
}

func (a *App) applyReloadedConfig(cfg *config.Config) error {
	if cfg == nil {
		return nil
	}

	previous := a.currentConfig()
	restartPSI := previous == nil ||
		previous.GetPSIEventDriven() != cfg.GetPSIEventDriven() ||
		previous.GetPSICPUStallThreshold() != cfg.GetPSICPUStallThreshold() ||
		previous.GetPSIOStallThreshold() != cfg.GetPSIOStallThreshold() ||
		previous.GetPSIMemoryEventDriven() != cfg.GetPSIMemoryEventDriven() ||
		previous.GetPSIMemoryStallThreshold() != cfg.GetPSIMemoryStallThreshold() ||
		previous.GetPSIWindowUs() != cfg.GetPSIWindowUs()
	a.setCurrentConfig(cfg)
	if !restartPSI {
		a.publishFallbackCPUSamplingInterval(a.controlCycleInterval())
		a.notifyConfigReloaded()
		return nil
	}

	a.stopPSIWatcher()
	a.startPSIWatcherWithConfig(cfg)
	a.publishFallbackCPUSamplingInterval(a.controlCycleInterval())
	a.notifyConfigReloaded()
	return nil
}

func (a *App) notifyConfigReloaded() {
	select {
	case a.configReloaded <- struct{}{}:
	default:
	}
}

func (a *App) stopPSIWatcher() {
	a.psiMu.Lock()
	psiWatcher := a.psiWatcher
	a.psiWatcher = nil
	a.psiEvents = nil
	a.psiEventDriven = false
	a.psiMu.Unlock()

	if psiWatcher != nil {
		psiWatcher.Stop()
	}
}

func selectPressurePath(cgroupPath string, procPath string) string {
	if pressureFileExists(cgroupPath) {
		return cgroupPath
	}
	if pressureFileExists(procPath) {
		return procPath
	}
	return ""
}

func pressureFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (a *App) isPSIEventDrivenActive() bool {
	cfg := a.currentConfig()
	a.psiMu.RLock()
	active := a.psiEventDriven
	a.psiMu.RUnlock()
	return cfg != nil && cfg.GetPSIEventDriven() && active
}

func (a *App) psiEventChannel() <-chan cgroup.PSIEvent {
	a.psiMu.RLock()
	defer a.psiMu.RUnlock()
	return a.psiEvents
}

func (a *App) handlePSIEvent(psiEvent cgroup.PSIEvent, cycleComplete *chan struct{}) {
	a.logger.Debug("PSI event received",
		"type", psiEvent.Type,
		"uid", psiEvent.UID,
		"some_avg10", psiEvent.SomeAvg10,
	)

	if psiEvent.UID > 0 {
		a.stateManager.OnUserPSIEvent(psiEvent)
	}

	psiScope := "system"
	if psiEvent.UID > 0 {
		psiScope = "user"
	}
	if a.prometheusExporter != nil {
		a.prometheusExporter.RecordPSIEvent(psiEvent.Type, psiScope, psiEvent.Timestamp)
	}

	if !a.acquireCycleSlot(*cycleComplete, "psi", 0) {
		return
	}

	*cycleComplete = make(chan struct{})
	trigger := "psi_" + psiScope + "_" + psiEvent.Type
	if err := a.stateManager.RunControlCycleWithTrigger(a.ctx, trigger); err != nil {
		if !a.reportShutdownCycleCancellation(err, trigger) {
			a.logger.Error("Error in control cycle (PSI-triggered)", "error", err)
		}
	}
	close(*cycleComplete)
}
