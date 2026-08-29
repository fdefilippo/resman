package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type blockIOCounters struct {
	readBytes  uint64
	writeBytes uint64
	readOps    uint64
	writeOps   uint64
}

type blockIOAccountingState struct {
	path   string
	base   blockIOCounters
	offset blockIOCounters
}

// UserCgroupPlacementIncompleteError reports a transient split between the
// authoritative and alternate ResMan cgroups for one user.
type UserCgroupPlacementIncompleteError struct {
	UID           int
	AlternatePath string
	DesiredPath   string
	Processes     int
}

func (e *UserCgroupPlacementIncompleteError) Error() string {
	return fmt.Sprintf(
		"split cgroup placement for UID %d: %s still contains %d processes while %s is authoritative",
		e.UID,
		e.AlternatePath,
		e.Processes,
		e.DesiredPath,
	)
}

func (c blockIOCounters) add(other blockIOCounters) blockIOCounters {
	return blockIOCounters{
		readBytes:  c.readBytes + other.readBytes,
		writeBytes: c.writeBytes + other.writeBytes,
		readOps:    c.readOps + other.readOps,
		writeOps:   c.writeOps + other.writeOps,
	}
}

func (c blockIOCounters) delta(base blockIOCounters) blockIOCounters {
	return blockIOCounters{
		readBytes:  monotonicDelta(c.readBytes, base.readBytes),
		writeBytes: monotonicDelta(c.writeBytes, base.writeBytes),
		readOps:    monotonicDelta(c.readOps, base.readOps),
		writeOps:   monotonicDelta(c.writeOps, base.writeOps),
	}
}

func monotonicDelta(current, previous uint64) uint64 {
	if current < previous {
		return 0
	}
	return current - previous
}

func (m *Manager) logicalBlockIOCounters(uid int) (blockIOCounters, error) {
	for attempt := 0; attempt < 3; attempt++ {
		m.blockIOMu.Lock()
		state, exists := m.blockIOAccounting[uid]
		m.blockIOMu.Unlock()

		if !exists {
			initialized, err := m.initializeBlockIOAccounting(uid)
			if err != nil {
				return blockIOCounters{}, err
			}
			state = initialized
		}

		raw, err := m.readManagedBlockIOCounters(state.path)
		if err != nil {
			m.blockIOMu.Lock()
			current, stillCurrent := m.blockIOAccounting[uid]
			stateChanged := !stillCurrent || current != state
			m.blockIOMu.Unlock()
			if stateChanged {
				continue
			}
			return blockIOCounters{}, err
		}

		m.blockIOMu.Lock()
		current, unchanged := m.blockIOAccounting[uid]
		unchanged = unchanged && current == state
		m.blockIOMu.Unlock()
		if unchanged {
			return state.offset.add(raw.delta(state.base)), nil
		}
	}
	return blockIOCounters{}, fmt.Errorf("cgroup placement for UID %d changed repeatedly while reading block I/O counters", uid)
}

// initializeBlockIOAccounting establishes a zero logical baseline for an
// already tracked cgroup. It performs cgroup I/O before publishing the state so
// readers never hold blockIOMu across filesystem operations.
func (m *Manager) initializeBlockIOAccounting(uid int) (blockIOAccountingState, error) {
	path, exists := m.getCgroupPath(uid)
	if !exists {
		return blockIOAccountingState{}, fmt.Errorf("cgroup for UID %d not found", uid)
	}
	raw, err := m.readManagedBlockIOCounters(path)
	if err != nil {
		return blockIOAccountingState{}, err
	}
	if currentPath, stillExists := m.getCgroupPath(uid); !stillExists || filepath.Clean(currentPath) != filepath.Clean(path) {
		return blockIOAccountingState{}, fmt.Errorf("cgroup placement for UID %d changed while initializing block I/O counters", uid)
	}

	initialized := blockIOAccountingState{path: path, base: raw}
	m.blockIOMu.Lock()
	if current, alreadyInitialized := m.blockIOAccounting[uid]; alreadyInitialized {
		initialized = current
	} else {
		m.blockIOAccounting[uid] = initialized
	}
	m.blockIOMu.Unlock()
	return initialized, nil
}

