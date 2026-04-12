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
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/fdefilippo/resman/logging"
	"github.com/fsnotify/fsnotify"
)

// ConfigChangeHandler è l'interfaccia per gestire cambiamenti di configurazione.
type ConfigChangeHandler interface {
	OnConfigChange(*Config) error
}

// Watcher monitora i cambiamenti al file di configurazione.
type Watcher struct {
	configPath    string
	currentConfig *Config
	logger        *logging.Logger

	watcher *fsnotify.Watcher
	mu      sync.RWMutex

	// Callback chiamato quando la configurazione cambia
	onChange ConfigChangeHandler

	// Stato interno
	isRunning    bool
	stopChan     chan struct{}
	lastModTime  time.Time
	lastFileSize int64
}

// HandleConfigChange forza il ricaricamento della configurazione.
// Può essere chiamato esternamente (es. da SIGHUP handler).
func (w *Watcher) HandleConfigChange() {
	w.logger.Info("Manual configuration reload triggered")
	w.handleConfigChange()
}

// NewWatcher crea un nuovo watcher per il file di configurazione.
func NewWatcher(configPath string, initialConfig *Config, onChange ConfigChangeHandler) (*Watcher, error) {
	logger := logging.GetLogger()

	// Crea il watcher di fsnotify
	fswatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to create fsnotify file watcher: %w", err)
	}

	// Ottieni info sul file corrente
	fileInfo, err := os.Stat(configPath)
	if err != nil {
		fswatcher.Close()
		return nil, fmt.Errorf("cannot stat config file at %s: %w", configPath, err)
	}

	watcher := &Watcher{
		configPath:    configPath,
		currentConfig: initialConfig,
		logger:        logger,
		watcher:       fswatcher,
		onChange:      onChange,
		stopChan:      make(chan struct{}),
		lastModTime:   fileInfo.ModTime(),
		lastFileSize:  fileInfo.Size(),
	}

	// Aggiungi il file al watcher
	if err := fswatcher.Add(configPath); err != nil {
		fswatcher.Close()
		return nil, fmt.Errorf("failed to add config file %s to watcher: %w", configPath, err)
	}

	logger.Info("Configuration watcher initialized", "file", configPath)
	return watcher, nil
}

// Start avvia il watcher.
func (w *Watcher) Start() error {
	w.mu.Lock()
	if w.isRunning {
		w.mu.Unlock()
		return fmt.Errorf("watcher already running")
	}
	w.isRunning = true
	w.mu.Unlock()

	w.logger.Info("Starting configuration watcher")

	go w.watchLoop()

	return nil
}

// Stop ferma il watcher.
func (w *Watcher) Stop() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.isRunning {
		return nil
	}

	w.logger.Info("Stopping configuration watcher")
	close(w.stopChan)
	w.watcher.Close()
	w.isRunning = false

	return nil
}

// watchLoop è il loop principale che monitora i cambiamenti.
func (w *Watcher) watchLoop() {
	debounceTimer := time.NewTimer(0)
	if !debounceTimer.Stop() {
		<-debounceTimer.C
	}
	defer debounceTimer.Stop()

	var pendingReload bool

	// Timer per controllo periodico (ogni 30 secondi)
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

			w.logger.Debug("File system event",
				"file", event.Name,
				"op", event.Op.String(),
			)

			// Interessa solo scritture, rinomine o rimozioni
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}

			// Verifica se è il nostro file di configurazione
			if event.Name != w.configPath {
				continue
			}

			// Debounce: aspetta altri cambiamenti per 2 secondi
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
			// Controllo periodico della configurazione
			w.logger.Debug("Periodic config check")
			w.checkConfigChange()

		case <-debounceTimer.C:
			if pendingReload {
				pendingReload = false
				w.handleConfigChange()
			}
		}
	}
}

// checkConfigChange verifica se la configurazione è cambiata
func (w *Watcher) checkConfigChange() {
	fileInfo, err := os.Stat(w.configPath)
	if err != nil {
		return
	}

	w.mu.RLock()
	sameModTime := fileInfo.ModTime().Equal(w.lastModTime)
	sameSize := fileInfo.Size() == w.lastFileSize
	w.mu.RUnlock()

	if !sameModTime || !sameSize {
		w.logger.Info("Config change detected via periodic check, reloading")
		w.handleConfigChange()
	}
}

// handleConfigChange gestisce il cambio di configurazione.
func (w *Watcher) handleConfigChange() {
	w.logger.Info("Configuration file changed, attempting to reload")

	// Verifica se il file esiste ancora
	fileInfo, err := os.Stat(w.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			w.logger.Error("Configuration file removed", "file", w.configPath)
			return
		}
		w.logger.Error("Cannot stat config file", "error", err)
		return
	}

	// Verifica se il file è realmente cambiato (evita falsi positivi)
	w.mu.RLock()
	sameModTime := fileInfo.ModTime().Equal(w.lastModTime)
	sameSize := fileInfo.Size() == w.lastFileSize
	w.mu.RUnlock()

	if sameModTime && sameSize {
		w.logger.Debug("Config file not actually changed (same mod time and size)")
		return
	}

	// Prova a caricare la nuova configurazione
	newConfig, err := LoadAndValidate(w.configPath)
	if err != nil {
		w.logger.Error("Failed to reload configuration",
			"file", w.configPath,
			"error", err,
		)
		return
	}

	w.logger.Info("Configuration reloaded successfully")

	// Chiama il callback per applicare la nuova configurazione
	if w.onChange != nil {
		if err := w.onChange.OnConfigChange(newConfig); err != nil { // CORRETTO: OnConfigChange()
			w.logger.Error("Failed to apply new configuration",
				"error", err,
			)
			// Non aggiornare la configurazione corrente se fallisce
			return
		}
	}

	// Aggiorna lo stato interno
	w.mu.Lock()
	w.currentConfig = newConfig
	w.lastModTime = fileInfo.ModTime()
	w.lastFileSize = fileInfo.Size()
	w.mu.Unlock()

	w.logger.Info("New configuration applied successfully")
}

// GetCurrentConfig restituisce la configurazione corrente.
func (w *Watcher) GetCurrentConfig() *Config {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.currentConfig
}

// IsRunning restituce true se il watcher è in esecuzione.
func (w *Watcher) IsRunning() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.isRunning
}
