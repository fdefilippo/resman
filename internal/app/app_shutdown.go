package app

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

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
					a.cancel()

					go func() {
						timeout := time.Duration(cfg.GetMCPShutdownTimeout()*2) * time.Second
						time.Sleep(timeout)
						a.logger.Warn("Shutdown timeout exceeded — cleanup did not complete. Forcing exit.",
							"timeout_seconds", cfg.GetMCPShutdownTimeout()*2,
						)
						// Last-resort force exit; nothing actionable if it fails.
						_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
					}()
				}
			}
		}
	}()
}
func (a *App) shutdown() error {
	a.logger.Info("Shutting down main control loop")
	var shutdownErrors []error

	if a.configWatcher != nil {
		if err := a.configWatcher.Stop(); err != nil {
			a.logger.Error("Error stopping config watcher", "error", err)
			shutdownErrors = append(shutdownErrors, fmt.Errorf("stop config watcher: %w", err))
		}
	}

	if a.mcpServer != nil {
		if err := a.mcpServer.Stop(); err != nil {
			a.logger.Error("Error stopping MCP server", "error", err)
			shutdownErrors = append(shutdownErrors, fmt.Errorf("stop MCP server: %w", err))
		}
	}

	a.psiMu.RLock()
	psiWatcherActive := a.psiWatcher != nil
	a.psiMu.RUnlock()
	if psiWatcherActive {
		a.stopPSIWatcher()
		a.logger.Info("PSI watcher stopped")
	}

	if a.metricsCollector != nil {
		a.metricsCollector.Stop()
		a.logger.Info("Metrics collector stopped")
	}

	if err := a.stateManager.Cleanup(); err != nil {
		a.logger.Error("Error during state manager cleanup", "error", err)
		fmt.Fprintf(os.Stderr, "\nWarning: Error during cleanup: %v\n", err)
		shutdownErrors = append(shutdownErrors, fmt.Errorf("cleanup state manager: %w", err))
	}

	if a.dbManager != nil {
		if err := a.dbManager.Close(); err != nil {
			a.logger.Error("Error closing database manager", "error", err)
			shutdownErrors = append(shutdownErrors, fmt.Errorf("close database manager: %w", err))
		}
	}

	if len(shutdownErrors) > 0 {
		return fmt.Errorf("shutdown incomplete: %w", errors.Join(shutdownErrors...))
	}
	if err := a.logger.InfoChecked("Shutdown completed"); err != nil {
		return fmt.Errorf("write shutdown completion log: %w", err)
	}
	return nil
}
