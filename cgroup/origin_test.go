package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

func newOriginTestManager(t *testing.T) (*Manager, string) {
	t.Helper()

	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = root
	cfg.CgroupBase = "resman"
	cfg.CreatedCgroupsFile = filepath.Join(root, "state", "resman-cgroups.txt")

	recoveryRoot := filepath.Join(root, "resman", "recovery")
	if err := os.MkdirAll(recoveryRoot, 0755); err != nil {
		t.Fatalf("failed to create recovery test root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(recoveryRoot, "cgroup.subtree_control"), nil, 0644); err != nil {
		t.Fatalf("failed to create recovery subtree control: %v", err)
	}

	manager := &Manager{
		cfg:                cfg,
		logger:             logging.GetLogger(),
		createdCgroups:     make(map[int]string),
		createdCgroupsFile: cfg.CreatedCgroupsFile,
		processOrigins:     make(map[int]processOrigin),
		processOriginsFile: processOriginsPath(cfg.CreatedCgroupsFile),
		procRoot:           filepath.Join(root, "proc"),
		pidNamespace:       pidNamespaceIdentity{device: 1, inode: 1},
		readPIDNamespace: func(int) (pidNamespaceIdentity, error) {
			return pidNamespaceIdentity{device: 1, inode: 1}, nil
		},
	}
	return manager, root
}

func writeFakeProcess(t *testing.T, manager *Manager, pid, ppid, sessionID int, startTime uint64, uid int, cgroupPath string) {
	t.Helper()

	procPath := filepath.Join(manager.getProcRoot(), strconv.Itoa(pid))
	if err := os.MkdirAll(procPath, 0755); err != nil {
		t.Fatalf("failed to create fake proc path: %v", err)
	}

	fields := []string{"S", strconv.Itoa(ppid), strconv.Itoa(sessionID), strconv.Itoa(sessionID)}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, strconv.FormatUint(startTime, 10))
	stat := fmt.Sprintf("%d (process %d) %s\n", pid, pid, strings.Join(fields, " "))
	if err := os.WriteFile(filepath.Join(procPath, "stat"), []byte(stat), 0644); err != nil {
		t.Fatalf("failed to write fake process stat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(procPath, "cgroup"), []byte("0::"+cgroupPath+"\n"), 0644); err != nil {
		t.Fatalf("failed to write fake process cgroup: %v", err)
	}
	status := fmt.Sprintf("Name:\tprocess\nUid:\t%d\t%d\t%d\t%d\n", uid, uid, uid, uid)
	if err := os.WriteFile(filepath.Join(procPath, "status"), []byte(status), 0644); err != nil {
		t.Fatalf("failed to write fake process status: %v", err)
	}
}

func createFakeCgroup(t *testing.T, root, cgroupPath string) string {
	t.Helper()

	path := filepath.Join(root, strings.TrimPrefix(filepath.Clean(cgroupPath), "/"))
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("failed to create fake cgroup %s: %v", path, err)
	}
	return path
}

