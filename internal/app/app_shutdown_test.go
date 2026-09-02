package app

import (
	"context"
	"errors"
	"os"
	"sync"
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

type manualShutdownTimer struct {
	ch       chan time.Time
	stopped  chan struct{}
	stopOnce sync.Once
}

type lateFiringShutdownTimer struct {
	ch       chan time.Time
	stopOnce sync.Once
}

func newLateFiringShutdownTimer() *lateFiringShutdownTimer {
	return &lateFiringShutdownTimer{ch: make(chan time.Time, 1)}
}

func (t *lateFiringShutdownTimer) C() <-chan time.Time { return t.ch }
func (t *lateFiringShutdownTimer) Stop() bool {
	t.stopOnce.Do(func() { t.ch <- time.Now() })
	return false
}

func newManualShutdownTimer() *manualShutdownTimer {
	return &manualShutdownTimer{
		ch:      make(chan time.Time, 1),
		stopped: make(chan struct{}),
	}
}

func (t *manualShutdownTimer) C() <-chan time.Time { return t.ch }
func (t *manualShutdownTimer) Stop() bool {
	t.stopOnce.Do(func() { close(t.stopped) })
	return true
}

type shutdownWarning struct {
	message string
	fields  []interface{}
}

type watchdogLogger struct {
	warnings chan shutdownWarning
	events   chan string
}

func (*watchdogLogger) Debug(string, ...interface{}) {}
func (*watchdogLogger) Info(string, ...interface{})  {}
func (l *watchdogLogger) Warn(message string, fields ...interface{}) {
	l.warnings <- shutdownWarning{message: message, fields: append([]interface{}(nil), fields...)}
	if l.events != nil {
		l.events <- "warning"
	}
}
func (*watchdogLogger) Error(string, ...interface{})             {}
func (*watchdogLogger) InfoChecked(string, ...interface{}) error { return nil }

func warningField(fields []interface{}, key string) (interface{}, bool) {
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == key {
			return fields[i+1], true
		}
	}
	return nil, false
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

func TestTerminationSignalUsesDaemonShutdownDeadlineWhenMCPIsDisabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MCPEnabled = false
	cfg.MCPShutdownTimeout = 1
	cfg.DaemonShutdownTimeout = 11
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	timer := newManualShutdownTimer()
	capturedTimeout := make(chan time.Duration, 1)
	application := &App{
		cfg:     cfg,
		ctx:     ctx,
		cancel:  cancel,
		sigChan: signals,
		logger:  &watchdogLogger{warnings: make(chan shutdownWarning, 1)},
	}
	application.shutdownDeadline.timerFactory = func(timeout time.Duration) shutdownTimer {
		capturedTimeout <- timeout
		return timer
	}
	application.shutdownDeadline.forceExit = func() error { return nil }
	updated := config.DefaultConfig()
	updated.MCPEnabled = false
	updated.MCPShutdownTimeout = 1
	updated.DaemonShutdownTimeout = 37
	application.setCurrentConfig(updated)
	application.startSignalHandler()

	signals <- syscall.SIGTERM
	select {
	case timeout := <-capturedTimeout:
		if timeout != 37*time.Second {
			t.Fatalf("shutdown watchdog timeout = %s, want 37s", timeout)
		}
	case <-time.After(time.Second):
		t.Fatal("termination signal did not start the shutdown watchdog")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("termination signal did not cancel the application context")
	}
	application.finishShutdownWatchdog()
}

func TestShutdownCompletionCancelsWatchdog(t *testing.T) {
	stateManager, err := state.NewManager(
		config.DefaultConfig(),
		nil,
		&shutdownCgroupManager{},
		nil,
	)
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	timer := newManualShutdownTimer()
	forced := make(chan struct{}, 1)
	application := &App{
		logger:       &shutdownLogger{},
		stateManager: stateManager,
	}
	application.shutdownDeadline.timerFactory = func(time.Duration) shutdownTimer { return timer }
	application.shutdownDeadline.forceExit = func() error {
		forced <- struct{}{}
		return nil
	}
	application.startShutdownWatchdog(time.Hour)

	if err := application.shutdown(); err != nil {
		t.Fatalf("shutdown() error: %v", err)
	}
	select {
	case <-timer.stopped:
	case <-time.After(time.Second):
		t.Fatal("completed shutdown did not stop its watchdog timer")
	}
	select {
	case <-forced:
		t.Fatal("completed shutdown invoked the forced-exit boundary")
	default:
	}
}

func TestShutdownCompletionWinsAgainstAlreadyExpiredWatchdog(t *testing.T) {
	const attempts = 256
	for attempt := 0; attempt < attempts; attempt++ {
		timer := newLateFiringShutdownTimer()
		forced := make(chan struct{}, 1)
		application := &App{
			logger: &watchdogLogger{warnings: make(chan shutdownWarning, 1)},
		}
		application.shutdownDeadline.timerFactory = func(time.Duration) shutdownTimer { return timer }
		application.shutdownDeadline.forceExit = func() error {
			forced <- struct{}{}
			return nil
		}
		application.startShutdownWatchdog(time.Hour)

		application.finishShutdownWatchdog()
		select {
		case <-forced:
			t.Fatalf("attempt %d invoked forced exit after shutdown completion", attempt)
		default:
		}
	}
}

func TestShutdownDeadlineRecordsOutstandingStageBeforeForceExit(t *testing.T) {
	timer := newManualShutdownTimer()
	warnings := make(chan shutdownWarning, 1)
	events := make(chan string, 2)
	application := &App{
		logger: &watchdogLogger{warnings: warnings, events: events},
	}
	application.shutdownDeadline.timerFactory = func(time.Duration) shutdownTimer { return timer }
	application.shutdownDeadline.forceExit = func() error {
		events <- "force_exit"
		return nil
	}
	application.startShutdownWatchdog(47 * time.Second)
	application.setShutdownStage("state_manager_cleanup")
	timer.ch <- time.Now()

	for _, want := range []string{"warning", "force_exit"} {
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("shutdown deadline event = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("expired shutdown deadline did not invoke the forced-exit boundary")
		}
	}
	warning := <-warnings
	if warning.message != "Daemon shutdown deadline exceeded; forcing exit" {
		t.Fatalf("warning message = %q", warning.message)
	}
	stage, ok := warningField(warning.fields, "outstanding_stage")
	if !ok || stage != "state_manager_cleanup" {
		t.Fatalf("outstanding_stage = %v, present = %t", stage, ok)
	}
	timeout, ok := warningField(warning.fields, "timeout_seconds")
	if !ok || timeout != int64(47) {
		t.Fatalf("timeout_seconds = %v, present = %t", timeout, ok)
	}
	application.finishShutdownWatchdog()
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
