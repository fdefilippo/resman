/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 */

package state

import (
	"context"
	"testing"
	"time"

	"github.com/fdefilippo/resman/internal/systemdunit"
)

func activeSystemdCPUPointsManager(t *testing.T) (*Manager, *fakeSystemdCPUUnitAdapter) {
	t.Helper()
	policy := testCPUPointsPolicy(t, map[string]struct {
		uid    int
		points int
	}{"alice": {uid: 1000, points: 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	manager.cfg.UserIncludeList = []string{"^alice$"}
	manager.cfg.PollingInterval = 30
	if err := manager.activateSystemdCPUPoints(&SystemMetrics{CPUEligibleUsers: []int{1000}}); err != nil {
		t.Fatalf("activate CPU Points plan: %v", err)
	}
	return manager, adapter
}

// TestCompleteCPUPointsVerificationRunsAtMostOncePerPollingInterval pins
// resman-2wm: on a 45-user host a complete verification reread every owned
// lease, the topology three times and every slice twice, about 0.8 s, and PSI
// triggered it about once per second. A plan confirmed less than one
// POLLING_INTERVAL ago is reused; anything that is not a fresh, complete,
// undegraded plan forces the complete verification.
func TestCompleteCPUPointsVerificationRunsAtMostOncePerPollingInterval(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*Manager)
		wantVerify bool
	}{
		{name: "fresh complete plan is reused", prepare: func(*Manager) {}},
		{
			name:       "plan older than the polling interval",
			prepare:    func(m *Manager) { m.systemdCPUVerifiedAt = time.Now().Add(-31 * time.Second) },
			wantVerify: true,
		},
		{
			name:       "plan exactly one polling interval old",
			prepare:    func(m *Manager) { m.systemdCPUVerifiedAt = time.Now().Add(-30 * time.Second) },
			wantVerify: true,
		},
		{
			name:       "verification time in the future after a clock step",
			prepare:    func(m *Manager) { m.systemdCPUVerifiedAt = time.Now().Add(time.Hour) },
			wantVerify: true,
		},
		{name: "incomplete plan", prepare: func(m *Manager) { m.systemdCPUComplete = false }, wantVerify: true},
		{name: "degraded plan", prepare: func(m *Manager) { m.cpuPointsDegraded = true }, wantVerify: true},
		{
			name: "pending policy",
			prepare: func(m *Manager) {
				pending := m.cpuPointsPolicy
				m.pendingCPUPointsPolicy = &pending
			},
			wantVerify: true,
		},
		{name: "never verified", prepare: func(m *Manager) { m.systemdCPUVerifiedAt = time.Time{} }, wantVerify: true},
		{
			name: "configuration update",
			prepare: func(m *Manager) {
				next := *m.GetConfig()
				m.UpdateConfig(&next)
			},
			wantVerify: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, adapter := activeSystemdCPUPointsManager(t)
			tt.prepare(manager)
			adapter.mu.Lock()
			reconcilesBefore, confirmsBefore := adapter.reconcileCalls, adapter.confirmCalls
			adapter.mu.Unlock()

			run := &controlCycleContext{ctx: context.Background(), cfg: manager.GetConfig(), startTime: time.Now()}
			if err := manager.stageReconcileCPUPoints(run); err != nil {
				t.Fatalf("stageReconcileCPUPoints() error: %v", err)
			}
			adapter.mu.Lock()
			verified := adapter.reconcileCalls > reconcilesBefore && adapter.confirmCalls > confirmsBefore
			untouched := adapter.reconcileCalls == reconcilesBefore && adapter.confirmCalls == confirmsBefore
			adapter.mu.Unlock()
			if tt.wantVerify && !verified {
				t.Fatal("complete verification did not run")
			}
			if !tt.wantVerify && !untouched {
				t.Fatal("fresh complete plan was verified again within one polling interval")
			}
			if !run.systemdOwnedVerified {
				t.Fatal("cycle did not record that the owned leases are verified")
			}
			if tt.wantVerify && time.Since(manager.systemdCPUVerifiedAt) > time.Minute {
				t.Fatalf("verification time = %s, want refreshed by the complete verification", manager.systemdCPUVerifiedAt)
			}
		})
	}
}

