package app

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/fdefilippo/resman/config"
)

type shutdownTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type realShutdownTimer struct {
	*time.Timer
}

func (t *realShutdownTimer) C() <-chan time.Time {
	return t.Timer.C
}

type shutdownDeadlineState struct {
	mu           sync.Mutex
	wg           sync.WaitGroup
	active       bool
	completed    bool
	stage        string
	done         chan struct{}
	timer        shutdownTimer
	timerFactory func(time.Duration) shutdownTimer
	forceExit    func() error
}

func newShutdownTimer(timeout time.Duration) shutdownTimer {
	return &realShutdownTimer{Timer: time.NewTimer(timeout)}
}

func forceShutdownExit() error {
	return syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
}

func daemonShutdownTimeout(cfg *config.Config) time.Duration {
	return time.Duration(cfg.GetDaemonShutdownTimeout()) * time.Second
}

func (a *App) startShutdownWatchdog(timeout time.Duration) {
	timerFactory := a.shutdownDeadline.timerFactory
	if timerFactory == nil {
		timerFactory = newShutdownTimer
	}
	timer := timerFactory(timeout)
	done := make(chan struct{})

	a.shutdownDeadline.mu.Lock()
	if a.shutdownDeadline.active || a.shutdownDeadline.completed || a.shutdownDeadline.done != nil {
		a.shutdownDeadline.mu.Unlock()
		timer.Stop()
		return
	}
	a.shutdownDeadline.active = true
	a.shutdownDeadline.stage = "waiting_for_control_loop"
	a.shutdownDeadline.done = done
	a.shutdownDeadline.timer = timer
	a.shutdownDeadline.wg.Add(1)
	a.shutdownDeadline.mu.Unlock()

	go func() {
		defer a.shutdownDeadline.wg.Done()
		select {
		case <-timer.C():
			stage, ok := a.claimShutdownDeadline()
			if !ok {
				return
			}
			a.logger.Warn("Daemon shutdown deadline exceeded; forcing exit",
				"timeout_seconds", int64(timeout/time.Second),
				"outstanding_stage", stage,
			)
			fmt.Fprintf(os.Stderr,
				"Fatal: daemon shutdown deadline exceeded after %s at stage %s; forcing exit\n",
				timeout, stage,
			)
			forceExit := a.shutdownDeadline.forceExit
			if forceExit == nil {
				forceExit = forceShutdownExit
			}
			_ = forceExit()
		case <-done:
		}
	}()
}

func (a *App) claimShutdownDeadline() (string, bool) {
	a.shutdownDeadline.mu.Lock()
	defer a.shutdownDeadline.mu.Unlock()
	if !a.shutdownDeadline.active || a.shutdownDeadline.completed {
		return "", false
	}
	a.shutdownDeadline.active = false
	return a.shutdownDeadline.stage, true
}

func (a *App) setShutdownStage(stage string) {
	a.shutdownDeadline.mu.Lock()
	if a.shutdownDeadline.active && !a.shutdownDeadline.completed {
		a.shutdownDeadline.stage = stage
	}
	a.shutdownDeadline.mu.Unlock()
}

func (a *App) finishShutdownWatchdog() {
	a.shutdownDeadline.mu.Lock()
	if a.shutdownDeadline.completed {
		a.shutdownDeadline.mu.Unlock()
		a.shutdownDeadline.wg.Wait()
		return
	}
	a.shutdownDeadline.completed = true
	a.shutdownDeadline.active = false
	done := a.shutdownDeadline.done
	timer := a.shutdownDeadline.timer
	a.shutdownDeadline.mu.Unlock()

	if timer != nil {
		timer.Stop()
	}
	if done != nil {
		close(done)
	}
	a.shutdownDeadline.wg.Wait()
}

func (a *App) startSignalHandler() {
	go func() {
		for {
			select {
			case <-a.ctx.Done():
				return
			case sig := <-a.sigChan:
				switch sig {
				case syscall.SIGHUP:
					a.logger.Info("Received SIGHUP, forcing configuration reload")
					if a.configWatcher != nil {
						go func() {
							// ForceReload owns the single terminal outcome record.
							_ = a.configWatcher.ForceReload(a.ctx)
						}()
					} else {
						a.logger.Warn("Config watcher not available for SIGHUP reload")
					}
				case syscall.SIGINT, syscall.SIGTERM:
					cfg := a.currentConfig()
					a.logger.Info("Received termination signal, initiating shutdown",
						"signal", sig.String(),
					)
					a.startShutdownWatchdog(daemonShutdownTimeout(cfg))
					a.cancel()
				}
			}
		}
	}()
}
func (a *App) shutdown() error {
	defer a.finishShutdownWatchdog()

	a.logger.Info("Shutting down main control loop")
	var shutdownErrors []error

	if a.configWatcher != nil {
		a.setShutdownStage("config_watcher_stop")
		if err := a.configWatcher.Stop(); err != nil {
			a.logger.Error("Error stopping config watcher", "error", err)
			shutdownErrors = append(shutdownErrors, fmt.Errorf("stop config watcher: %w", err))
		}
	}

	if a.mcpServer != nil {
		a.setShutdownStage("mcp_server_stop")
		if err := a.mcpServer.Stop(); err != nil {
			a.logger.Error("Error stopping MCP server", "error", err)
			shutdownErrors = append(shutdownErrors, fmt.Errorf("stop MCP server: %w", err))
		}
	}

	a.psiMu.RLock()
	psiWatcherActive := a.psiWatcher != nil
	a.psiMu.RUnlock()
	if psiWatcherActive {
		a.setShutdownStage("psi_watcher_stop")
		a.stopPSIWatcher()
		a.logger.Info("PSI watcher stopped")
	}

	if a.metricsCollector != nil {
		a.setShutdownStage("metrics_collector_stop")
		a.metricsCollector.Stop()
		a.logger.Info("Metrics collector stopped")
	}

	a.setShutdownStage("state_manager_cleanup")
	if err := a.stateManager.Cleanup(); err != nil {
		a.logger.Error("Error during state manager cleanup", "error", err)
		fmt.Fprintf(os.Stderr, "\nWarning: Error during cleanup: %v\n", err)
		shutdownErrors = append(shutdownErrors, fmt.Errorf("cleanup state manager: %w", err))
	}

	if a.dbManager != nil {
		a.setShutdownStage("metrics_database_close")
		if err := a.dbManager.Close(); err != nil {
			a.logger.Error("Error closing database manager", "error", err)
			shutdownErrors = append(shutdownErrors, fmt.Errorf("close database manager: %w", err))
		}
	}

	if len(shutdownErrors) > 0 {
		return fmt.Errorf("shutdown incomplete: %w", errors.Join(shutdownErrors...))
	}
	a.setShutdownStage("completion_log")
	if err := a.logger.InfoChecked("Shutdown completed"); err != nil {
		return fmt.Errorf("write shutdown completion log: %w", err)
	}
	return nil
}