func TestMoveProcessPersistsOriginBeforeMigration(t *testing.T) {
	manager, root := newOriginTestManager(t)
	originalPath := "/user.slice/user-1000.slice/session-7.scope"
	writeFakeProcess(t, manager, 101, 1, 101, 5000, 1000, originalPath)
	createFakeCgroup(t, root, originalPath)

	sharedUserPath := createFakeCgroup(t, root, "/resman/limited/user_1000")
	manager.writePID = func(path string, pid int) error {
		if _, err := os.Stat(manager.processOriginsFile); err != nil {
			return fmt.Errorf("origin state was not persisted before migration: %w", err)
		}
		if path != filepath.Join(sharedUserPath, "cgroup.procs") || pid != 101 {
			return fmt.Errorf("unexpected migration target %s for PID %d", path, pid)
		}
		return nil
	}

	if _, err := manager.MoveProcessToSharedCgroup(101, filepath.Dir(sharedUserPath), 1000); err != nil {
		t.Fatalf("MoveProcessToSharedCgroup() error: %v", err)
	}

	origin, ok := manager.snapshotProcessOrigins()[101]
	if !ok {
		t.Fatal("expected persisted origin for PID 101")
	}
	if origin.CgroupPath != originalPath || origin.StartTime != 5000 {
		t.Fatalf("unexpected origin record: %+v", origin)
	}
	info, err := os.Stat(manager.processOriginsFile)
	if err != nil {
		t.Fatalf("failed to stat process origin state: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("process origin state mode = %o, want 600", info.Mode().Perm())
	}

	reloaded := &Manager{
		processOrigins:     make(map[int]processOrigin),
		processOriginsFile: manager.processOriginsFile,
	}
	if err := reloaded.loadProcessOrigins(); err != nil {
		t.Fatalf("loadProcessOrigins() error: %v", err)
	}
	if got := reloaded.processOrigins[101]; got.StartTime != 5000 || got.CgroupPath != originalPath {
		t.Fatalf("reloaded origin = %+v", got)
	}
}

func TestMoveProcessBatchPersistsAllOriginsOnceBeforeMigration(t *testing.T) {
	manager, root := newOriginTestManager(t)
	destination := createFakeCgroup(t, root, "/resman/limited/user_1000")
	pids := []int{111, 112, 113}
	for i, pid := range pids {
		origin := fmt.Sprintf("/user.slice/user-1000.slice/session-%d.scope", pid)
		writeFakeProcess(t, manager, pid, 1, pid, uint64(5100+i), 1000, origin)
		createFakeCgroup(t, root, origin)
	}

	persistCalls := 0
	persisted := false
	manager.persistOrigins = func() error {
		persistCalls++
		persisted = true
		return nil
	}
	writeCalls := 0
	manager.writePID = func(path string, pid int) error {
		if !persisted {
			return fmt.Errorf("PID %d moved before origins were persisted", pid)
		}
		if path != filepath.Join(destination, "cgroup.procs") {
			return fmt.Errorf("unexpected migration target %s", path)
		}
		writeCalls++
		return nil
	}

	moved, _, moveErrors, err := manager.moveProcessBatch(pids, 1000, destination)
	if err != nil {
		t.Fatalf("moveProcessBatch() error: %v", err)
	}
	if len(moveErrors) != 0 {
		t.Fatalf("moveProcessBatch() move errors: %v", moveErrors)
	}
	if len(moved) != len(pids) || writeCalls != len(pids) {
		t.Fatalf("moved=%v writeCalls=%d, want %d processes", moved, writeCalls, len(pids))
	}
	if persistCalls != 1 {
		t.Fatalf("origin persist calls = %d, want 1", persistCalls)
	}
	if len(manager.snapshotProcessOrigins()) != len(pids) {
		t.Fatalf("persisted origins = %d, want %d", len(manager.snapshotProcessOrigins()), len(pids))
	}
}

func TestMoveProcessBatchRollsBackBeforeAnyMigration(t *testing.T) {
	manager, root := newOriginTestManager(t)
	destination := createFakeCgroup(t, root, "/resman/limited/user_1000")
	writeFakeProcess(t, manager, 121, 1, 121, 5200, 1000, "/user.slice/session-121.scope")

	manager.persistOrigins = func() error {
		return fmt.Errorf("disk unavailable")
	}
	writeCalled := false
	manager.writePID = func(string, int) error {
		writeCalled = true
		return nil
	}

	if _, _, _, err := manager.moveProcessBatch([]int{121}, 1000, destination); err == nil {
		t.Fatal("moveProcessBatch() should fail when origin persistence fails")
	}
	if writeCalled {
		t.Fatal("process migration started before the origin transaction committed")
	}
	if len(manager.snapshotProcessOrigins()) != 0 {
		t.Fatal("failed origin transaction was not rolled back")
	}
}

func TestProcessOriginSnapshotRemainsAvailableWhilePersistenceBlocks(t *testing.T) {
	manager, root := newOriginTestManager(t)
	destination := createFakeCgroup(t, root, "/resman/limited/user_1000")
	origin := "/user.slice/session-122.scope"
	writeFakeProcess(t, manager, 122, 1, 122, 5201, 1000, origin)
	createFakeCgroup(t, root, origin)

	started := make(chan struct{})
	release := make(chan struct{})
	manager.persistOrigins = func() error {
		close(started)
		<-release
		return nil
	}
	moveDone := make(chan error, 1)
	go func() {
		_, _, _, err := manager.moveProcessBatch([]int{122}, 1000, destination)
		moveDone <- err
	}()
	<-started

	snapshotDone := make(chan map[int]processOrigin, 1)
	go func() { snapshotDone <- manager.snapshotProcessOrigins() }()
	select {
	case snapshot := <-snapshotDone:
		if len(snapshot) != 0 {
			t.Fatalf("uncommitted process origins became visible: %+v", snapshot)
		}
	case <-time.After(time.Second):
		close(release)
		<-moveDone
		t.Fatal("snapshotProcessOrigins() blocked behind persistence")
	}
	close(release)
	if err := <-moveDone; err != nil {
		t.Fatalf("moveProcessBatch() error: %v", err)
	}
	if _, ok := manager.snapshotProcessOrigins()[122]; !ok {
		t.Fatal("committed process origin was not published")
	}
}

func TestMoveProcessBatchSkipsProcessesAlreadyInDestination(t *testing.T) {
	manager, root := newOriginTestManager(t)
	destinationCgroup := "/resman/limited/user_1000"
	destination := createFakeCgroup(t, root, destinationCgroup)
	writeFakeProcess(t, manager, 131, 1, 131, 5300, 1000, destinationCgroup)

	writeCalls := 0
	manager.writePID = func(string, int) error {
		writeCalls++
		return nil
	}

	moved, result, moveErrors, err := manager.moveProcessBatch([]int{131}, 1000, destination)
	if err != nil {
		t.Fatalf("moveProcessBatch() error: %v", err)
	}
	if len(moved) != 0 || len(moveErrors) != 0 {
		t.Fatalf("moved=%v moveErrors=%v, want no migration", moved, moveErrors)
	}
	if result.AlreadyPresent != 1 || !result.Applied() {
		t.Fatalf("move result = %+v, want one process already present", result)
	}
	if writeCalls != 0 {
		t.Fatalf("cgroup.procs writes = %d, want 0", writeCalls)
	}
	if len(manager.snapshotProcessOrigins()) != 0 {
		t.Fatal("process already in destination should not be recorded as its own origin")
	}
}

func TestMoveProcessBatchEnforcesPIDNamespaceBoundary(t *testing.T) {
	tests := []struct {
		name            string
		candidate       func(pidNamespaceIdentity) (pidNamespaceIdentity, error)
		wantMoved       int
		wantMismatch    int
		wantUnavailable int
		wantDisappeared int
	}{
		{
			name:      "same namespace moves",
			candidate: func(host pidNamespaceIdentity) (pidNamespaceIdentity, error) { return host, nil },
			wantMoved: 1,
		},
		{
			name: "different namespace is skipped",
			candidate: func(pidNamespaceIdentity) (pidNamespaceIdentity, error) {
				return pidNamespaceIdentity{device: 2, inode: 2}, nil
			},
			wantMismatch: 1,
		},
		{
			name: "unavailable namespace is skipped",
			candidate: func(pidNamespaceIdentity) (pidNamespaceIdentity, error) {
				return pidNamespaceIdentity{}, syscall.EACCES
			},
			wantUnavailable: 1,
		},
		{
			name: "disappeared namespace is not an ingress failure",
			candidate: func(pidNamespaceIdentity) (pidNamespaceIdentity, error) {
				return pidNamespaceIdentity{}, os.ErrNotExist
			},
			wantDisappeared: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, root := newOriginTestManager(t)
			destination := createFakeCgroup(t, root, "/resman/limited/user_1000")
			origin := "/user.slice/session-141.scope"
			writeFakeProcess(t, manager, 141, 1, 141, 5400, 1000, origin)
			createFakeCgroup(t, root, origin)

			hostNamespace := manager.pidNamespace
			manager.readPIDNamespace = func(int) (pidNamespaceIdentity, error) {
				return tt.candidate(hostNamespace)
			}
			writeCalls := 0
			manager.writePID = func(string, int) error {
				writeCalls++
				return nil
			}

			_, result, moveErrors, err := manager.moveProcessBatch([]int{141}, 1000, destination)
			if err != nil {
				t.Fatalf("moveProcessBatch() error: %v", err)
			}
			if len(moveErrors) != 0 {
				t.Fatalf("move errors = %v, want none", moveErrors)
			}
			if result.Moved != tt.wantMoved ||
				result.PIDNamespaceMismatches != tt.wantMismatch ||
				result.PIDNamespaceUnavailable != tt.wantUnavailable ||
				result.Disappeared != tt.wantDisappeared {
				t.Fatalf("move result = %+v", result)
			}
			if writeCalls != tt.wantMoved {
				t.Fatalf("cgroup.procs writes = %d, want %d", writeCalls, tt.wantMoved)
			}
			_, originRecorded := manager.snapshotProcessOrigins()[141]
			if originRecorded != (tt.wantMoved == 1) {
				t.Fatalf("origin recorded = %t, want %t", originRecorded, tt.wantMoved == 1)
			}
		})
	}
}

