package cgroup

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const processOriginsStateVersion = 1

type processOrigin struct {
	PID              int    `json:"pid"`
	UID              int    `json:"uid"`
	PPID             int    `json:"ppid"`
	SessionID        int    `json:"session_id"`
	SessionStartTime uint64 `json:"session_start_time"`
	StartTime        uint64 `json:"start_time"`
	CgroupPath       string `json:"cgroup_path"`
}

type processOriginsState struct {
	Version int             `json:"version"`
	Origins []processOrigin `json:"origins"`
}

type processIdentity struct {
	PID       int
	PPID      int
	SessionID int
	StartTime uint64
}

type processRestore struct {
	PID         int
	StartTime   uint64
	Destination string
	Recovery    bool
}

// ProcessOriginUnavailableError reports a process that reconciliation cannot
// restore without guessing a destination. The process remains constrained.
type ProcessOriginUnavailableError struct {
	PID int
	UID int
}

func (e *ProcessOriginUnavailableError) Error() string {
	return fmt.Sprintf(
		"cannot safely restore PID %d for UID %d: its recorded origin is unavailable",
		e.PID,
		e.UID,
	)
}

func processOriginsPath(createdCgroupsFile string) string {
	ext := filepath.Ext(createdCgroupsFile)
	base := strings.TrimSuffix(createdCgroupsFile, ext)
	return base + "-origins.json"
}

func isFiniteCPUQuota(quota string) bool {
	fields := strings.Fields(quota)
	return len(fields) == 2 && fields[0] != "max"
}

func (m *Manager) getProcRoot() string {
	if m.procRoot == "" {
		return "/proc"
	}
	return m.procRoot
}

func (m *Manager) getProcessOriginsFile() string {
	if m.processOriginsFile != "" {
		return m.processOriginsFile
	}
	return processOriginsPath(m.createdCgroupsFile)
}

func (m *Manager) loadProcessOrigins() error {
	leaveOperation := m.originGate.Enter()
	defer leaveOperation()

	stateFile := m.getProcessOriginsFile()
	data, err := os.ReadFile(stateFile)
	if os.IsNotExist(err) {
		m.replaceProcessOrigins(make(map[int]processOrigin))
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", stateFile, err)
	}

	var state processOriginsState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("failed to parse %s: %w", stateFile, err)
	}
	if state.Version != processOriginsStateVersion {
		return fmt.Errorf("unsupported process origin state version %d", state.Version)
	}

	origins := make(map[int]processOrigin, len(state.Origins))
	for _, origin := range state.Origins {
		if origin.PID <= 0 || origin.UID < 0 || origin.StartTime == 0 || origin.CgroupPath == "" {
			return fmt.Errorf("invalid process origin record for PID %d", origin.PID)
		}
		origins[origin.PID] = origin
	}
	m.replaceProcessOrigins(origins)
	return nil
}

func (m *Manager) persistProcessOrigins(origins map[int]processOrigin) error {
	stateFile := m.getProcessOriginsFile()
	if len(origins) == 0 {
		if err := os.Remove(stateFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove empty process origin state %s: %w", stateFile, err)
		}
		return nil
	}

	records := make([]processOrigin, 0, len(origins))
	for _, origin := range origins {
		records = append(records, origin)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].PID < records[j].PID
	})

	data, err := json.MarshalIndent(processOriginsState{
		Version: processOriginsStateVersion,
		Origins: records,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode process origin state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(stateFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create process origin state directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".resman-process-origins-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary process origin state: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to set process origin state permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write process origin state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to sync process origin state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close process origin state: %w", err)
	}
	if err := os.Rename(tmpPath, stateFile); err != nil {
		return fmt.Errorf("failed to replace process origin state %s: %w", stateFile, err)
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("failed to open process origin state directory %s: %w", dir, err)
	}
	if err := dirHandle.Sync(); err != nil {
		_ = dirHandle.Close()
		return fmt.Errorf("failed to sync process origin state directory %s: %w", dir, err)
	}
	if err := dirHandle.Close(); err != nil {
		return fmt.Errorf("failed to close process origin state directory %s: %w", dir, err)
	}
	return nil
}