// EnsureUserCgroupPlacement keeps a user in the requested ResMan cgroup while
// preserving a monotonic logical io.stat counter across placement changes.
// An empty sharedPath selects the standalone observation cgroup; a non-empty
// path selects that shared hierarchy's per-user child.
func (m *Manager) EnsureUserCgroupPlacement(uid int, sharedPath, normalQuota string) (string, error) {
	if uid == 0 {
		return "", fmt.Errorf("refusing to place root processes in a managed cgroup")
	}

	desiredPath := m.getUserCgroupPath(uid)
	if sharedPath != "" {
		desiredPath = filepath.Join(sharedPath, fmt.Sprintf("user_%d", uid))
	}

	currentPath, exists := m.getCgroupPath(uid)
	if exists && filepath.Clean(currentPath) == filepath.Clean(desiredPath) {
		if _, err := m.ReconcileUserProcessMembership(uid, sharedPath, normalQuota); err != nil {
			return "", err
		}
		if err := m.cleanupAlternateUserCgroup(uid, desiredPath); err != nil {
			return "", err
		}
		return desiredPath, nil
	}
	if !exists {
		if err := m.createAndPopulateUserCgroup(uid, sharedPath, desiredPath); err != nil {
			return "", err
		}
		return desiredPath, nil
	}

	if err := m.transitionUserCgroup(uid, currentPath, desiredPath, normalQuota); err != nil {
		return "", err
	}
	return desiredPath, nil
}

func (m *Manager) createAndPopulateUserCgroup(uid int, sharedPath, desiredPath string) (retErr error) {
	if err := m.createUserCgroupDirectory(uid, desiredPath); err != nil {
		return err
	}
	defer func() {
		if retErr == nil {
			return
		}
		cleanupErr := m.CleanupUserCgroup(uid)
		retErr = errors.Join(retErr, cleanupErr)
	}()

	initial, err := m.readManagedBlockIOCounters(desiredPath)
	if err != nil {
		return fmt.Errorf("read initial block I/O counters for UID %d: %w", uid, err)
	}
	if err := m.trackCgroupPath(uid, desiredPath); err != nil {
		return fmt.Errorf("track cgroup placement for UID %d: %w", uid, err)
	}
	m.blockIOMu.Lock()
	m.blockIOAccounting[uid] = blockIOAccountingState{path: desiredPath, base: initial}
	m.blockIOMu.Unlock()

	if sharedPath == "" {
		if err := m.MoveAllUserProcesses(uid); err != nil {
			return fmt.Errorf("populate standalone observation cgroup for UID %d: %w", uid, err)
		}
	} else if err := m.MoveAllUserProcessesToSharedCgroup(uid, sharedPath); err != nil {
		return fmt.Errorf("populate shared cgroup for UID %d: %w", uid, err)
	}
	return nil
}

func (m *Manager) createUserCgroupDirectory(uid int, path string) error {
	if err := os.MkdirAll(path, 0755); err != nil {
		return fmt.Errorf("create cgroup directory %s for UID %d: %w", path, uid, err)
	}
	if sharedRoot := filepath.Dir(path); filepath.Clean(sharedRoot) != filepath.Clean(m.getBaseCgroupPath()) {
		weightPath := filepath.Join(path, "cpu.weight")
		if err := os.WriteFile(weightPath, []byte("100"), 0644); err != nil {
			return fmt.Errorf("set default CPU weight for UID %d: %w", uid, err)
		}
	}
	return nil
}

func (m *Manager) transitionUserCgroup(uid int, oldPath, newPath, normalQuota string) (retErr error) {
	if err := m.createUserCgroupDirectory(uid, newPath); err != nil {
		return err
	}
	cleanupNew := true
	defer func() {
		if cleanupNew {
			retErr = errors.Join(retErr, m.removeManagedCgroupPath(newPath))
		}
	}()

	if _, err := m.reconcileUserProcessMembershipAt(uid, oldPath, normalQuota); err != nil {
		return fmt.Errorf("reconcile source placement for UID %d: %w", uid, err)
	}

	accounting, err := m.initializeBlockIOAccounting(uid)
	if err != nil {
		return fmt.Errorf("initialize source block I/O accounting for UID %d: %w", uid, err)
	}
	if filepath.Clean(accounting.path) != filepath.Clean(oldPath) {
		return fmt.Errorf("tracked block I/O source for UID %d is %s, expected %s", uid, accounting.path, oldPath)
	}
	newBase, err := m.readManagedBlockIOCounters(newPath)
	if err != nil {
		return fmt.Errorf("read destination block I/O counters for UID %d: %w", uid, err)
	}
	oldPIDs, err := m.readPidsFromFile(filepath.Join(oldPath, "cgroup.procs"))
	if err != nil {
		return fmt.Errorf("read source cgroup processes for UID %d: %w", uid, err)
	}
	moved, moveErrors, err := m.moveProcessBatch(oldPIDs, uid, newPath)
	if err != nil {
		return fmt.Errorf("move UID %d between managed cgroups: %w", uid, err)
	}
	if len(moveErrors) > 0 {
		var errs []error
		for pid, moveErr := range moveErrors {
			errs = append(errs, fmt.Errorf("move PID %d: %w", pid, moveErr))
		}
		errs = append(errs, m.rollbackUserCgroupTransition(uid, moved, oldPath))
		return errors.Join(errs...)
	}
	oldFinal, err := m.readManagedBlockIOCounters(oldPath)
	if err != nil {
		return errors.Join(
			fmt.Errorf("read final source block I/O counters for UID %d: %w", uid, err),
			m.rollbackUserCgroupTransition(uid, moved, oldPath),
		)
	}
	logicalFinal := accounting.offset.add(oldFinal.delta(accounting.base))

	if err := m.trackCgroupPath(uid, newPath); err != nil {
		return errors.Join(
			fmt.Errorf("publish cgroup placement transition for UID %d: %w", uid, err),
			m.rollbackUserCgroupTransition(uid, moved, oldPath),
		)
	}
	m.blockIOMu.Lock()
	m.blockIOAccounting[uid] = blockIOAccountingState{
		path:   newPath,
		base:   newBase,
		offset: logicalFinal,
	}
	m.blockIOMu.Unlock()
	cleanupNew = false
	if err := m.removeManagedCgroupPath(oldPath); err != nil {
		m.logger.Warn("User cgroup placement changed but empty source cleanup is deferred",
			"uid", uid,
			"old_path", oldPath,
			"new_path", newPath,
			"error", err,
		)
	}
	return nil
}