func TestMoveProcessBatchAllowsHostAndSkipsNestedNamespaceForOneUID(t *testing.T) {
	manager, root := newOriginTestManager(t)
	destination := createFakeCgroup(t, root, "/resman/limited/user_1000")
	for _, pid := range []int{151, 152} {
		origin := fmt.Sprintf("/user.slice/session-%d.scope", pid)
		writeFakeProcess(t, manager, pid, 1, pid, uint64(5500+pid), 1000, origin)
		createFakeCgroup(t, root, origin)
	}
	manager.readPIDNamespace = func(pid int) (pidNamespaceIdentity, error) {
		if pid == 152 {
			return pidNamespaceIdentity{device: 2, inode: 2}, nil
		}
		return manager.pidNamespace, nil
	}
	var written []int
	manager.writePID = func(_ string, pid int) error {
		written = append(written, pid)
		return nil
	}

	_, result, moveErrors, err := manager.moveProcessBatch([]int{151, 152}, 1000, destination)
	if err != nil {
		t.Fatalf("moveProcessBatch() error: %v", err)
	}
	if len(moveErrors) != 0 || !result.Applied() || result.Moved != 1 || result.PIDNamespaceMismatches != 1 {
		t.Fatalf("move result = %+v, errors = %v", result, moveErrors)
	}
	if len(written) != 1 || written[0] != 151 {
		t.Fatalf("written PIDs = %v, want [151]", written)
	}
	if _, ok := manager.snapshotProcessOrigins()[152]; ok {
		t.Fatal("nested-namespace PID acquired an origin despite skipped ingress")
	}
}

