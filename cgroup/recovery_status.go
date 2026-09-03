package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// RecoveryOccupant identifies one live process stranded in a ResMan recovery leaf.
type RecoveryOccupant struct {
	UID       int
	PID       int
	StartTime uint64
}

// RecoverySnapshot is the current non-mutating view of the recovery hierarchy.
type RecoverySnapshot struct {
	Occupants []RecoveryOccupant
	EmptyUIDs []int
}

// RecoverySnapshot returns all live recovery occupants without treating the
// recovery hierarchy as an authoritative process origin.
func (m *Manager) RecoverySnapshot() (RecoverySnapshot, error) {
	var result RecoverySnapshot
	root := m.getRecoveryRootPath()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("failed to inspect recovery hierarchy %s: %w", root, err)
	}

	var snapshotErrors []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "user_") {
			continue
		}
		uid, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), "user_"))
		if err != nil || uid < 0 {
			snapshotErrors = append(snapshotErrors, fmt.Errorf("invalid recovery cgroup %s", entry.Name()))
			continue
		}
		pids, err := m.readPidsFromFile(filepath.Join(root, entry.Name(), "cgroup.procs"))
		if err != nil {
			snapshotErrors = append(snapshotErrors, fmt.Errorf("failed to inspect recovery processes for UID %d: %w", uid, err))
			continue
		}
		if len(pids) == 0 {
			result.EmptyUIDs = append(result.EmptyUIDs, uid)
			continue
		}
		for _, pid := range pids {
			identity, err := m.readProcessIdentity(pid)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				snapshotErrors = append(snapshotErrors, fmt.Errorf("failed to identify recovery PID %d for UID %d: %w", pid, uid, err))
				continue
			}
			result.Occupants = append(result.Occupants, RecoveryOccupant{
				UID:       uid,
				PID:       pid,
				StartTime: identity.StartTime,
			})
		}
	}
	sort.Slice(result.Occupants, func(i, j int) bool {
		if result.Occupants[i].UID != result.Occupants[j].UID {
			return result.Occupants[i].UID < result.Occupants[j].UID
		}
		return result.Occupants[i].PID < result.Occupants[j].PID
	})
	sort.Ints(result.EmptyUIDs)
	return result, errors.Join(snapshotErrors...)
}