func (m *Manager) flushProcessOrigins(origins map[int]processOrigin) error {
	if m.persistOrigins != nil {
		return m.persistOrigins()
	}
	return m.persistProcessOrigins(origins)
}

func (m *Manager) removeProcessOrigins(pids map[int]bool) error {
	if len(pids) == 0 {
		return nil
	}

	leaveOperation := m.originGate.Enter()
	defer leaveOperation()
	return m.removeProcessOriginsUnderGate(pids)
}

func (m *Manager) removeProcessOriginsUnderGate(pids map[int]bool) error {
	next := m.snapshotProcessOrigins()
	changed := false
	for pid := range pids {
		if _, ok := next[pid]; ok {
			delete(next, pid)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := m.flushProcessOrigins(next); err != nil {
		return err
	}
	m.replaceProcessOrigins(next)
	return nil
}

func (m *Manager) snapshotProcessOrigins() map[int]processOrigin {
	m.originMu.Lock()
	defer m.originMu.Unlock()

	origins := make(map[int]processOrigin, len(m.processOrigins))
	for pid, origin := range m.processOrigins {
		origins[pid] = origin
	}
	return origins
}

func (m *Manager) replaceProcessOrigins(origins map[int]processOrigin) {
	m.originMu.Lock()
	defer m.originMu.Unlock()
	m.processOrigins = origins
}

func (m *Manager) pruneInactiveProcessOrigins(uid int) error {
	leaveOperation := m.originGate.Enter()
	defer leaveOperation()

	origins := m.snapshotProcessOrigins()
	remove := make(map[int]bool)
	basePath := m.getBaseCgroupPath()

	for pid, origin := range origins {
		if uid >= 0 && origin.UID != uid {
			continue
		}
		identity, err := m.readProcessIdentity(pid)
		if os.IsNotExist(err) {
			remove[pid] = true
			continue
		}
		if err != nil {
			continue
		}
		if identity.StartTime != origin.StartTime {
			remove[pid] = true
			continue
		}
		currentPath, err := m.readUnifiedCgroupPath(pid)
		if os.IsNotExist(err) {
			remove[pid] = true
			continue
		}
		if err != nil {
			continue
		}
		if !pathWithin(m.cgroupPathOnFilesystem(currentPath), basePath) {
			remove[pid] = true
		}
	}
	return m.removeProcessOriginsUnderGate(remove)
}

func (m *Manager) readProcessIdentity(pid int) (processIdentity, error) {
	statPath := filepath.Join(m.getProcRoot(), strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return processIdentity{}, err
	}

	line := string(data)
	closeParen := strings.LastIndex(line, ")")
	if closeParen < 0 || closeParen+2 >= len(line) {
		return processIdentity{}, fmt.Errorf("invalid process stat format for PID %d", pid)
	}
	fields := strings.Fields(line[closeParen+1:])
	if len(fields) <= 19 {
		return processIdentity{}, fmt.Errorf("process stat for PID %d has %d fields after comm", pid, len(fields))
	}

	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return processIdentity{}, fmt.Errorf("invalid PPID for PID %d: %w", pid, err)
	}
	sessionID, err := strconv.Atoi(fields[3])
	if err != nil {
		return processIdentity{}, fmt.Errorf("invalid session ID for PID %d: %w", pid, err)
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return processIdentity{}, fmt.Errorf("invalid start time for PID %d: %w", pid, err)
	}

	return processIdentity{
		PID:       pid,
		PPID:      ppid,
		SessionID: sessionID,
		StartTime: startTime,
	}, nil
}

func (m *Manager) readUnifiedCgroupPath(pid int) (string, error) {
	cgroupFile := filepath.Join(m.getProcRoot(), strconv.Itoa(pid), "cgroup")
	file, err := os.Open(cgroupFile)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "0::") {
			path := strings.TrimPrefix(line, "0::")
			path = strings.TrimSuffix(path, " (deleted)")
			if path == "" {
				return "", fmt.Errorf("empty unified cgroup path for PID %d", pid)
			}
			return filepath.Clean("/" + strings.TrimPrefix(path, "/")), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("unified cgroup entry not found for PID %d", pid)
}

func (m *Manager) cgroupPathOnFilesystem(cgroupPath string) string {
	clean := filepath.Clean("/" + strings.TrimPrefix(cgroupPath, "/"))
	return filepath.Join(m.getConfig().CgroupRoot, strings.TrimPrefix(clean, "/"))
}

func pathWithin(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func (m *Manager) getRecoveryRootPath() string {
	return filepath.Join(m.getBaseCgroupPath(), "recovery")
}

func (m *Manager) getRecoveryCgroupPath(uid int) string {
	return filepath.Join(m.getRecoveryRootPath(), fmt.Sprintf("user_%d", uid))
}

func (m *Manager) isRecoveryPath(path string) bool {
	return pathWithin(path, m.getRecoveryRootPath())
}

func (m *Manager) resolveInheritedOrigin(identity processIdentity, uid int, origins map[int]processOrigin) (string, bool) {
	visited := make(map[int]bool)
	parentPID := identity.PPID
	for depth := 0; parentPID > 1 && depth < 64 && !visited[parentPID]; depth++ {
		visited[parentPID] = true
		parentIdentity, err := m.readProcessIdentity(parentPID)
		if err != nil {
			break
		}
		if origin, ok := origins[parentPID]; ok &&
			origin.UID == uid &&
			origin.StartTime == parentIdentity.StartTime {
			return origin.CgroupPath, true
		}
		parentPID = parentIdentity.PPID
	}

	var sessionPath string
	var sessionStartTime uint64
	for _, origin := range origins {
		if origin.PID == identity.PID ||
			origin.UID != uid ||
			origin.SessionID != identity.SessionID {
			continue
		}
		if sessionPath == "" {
			sessionPath = origin.CgroupPath
		} else if sessionPath != origin.CgroupPath {
			return "", false
		}
		if origin.SessionStartTime != 0 {
			if sessionStartTime == 0 {
				sessionStartTime = origin.SessionStartTime
			} else if sessionStartTime != origin.SessionStartTime {
				return "", false
			}
		}
	}
	if sessionPath == "" {
		return "", false
	}

	sessionIdentity, err := m.readProcessIdentity(identity.SessionID)
	if err == nil {
		if sessionStartTime == 0 || sessionIdentity.StartTime != sessionStartTime {
			return "", false
		}
	} else if !os.IsNotExist(err) {
		return "", false
	}
	return sessionPath, true
}

func (m *Manager) newProcessOrigin(identity processIdentity, uid int, cgroupPath string) processOrigin {
	var sessionStartTime uint64
	if sessionIdentity, err := m.readProcessIdentity(identity.SessionID); err == nil {
		sessionStartTime = sessionIdentity.StartTime
	}
	return processOrigin{
		PID:              identity.PID,
		UID:              uid,
		PPID:             identity.PPID,
		SessionID:        identity.SessionID,
		SessionStartTime: sessionStartTime,
		StartTime:        identity.StartTime,
		CgroupPath:       cgroupPath,
	}
}

func (m *Manager) captureProcessOriginsExpected(
	pids []int,
	uid int,
	destination string,
	expectedStartTimes map[int]uint64,
) ([]int, map[int]uint64, map[int]bool, int, map[int]bool, error) {
	leaveOperation := m.originGate.Enter()
	defer leaveOperation()

	next := m.snapshotProcessOrigins()

	type pendingOrigin struct {
		identity processIdentity
	}
	pending := make([]pendingOrigin, 0)
	movable := make([]int, 0, len(pids))
	startTimes := make(map[int]uint64, len(pids))
	reused := make(map[int]bool)
	newlyCaptured := make(map[int]bool)
	alreadyPresent := 0
	basePath := m.getBaseCgroupPath()
	changed := false
	setOrigin := func(origin processOrigin) {
		next[origin.PID] = origin
		newlyCaptured[origin.PID] = true
		changed = true
	}

	for _, pid := range pids {
		identity, err := m.readProcessIdentity(pid)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, nil, reused, alreadyPresent, newlyCaptured, fmt.Errorf("failed to identify PID %d before migration: %w", pid, err)
		}
		if expected, ok := expectedStartTimes[pid]; ok && identity.StartTime != expected {
			reused[pid] = true
			continue
		}
		currentPath, err := m.readUnifiedCgroupPath(pid)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, nil, reused, alreadyPresent, newlyCaptured, fmt.Errorf("failed to read cgroup for PID %d before migration: %w", pid, err)
		}
		currentFilesystemPath := m.cgroupPathOnFilesystem(currentPath)
		if filepath.Clean(currentFilesystemPath) == filepath.Clean(destination) {
			alreadyPresent++
			continue
		}
		movable = append(movable, pid)
		startTimes[pid] = identity.StartTime
		if existing, ok := next[pid]; ok && existing.StartTime == identity.StartTime {
			continue
		}
		if !pathWithin(currentFilesystemPath, basePath) || m.isRecoveryPath(currentFilesystemPath) {
			setOrigin(m.newProcessOrigin(identity, uid, currentPath))
			continue
		}
		pending = append(pending, pendingOrigin{identity: identity})
	}

	for _, candidate := range pending {
		inheritedPath, ok := m.resolveInheritedOrigin(candidate.identity, uid, next)
		if !ok {
			continue
		}
		setOrigin(m.newProcessOrigin(candidate.identity, uid, inheritedPath))
	}

	if !changed {
		return movable, startTimes, reused, alreadyPresent, newlyCaptured, nil
	}
	if err := m.flushProcessOrigins(next); err != nil {
		return nil, nil, reused, alreadyPresent, nil, fmt.Errorf("failed to persist process origins before migration: %w", err)
	}
	m.replaceProcessOrigins(next)
	return movable, startTimes, reused, alreadyPresent, newlyCaptured, nil
}