func TestMoveProcessBatchRechecksNamespaceImmediatelyBeforeWrite(t *testing.T) {
	tests := []struct {
		name            string
		finalIdentity   pidNamespaceIdentity
		finalErr        error
		wantMismatch    int
		wantUnavailable int
		wantDisappeared int
	}{
		{
			name:          "namespace changes before ingress",
			finalIdentity: pidNamespaceIdentity{device: 2, inode: 2},
			wantMismatch:  1,
		},
		{
			name:            "namespace becomes unreadable before ingress",
			finalErr:        syscall.EACCES,
			wantUnavailable: 1,
		},
		{
			name:            "process disappears before ingress",
			finalErr:        os.ErrNotExist,
			wantDisappeared: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, root := newOriginTestManager(t)
			destination := createFakeCgroup(t, root, "/resman/limited/user_1000")
			origin := "/user.slice/session-161.scope"
			writeFakeProcess(t, manager, 161, 1, 161, 5600, 1000, origin)
			createFakeCgroup(t, root, origin)

			reads := 0
			manager.readPIDNamespace = func(int) (pidNamespaceIdentity, error) {
				reads++
				if reads == 1 {
					return manager.pidNamespace, nil
				}
				return tt.finalIdentity, tt.finalErr
			}
			writeCalled := false
			manager.writePID = func(string, int) error {
				writeCalled = true
				return nil
			}

			_, result, moveErrors, err := manager.moveProcessBatch([]int{161}, 1000, destination)
			if err != nil {
				t.Fatalf("moveProcessBatch() error: %v", err)
			}
			if len(moveErrors) != 0 {
				t.Fatalf("move errors = %v, want none", moveErrors)
			}
			if reads != 2 || writeCalled ||
				result.PIDNamespaceMismatches != tt.wantMismatch ||
				result.PIDNamespaceUnavailable != tt.wantUnavailable ||
				result.Disappeared != tt.wantDisappeared {
				t.Fatalf("reads=%d writeCalled=%t result=%+v", reads, writeCalled, result)
			}
			if _, ok := manager.snapshotProcessOrigins()[161]; ok {
				t.Fatal("origin persisted for a process rejected at the final namespace boundary")
			}
		})
	}
}

