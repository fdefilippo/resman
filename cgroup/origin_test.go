package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

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

	if err := manager.MoveProcessToSharedCgroup(101, filepath.Dir(sharedUserPath), 1000); err != nil {
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

	moved, moveErrors, err := manager.moveProcessBatch(pids, 1000, destination)
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

	if _, _, err := manager.moveProcessBatch([]int{121}, 1000, destination); err == nil {
		t.Fatal("moveProcessBatch() should fail when origin persistence fails")
	}
	if writeCalled {
		t.Fatal("process migration started before the origin transaction committed")
	}
	if len(manager.snapshotProcessOrigins()) != 0 {
		t.Fatal("failed origin transaction was not rolled back")
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
	manager.originMu.Lock()
	err := manager.persistProcessOriginsLocked()
	manager.originMu.Unlock()
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
	manager.originMu.Lock()
	err := manager.persistProcessOriginsLocked()
	manager.originMu.Unlock()
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
	if _, ok := manager.snapshotProcessOrigins()[901]; ok {
		t.Fatal("origin record should be removed after exact restore")
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
	manager.originMu.Lock()
	err := manager.persistProcessOriginsLocked()
	manager.originMu.Unlock()
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
