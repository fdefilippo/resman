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
// metrics/db_writer.go
package metrics

import (
	"fmt"
	"sync"
	"time"

	"github.com/fdefilippo/resman/database"
)

// DBWriter coordinates periodic writes to the metrics database.
type DBWriter struct {
	dbManager     *database.DatabaseManager
	writeInterval time.Duration
	mu            sync.RWMutex
	lastWriteTime time.Time
	enabled       bool
}

// NewDBWriter creates a periodic metrics database writer.
func NewDBWriter(dbManager *database.DatabaseManager, writeIntervalSeconds int) *DBWriter {
	return &DBWriter{
		dbManager:     dbManager,
		writeInterval: time.Duration(writeIntervalSeconds) * time.Second,
		enabled:       true,
	}
}

// WriteMetricsBatch writes one system sample and all user samples atomically.
func (w *DBWriter) WriteMetricsBatch(userMetrics map[int]*UserMetrics, totalCPUUsage float64, totalCores int, systemLoad float64, limitsActive bool, limitedUsersCount int) error {
	w.mu.RLock()
	enabled := w.enabled
	w.mu.RUnlock()
	if !enabled || w.dbManager == nil {
		return nil
	}

	timestamp := time.Now().UTC()
	systemRecord := &database.SystemMetricsRecord{
		TotalCPUUsagePercent: totalCPUUsage,
		TotalCores:           totalCores,
		SystemLoad:           systemLoad,
		LimitsActive:         limitsActive,
		LimitedUsersCount:    limitedUsersCount,
		Timestamp:            timestamp,
	}
	userRecords := make([]*database.UserMetricsRecord, 0, len(userMetrics))
	for uid, metrics := range userMetrics {
		if metrics == nil {
			return fmt.Errorf("user metrics for UID %d are nil", uid)
		}
		userRecords = append(userRecords, &database.UserMetricsRecord{
			UID:               uid,
			Username:          metrics.Username,
			CPUUsagePercent:   metrics.CPUUsage,
			MemoryUsageBytes:  int64(metrics.MemoryUsage),
			ProcessCount:      metrics.ProcessCount,
			EligibleForCPU:    metrics.EligibleForCPU,
			EligibleForRAM:    metrics.EligibleForRAM,
			EligibleForIO:     metrics.EligibleForIO,
			CPULimitRequested: metrics.CPULimitRequested,
			CPULimitActive:    metrics.CPULimitActive,
			RAMLimitRequested: metrics.RAMLimitRequested,
			RAMLimitActive:    metrics.RAMLimitActive,
			IOLimitRequested:  metrics.IOLimitRequested,
			IOLimitActive:     metrics.IOLimitActive,
			Timestamp:         timestamp,
		})
	}

	if err := w.dbManager.WriteMetricsBatch(systemRecord, userRecords); err != nil {
		return fmt.Errorf("failed to write metrics collection batch: %w", err)
	}
	return nil
}

// ShouldWrite verifica se è il momento di scrivere nel database
func (w *DBWriter) ShouldWrite() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()

	if !w.enabled {
		return false
	}

	return time.Since(w.lastWriteTime) >= w.writeInterval
}

// MarkWritten marca la scrittura come avvenuta
func (w *DBWriter) MarkWritten() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastWriteTime = time.Now()
}

// SetEnabled abilita o disabilita la scrittura
func (w *DBWriter) SetEnabled(enabled bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enabled = enabled
}

// Close chiude il DBWriter
func (w *DBWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enabled = false
	return nil
}