func TestBuildRestorePlanInheritsParentOrigin(t *testing.T) {
	manager, root := newOriginTestManager(t)
	originalPath := "/user.slice/user-1000.slice/session-8.scope"
	originalFilesystemPath := createFakeCgroup(t, root, originalPath)
	writeFakeProcess(t, manager, 201, 1, 201, 6000, 1000, "/resman/limited/user_1000")
	writeFakeProcess(t, manager, 202, 201, 201, 6001, 1000, "/resman/limited/user_1000")
	manager.processOrigins[201] = processOrigin{
		PID:        201,
		UID:        1000,
		PPID:       1,
		SessionID:  201,
		StartTime:  6000,
		CgroupPath: originalPath,
	}

	plans, _, err := manager.buildRestorePlan(1000, []int{201, 202}, "max 100000")
	if err != nil {
		t.Fatalf("buildRestorePlan() error: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("restore plan count = %d, want 2", len(plans))
	}
	for _, plan := range plans {
		if plan.Destination != originalFilesystemPath {
			t.Fatalf("PID %d destination = %s, want %s", plan.PID, plan.Destination, originalFilesystemPath)
		}
		if plan.Recovery {
			t.Fatalf("PID %d unexpectedly planned for recovery", plan.PID)
		}
	}
}

func TestBuildRestorePlanInheritsSessionOriginAfterParentExit(t *testing.T) {
	manager, root := newOriginTestManager(t)
	originalPath := "/user.slice/user-1000.slice/session-10.scope"
	originalFilesystemPath := createFakeCgroup(t, root, originalPath)
	writeFakeProcess(t, manager, 600, 1, 600, 10000, 1000, "/resman/limited/user_1000")
	writeFakeProcess(t, manager, 602, 1, 600, 10002, 1000, "/resman/limited/user_1000")
	manager.processOrigins[601] = processOrigin{
		PID:              601,
		UID:              1000,
		PPID:             600,
		SessionID:        600,
		SessionStartTime: 10000,
		StartTime:        10001,
		CgroupPath:       originalPath,
	}
	if err := os.RemoveAll(filepath.Join(manager.getProcRoot(), "600")); err != nil {
		t.Fatalf("failed to remove exited session leader: %v", err)
	}

	plans, _, err := manager.buildRestorePlan(1000, []int{602}, "max 100000")
	if err != nil {
		t.Fatalf("buildRestorePlan() error: %v", err)
	}
	if len(plans) != 1 || plans[0].Destination != originalFilesystemPath || plans[0].Recovery {
		t.Fatalf("unexpected session-inherited restore plan: %+v", plans)
	}
}

func TestBuildRestorePlanRejectsReusedSessionID(t *testing.T) {
	manager, _ := newOriginTestManager(t)
	writeFakeProcess(t, manager, 610, 1, 610, 11000, 1000, "/resman/limited/user_1000")
	writeFakeProcess(t, manager, 611, 1, 610, 11001, 1000, "/resman/limited/user_1000")
	manager.processOrigins[612] = processOrigin{
		PID:              612,
		UID:              1000,
		PPID:             1,
		SessionID:        610,
		SessionStartTime: 10900,
		StartTime:        10901,
		CgroupPath:       "/user.slice/user-1000.slice/session-old.scope",
	}

	plans, _, err := manager.buildRestorePlan(1000, []int{611}, "max 100000")
	if err != nil {
		t.Fatalf("buildRestorePlan() error: %v", err)
	}
	if len(plans) != 1 || !plans[0].Recovery {
		t.Fatalf("reused session ID should use recovery: %+v", plans)
	}
}

func TestBuildRestorePlanUsesRecoveryForReusedPID(t *testing.T) {
	manager, _ := newOriginTestManager(t)
	writeFakeProcess(t, manager, 301, 1, 301, 7001, 1000, "/resman/limited/user_1000")
	manager.processOrigins[301] = processOrigin{
		PID:        301,
		UID:        1000,
		PPID:       1,
		SessionID:  301,
		StartTime:  7000,
		CgroupPath: "/user.slice/user-1000.slice/session-old.scope",
	}

	plans, processed, err := manager.buildRestorePlan(1000, []int{301}, "250000 100000")
	if err != nil {
		t.Fatalf("buildRestorePlan() error: %v", err)
	}
	if !processed[301] {
		t.Fatal("expected stale origin for reused PID to be removed")
	}
	if len(plans) != 1 || !plans[0].Recovery {
		t.Fatalf("unexpected restore plans: %+v", plans)
	}
	assertFileContent(t, filepath.Join(manager.getRecoveryCgroupPath(1000), "cpu.max"), "250000 100000")
}

func TestBuildRestorePlanUsesRecoveryWhenOriginDisappears(t *testing.T) {
	manager, _ := newOriginTestManager(t)
	writeFakeProcess(t, manager, 401, 1, 401, 8000, 1000, "/resman/limited/user_1000")
	manager.processOrigins[401] = processOrigin{
		PID:        401,
		UID:        1000,
		PPID:       1,
		SessionID:  401,
		StartTime:  8000,
		CgroupPath: "/user.slice/user-1000.slice/session-gone.scope",
	}

	plans, _, err := manager.buildRestorePlan(1000, []int{401}, "max 100000")
	if err != nil {
		t.Fatalf("buildRestorePlan() error: %v", err)
	}
	if len(plans) != 1 || plans[0].Destination != manager.getRecoveryCgroupPath(1000) || !plans[0].Recovery {
		t.Fatalf("unexpected recovery plan: %+v", plans)
	}
}

func TestRestoreProcessesTreatsESRCHAsSuccess(t *testing.T) {
	manager, root := newOriginTestManager(t)
	originalPath := "/user.slice/user-1000.slice/session-9.scope"
	createFakeCgroup(t, root, originalPath)
	writeFakeProcess(t, manager, 501, 1, 501, 9000, 1000, "/resman/limited/user_1000")
	manager.processOrigins[501] = processOrigin{
		PID:        501,
		UID:        1000,
		PPID:       1,
		SessionID:  501,
		StartTime:  9000,
		CgroupPath: originalPath,
	}
	err := manager.persistProcessOrigins(manager.snapshotProcessOrigins())
	if err != nil {
		t.Fatalf("failed to persist test origin: %v", err)
	}
	manager.writePID = func(string, int) error {
		return syscall.ESRCH
	}

	usedRecovery, err := manager.restoreProcesses(1000, []int{501}, "max 100000")
	if err != nil {
		t.Fatalf("restoreProcesses() error: %v", err)
	}
	if usedRecovery {
		t.Fatal("ESRCH should not require recovery")
	}
	if _, ok := manager.snapshotProcessOrigins()[501]; ok {
		t.Fatal("origin record should be removed after ESRCH")
	}
	if _, err := os.Stat(manager.processOriginsFile); !os.IsNotExist(err) {
		t.Fatalf("empty origin state should be removed, stat err=%v", err)
	}
}

func TestRestoreProcessesRestoresExactCgroup(t *testing.T) {
	manager, root := newOriginTestManager(t)
	namespaceReads := 0
	manager.readPIDNamespace = func(int) (pidNamespaceIdentity, error) {
		namespaceReads++
		return pidNamespaceIdentity{device: 2, inode: 2}, nil
	}
	originalPath := "/user.slice/user-1000.slice/session-13.scope"
	originalFilesystemPath := createFakeCgroup(t, root, originalPath)
	writeFakeProcess(t, manager, 901, 1, 901, 13000, 1000, "/resman/limited/user_1000")
	manager.processOrigins[901] = processOrigin{
		PID:        901,
		UID:        1000,
		PPID:       1,
		SessionID:  901,
		StartTime:  13000,
		CgroupPath: originalPath,
	}
	err := manager.persistProcessOrigins(manager.snapshotProcessOrigins())
	if err != nil {
		t.Fatalf("failed to persist test origin: %v", err)
	}

	manager.writePID = func(path string, pid int) error {
		if path != filepath.Join(originalFilesystemPath, "cgroup.procs") || pid != 901 {
			return fmt.Errorf("unexpected exact restore target %s for PID %d", path, pid)
		}
		return nil
	}

	usedRecovery, err := manager.restoreProcesses(1000, []int{901}, "max 100000")
	if err != nil {
		t.Fatalf("restoreProcesses() error: %v", err)
	}
	if usedRecovery {
		t.Fatal("exact restore should not use recovery")
	}
	if namespaceReads != 0 {
		t.Fatalf("restore performed %d PID namespace reads, want none", namespaceReads)
	}
	if _, ok := manager.snapshotProcessOrigins()[901]; ok {
		t.Fatal("origin record should be removed after exact restore")
	}
}

func TestRestoreProcessesSelectsSafeDestinationForDelegatedOrigins(t *testing.T) {
	tests := []struct {
		name             string
		originPath       string
		originIsInternal bool
		wantRecovery     bool
	}{
		{
			name:         "leaf origin is restored exactly",
			originPath:   "/user.slice/user-1000.slice/session-14.scope",
			wantRecovery: false,
		},
		{
			name:             "delegated internal origin uses recovery leaf",
			originPath:       "/",
			originIsInternal: true,
			wantRecovery:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, root := newOriginTestManager(t)
			const pid = 902
			const startTime = 13001
			limitedPath := "/resman/limited/user_1000"
			writeFakeProcess(t, manager, pid, 1, pid, startTime, 1000, limitedPath)
			createFakeCgroup(t, root, limitedPath)
			originFilesystemPath := createFakeCgroup(t, root, tt.originPath)
			manager.processOrigins[pid] = processOrigin{
				PID:        pid,
				UID:        1000,
				PPID:       1,
				SessionID:  pid,
				StartTime:  startTime,
				CgroupPath: tt.originPath,
			}

			var destinations []string
			manager.writePID = func(path string, movedPID int) error {
				destinations = append(destinations, path)
				if movedPID != pid {
					return fmt.Errorf("unexpected PID %d", movedPID)
				}
				if tt.originIsInternal && path == filepath.Join(originFilesystemPath, "cgroup.procs") {
					return &os.PathError{Op: "write", Path: path, Err: syscall.EBUSY}
				}

				destination := filepath.Dir(path)
				relative, err := filepath.Rel(root, destination)
				if err != nil {
					return err
				}
				writeFakeProcess(t, manager, pid, 1, pid, startTime, 1000, "/"+relative)
				return nil
			}

			usedRecovery, err := manager.restoreProcesses(1000, []int{pid}, "max 100000")
			if err != nil {
				t.Fatalf("restoreProcesses() error: %v", err)
			}
			if usedRecovery != tt.wantRecovery {
				t.Fatalf("usedRecovery = %t, want %t", usedRecovery, tt.wantRecovery)
			}

			wantDestination := originFilesystemPath
			wantAttempts := 1
			if tt.wantRecovery {
				wantDestination = manager.getRecoveryCgroupPath(1000)
				wantAttempts = 2
			}
			if len(destinations) != wantAttempts {
				t.Fatalf("restore attempts = %v, want %d", destinations, wantAttempts)
			}
			currentPath, err := manager.readUnifiedCgroupPath(pid)
			if err != nil {
				t.Fatalf("readUnifiedCgroupPath() error: %v", err)
			}
			if got := manager.cgroupPathOnFilesystem(currentPath); got != wantDestination {
				t.Fatalf("final cgroup = %s, want %s", got, wantDestination)
			}
			if identity, err := manager.readProcessIdentity(pid); err != nil || identity.StartTime != startTime {
				t.Fatalf("final process identity = %+v, err=%v, want start time %d", identity, err, startTime)
			}
			if _, ok := manager.snapshotProcessOrigins()[pid]; ok {
				t.Fatal("origin record remained after successful restore")
			}
		})
	}
}

