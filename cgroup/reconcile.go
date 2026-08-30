package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fdefilippo/resman/internal/processpolicy"
)

// ProcessMembershipResult describes one bounded reconciliation pass.
type ProcessMembershipResult struct {
	Scanned          int
	MovedIn          int
	RestoredExcluded int
	SkippedReused    int
	Ingress          ProcessMoveResult
}

type processMembershipCandidate struct {
	startTime  uint64
	selected   bool
	diagnostic error
}

// ReconcileUserProcessMembership makes the current process policy true for an
// already limited user. An empty sharedPath selects the tracked standalone
// resource cgroup; otherwise the destination is the user's shared child.
func (m *Manager) ReconcileUserProcessMembership(
	uid int,
	sharedPath string,
	normalQuota string,
) (ProcessMembershipResult, error) {
	var result ProcessMembershipResult
	if uid == 0 {
		return result, fmt.Errorf("refusing to reconcile root process membership")
	}

	target, err := m.processMembershipTarget(uid, sharedPath)
	if err != nil {
		return result, err
	}
	return m.reconcileUserProcessMembershipAt(uid, target, normalQuota)
}

func (m *Manager) reconcileUserProcessMembershipAt(
	uid int,
	target string,
	normalQuota string,
) (ProcessMembershipResult, error) {
	var result ProcessMembershipResult
	currentPIDs, err := m.readPidsFromFile(filepath.Join(target, "cgroup.procs"))
	if err != nil {
		return result, fmt.Errorf("read limited process membership for UID %d: %w", uid, err)
	}
	allUserPIDs, err := m.processIDsForUID(uid)
	if err != nil {
		return result, fmt.Errorf("discover processes for limited UID %d: %w", uid, err)
	}

	current := make(map[int]bool, len(currentPIDs))
	for _, pid := range currentPIDs {
		current[pid] = true
	}

	incoming := make([]int, 0)
	incomingStarts := make(map[int]uint64)
	outgoing := make([]int, 0)
	outgoingStarts := make(map[int]uint64)
	var inspectErrors []error
	seen := make(map[int]bool, len(currentPIDs)+len(allUserPIDs))

	for _, pid := range currentPIDs {
		seen[pid] = true
		candidate, exists, inspectErr := m.inspectProcessMembershipCandidate(pid)
		if inspectErr != nil {
			inspectErrors = append(inspectErrors, fmt.Errorf("inspect limited PID %d: %w", pid, inspectErr))
			continue
		}
		if !exists {
			continue
		}
		if candidate.diagnostic != nil {
			inspectErrors = append(inspectErrors, fmt.Errorf("inspect limited PID %d: %w", pid, candidate.diagnostic))
		}
		if !candidate.selected {
			outgoing = append(outgoing, pid)
			outgoingStarts[pid] = candidate.startTime
		} else {
			result.Ingress.AlreadyPresent++
		}
	}

	for _, pid := range allUserPIDs {
		if seen[pid] {
			continue
		}
		seen[pid] = true
		candidate, exists, inspectErr := m.inspectProcessMembershipCandidate(pid)
		if inspectErr != nil {
			inspectErrors = append(inspectErrors, fmt.Errorf("inspect discovered PID %d: %w", pid, inspectErr))
			continue
		}
		if !exists {
			continue
		}
		if candidate.diagnostic != nil {
			inspectErrors = append(inspectErrors, fmt.Errorf("inspect discovered PID %d: %w", pid, candidate.diagnostic))
		}
		if !candidate.selected {
			continue
		}
		incoming = append(incoming, pid)
		incomingStarts[pid] = candidate.startTime
	}
	result.Scanned = len(seen)

	restored, _, restoreReused, restoreErr := m.restoreProcessesExpected(
		uid,
		outgoing,
		normalQuota,
		outgoingStarts,
		target,
		false,
	)
	result.RestoredExcluded = restored

	moved, ingress, moveErrors, moveReused, moveErr := m.moveProcessBatchExpected(
		incoming,
		uid,
		target,
		incomingStarts,
	)
	result.MovedIn = len(moved)
	result.Ingress.add(ingress)
	reused := make(map[int]bool, len(restoreReused)+len(moveReused))
	for pid := range restoreReused {
		reused[pid] = true
	}
	for pid := range moveReused {
		reused[pid] = true
	}
	result.SkippedReused = len(reused)

	reconcileErrors := append(inspectErrors, restoreErr, moveErr)
	for pid, moveFailure := range moveErrors {
		reconcileErrors = append(reconcileErrors, fmt.Errorf("move PID %d into limited cgroup: %w", pid, moveFailure))
	}
	return result, errors.Join(reconcileErrors...)
}

func (m *Manager) processMembershipTarget(uid int, sharedPath string) (string, error) {
	if sharedPath != "" {
		target := filepath.Join(sharedPath, fmt.Sprintf("user_%d", uid))
		if _, err := os.Stat(target); err != nil {
			return "", fmt.Errorf("inspect shared process cgroup for UID %d: %w", uid, err)
		}
		return target, nil
	}
	target, exists := m.getCgroupPath(uid)
	if !exists {
		return "", fmt.Errorf("standalone process cgroup for UID %d is not tracked", uid)
	}
	if _, err := os.Stat(target); err != nil {
		return "", fmt.Errorf("inspect standalone process cgroup for UID %d: %w", uid, err)
	}
	return target, nil
}

func (m *Manager) inspectProcessMembershipCandidate(pid int) (processMembershipCandidate, bool, error) {
	identity, err := m.readProcessIdentity(pid)
	if os.IsNotExist(err) {
		return processMembershipCandidate{}, false, nil
	}
	if err != nil {
		return processMembershipCandidate{}, false, err
	}
	processInfo, identityErr := m.getProcessInfo(pid)
	if os.IsNotExist(identityErr) {
		return processMembershipCandidate{}, false, nil
	}
	if len(processInfo) == 0 {
		return processMembershipCandidate{}, false, identityErr
	}
	confirmedIdentity, err := m.readProcessIdentity(pid)
	if os.IsNotExist(err) {
		return processMembershipCandidate{}, false, nil
	}
	if err != nil {
		return processMembershipCandidate{}, false, err
	}
	if confirmedIdentity.StartTime != identity.StartTime {
		return processMembershipCandidate{}, false, nil
	}
	selection := processpolicy.Evaluate(
		m.getConfig(),
		processInfo["executable"],
		processInfo["name"],
	)
	return processMembershipCandidate{
		startTime:  identity.StartTime,
		selected:   selection.Enforceable,
		diagnostic: identityErr,
	}, true, nil
}