func (m *Manager) moveProcessBatch(pids []int, uid int, destination string) ([]int, ProcessMoveResult, map[int]error, error) {
	moved, result, moveErrors, _, err := m.moveProcessBatchExpected(pids, uid, destination, nil)
	return moved, result, moveErrors, err
}

func (m *Manager) moveProcessBatchExpected(
	pids []int,
	uid int,
	destination string,
	expectedStartTimes map[int]uint64,
) ([]int, ProcessMoveResult, map[int]error, map[int]bool, error) {
	allowed, result := m.filterPIDNamespaceCandidates(pids)
	movable, capturedStartTimes, reused, alreadyPresent, newlyCaptured, err := m.captureProcessOriginsExpected(allowed, uid, destination, expectedStartTimes)
	result.AlreadyPresent = alreadyPresent
	result.Reused = len(reused)
	if err != nil {
		m.logPIDNamespaceSkips(uid, result)
		return nil, result, nil, reused, err
	}

	moved := make([]int, 0, len(movable))
	moveErrors := make(map[int]error)
	disappeared := make(map[int]bool)
	cgroupProcsFile := filepath.Join(destination, "cgroup.procs")
	for _, pid := range movable {
		identity, identityErr := m.readProcessIdentity(pid)
		switch {
		case os.IsNotExist(identityErr):
			disappeared[pid] = true
			result.Disappeared++
			continue
		case identityErr != nil:
			moveErrors[pid] = fmt.Errorf("failed to revalidate process identity: %w", identityErr)
			continue
		case identity.StartTime != capturedStartTimes[pid]:
			reused[pid] = true
			disappeared[pid] = true
			result.Reused++
			continue
		}
		reason, namespaceErr := m.verifyPIDNamespaceIngress(pid)
		if reason != "" || namespaceErr != nil {
			mergePIDNamespaceSkip(&result, reason, namespaceErr)
			if os.IsNotExist(namespaceErr) || newlyCaptured[pid] {
				disappeared[pid] = true
			}
			continue
		}
		if err := m.writePIDToCgroup(cgroupProcsFile, pid); errors.Is(err, syscall.ESRCH) {
			disappeared[pid] = true
			result.Disappeared++
		} else if err != nil {
			moveErrors[pid] = err
		} else {
			moved = append(moved, pid)
			result.Moved++
		}
	}
	m.logPIDNamespaceSkips(uid, result)
	if err := m.removeProcessOrigins(disappeared); err != nil {
		return moved, result, moveErrors, reused, fmt.Errorf("failed to remove origins for exited or reused processes: %w", err)
	}
	return moved, result, moveErrors, reused, nil
}