func TestRestoreProcessesRevalidatesEveryPlannedPIDStartTime(t *testing.T) {
	manager, root := newOriginTestManager(t)
	originalPath := "/user.slice/user-1000.slice/session-15.scope"
	originalFilesystemPath := createFakeCgroup(t, root, originalPath)
	const firstPID = 903
	const reusedPID = 904
	for _, pid := range []int{firstPID, reusedPID} {
		writeFakeProcess(t, manager, pid, 1, pid, uint64(14000+pid), 1000, "/resman/limited/user_1000")
		manager.processOrigins[pid] = processOrigin{
			PID:        pid,
			UID:        1000,
			PPID:       1,
			SessionID:  pid,
			StartTime:  uint64(14000 + pid),
			CgroupPath: originalPath,
		}
	}

	var moved []int
	manager.writePID = func(path string, pid int) error {
		if path != filepath.Join(originalFilesystemPath, "cgroup.procs") {
			return fmt.Errorf("unexpected restore destination %s", path)
		}
		moved = append(moved, pid)
		if pid == firstPID {
			writeFakeProcess(t, manager, reusedPID, 1, reusedPID, 99999, 1000, "/resman/limited/user_1000")
		}
		return nil
	}

	restored, usedRecovery, reused, err := manager.restoreProcessesExpected(
		1000,
		[]int{firstPID, reusedPID},
		"max 100000",
		nil,
		"",
		true,
	)
	if err != nil {
		t.Fatalf("restoreProcessesExpected() error: %v", err)
	}
	if restored != 1 || usedRecovery {
		t.Fatalf("restored=%d usedRecovery=%t, want one exact restore", restored, usedRecovery)
	}
	if len(moved) != 1 || moved[0] != firstPID {
		t.Fatalf("moved PIDs = %v, want only %d", moved, firstPID)
	}
	if !reused[reusedPID] {
		t.Fatalf("PID %d reuse was not reported", reusedPID)
	}
	if _, ok := manager.snapshotProcessOrigins()[reusedPID]; ok {
		t.Fatal("stale origin remained after PID reuse was detected")
	}
}

