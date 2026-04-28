package app

import (
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
							time.Sleep(100 * time.Millisecond)
							a.configWatcher.HandleConfigChange()
						}()
					} else {
						a.logger.Warn("Config watcher not available for SIGHUP reload")
					}
				case syscall.SIGINT, syscall.SIGTERM:
					a.logger.Info("Received termination signal, initiating shutdown",
						"signal", sig.String(),
					)
					a.cancel()

					go func() {
						timeout := time.Duration(a.cfg.GetMCPShutdownTimeout()*2) * time.Second
						time.Sleep(timeout)
						a.logger.Warn("Shutdown timeout exceeded — cleanup did not complete. Forcing exit.",
							"timeout_seconds", a.cfg.GetMCPShutdownTimeout()*2,
						)
						syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
					}()
				}
			}
		}
	}()
}
func (a *App) shutdown() {
	a.logger.Info("Shutting down main control loop")

	if a.configWatcher != nil {
		a.configWatcher.Stop()
	}

	if err := a.stateManager.Cleanup(); err != nil {
		a.logger.Error("Error during state manager cleanup", "error", err)
		fmt.Fprintf(os.Stderr, "\nWarning: Error during cleanup: %v\n", err)
	}

	if a.mcpServer != nil {
		if err := a.mcpServer.Stop(); err != nil {
			a.logger.Error("Error stopping MCP server", "error", err)
		}
	}

	if a.dbManager != nil {
		if err := a.dbManager.Close(); err != nil {
			a.logger.Error("Error closing database manager", "error", err)
		}
	}

	if a.metricsCollector != nil {
		a.metricsCollector.Stop()
		a.logger.Info("Metrics collector stopped")
	}

	if a.psiWatcher != nil {
		a.psiWatcher.Stop()
		a.logger.Info("PSI watcher stopped")
	}

	a.logger.Info("Shutdown completed")
}
