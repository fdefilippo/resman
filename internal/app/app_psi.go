package app

import (
	"os"

	"github.com/fdefilippo/resman/cgroup"
)

func (a *App) startPSIWatcher() {
	if !a.cfg.GetPSIEventDriven() {
		return
	}

	cgroupCPUPressure := a.cfg.CgroupRoot + "/cpu.pressure"
	cgroupIOPressure := a.cfg.CgroupRoot + "/io.pressure"
	sysCPUPressure := selectPressurePath(cgroupCPUPressure, "/proc/pressure/cpu")
	sysIOPressure := selectPressurePath(cgroupIOPressure, "/proc/pressure/io")
	if sysCPUPressure == "" && sysIOPressure == "" {
		a.logger.Warn("PSI event-driven mode unavailable, falling back to polling",
			"cgroup_cpu_pressure_path", cgroupCPUPressure,
			"cgroup_io_pressure_path", cgroupIOPressure,
			"proc_cpu_pressure_path", "/proc/pressure/cpu",
			"proc_io_pressure_path", "/proc/pressure/io",
			"polling_interval_seconds", a.cfg.GetPollingInterval(),
		)
		return
	}

	psiWatcher := cgroup.NewPSIWatcher(uint64(a.cfg.GetPSIWindowUs()))
	psiWatcher.SetThreshold("cpu", uint64(a.cfg.GetPSICPUStallThreshold()))
	psiWatcher.SetThreshold("io", uint64(a.cfg.GetPSIOStallThreshold()))
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
			"polling_interval_seconds", a.cfg.GetPollingInterval(),
		)
		return
	}

	a.psiWatcher = psiWatcher
	a.psiEvents = psiWatcher.Events()
	a.psiEventDriven = true
	if pressureFileExists(cgroupCPUPressure) || pressureFileExists(cgroupIOPressure) {
		a.stateManager.RegisterPSIWatcher(psiWatcher)
	} else {
		a.logger.Info("Per-user PSI boosting disabled because cgroup pressure files are unavailable")
	}
	a.logger.Info("PSI event-driven mode enabled",
		"cpu_threshold_us", a.cfg.GetPSICPUStallThreshold(),
		"io_threshold_us", a.cfg.GetPSIOStallThreshold(),
		"window_us", a.cfg.GetPSIWindowUs(),
		"cpu_pressure_path", sysCPUPressure,
		"io_pressure_path", sysIOPressure,
		"system_monitors", monitored,
		"note", "PSI events trigger user CPU weight boosts and extra control cycles",
	)
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
	return a.cfg.GetPSIEventDriven() && a.psiEventDriven
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