// TestNewUserSliceJoinsThePlanAtTheNextCompleteVerification shows the bound
// on topology turnover: a slice that appears while the plan is reused is
// governed after the next complete verification, within one POLLING_INTERVAL.
func TestNewUserSliceJoinsThePlanAtTheNextCompleteVerification(t *testing.T) {
	manager, adapter := activeSystemdCPUPointsManager(t)
	manager.cfg.UserIncludeList = []string{"^alice$", "^bob$"}
	adapter.replaceTopology(testSystemdTopology(0, 1000, 1001))

	run := &controlCycleContext{ctx: context.Background(), cfg: manager.GetConfig(), startTime: time.Now()}
	if err := manager.stageReconcileCPUPoints(run); err != nil {
		t.Fatalf("reused cycle error: %v", err)
	}
	if manager.GetStatus().ActivelyLimitedUsersCount != 1 {
		t.Fatalf("reused plan governs %d users, want the confirmed plan only", manager.GetStatus().ActivelyLimitedUsersCount)
	}

	manager.mu.Lock()
	manager.systemdCPUVerifiedAt = time.Now().Add(-time.Duration(manager.cfg.PollingInterval+1) * time.Second)
	manager.mu.Unlock()
	run = &controlCycleContext{ctx: context.Background(), cfg: manager.GetConfig(), startTime: time.Now()}
	if err := manager.stageReconcileCPUPoints(run); err != nil {
		t.Fatalf("verifying cycle error: %v", err)
	}
	if got := manager.GetStatus().ActivelyLimitedUsersCount; got != 2 {
		t.Fatalf("plan governs %d users after the complete verification, want 2", got)
	}
}

// TestDisabledIOWeightStageSkipsAnOwnedLeasePassAlreadyDoneThisCycle pins the
// second resman-2wm redundancy: with IO_WEIGHT_DEVICES empty, the I/O stage
// reread every owned slice through the shared adapter right after the CPU
// stage had verified them, about 116 ms per cycle.
func TestDisabledIOWeightStageSkipsAnOwnedLeasePassAlreadyDoneThisCycle(t *testing.T) {
	tests := []struct {
		name          string
		ownedVerified bool
		ioWeightLease bool
		wantReconcile bool
	}{
		{name: "verified this cycle without weight leases", ownedVerified: true},
		{name: "not verified this cycle", wantReconcile: true},
		{name: "a weight lease must still be restored", ownedVerified: true, ioWeightLease: true, wantReconcile: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, adapter := activeSystemdCPUPointsManager(t)
			manager.systemdIOWeights = adapter
			manager.cfg.IOWeightDevices = ""
			if tt.ioWeightLease {
				adapter.mu.Lock()
				adapter.activeProperties = map[string]map[systemdunit.PropertyName]bool{
					"user-1000.slice": {systemdunit.PropertyIODeviceWeight: true},
				}
				adapter.mu.Unlock()
			}
			adapter.mu.Lock()
			before := adapter.reconcileCalls
			adapter.mu.Unlock()

			run := &controlCycleContext{ctx: context.Background(), cfg: manager.GetConfig(), startTime: time.Now(), systemdOwnedVerified: tt.ownedVerified}
			_ = manager.stageReconcileIODeviceWeights(run)

			adapter.mu.Lock()
			reconciled := adapter.reconcileCalls > before
			adapter.mu.Unlock()
			if reconciled != tt.wantReconcile {
				t.Fatalf("owned-lease pass ran = %t, want %t", reconciled, tt.wantReconcile)
			}
			if status := manager.GetIODeviceWeightStatus(); status.Programmed {
				t.Fatalf("weighted I/O status = %+v, want nothing programmed", status)
			}
		})
	}
}
