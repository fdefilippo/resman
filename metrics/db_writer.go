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
    "sync"
    "time"

    "github.com/fdefilippo/resman/database"
    "github.com/fdefilippo/resman/logging"
)

// DBWriter gestisce la scrittura delle metriche nel database
type DBWriter struct {
    dbManager      *database.DatabaseManager
    logger         *logging.Logger
    writeInterval  time.Duration
    mu             sync.RWMutex
    lastWriteTime  time.Time
    enabled        bool
}

// NewDBWriter crea un nuovo DBWriter
func NewDBWriter(dbManager *database.DatabaseManager, writeIntervalSeconds int) *DBWriter {
    logger := logging.GetLogger()
    
    return &DBWriter{
        dbManager:     dbManager,
        logger:        logger,
        writeInterval: time.Duration(writeIntervalSeconds) * time.Second,
        enabled:       true,
    }
}

// WriteUserMetrics scrive le metriche utente nel database
func (w *DBWriter) WriteUserMetrics(uid int, username string, cpuUsage float64, memoryUsage uint64, processCount int, isLimited bool, cgroupPath string, cpuQuota string) {
    w.mu.RLock()
    if !w.enabled {
        w.mu.RUnlock()
        return
    }
    w.mu.RUnlock()

    if w.dbManager == nil {
        return
    }

    record := &database.UserMetricsRecord{
        UID:              uid,
        Username:         username,
        CPUUsagePercent:  cpuUsage,
        MemoryUsageBytes: int64(memoryUsage),
        ProcessCount:     processCount,
        CgroupPath:       cgroupPath,
        CPUQuota:         cpuQuota,
        IsLimited:        isLimited,
        Timestamp:        time.Now(),
    }

    if err := w.dbManager.WriteUserMetrics(record); err != nil {
        w.logger.Debug("Failed to write user metrics to database", "uid", uid, "username", username, "error", err)
    }
}

// WriteSystemMetrics scrive le metriche di sistema nel database
func (w *DBWriter) WriteSystemMetrics(totalCPUUsage float64, totalCores int, systemLoad float64, limitsActive bool, limitedUsersCount int) {
    w.mu.RLock()
    if !w.enabled {
        w.mu.RUnlock()
        return
    }
    w.mu.RUnlock()

    if w.dbManager == nil {
        return
    }

    record := &database.SystemMetricsRecord{
        TotalCPUUsagePercent: totalCPUUsage,
        TotalCores:           totalCores,
        SystemLoad:           systemLoad,
        LimitsActive:         limitsActive,
        LimitedUsersCount:    limitedUsersCount,
        Timestamp:            time.Now(),
    }

    if err := w.dbManager.WriteSystemMetrics(record); err != nil {
        w.logger.Debug("Failed to write system metrics to database", "error", err)
    }
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
