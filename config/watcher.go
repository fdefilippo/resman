/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <https://www.gnu.org/licenses/>.
 */
// config/watcher.go
package config

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fdefilippo/resman/logging"
	"github.com/fsnotify/fsnotify"
)

// ErrWatcherStopped reports that a reload cannot start after watcher shutdown.
var ErrWatcherStopped = errors.New("configuration watcher is stopped")

// ConfigChangeHandler applies one validated configuration snapshot.
type ConfigChangeHandler interface {
	OnConfigChange(*Config) error
}

type watcherLogger interface {
	Debug(string, ...interface{})
	Info(string, ...interface{})
	Warn(string, ...interface{})
	Error(string, ...interface{})
}

type reloadOutcomeError struct {
	err       error
	processed bool
}

func (e *reloadOutcomeError) Error() string {
	return e.err.Error()
}

func (e *reloadOutcomeError) Unwrap() error {
	return e.err
}

// Watcher monitors and reloads one configuration file.
type Watcher struct {
	configPath    string
	currentConfig *Config
	logger        watcherLogger

	watcher    *fsnotify.Watcher
	mu         sync.RWMutex
	reloadGate chan struct{}
	loopWG     sync.WaitGroup
	reloadWG   sync.WaitGroup

	// Callback invoked after the configuration has been validated.
	onChange ConfigChangeHandler

	// Internal lifecycle state.
	isRunning    bool
	isStopped    bool
	stopChan     chan struct{}
	lastModTime  time.Time
	lastFileSize int64
	lastDigest   [sha256.Size]byte
	hasDigest    bool
}

// Reload synchronously validates and applies a file version that has not
// already been processed. Context cancellation is honored while waiting behind
// another reload; once apply has started it runs to a concrete result so callers
// never receive optimistic success for work still in progress.
func (w *Watcher) Reload(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("reload context cannot be nil")
	}
	w.logger.Info("Manual configuration reload triggered")
	err := w.handleConfigChange(ctx, false)
	w.reportReloadOutcome("manual", err)
	return err
}

// ForceReload reapplies the current file even when its content has already been
// processed, allowing SIGHUP to pick up changed environment overrides.
func (w *Watcher) ForceReload(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("reload context cannot be nil")
	}
	w.logger.Info("Forced configuration reload triggered")
	err := w.handleConfigChange(ctx, true)
	w.reportReloadOutcome("forced", err)
	return err
}

// NewWatcher creates a watcher for one configuration file.
func NewWatcher(configPath string, initialConfig *Config, onChange ConfigChangeHandler) (*Watcher, error) {
	logger := logging.GetLogger()

	absoluteConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve config path %s: %w", configPath, err)
	}
	configPath = filepath.Clean(absoluteConfigPath)

	// Create the fsnotify watcher.
	fswatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to create fsnotify file watcher: %w", err)
	}

	// Capture the current file identity.
	fileInfo, err := os.Stat(configPath)
	if err != nil {
		_ = fswatcher.Close()
		return nil, fmt.Errorf("cannot stat config file at %s: %w", configPath, err)
	}
	fileContent, err := os.ReadFile(configPath)
	if err != nil {
		_ = fswatcher.Close()
		return nil, fmt.Errorf("cannot read config file at %s: %w", configPath, err)
	}

	watcher := &Watcher{
		configPath:    configPath,
		currentConfig: initialConfig,
		logger:        logger,
		watcher:       fswatcher,
		onChange:      onChange,
		stopChan:      make(chan struct{}),
		reloadGate:    make(chan struct{}, 1),
		lastModTime:   fileInfo.ModTime(),
		lastFileSize:  fileInfo.Size(),
		lastDigest:    sha256.Sum256(fileContent),
		hasDigest:     true,
	}
	watcher.reloadGate <- struct{}{}

	watchPath := filepath.Dir(configPath)
	if err := fswatcher.Add(watchPath); err != nil {
		_ = fswatcher.Close()
		return nil, fmt.Errorf("failed to watch config directory %s: %w", watchPath, err)
	}

	logger.Info("Configuration watcher initialized", "file", configPath, "directory", watchPath)
	return watcher, nil
}

