package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fdefilippo/resman/logging"
)

var watcherTestLogPath string

func TestMain(m *testing.M) {
	testDir, err := os.MkdirTemp("", "resman-config-watcher-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "failed to create watcher test directory: %v\n", err)
		os.Exit(1)
	}
	watcherTestLogPath = filepath.Join(testDir, "resman.log")
	logging.InitLogger("DEBUG", watcherTestLogPath, 16*1024*1024, false)

	code := m.Run()
	_ = logging.GetLogger().Close()
	_ = os.RemoveAll(testDir)
	os.Exit(code)
}

type failingConfigChangeHandler struct {
	calls int
}

func (h *failingConfigChangeHandler) OnConfigChange(*Config) error {
	h.calls++
	return errors.New("partial apply")
}

type errorConfigChangeHandler struct {
	err error
}

func (h *errorConfigChangeHandler) OnConfigChange(*Config) error {
	return h.err
}

type watcherLogEntry struct {
	level   string
	message string
	fields  map[string]string
}

type recordingWatcherLogger struct {
	mu      sync.Mutex
	entries []watcherLogEntry
}

func (*recordingWatcherLogger) Debug(string, ...interface{}) {}
func (*recordingWatcherLogger) Info(string, ...interface{})  {}
func (l *recordingWatcherLogger) Warn(message string, keyvals ...interface{}) {
	l.record("WARN", message, keyvals...)
}
func (l *recordingWatcherLogger) Error(message string, keyvals ...interface{}) {
	l.record("ERROR", message, keyvals...)
}

func (l *recordingWatcherLogger) record(level, message string, keyvals ...interface{}) {
	fields := make(map[string]string, len(keyvals)/2)
	for i := 0; i+1 < len(keyvals); i += 2 {
		fields[fmt.Sprint(keyvals[i])] = fmt.Sprint(keyvals[i+1])
	}
	l.mu.Lock()
	l.entries = append(l.entries, watcherLogEntry{level: level, message: message, fields: fields})
	l.mu.Unlock()
}

func (l *recordingWatcherLogger) snapshot() []watcherLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]watcherLogEntry(nil), l.entries...)
}

type channelConfigChangeHandler struct {
	values chan int
}

func (h *channelConfigChangeHandler) OnConfigChange(cfg *Config) error {
	h.values <- cfg.CPUThreshold
	return nil
}

type blockingConfigChangeHandler struct {
	entered chan struct{}
	release chan struct{}
	active  atomic.Int32
	max     atomic.Int32
}

func (h *blockingConfigChangeHandler) OnConfigChange(*Config) error {
	active := h.active.Add(1)
	for {
		current := h.max.Load()
		if active <= current || h.max.CompareAndSwap(current, active) {
			break
		}
	}
	h.entered <- struct{}{}
	<-h.release
	h.active.Add(-1)
	return nil
}