func (m *Manager) cleanupAlternateUserCgroup(uid int, desiredPath string) error {
	standalonePath := m.getUserCgroupPath(uid)
	sharedPath := filepath.Join(m.getBaseCgroupPath(), "limited", fmt.Sprintf("user_%d", uid))
	alternatePath := standalonePath
	if filepath.Clean(desiredPath) == filepath.Clean(standalonePath) {
		alternatePath = sharedPath
	}
	if _, err := os.Stat(alternatePath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect alternate cgroup placement for UID %d at %s: %w", uid, alternatePath, err)
	}
	pids, err := m.readPidsFromFile(filepath.Join(alternatePath, "cgroup.procs"))
	if err != nil {
		return fmt.Errorf("read alternate cgroup placement for UID %d at %s: %w", uid, alternatePath, err)
	}
	if len(pids) > 0 {
		return &UserCgroupPlacementIncompleteError{
			UID:           uid,
			AlternatePath: alternatePath,
			DesiredPath:   desiredPath,
			Processes:     len(pids),
		}
	}
	if err := m.removeManagedCgroupPath(alternatePath); err != nil {
		return fmt.Errorf("remove stale alternate cgroup placement for UID %d at %s: %w", uid, alternatePath, err)
	}
	return nil
}

func (m *Manager) rollbackUserCgroupTransition(uid int, moved []int, oldPath string) error {
	if len(moved) > 0 {
		_, moveErrors, err := m.moveProcessBatch(moved, uid, oldPath)
		if err != nil {
			return fmt.Errorf("rollback process placement for UID %d: %w", uid, err)
		}
		if len(moveErrors) > 0 {
			var errs []error
			for pid, moveErr := range moveErrors {
				errs = append(errs, fmt.Errorf("rollback PID %d: %w", pid, moveErr))
			}
			return errors.Join(errs...)
		}
	}
	if err := m.trackCgroupPath(uid, oldPath); err != nil {
		return fmt.Errorf("restore tracked cgroup for UID %d: %w", uid, err)
	}
	return nil
}

func (m *Manager) removeManagedCgroupPath(path string) error {
	var result cgroupRemovalResult
	var err error
	if m.removeManagedCgroup != nil {
		result, err = m.removeManagedCgroup(path)
	} else {
		result, err = removeCgroupWithRetry(path)
	}
	if result.retried && m.observeRemovalRetry != nil {
		m.observeRemovalRetry()
	}
	return err
}

func readBlockIOCounters(cgroupPath string) (blockIOCounters, error) {
	readBytes, writeBytes, readOps, writeOps, err := readIOStatsFile(filepath.Join(cgroupPath, "io.stat"))
	if err != nil {
		return blockIOCounters{}, err
	}
	return blockIOCounters{
		readBytes: readBytes, writeBytes: writeBytes,
		readOps: readOps, writeOps: writeOps,
	}, nil
}

func (m *Manager) readManagedBlockIOCounters(cgroupPath string) (blockIOCounters, error) {
	if m.readBlockIOStats != nil {
		return m.readBlockIOStats(cgroupPath)
	}
	return readBlockIOCounters(cgroupPath)
}
