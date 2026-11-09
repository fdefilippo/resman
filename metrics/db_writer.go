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
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/internal/controlhistory"
)

// DBWriter coordinates periodic writes to the metrics database.
type DBWriter struct {
	dbManager          *database.DatabaseManager
	writeInterval      time.Duration
	mu                 sync.RWMutex
	lastWriteTime      time.Time
	enabled            bool
	lastWriteFailed    bool
	historyWriteFailed bool
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
func (w *DBWriter) WriteMetricsBatch(batch PersistenceBatch) (writeErr error) {
	w.mu.RLock()
	enabled := w.enabled
	w.mu.RUnlock()
	if !enabled || w.dbManager == nil {
		return nil
	}

	defer func() { w.mu.Lock(); w.lastWriteFailed = writeErr != nil; w.mu.Unlock() }()

	system := batch.System
	timestamp := system.IntervalEnd.UTC()
	systemRecord := &database.SystemMetricsRecord{
		IODeviceWeightState: system.IODeviceWeightState, IODeviceWeightReason: system.IODeviceWeightReason,
		IODeviceWeightSelector:               system.IODeviceWeightSelector,
		IODeviceWeightMechanism:              system.IODeviceWeightMechanism,
		IODeviceWeightClassificationAttempts: system.IODeviceWeightClassificationAttempts,
		IODeviceWeightProbeAttempts:          system.IODeviceWeightProbeAttempts,
		IODeviceWeightProgrammed:             system.IODeviceWeightProgrammed, IODeviceWeightReadBack: system.IODeviceWeightReadBack,
		IODeviceWeightProgrammedState:               system.IODeviceWeightProgrammedState,
		IODeviceWeightReadBackState:                 system.IODeviceWeightReadBackState,
		IODeviceWeightFunctionallyAccepted:          system.IODeviceWeightFunctionallyAccepted,
		IODeviceWeightEffectQualified:               system.IODeviceWeightEffectQualified,
		IODeviceWeightEffectQualificationProvenance: system.IODeviceWeightEffectQualificationProvenance,
		IODeviceWeightAuthorityCoverage:             system.IODeviceWeightAuthorityCoverage,
		IODeviceWeightCompleteUsers:                 system.IODeviceWeightCompleteUsers,
		IODeviceWeightPartialUsers:                  system.IODeviceWeightPartialUsers,
		IODeviceWeightUnavailableUsers:              system.IODeviceWeightUnavailableUsers,
		IODeviceWeightSiblingSlices:                 system.IODeviceWeightSiblingSlices,
		IODeviceWeightTotalPoints:                   system.IODeviceWeightTotalPoints,
		IODeviceWeightRequestedAt:                   system.IODeviceWeightRequestedAt,
		IODeviceWeightNextRetryAt:                   system.IODeviceWeightNextRetryAt,
		IODeviceWeightValuesJSON:                    system.IODeviceWeightValuesJSON,
		IODeviceWeightObservedDelivery:              system.IODeviceWeightObservedDelivery,
		DenominatorState:                            string(system.DenominatorState), EnforcementMode: system.EnforcementMode,
		SampleEpochID:                  system.SampleEpochID,
		IntervalStart:                  system.IntervalStart,
		IntervalEnd:                    timestamp,
		TotalCPUUsagePercent:           system.TotalCPUUsagePercent,
		TotalCores:                     system.TotalCores,
		SystemLoad:                     system.SystemLoad,
		CPULimitsActive:                system.CPULimitsActive,
		ResourceLimitsActive:           system.ResourceLimitsActive,
		AnyLimitsActive:                system.AnyLimitsActive,
		CPUActivelyLimitedUsersCount:   system.CPUActivelyLimitedUsersCount,
		ActivelyLimitedUsersCount:      system.ActivelyLimitedUsersCount,
		NominalParentPoolPoints:        system.NominalParentPoolPoints,
		CPUCapacityAvailable:           system.CPUCapacityAvailable,
		OnlineCPUs:                     system.OnlineCPUs,
		ProgrammedParentQuotaUsec:      system.ProgrammedParentQuotaUsec,
		ProgrammedParentPeriodUsec:     system.ProgrammedParentPeriodUsec,
		CPUPointsDegraded:              system.CPUPointsDegraded,
		AppliedGuaranteePoints:         system.AppliedGuaranteePoints,
		ProgrammedGuaranteeWeight:      system.ProgrammedGuaranteeWeight,
		ConfiguredBestEffortPoints:     system.ConfiguredBestEffortPoints,
		ParentCPUQuota:                 system.ParentCPUQuota,
		ProgrammedSiblingWeightSum:     system.ProgrammedSiblingWeightSum,
		ProgrammedBestEffortWeight:     system.ProgrammedBestEffortWeight,
		ParentCPUUsageUsecDelta:        system.ParentCPUUsageUsecDelta,
		ObservedSiblingWeightSum:       system.ObservedSiblingWeightSum,
		ConfiguredRootPoints:           system.ConfiguredRootPoints,
		ParentCPUPeriodsDelta:          system.ParentCPUPeriodsDelta,
		ParentCPUThrottledPeriodsDelta: system.ParentCPUThrottledPeriodsDelta,
		ParentCPUThrottledUsecDelta:    system.ParentCPUThrottledUsecDelta,
		Timestamp:                      timestamp,
	}
	uids := make([]int, 0, len(batch.Users))
	for uid := range batch.Users {
		uids = append(uids, uid)
	}
	sort.Ints(uids)
	userRecords := make([]*database.UserMetricsRecord, 0, len(batch.Users))
	for _, uid := range uids {
		user := batch.Users[uid]
		if user.Metrics == nil {
			return fmt.Errorf("user metrics for UID %d are nil", uid)
		}
		metrics := user.Metrics
		userRecords = append(userRecords, &database.UserMetricsRecord{
			ProcessObservationUnavailable: user.ProcessObservationUnavailable,
			CPUAuthorityCoverage:          user.CPUAuthorityCoverage, IOCoverage: user.IOCoverage,
			SampleEpochID:                     system.SampleEpochID,
			IntervalStart:                     system.IntervalStart,
			IntervalEnd:                       timestamp,
			UID:                               uid,
			Username:                          metrics.Username,
			CPUUsagePercent:                   metrics.CPUUsage,
			MemoryUsageBytes:                  int64(metrics.MemoryUsage),
			ProcessCount:                      metrics.ProcessCount,
			CgroupPath:                        user.CgroupPath,
			CPUQuota:                          user.CPUQuota,
			ConfiguredGuaranteePoints:         user.ConfiguredGuaranteePoints,
			ConfiguredCPUClass:                user.ConfiguredClass,
			CPUPointsLifecycleState:           string(user.LifecycleState),
			AppliedCPUClass:                   user.AppliedClass,
			AppliedCPUWeight:                  user.AppliedWeight,
			CPUWeight:                         user.CPUWeight,
			LeafCPUUsageUsecDelta:             user.LeafCPUUsageUsecDelta,
			EnforceableProcessCount:           metrics.EnforceableUsage.ProcessCount,
			RAMCgroupUsageBytes:               user.RAMCgroupUsageBytes,
			RAMCoverage:                       user.RAMCoverage,
			RAMCoverageIncompleteProcessCount: user.RAMCoverageIncompleteProcessCount,
			RAMSwapDisabled:                   user.RAMSwapDisabled,
			MemoryHighLimit:                   user.MemoryHighLimit,
			MemoryMaxLimit:                    user.MemoryMaxLimit,
			MemorySwapMax:                     user.MemorySwapMax,
			MemoryHighEventsDelta:             user.MemoryHighEventsDelta,
			MemoryMaxEventsDelta:              user.MemoryMaxEventsDelta,
			MemoryOOMEventsDelta:              user.MemoryOOMEventsDelta,
			MemoryOOMKillEventsDelta:          user.MemoryOOMKillEventsDelta,
			EligibleForCPU:                    metrics.EligibleForCPU,
			EligibleForRAM:                    metrics.EligibleForRAM,
			EligibleForIO:                     metrics.EligibleForIO,
			CPULimitRequested:                 metrics.CPULimitRequested,
			CPULimitActive:                    metrics.CPULimitActive,
			RAMLimitRequested:                 metrics.RAMLimitRequested,
			RAMLimitActive:                    metrics.RAMLimitActive,
			IOLimitRequested:                  metrics.IOLimitRequested,
			IOLimitActive:                     metrics.IOLimitActive,
			Timestamp:                         timestamp,
		})
	}

	if err := w.dbManager.WriteMetricsBatch(systemRecord, userRecords); err != nil {
		return fmt.Errorf("failed to write metrics collection batch: %w", err)
	}
	return nil
}

// ShouldWrite reports whether the database write interval has elapsed.
func (w *DBWriter) ShouldWrite() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()

	if !w.enabled {
		return false
	}

	return time.Since(w.lastWriteTime) >= w.writeInterval
}

