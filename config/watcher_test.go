package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fdefilippo/resman/logging"
)

type failingConfigChangeHandler struct {
	calls int
}

func (h *failingConfigChangeHandler) OnConfigChange(*Config) error {
	h.calls++
	return errors.New("partial apply")
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