func TestWatcherDoesNotLogEventsForOtherFiles(t *testing.T) {
	configPath := filepath.Join(filepath.Dir(watcherTestLogPath), "resman-loop.conf")
	if err := os.WriteFile(configPath, []byte("CPU_THRESHOLD=80\n"), 0600); err != nil {
		t.Fatalf("WriteFile(config) error: %v", err)
	}
	logInfo, err := os.Stat(watcherTestLogPath)
	if err != nil {
		t.Fatalf("Stat(log) error: %v", err)
	}

	watcher, err := NewWatcher(configPath, DefaultConfig(), &channelConfigChangeHandler{
		values: make(chan int, 1),
	})
	if err != nil {
		t.Fatalf("NewWatcher() error: %v", err)
	}
	if err := watcher.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	unrelatedPath := filepath.Join(filepath.Dir(configPath), "unrelated.txt")
	if err := os.WriteFile(unrelatedPath, []byte("event\n"), 0600); err != nil {
		_ = watcher.Stop()
		t.Fatalf("WriteFile(unrelated) error: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := watcher.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}

	logData, err := os.ReadFile(watcherTestLogPath)
	if err != nil {
		t.Fatalf("ReadFile(log) error: %v", err)
	}
	if logInfo.Size() > int64(len(logData)) {
		t.Fatalf("log shrank from %d to %d bytes during test", logInfo.Size(), len(logData))
	}
	newLog := string(logData[logInfo.Size():])
	if strings.Contains(newLog, "File system event file="+watcherTestLogPath) {
		t.Fatal("watcher logged its own log-file event")
	}
	if strings.Contains(newLog, "File system event file="+unrelatedPath) {
		t.Fatal("watcher logged an unrelated directory event")
	}
}

func TestWatcherRecordsFailedApplyVersion(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "resman.conf")
	if err := os.WriteFile(configPath, []byte("CPU_THRESHOLD=80\n"), 0600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	fileInfo, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}

	initialConfig := DefaultConfig()
	handler := &failingConfigChangeHandler{}
	watcher := &Watcher{
		configPath:    configPath,
		currentConfig: initialConfig,
		logger:        logging.GetLogger(),
		onChange:      handler,
		isRunning:     true,
		lastModTime:   time.Time{},
		lastFileSize:  -1,
	}

	err = watcher.handleConfigChange(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "partial apply") {
		t.Fatalf("handleConfigChange() error = %v, want partial apply", err)
	}

	if handler.calls != 1 {
		t.Fatalf("handler calls = %d, want 1", handler.calls)
	}
	if !watcher.lastModTime.Equal(fileInfo.ModTime()) || watcher.lastFileSize != fileInfo.Size() {
		t.Fatal("failed apply did not record the processed file version")
	}
	if watcher.currentConfig == initialConfig || watcher.currentConfig.CPUThreshold != 80 {
		t.Fatal("partial apply did not retain the newly processed config")
	}

	watcher.checkConfigChange()
	if handler.calls != 1 {
		t.Fatalf("unchanged failed version was retried: handler calls = %d", handler.calls)
	}
}

func TestWatcherReportsOneTerminalReloadOutcome(t *testing.T) {
	genuineErr := errors.New("apply failed")
	tests := []struct {
		name           string
		content        string
		handlerErr     error
		wantLevel      string
		wantMessage    string
		wantProcessed  string
		wantFields     string
		wantErrorField bool
	}{
		{
			name:          "one restart-required field",
			content:       "CPU_THRESHOLD=80\n",
			handlerErr:    &RestartRequiredError{Fields: []string{"SERVER_ROLE"}},
			wantLevel:     "WARN",
			wantMessage:   "Configuration change rejected until restart",
			wantProcessed: "true",
			wantFields:    "SERVER_ROLE",
		},
		{
			name:          "multiple restart-required fields use one sorted record",
			content:       "CPU_THRESHOLD=80\n",
			handlerErr:    &RestartRequiredError{Fields: []string{"USE_SYSLOG", "SERVER_ROLE"}},
			wantLevel:     "WARN",
			wantMessage:   "Configuration change rejected until restart",
			wantProcessed: "true",
			wantFields:    "SERVER_ROLE,USE_SYSLOG",
		},
		{
			name:           "genuine apply failure",
			content:        "CPU_THRESHOLD=80\n",
			handlerErr:     genuineErr,
			wantLevel:      "ERROR",
			wantMessage:    "Configuration reload failed",
			wantProcessed:  "true",
			wantErrorField: true,
		},
		{
			name:           "validation failure",
			content:        "UNKNOWN_RELOAD_KEY=1\n",
			wantLevel:      "ERROR",
			wantMessage:    "Configuration reload failed",
			wantProcessed:  "false",
			wantErrorField: true,
		},
		{
			name:    "mixed failure stays error",
			content: "CPU_THRESHOLD=80\n",
			handlerErr: errors.Join(
				&RestartRequiredError{Fields: []string{"SERVER_ROLE"}},
				genuineErr,
			),
			wantLevel:      "ERROR",
			wantMessage:    "Configuration reload failed",
			wantProcessed:  "true",
			wantFields:     "SERVER_ROLE",
			wantErrorField: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "resman.conf")
			if err := os.WriteFile(configPath, []byte(tt.content), 0600); err != nil {
				t.Fatalf("WriteFile() error: %v", err)
			}
			logger := &recordingWatcherLogger{}
			watcher := &Watcher{
				configPath:    configPath,
				currentConfig: DefaultConfig(),
				logger:        logger,
				onChange:      &errorConfigChangeHandler{err: tt.handlerErr},
				isRunning:     true,
			}

			reloadErr := watcher.ForceReload(context.Background())
			if reloadErr == nil {
				t.Fatal("ForceReload() error = nil, want terminal outcome")
			}
			if tt.wantFields != "" {
				var restartErr *RestartRequiredError
				if !errors.As(reloadErr, &restartErr) {
					t.Fatalf("ForceReload() error = %v, want wrapped RestartRequiredError", reloadErr)
				}
			}

			entries := logger.snapshot()
			if len(entries) != 1 {
				t.Fatalf("terminal log entries = %v, want exactly one", entries)
			}
			entry := entries[0]
			if entry.level != tt.wantLevel || entry.message != tt.wantMessage {
				t.Fatalf("terminal log = %s %q, want %s %q", entry.level, entry.message, tt.wantLevel, tt.wantMessage)
			}
			if entry.fields["source"] != "forced" || entry.fields["processed"] != tt.wantProcessed {
				t.Fatalf("terminal fields = %v, want source=forced processed=%s", entry.fields, tt.wantProcessed)
			}
			if entry.fields["rejected_fields"] != tt.wantFields {
				t.Fatalf("rejected_fields = %q, want %q", entry.fields["rejected_fields"], tt.wantFields)
			}
			_, hasErrorField := entry.fields["error"]
			if hasErrorField != tt.wantErrorField {
				t.Fatalf("error field present = %t, want %t: %v", hasErrorField, tt.wantErrorField, entry.fields)
			}
		})
	}
}