func (m *Manager) ensureRecoveryCgroup(uid int, normalQuota string) (string, error) {
	if !isValidCPUQuotaFormat(normalQuota) {
		return "", fmt.Errorf("invalid CPU_QUOTA_NORMAL %q for recovery cgroup", normalQuota)
	}

	recoveryRoot := m.getRecoveryRootPath()
	if err := os.MkdirAll(recoveryRoot, 0755); err != nil {
		return "", fmt.Errorf("failed to create recovery cgroup root %s: %w", recoveryRoot, err)
	}
	if err := m.writeControllerIfMissing(filepath.Join(recoveryRoot, "cgroup.subtree_control"), "+cpu"); err != nil {
		return "", fmt.Errorf("failed to enable CPU controller in recovery cgroup %s: %w", recoveryRoot, err)
	}

	recoveryPath := m.getRecoveryCgroupPath(uid)
	if err := os.MkdirAll(recoveryPath, 0755); err != nil {
		return "", fmt.Errorf("failed to create recovery cgroup for UID %d: %w", uid, err)
	}
	if err := os.WriteFile(filepath.Join(recoveryPath, "cpu.max"), []byte(normalQuota), 0644); err != nil {
		return "", fmt.Errorf("failed to apply normal CPU quota to recovery cgroup for UID %d: %w", uid, err)
	}
	return recoveryPath, nil
}

