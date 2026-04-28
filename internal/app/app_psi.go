package app

import (
	"github.com/fdefilippo/resman/cgroup"
)

func (a *App) startPSIWatcher() {
	if !a.cfg.GetPSIEventDriven() {
		return
	}

	psiWatcher := cgroup.NewPSIWatcher(uint64(a.cfg.GetPSIWindowUs()))
	psiWatcher.SetThreshold("cpu", uint64(a.cfg.GetPSICPUStallThreshold()))
	psiWatcher.SetThreshold("io", uint64(a.cfg.GetPSIOStallThreshold()))
	if err := psiWatcher.Start(); err != nil {
		a.logger.Warn("Failed to start PSI watcher, falling back to polling", "error", err)
		return
	}

	sysCPUPressure := a.cfg.CgroupRoot + "/cpu.pressure"
	sysIOPressure := a.cfg.CgroupRoot + "/io.pressure"
	if err := psiWatcher.AddMonitor(0, "cpu", sysCPUPressure); err != nil {
		a.logger.Warn("Failed to monitor system cpu.pressure", "error", err)
	}
	if err := psiWatcher.AddMonitor(0, "io", sysIOPressure); err != nil {
		a.logger.Warn("Failed to monitor system io.pressure", "error", err)
	}

	a.psiWatcher = psiWatcher
	a.psiEvents = psiWatcher.Events()
	a.stateManager.RegisterPSIWatcher(psiWatcher)
	a.logger.Info("PSI event-driven mode enabled",
		"cpu_threshold_us", a.cfg.GetPSICPUStallThreshold(),
		"io_threshold_us", a.cfg.GetPSIOStallThreshold(),
		"window_us", a.cfg.GetPSIWindowUs(),
		"note", "PSI events trigger user CPU weight boosts and extra control cycles",
	)
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