func TestAutomaticWatcherReloadUsesTheSameSingleOutcomeRecord(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "resman.conf")
	if err := os.WriteFile(configPath, []byte("CPU_THRESHOLD=80\n"), 0600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	logger := &recordingWatcherLogger{}
	watcher := &Watcher{
		configPath:    configPath,
		currentConfig: DefaultConfig(),
		logger:        logger,
		onChange: &errorConfigChangeHandler{err: &RestartRequiredError{
			Fields: []string{"SERVER_ROLE"},
		}},
		isRunning: true,
	}

	watcher.reloadFromEvent(false)

	entries := logger.snapshot()
	if len(entries) != 1 {
		t.Fatalf("terminal log entries = %v, want exactly one", entries)
	}
	if entries[0].level != "WARN" || entries[0].fields["source"] != "automatic" {
		t.Fatalf("automatic terminal log = %+v, want one automatic WARN", entries[0])
	}
}

func TestWatcherKeepsCurrentConfigAfterInvalidEnvironmentOverride(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "resman.conf")
	if err := os.WriteFile(configPath, []byte("CPU_THRESHOLD=80\n"), 0600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	t.Setenv("CPU_THRESHOLD", "8O")

	initialConfig := DefaultConfig()
	handler := &channelConfigChangeHandler{values: make(chan int, 1)}
	watcher := &Watcher{
		configPath:    configPath,
		currentConfig: initialConfig,
		logger:        logging.GetLogger(),
		onChange:      handler,
		isRunning:     true,
	}

	if err := watcher.handleConfigChange(context.Background(), true); err == nil {
		t.Fatal("handleConfigChange() accepted invalid environment override")
	}

	if watcher.currentConfig != initialConfig {
		t.Fatal("invalid environment override replaced the current configuration")
	}
	select {
	case value := <-handler.values:
		t.Fatalf("reload handler called with CPU_THRESHOLD=%d", value)
	default:
	}
}

func TestWatcherHandlesRepeatedAtomicReplacement(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "resman.conf")
	writeAtomicConfig(t, configPath, 80)

	handler := &channelConfigChangeHandler{values: make(chan int, 2)}
	watcher, err := NewWatcher(configPath, DefaultConfig(), handler)
	if err != nil {
		t.Fatalf("NewWatcher() error: %v", err)
	}
	if err := watcher.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	t.Cleanup(func() {
		if err := watcher.Stop(); err != nil {
			t.Errorf("Stop() error: %v", err)
		}
	})

	for _, threshold := range []int{81, 82} {
		writeAtomicConfig(t, configPath, threshold)
		select {
		case got := <-handler.values:
			if got != threshold {
				t.Fatalf("reloaded CPU_THRESHOLD = %d, want %d", got, threshold)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for atomic replacement with CPU_THRESHOLD=%d", threshold)
		}
	}
}

