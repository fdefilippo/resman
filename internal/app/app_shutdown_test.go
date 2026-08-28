package app

import (
	"context"
	"errors"
	"testing"

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