// Start begins monitoring the configuration directory.
func (w *Watcher) Start() error {
	w.mu.Lock()
	if w.isStopped {
		w.mu.Unlock()
		return fmt.Errorf("watcher already stopped")
	}
	if w.isRunning {
		w.mu.Unlock()
		return fmt.Errorf("watcher already running")
	}
	w.isRunning = true
	w.loopWG.Add(1)
	w.mu.Unlock()

	w.logger.Info("Starting configuration watcher")

	go w.watchLoop()

	return nil
}

// Stop prevents new reloads and waits for in-flight work.
func (w *Watcher) Stop() error {
	w.mu.Lock()
	if w.isStopped {
		w.mu.Unlock()
		w.loopWG.Wait()
		w.reloadWG.Wait()
		return nil
	}

	w.isRunning = false
	w.isStopped = true
	close(w.stopChan)
	w.mu.Unlock()
	w.logger.Info("Stopping configuration watcher")

	err := w.watcher.Close()
	w.loopWG.Wait()
	w.reloadWG.Wait()
	if err != nil {
		return fmt.Errorf("failed to close config watcher: %w", err)
	}

	return nil
}

// watchLoop monitors filesystem events and the periodic fallback check.
func (w *Watcher) watchLoop() {
	defer w.loopWG.Done()

	debounceTimer := time.NewTimer(0)
	if !debounceTimer.Stop() {
		<-debounceTimer.C
	}
	defer debounceTimer.Stop()

	var pendingReload bool

	// Periodic fallback check every 30 seconds.
	periodicCheck := time.NewTicker(30 * time.Second)
	defer periodicCheck.Stop()

	for {
		select {
		case <-w.stopChan:
			w.logger.Debug("Watcher stopping")
			return

		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}

			// Ignore unrelated directory events before logging. Logging them can
			// feed back into fsnotify when the log shares the config directory.
			if filepath.Clean(event.Name) != w.configPath {
				continue
			}

			// Only writes, creates, removals, and renames can change the file.
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}

			w.logger.Debug("File system event",
				"file", event.Name,
				"op", event.Op.String(),
			)

			// Coalesce bursts of filesystem events for two seconds.
			if !pendingReload {
				pendingReload = true
				debounceTimer.Reset(2 * time.Second)
				w.logger.Info("Config change detected via fsnotify, reloading in 2 seconds")
			}

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			w.logger.Warn("File watcher error", "error", err)

		case <-periodicCheck.C:
			// Periodic configuration check.
			w.logger.Debug("Periodic config check")
			w.checkConfigChange()

		case <-debounceTimer.C:
			if pendingReload {
				pendingReload = false
				w.reloadFromEvent(false)
			}
		}
	}
}

// checkConfigChange compares the current content with the last processed version.
func (w *Watcher) checkConfigChange() {
	w.reloadFromEvent(false)
}

func (w *Watcher) reloadFromEvent(force bool) {
	err := w.handleConfigChange(context.Background(), force)
	if err != nil && !errors.Is(err, ErrWatcherStopped) {
		w.reportReloadOutcome("automatic", err)
	}
}

func (w *Watcher) reportReloadOutcome(source string, err error) {
	if err == nil {
		return
	}

	classification := ClassifyReloadError(err)
	keyvals := []interface{}{
		"source", source,
		"processed", reloadOutcomeWasProcessed(err),
	}
	if len(classification.RestartRequiredFields) > 0 {
		keyvals = append(keyvals, "rejected_fields", strings.Join(classification.RestartRequiredFields, ","))
	}
	if classification.OnlyRestartRequired {
		w.logger.Warn("Configuration change rejected until restart", keyvals...)
		return
	}

	keyvals = append(keyvals, "error", err)
	w.logger.Error("Configuration reload failed", keyvals...)
}