func TestWatcherSerializesReloadsAndStopWaitsForCallback(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "resman.conf")
	if err := os.WriteFile(configPath, []byte("CPU_THRESHOLD=80\n"), 0600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	handler := &blockingConfigChangeHandler{
		entered: make(chan struct{}, 2),
		release: make(chan struct{}, 2),
	}
	watcher, err := NewWatcher(configPath, DefaultConfig(), handler)
	if err != nil {
		t.Fatalf("NewWatcher() error: %v", err)
	}
	if err := watcher.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	var reloadWG sync.WaitGroup
	reloadErrors := make(chan error, 2)
	reloadWG.Add(2)
	for range 2 {
		go func() {
			defer reloadWG.Done()
			reloadErrors <- watcher.ForceReload(context.Background())
		}()
	}

	select {
	case <-handler.entered:
	case <-time.After(time.Second):
		t.Fatal("first reload did not enter callback")
	}
	select {
	case <-handler.entered:
		t.Fatal("reload callbacks ran concurrently")
	case <-time.After(100 * time.Millisecond):
	}

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- watcher.Stop()
	}()
	select {
	case err := <-stopDone:
		t.Fatalf("Stop() returned before callback completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	handler.release <- struct{}{}
	select {
	case <-handler.entered:
		t.Fatal("queued reload entered callback after Stop() began")
	case <-time.After(100 * time.Millisecond):
	}

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop() error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop() did not wait for callback completion")
	}
	reloadWG.Wait()
	close(reloadErrors)
	for err := range reloadErrors {
		if err != nil && !errors.Is(err, ErrWatcherStopped) {
			t.Errorf("Reload() error = %v, want nil or ErrWatcherStopped", err)
		}
	}
	if got := handler.max.Load(); got != 1 {
		t.Fatalf("maximum concurrent callbacks = %d, want 1", got)
	}
}

func TestWatcherReloadReturnsDeadlineWhileQueued(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "resman.conf")
	if err := os.WriteFile(configPath, []byte("CPU_THRESHOLD=80\n"), 0600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	handler := &blockingConfigChangeHandler{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}, 1),
	}
	watcher, err := NewWatcher(configPath, DefaultConfig(), handler)
	if err != nil {
		t.Fatalf("NewWatcher() error: %v", err)
	}
	if err := watcher.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- watcher.ForceReload(context.Background())
	}()
	<-handler.entered

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := watcher.ForceReload(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ForceReload() error = %v, want context deadline exceeded", err)
	}

	handler.release <- struct{}{}
	if err := <-firstDone; err != nil {
		t.Fatalf("first Reload() error: %v", err)
	}
	if err := watcher.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}

func TestWatcherReloadRejectsFileChangedDuringApplication(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "resman.conf")
	writeAtomicConfig(t, configPath, 80)
	handler := &blockingConfigChangeHandler{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}, 1),
	}
	watcher, err := NewWatcher(configPath, DefaultConfig(), handler)
	if err != nil {
		t.Fatalf("NewWatcher() error: %v", err)
	}
	if err := watcher.Start(); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	writeAtomicConfig(t, configPath, 81)
	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- watcher.Reload(context.Background())
	}()
	<-handler.entered
	writeAtomicConfig(t, configPath, 82)
	handler.release <- struct{}{}

	if err := <-reloadDone; err == nil || !strings.Contains(err.Error(), "changed while runtime application") {
		t.Fatalf("Reload() error = %v, want concurrent file-change rejection", err)
	}
	if got := watcher.currentConfig.CPUThreshold; got != 81 {
		t.Fatalf("applied snapshot CPU_THRESHOLD = %d, want 81", got)
	}
	if err := watcher.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}

func writeAtomicConfig(t *testing.T, configPath string, threshold int) {
	t.Helper()
	tmpPath := configPath + ".tmp"
	content := []byte(fmt.Sprintf("CPU_THRESHOLD=%d\n", threshold))
	if err := os.WriteFile(tmpPath, content, 0600); err != nil {
		t.Fatalf("WriteFile(%s) error: %v", tmpPath, err)
	}
	if err := os.Rename(tmpPath, configPath); err != nil {
		t.Fatalf("Rename(%s, %s) error: %v", tmpPath, configPath, err)
	}
}
