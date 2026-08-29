package app

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
	"github.com/fdefilippo/resman/state"
)

type shutdownCgroupManager struct {
	state.CgroupManager
	cleanupErr error
}

type shutdownLogger struct {
	completionErr error
}

func (*shutdownLogger) Debug(string, ...interface{}) {}
func (*shutdownLogger) Info(string, ...interface{})  {}
func (*shutdownLogger) Warn(string, ...interface{})  {}
func (*shutdownLogger) Error(string, ...interface{}) {}
func (l *shutdownLogger) InfoChecked(message string, _ ...interface{}) error {
	if message == "Shutdown completed" {
		return l.completionErr
	}
	return nil
}

type signalConfigWatcher struct {
	forced chan struct{}
	err    error
}

func (*signalConfigWatcher) Reload(context.Context) error { return nil }
func (w *signalConfigWatcher) ForceReload(context.Context) error {
	w.forced <- struct{}{}
	return w.err
}
func (*signalConfigWatcher) Stop() error { return nil }

type signalHandlerLogger struct {
	errors chan string
}

func (*signalHandlerLogger) Debug(string, ...interface{}) {}
func (*signalHandlerLogger) Info(string, ...interface{})  {}
func (*signalHandlerLogger) Warn(string, ...interface{})  {}
func (l *signalHandlerLogger) Error(message string, _ ...interface{}) {
	l.errors <- message
}
func (*signalHandlerLogger) InfoChecked(string, ...interface{}) error { return nil }

func (m *shutdownCgroupManager) CleanupAll() error {
	return m.cleanupErr
}

func TestShutdownReportsIncompleteStateCleanup(t *testing.T) {
	tests := []struct {
		name       string
		cleanupErr error
		wantErr    bool
	}{
		{name: "complete cleanup", wantErr: false},
		{name: "incomplete cleanup", cleanupErr: errors.New("process remains constrained"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateManager, err := state.NewManager(
				config.DefaultConfig(),
				nil,
				&shutdownCgroupManager{cleanupErr: tt.cleanupErr},
				nil,
			)
			if err != nil {
				t.Fatalf("NewManager() error: %v", err)
			}
			application := &App{
				logger:       logging.GetLogger(),
				stateManager: stateManager,
			}

			shutdownErr := application.shutdown()
			if tt.wantErr {
				if !errors.Is(shutdownErr, tt.cleanupErr) {
					t.Fatalf("shutdown() error = %v, want cleanup error", shutdownErr)
				}
				return
			}
			if shutdownErr != nil {
				t.Fatalf("shutdown() error = %v, want nil", shutdownErr)
			}
		})
	}
}

func TestSIGHUPDelegatesTerminalOutcomeReportingToWatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	watcher := &signalConfigWatcher{
		forced: make(chan struct{}, 1),
		err:    errors.New("reported restart-required outcome"),
	}
	logger := &signalHandlerLogger{errors: make(chan string, 1)}
	application := &App{
		ctx:           ctx,
		sigChan:       signals,
		logger:        logger,
		configWatcher: watcher,
	}
	application.startSignalHandler()

	signals <- syscall.SIGHUP
	select {
	case <-watcher.forced:
	case <-time.After(time.Second):
		t.Fatal("SIGHUP did not force a configuration reload")
	}
	select {
	case message := <-logger.errors:
		t.Fatalf("application duplicated watcher terminal outcome: %s", message)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestControlLoopPropagatesShutdownCleanupFailure(t *testing.T) {
	cleanupErr := errors.New("process remains constrained")
	stateManager, err := state.NewManager(
		config.DefaultConfig(),
		nil,
		&shutdownCgroupManager{cleanupErr: cleanupErr},
		nil,
	)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	application := &App{
		cfg:          config.DefaultConfig(),
		ctx:          ctx,
		logger:       logging.GetLogger(),
		stateManager: stateManager,
	}

	if err := application.runControlLoop(); !errors.Is(err, cleanupErr) {
		t.Fatalf("runControlLoop() error = %v, want cleanup failure", err)
	}
}

func TestShutdownPropagatesCompletionLogFailure(t *testing.T) {
	sinkErr := errors.New("injected shutdown log failure")
	stateManager, err := state.NewManager(
		config.DefaultConfig(),
		nil,
		&shutdownCgroupManager{},
		nil,
	)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	application := &App{
		logger:       &shutdownLogger{completionErr: sinkErr},
		stateManager: stateManager,
	}

	if err := application.shutdown(); !errors.Is(err, sinkErr) {
		t.Fatalf("shutdown() error = %v, want logging sink failure", err)
	}
}