// MarkWritten records a successful database write.
func (w *DBWriter) MarkWritten() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastWriteTime = time.Now()
}

// SetEnabled enables or disables database writes.
func (w *DBWriter) SetEnabled(enabled bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enabled = enabled
}

// Close disables the DBWriter.
func (w *DBWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enabled = false
	return nil
}

// Healthy reports enablement and the last attempted write, without database I/O.
func (w *DBWriter) Healthy() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.enabled && !w.lastWriteFailed && !w.historyWriteFailed
}

// WriteControlHistory persists terminal cycles independently of metrics sampling.
func (w *DBWriter) WriteControlHistory(ctx context.Context, entries []controlhistory.Entry) (writeErr error) {
	w.mu.RLock()
	enabled, db := w.enabled, w.dbManager
	w.mu.RUnlock()
	if !enabled || db == nil {
		return fmt.Errorf("control history database unavailable")
	}
	defer func() { w.mu.Lock(); w.historyWriteFailed = writeErr != nil; w.mu.Unlock() }()
	return db.WriteControlHistory(ctx, entries)
}

// ReadControlHistory queries the same optional store without changing sampling state.
func (w *DBWriter) ReadControlHistory(ctx context.Context, limit int) ([]controlhistory.Entry, error) {
	w.mu.RLock()
	enabled, db := w.enabled, w.dbManager
	w.mu.RUnlock()
	if !enabled || db == nil {
		return nil, fmt.Errorf("control history database unavailable")
	}
	return db.ReadControlHistory(ctx, limit)
}