func TestRestoreProcessesReusesRecoveryLeafForMultipleInternalOrigins(t *testing.T) {
	manager, root := newOriginTestManager(t)
	const uid = 1000
	pids := []int{905, 906}
	for _, pid := range pids {
		startTime := uint64(15000 + pid)
		writeFakeProcess(t, manager, pid, 1, pid, startTime, uid, "/resman/limited/user_1000")
		manager.processOrigins[pid] = processOrigin{
			PID:        pid,
			UID:        uid,
			PPID:       1,
			SessionID:  pid,
			StartTime:  startTime,
			CgroupPath: "/",
		}
	}

	rootProcs := filepath.Join(root, "cgroup.procs")
	recoveryProcs := filepath.Join(manager.getRecoveryCgroupPath(uid), "cgroup.procs")
	writes := make(map[string][]int)
	manager.writePID = func(path string, pid int) error {
		writes[path] = append(writes[path], pid)
		if path == rootProcs {
			return &os.PathError{Op: "write", Path: path, Err: syscall.EBUSY}
		}
		if path != recoveryProcs {
			return fmt.Errorf("unexpected restore destination %s", path)
		}
		return nil
	}

	restored, usedRecovery, _, err := manager.restoreProcessesExpected(
		uid,
		pids,
		"max 100000",
		nil,
		"",
		true,
	)
	if err != nil {
		t.Fatalf("restoreProcessesExpected() error: %v", err)
	}
	if restored != len(pids) || !usedRecovery {
		t.Fatalf("restored=%d usedRecovery=%t, want %d recovery restores", restored, usedRecovery, len(pids))
	}
	if len(writes[rootProcs]) != len(pids) || len(writes[recoveryProcs]) != len(pids) {
		t.Fatalf("restore writes = %#v, want every PID attempted at origin then recovery", writes)
	}
}

