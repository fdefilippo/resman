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

	cgroupCPUPressure := cfg.CgroupRoot + "/cpu.pressure"
	cgroupIOPressure := cfg.CgroupRoot + "/io.pressure"
	sysCPUPressure := selectPressurePath(cgroupCPUPressure, "/proc/pressure/cpu")
	sysIOPressure := selectPressurePath(cgroupIOPressure, "/proc/pressure/io")
	if sysCPUPressure == "" && sysIOPressure == "" {
		a.logger.Warn("PSI event-driven mode unavailable, falling back to polling",
			"cgroup_cpu_pressure_path", cgroupCPUPressure,
			"cgroup_io_pressure_path", cgroupIOPressure,
			"proc_cpu_pressure_path", "/proc/pressure/cpu",
			"proc_io_pressure_path", "/proc/pressure/io",
			"polling_interval_seconds", cfg.GetPollingInterval(),
		)
		return
	}

	psiWatcher := cgroup.NewPSIWatcher(uint64(cfg.GetPSIWindowUs()))
	psiWatcher.SetThreshold("cpu", uint64(cfg.GetPSICPUStallThreshold()))
	psiWatcher.SetThreshold("io", uint64(cfg.GetPSIOStallThreshold()))
	if err := psiWatcher.Start(); err != nil {
		a.logger.Warn("Failed to start PSI watcher, falling back to polling", "error", err)
		return
	}

	monitored := 0
	if sysCPUPressure != "" {
		if err := psiWatcher.AddMonitor(0, "cpu", sysCPUPressure); err != nil {
			a.logger.Warn("Failed to monitor system cpu.pressure", "path", sysCPUPressure, "error", err)
		} else {
			monitored++
		}
	}
	if sysIOPressure != "" {
		if err := psiWatcher.AddMonitor(0, "io", sysIOPressure); err != nil {
			a.logger.Warn("Failed to monitor system io.pressure", "path", sysIOPressure, "error", err)
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
	if pressureFileExists(cgroupCPUPressure) || pressureFileExists(cgroupIOPressure) {
		a.stateManager.RegisterPSIWatcher(psiWatcher)
	} else {
		a.logger.Info("Per-user PSI boosting disabled because cgroup pressure files are unavailable")
	}
	a.logger.Info("PSI event-driven mode enabled",
		"cpu_threshold_us", cfg.GetPSICPUStallThreshold(),
		"io_threshold_us", cfg.GetPSIOStallThreshold(),
		"window_us", cfg.GetPSIWindowUs(),
		"cpu_pressure_path", sysCPUPressure,
		"io_pressure_path", sysIOPressure,
		"system_monitors", monitored,
		"note", "PSI events trigger user CPU weight boosts and extra control cycles",
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

	if a.stateManager != nil {
		a.stateManager.RegisterPSIWatcher(nil)
	}
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
		a.logger.Error("Error in control cycle (PSI-triggered)", "error", err)
	}
	close(*cycleComplete)
}