func (m *Manager) writePIDToCgroup(cgroupProcsFile string, pid int) error {
	if m.writePID != nil {
		return m.writePID(cgroupProcsFile, pid)
	}
	return os.WriteFile(cgroupProcsFile, []byte(strconv.Itoa(pid)), 0644)
}

func (m *Manager) buildRestorePlan(uid int, pids []int, normalQuota string) ([]processRestore, map[int]bool, error) {
	plans, processedOrigins, _, err := m.buildRestorePlanExpected(uid, pids, normalQuota, nil, "", true)
	return plans, processedOrigins, err
}

func (m *Manager) buildRestorePlanExpected(
	uid int,
	pids []int,
	normalQuota string,
	expectedStartTimes map[int]uint64,
	expectedSource string,
	allowRecovery bool,
) ([]processRestore, map[int]bool, map[int]bool, error) {
	origins := m.snapshotProcessOrigins()
	processedOrigins := make(map[int]bool)
	reused := make(map[int]bool)
	plans := make([]processRestore, 0, len(pids))
	var recoveryPath string
	var planErrors []error

	for _, pid := range pids {
		identity, err := m.readProcessIdentity(pid)
		if os.IsNotExist(err) {
			processedOrigins[pid] = true
			continue
		}
		if err != nil {
			planErrors = append(planErrors, fmt.Errorf("failed to identify PID %d before restore: %w", pid, err))
			continue
		}
		if expected, ok := expectedStartTimes[pid]; ok && identity.StartTime != expected {
			reused[pid] = true
			continue
		}
		if expectedSource != "" {
			currentPath, currentErr := m.readUnifiedCgroupPath(pid)
			if os.IsNotExist(currentErr) {
				processedOrigins[pid] = true
				continue
			}
			if currentErr != nil {
				planErrors = append(planErrors, fmt.Errorf("failed to read cgroup for PID %d before restore: %w", pid, currentErr))
				continue
			}
			if filepath.Clean(m.cgroupPathOnFilesystem(currentPath)) != filepath.Clean(expectedSource) {
				continue
			}
		}

		originPath := ""
		if origin, ok := origins[pid]; ok {
			if origin.StartTime == identity.StartTime && origin.UID == uid {
				originPath = origin.CgroupPath
			} else {
				processedOrigins[pid] = true
			}
		}
		if originPath == "" {
			if inheritedPath, ok := m.resolveInheritedOrigin(identity, uid, origins); ok {
				originPath = inheritedPath
			}
		}

		destination := ""
		if originPath != "" {
			originFilesystemPath := m.cgroupPathOnFilesystem(originPath)
			if _, err := os.Stat(originFilesystemPath); err == nil {
				destination = originFilesystemPath
			} else if !os.IsNotExist(err) {
				planErrors = append(planErrors, fmt.Errorf("failed to stat original cgroup %s for PID %d: %w", originFilesystemPath, pid, err))
				continue
			}
		}

		recovery := destination == ""
		if recovery {
			if !allowRecovery {
				planErrors = append(planErrors, &ProcessOriginUnavailableError{PID: pid, UID: uid})
				continue
			}
			if recoveryPath == "" {
				recoveryPath, err = m.ensureRecoveryCgroup(uid, normalQuota)
				if err != nil {
					planErrors = append(planErrors, fmt.Errorf("prepare recovery cgroup for PID %d: %w", pid, err))
					continue
				}
			}
			destination = recoveryPath
		}

		plans = append(plans, processRestore{
			PID:         pid,
			StartTime:   identity.StartTime,
			Destination: destination,
			Recovery:    recovery || m.isRecoveryPath(destination),
		})
	}
	return plans, processedOrigins, reused, errors.Join(planErrors...)
}