func reloadOutcomeWasProcessed(err error) bool {
	var outcomeErr *reloadOutcomeError
	return errors.As(err, &outcomeErr) && outcomeErr.processed
}

// handleConfigChange serializes, validates, and applies one file version.
func (w *Watcher) handleConfigChange(ctx context.Context, force bool) error {
	w.mu.Lock()
	if !w.isRunning {
		w.mu.Unlock()
		return ErrWatcherStopped
	}
	if w.reloadGate == nil {
		w.reloadGate = make(chan struct{}, 1)
		w.reloadGate <- struct{}{}
	}
	reloadGate := w.reloadGate
	stopChan := w.stopChan
	w.reloadWG.Add(1)
	w.mu.Unlock()
	defer w.reloadWG.Done()

	select {
	case <-ctx.Done():
		return fmt.Errorf("waiting to reload configuration: %w", ctx.Err())
	case <-stopChan:
		return ErrWatcherStopped
	case <-reloadGate:
	}
	defer func() { reloadGate <- struct{}{} }()

	w.mu.RLock()
	running := w.isRunning
	w.mu.RUnlock()
	if !running {
		return ErrWatcherStopped
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("before reading configuration: %w", err)
	}

	// Verify that the file still exists.
	fileInfo, err := os.Stat(w.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("configuration file removed at %s: %w", w.configPath, err)
		}
		return fmt.Errorf("cannot stat configuration file %s: %w", w.configPath, err)
	}
	fileContent, err := os.ReadFile(w.configPath)
	if err != nil {
		return fmt.Errorf("cannot read configuration file %s: %w", w.configPath, err)
	}
	digest := sha256.Sum256(fileContent)

	// Avoid processing the same content twice. Metadata alone is insufficient:
	// atomic replacements can preserve both size and timestamp resolution.
	w.mu.RLock()
	sameContent := w.hasDigest && digest == w.lastDigest
	w.mu.RUnlock()

	if !force && sameContent {
		w.logger.Debug("Config file content has not changed")
		return nil
	}
	w.logger.Info("Configuration file changed, attempting to reload")

	// Load and validate a detached configuration snapshot.
	newConfig, err := LoadAndValidate(w.configPath)
	if err != nil {
		return fmt.Errorf("reload configuration from %s: %w", w.configPath, err)
	}

	w.logger.Info("Configuration validated successfully")
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("before applying configuration: %w", err)
	}
	currentContent, err := os.ReadFile(w.configPath)
	if err != nil {
		return fmt.Errorf("confirm configuration file %s before apply: %w", w.configPath, err)
	}
	if currentDigest := sha256.Sum256(currentContent); currentDigest != digest {
		return fmt.Errorf("configuration file %s changed while it was being validated", w.configPath)
	}

	var applyErr error
	if w.onChange != nil {
		if err := w.onChange.OnConfigChange(newConfig); err != nil {
			applyErr = err
		}
	}
	var confirmationErr error
	postApplyContent, err := os.ReadFile(w.configPath)
	if err != nil {
		confirmationErr = fmt.Errorf("confirm configuration file %s after apply: %w", w.configPath, err)
	} else if postApplyDigest := sha256.Sum256(postApplyContent); postApplyDigest != digest {
		confirmationErr = fmt.Errorf("configuration file %s changed while runtime application was in progress", w.configPath)
	}

	w.mu.Lock()
	w.currentConfig = newConfig
	if confirmationErr == nil {
		w.lastModTime = fileInfo.ModTime()
		w.lastFileSize = fileInfo.Size()
		w.lastDigest = digest
		w.hasDigest = true
	}
	w.mu.Unlock()

	if confirmationErr != nil {
		return &reloadOutcomeError{
			err:       errors.Join(applyErr, confirmationErr),
			processed: false,
		}
	}

	if applyErr != nil {
		return &reloadOutcomeError{
			err:       fmt.Errorf("apply configuration from %s: %w", w.configPath, applyErr),
			processed: true,
		}
	}

	w.logger.Info("New configuration applied successfully")
	return nil
}
