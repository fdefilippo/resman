package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func writeFakeProcessExecutable(t *testing.T, manager *Manager, pid int, executable string) {
	t.Helper()
	processPath := filepath.Join(manager.getProcRoot(), strconv.Itoa(pid))
	name := filepath.Base(executable)
	if err := os.WriteFile(filepath.Join(processPath, "comm"), []byte(name+"\n"), 0644); err != nil {
		t.Fatalf("failed to write fake comm: %v", err)
	}
	if err := os.Symlink(executable, filepath.Join(processPath, "exe")); err != nil {
		t.Fatalf("failed to create fake exe link: %v", err)
	}
}

func writeFakeCgroupMembers(t *testing.T, target string, members map[int]bool) {
	t.Helper()
	pids := make([]int, 0, len(members))
	for pid := range members {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	var data strings.Builder
	for _, pid := range pids {
		fmt.Fprintf(&data, "%d\n", pid)
	}
	if err := os.WriteFile(filepath.Join(target, "cgroup.procs"), []byte(data.String()), 0644); err != nil {
		t.Fatalf("failed to write fake cgroup.procs: %v", err)
	}
}

func updateFakeProcessCgroup(t *testing.T, manager *Manager, pid int, destination string) {
	t.Helper()
	relative, err := filepath.Rel(manager.getConfig().CgroupRoot, destination)
	if err != nil {
		t.Fatalf("failed to derive fake cgroup path: %v", err)
	}
	cgroupPath := "/" + filepath.ToSlash(relative)
	processPath := filepath.Join(manager.getProcRoot(), strconv.Itoa(pid), "cgroup")
	if err := os.WriteFile(processPath, []byte("0::"+cgroupPath+"\n"), 0644); err != nil {
		t.Fatalf("failed to update fake process cgroup: %v", err)
	}
}

func TestReconcileUserProcessMembershipMovesNewAndRestoresExcludedIdempotently(t *testing.T) {
	manager, root := newOriginTestManager(t)
	manager.getConfig().ProcessExcludeList = []string{"^systemd$"}
	uid := 1000
	sharedPath := createFakeCgroup(t, root, "/resman/limited")
	target := createFakeCgroup(t, root, "/resman/limited/user_1000")
	workerOrigin := "/user.slice/user-1000.slice/session-101.scope"
	systemdOrigin := "/user.slice/user-1000.slice/session-102.scope"
	workerOriginPath := createFakeCgroup(t, root, workerOrigin)
	systemdOriginPath := createFakeCgroup(t, root, systemdOrigin)
	writeFakeProcess(t, manager, 101, 1, 101, 5101, uid, workerOrigin)
	writeFakeProcessExecutable(t, manager, 101, "/usr/bin/worker")
	writeFakeProcess(t, manager, 102, 1, 102, 5102, uid, "/resman/limited/user_1000")
	writeFakeProcessExecutable(t, manager, 102, "/usr/lib/systemd/systemd")
	manager.processOrigins[102] = processOrigin{
		PID: 102, UID: uid, PPID: 1, SessionID: 102, StartTime: 5102, CgroupPath: systemdOrigin,
	}
	manager.scanProcessIDs = func() (map[int][]int, error) {
		return map[int][]int{uid: {101, 102}}, nil
	}

	members := map[int]bool{102: true}
	writeFakeCgroupMembers(t, target, members)
	writeCalls := 0
	manager.writePID = func(procsFile string, pid int) error {
		writeCalls++
		destination := filepath.Dir(procsFile)
		switch destination {
		case target:
			members[pid] = true
		case workerOriginPath, systemdOriginPath:
			delete(members, pid)
		default:
			return fmt.Errorf("unexpected destination %s", destination)
		}
		updateFakeProcessCgroup(t, manager, pid, destination)
		writeFakeCgroupMembers(t, target, members)
		return nil
	}

	result, err := manager.ReconcileUserProcessMembership(uid, sharedPath, "max 100000")
	if err != nil {
		t.Fatalf("ReconcileUserProcessMembership() error: %v", err)
	}
	if result.MovedIn != 1 || result.RestoredExcluded != 1 || result.SkippedReused != 0 {
		t.Fatalf("reconciliation result = %+v, want one move and one restore", result)
	}
	if !members[101] || members[102] {
		t.Fatalf("limited members after reconciliation = %v, want only PID 101", members)
	}
	if writeCalls != 2 {
		t.Fatalf("write calls = %d, want 2", writeCalls)
	}

	second, err := manager.ReconcileUserProcessMembership(uid, sharedPath, "max 100000")
	if err != nil {
		t.Fatalf("second ReconcileUserProcessMembership() error: %v", err)
	}
	if second.MovedIn != 0 || second.RestoredExcluded != 0 || second.SkippedReused != 0 {
		t.Fatalf("idempotent reconciliation result = %+v, want no changes", second)
	}
	if writeCalls != 2 {
		t.Fatalf("idempotent reconciliation performed another write: %d", writeCalls)
	}
}

func TestReconcileUserProcessMembershipFailsClosedWithoutAnOrigin(t *testing.T) {
	manager, root := newOriginTestManager(t)
	manager.getConfig().ProcessExcludeList = []string{"^systemd$"}
	uid := 1000
	sharedPath := createFakeCgroup(t, root, "/resman/limited")
	target := createFakeCgroup(t, root, "/resman/limited/user_1000")
	writeFakeProcess(t, manager, 201, 1, 201, 5201, uid, "/resman/limited/user_1000")
	writeFakeProcessExecutable(t, manager, 201, "/usr/lib/systemd/systemd")
	writeFakeCgroupMembers(t, target, map[int]bool{201: true})
	manager.scanProcessIDs = func() (map[int][]int, error) {
		return map[int][]int{uid: {201}}, nil
	}
	writeCalled := false
	manager.writePID = func(string, int) error {
		writeCalled = true
		return nil
	}

	_, err := manager.ReconcileUserProcessMembership(uid, sharedPath, "max 100000")
	var originErr *ProcessOriginUnavailableError
	if !errors.As(err, &originErr) || originErr.PID != 201 || originErr.UID != uid {
		t.Fatalf("ReconcileUserProcessMembership() error = %v, want unavailable-origin failure", err)
	}
	if writeCalled {
		t.Fatal("excluded process moved without a safe origin")
	}
}

func TestReconcileUserProcessMembershipRestoresValidPeersWhenOneOriginIsUnavailable(t *testing.T) {
	manager, root := newOriginTestManager(t)
	manager.getConfig().ProcessExcludeList = []string{"^systemd$"}
	const uid = 1000
	const blockedPID = 211
	const restorablePID = 212
	sharedPath := createFakeCgroup(t, root, "/resman/limited")
	target := createFakeCgroup(t, root, "/resman/limited/user_1000")
	restorableOrigin := "/user.slice/user-1000.slice/session-212.scope"
	restorableOriginPath := createFakeCgroup(t, root, restorableOrigin)

	for _, pid := range []int{blockedPID, restorablePID} {
		writeFakeProcess(t, manager, pid, 1, pid, uint64(5200+pid), uid, "/resman/limited/user_1000")
		writeFakeProcessExecutable(t, manager, pid, "/usr/lib/systemd/systemd")
	}
	manager.processOrigins[restorablePID] = processOrigin{
		PID:        restorablePID,
		UID:        uid,
		PPID:       1,
		SessionID:  restorablePID,
		StartTime:  uint64(5200 + restorablePID),
		CgroupPath: restorableOrigin,
	}
	members := map[int]bool{blockedPID: true, restorablePID: true}
	writeFakeCgroupMembers(t, target, members)
	manager.scanProcessIDs = func() (map[int][]int, error) {
		return map[int][]int{uid: {blockedPID, restorablePID}}, nil
	}
	var restored []int
	manager.writePID = func(procsFile string, pid int) error {
		if filepath.Dir(procsFile) != restorableOriginPath {
			return fmt.Errorf("unexpected destination %s", filepath.Dir(procsFile))
		}
		restored = append(restored, pid)
		delete(members, pid)
		updateFakeProcessCgroup(t, manager, pid, restorableOriginPath)
		writeFakeCgroupMembers(t, target, members)
		return nil
	}

	result, err := manager.ReconcileUserProcessMembership(uid, sharedPath, "max 100000")
	var originErr *ProcessOriginUnavailableError
	if !errors.As(err, &originErr) || originErr.PID != blockedPID {
		t.Fatalf("ReconcileUserProcessMembership() error = %v, want PID %d unavailable", err, blockedPID)
	}
	if result.RestoredExcluded != 1 || len(restored) != 1 || restored[0] != restorablePID {
		t.Fatalf("reconciliation result = %+v, restored=%v, want only PID %d restored", result, restored, restorablePID)
	}
	if !members[blockedPID] || members[restorablePID] {
		t.Fatalf("limited members after partial reconciliation = %v, want only blocked PID", members)
	}
}

func TestReconcileUserProcessMembershipSkipsExitAndPIDReuseBeforeMove(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*testing.T, *Manager, int)
		wantReused int
	}{
		{
			name: "process exits",
			mutate: func(t *testing.T, manager *Manager, pid int) {
				if err := os.RemoveAll(filepath.Join(manager.getProcRoot(), strconv.Itoa(pid))); err != nil {
					t.Fatalf("failed to remove exited fake process: %v", err)
				}
			},
		},
		{
			name: "PID is reused",
			mutate: func(t *testing.T, manager *Manager, pid int) {
				writeFakeProcess(t, manager, pid, 1, pid, 9999, 1000, "/user.slice/session-reused.scope")
			},
			wantReused: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, root := newOriginTestManager(t)
			uid := 1000
			pid := 301
			sharedPath := createFakeCgroup(t, root, "/resman/limited")
			target := createFakeCgroup(t, root, "/resman/limited/user_1000")
			origin := "/user.slice/session-301.scope"
			createFakeCgroup(t, root, origin)
			writeFakeProcess(t, manager, pid, 1, pid, 5301, uid, origin)
			writeFakeProcessExecutable(t, manager, pid, "/usr/bin/worker")
			writeFakeCgroupMembers(t, target, nil)
			manager.scanProcessIDs = func() (map[int][]int, error) {
				return map[int][]int{uid: {pid}}, nil
			}
			mutated := false
			manager.persistOrigins = func() error {
				if !mutated {
					mutated = true
					tt.mutate(t, manager, pid)
				}
				return nil
			}
			writeCalled := false
			manager.writePID = func(string, int) error {
				writeCalled = true
				return nil
			}

			result, err := manager.ReconcileUserProcessMembership(uid, sharedPath, "max 100000")
			if err != nil {
				t.Fatalf("ReconcileUserProcessMembership() error: %v", err)
			}
			if result.MovedIn != 0 || result.SkippedReused != tt.wantReused {
				t.Fatalf("reconciliation result = %+v, want no move and reused=%d", result, tt.wantReused)
			}
			if writeCalled {
				t.Fatal("stale PID was written to cgroup.procs")
			}
			if len(manager.snapshotProcessOrigins()) != 0 {
				t.Fatal("origin state retained an exited or reused PID")
			}
		})
	}
}