func (m *Manager) restoreProcesses(uid int, pids []int, normalQuota string) (bool, error) {
	_, usedRecovery, _, err := m.restoreProcessesExpected(uid, pids, normalQuota, nil, "", true)
	return usedRecovery, err
}

func (m *Manager) restoreProcessesExpected(
	uid int,
	pids []int,
	normalQuota string,
	expectedStartTimes map[int]uint64,
	expectedSource string,
	allowRecovery bool,
) (int, bool, map[int]bool, error) {
	plans, processedOrigins, reused, planErr := m.buildRestorePlanExpected(
		uid,
		pids,
		normalQuota,
		expectedStartTimes,
		expectedSource,
		allowRecovery,
	)
	restored := 0
	usedRecovery := false
	recoveryPath := ""
	var restoreErrors []error
	if planErr != nil {
		restoreErrors = append(restoreErrors, planErr)
	}
	for _, plan := range plans {
		identity, identityErr := m.readProcessIdentity(plan.PID)
		switch {
		case os.IsNotExist(identityErr):
			processedOrigins[plan.PID] = true
			continue
		case identityErr != nil:
			restoreErrors = append(restoreErrors, fmt.Errorf(
				"failed to revalidate PID %d before restore: %w",
				plan.PID,
				identityErr,
			))
			continue
		case identity.StartTime != plan.StartTime:
			reused[plan.PID] = true
			processedOrigins[plan.PID] = true
			continue
		}
		if expectedSource != "" {
			currentPath, currentErr := m.readUnifiedCgroupPath(plan.PID)
			if os.IsNotExist(currentErr) {
				processedOrigins[plan.PID] = true
				continue
			}
			if currentErr != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf(
					"failed to revalidate cgroup for PID %d before restore: %w",
					plan.PID,
					currentErr,
				))
				continue
			}
			if filepath.Clean(m.cgroupPathOnFilesystem(currentPath)) != filepath.Clean(expectedSource) {
				continue
			}
		}
		destination := plan.Destination
		err := m.writePIDToCgroup(filepath.Join(destination, "cgroup.procs"), plan.PID)
		if !plan.Recovery && (os.IsNotExist(err) || errors.Is(err, syscall.EBUSY)) {
			if recoveryPath == "" {
				recoveryPath, err = m.ensureRecoveryCgroup(uid, normalQuota)
			} else {
				err = nil
			}
			if err == nil {
				destination = recoveryPath
				err = m.writePIDToCgroup(filepath.Join(destination, "cgroup.procs"), plan.PID)
				plan.Recovery = true
			}
		}
		if errors.Is(err, syscall.ESRCH) {
			processedOrigins[plan.PID] = true
			continue
		}
		if err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("failed to restore PID %d to %s: %w", plan.PID, destination, err))
			continue
		}
		processedOrigins[plan.PID] = true
		restored++
		usedRecovery = usedRecovery || plan.Recovery
	}

	if err := m.removeProcessOrigins(processedOrigins); err != nil {
		restoreErrors = append(restoreErrors, fmt.Errorf("failed to update process origin state after restore: %w", err))
	}
	if err := m.pruneInactiveProcessOrigins(uid); err != nil {
		restoreErrors = append(restoreErrors, fmt.Errorf("failed to prune process origin state after restore: %w", err))
	}
	if len(restoreErrors) > 0 {
		return restored, usedRecovery, reused, errors.Join(restoreErrors...)
	}
	return restored, usedRecovery, reused, nil
}
