package config

import (
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

	watcher.handleConfigChange(false)

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
	reloadWG.Add(2)
	for range 2 {
		go func() {
			defer reloadWG.Done()
			watcher.HandleConfigChange()
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
	if got := handler.max.Load(); got != 1 {
		t.Fatalf("maximum concurrent callbacks = %d, want 1", got)
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