func TestRestoreProcessesFallsBackWhenOriginDisappearsDuringMove(t *testing.T) {
	manager, root := newOriginTestManager(t)
	originalPath := "/user.slice/user-1000.slice/session-12.scope"
	originalFilesystemPath := createFakeCgroup(t, root, originalPath)
	writeFakeProcess(t, manager, 801, 1, 801, 12000, 1000, "/resman/limited/user_1000")
	manager.processOrigins[801] = processOrigin{
		PID:        801,
		UID:        1000,
		PPID:       1,
		SessionID:  801,
		StartTime:  12000,
		CgroupPath: originalPath,
	}

	var destinations []string
	manager.writePID = func(path string, pid int) error {
		destinations = append(destinations, path)
		if path == filepath.Join(originalFilesystemPath, "cgroup.procs") {
			return os.ErrNotExist
		}
		if path != filepath.Join(manager.getRecoveryCgroupPath(1000), "cgroup.procs") {
			return fmt.Errorf("unexpected fallback destination %s", path)
		}
		return nil
	}

	usedRecovery, err := manager.restoreProcesses(1000, []int{801}, "max 100000")
	if err != nil {
		t.Fatalf("restoreProcesses() error: %v", err)
	}
	if !usedRecovery {
		t.Fatal("disappeared origin should use recovery")
	}
	if len(destinations) != 2 {
		t.Fatalf("restore attempts = %v, want original then recovery", destinations)
	}
}

func TestPruneInactiveProcessOriginsRemovesExitedProcesses(t *testing.T) {
	manager, _ := newOriginTestManager(t)
	manager.processOrigins[701] = processOrigin{
		PID:        701,
		UID:        1000,
		PPID:       1,
		SessionID:  701,
		StartTime:  11000,
		CgroupPath: "/user.slice/user-1000.slice/session-11.scope",
	}
	err := manager.persistProcessOrigins(manager.snapshotProcessOrigins())
	if err != nil {
		t.Fatalf("failed to persist test origin: %v", err)
	}

	if err := manager.pruneInactiveProcessOrigins(1000); err != nil {
		t.Fatalf("pruneInactiveProcessOrigins() error: %v", err)
	}
	if len(manager.snapshotProcessOrigins()) != 0 {
		t.Fatal("origin for exited process should be removed")
	}
	if _, err := os.Stat(manager.processOriginsFile); !os.IsNotExist(err) {
		t.Fatalf("empty origin state should be removed, stat err=%v", err)
	}
}
