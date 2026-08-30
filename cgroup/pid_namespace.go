package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// PIDNamespaceSkipReason is a bounded reason for declining cgroup ingress.
type PIDNamespaceSkipReason string

const (
	PIDNamespaceMismatch    PIDNamespaceSkipReason = "pid_namespace_mismatch"
	PIDNamespaceUnavailable PIDNamespaceSkipReason = "pid_namespace_unavailable"
)

// ProcessMoveResult describes one bounded ResMan-owned cgroup ingress operation.
type ProcessMoveResult struct {
	Candidates              int
	Moved                   int
	AlreadyPresent          int
	PIDNamespaceMismatches  int
	PIDNamespaceUnavailable int
	Disappeared             int
	Reused                  int
}

// Applied reports whether at least one process is now known to be in the
// requested ResMan-owned cgroup.
func (r ProcessMoveResult) Applied() bool {
	return r.Moved+r.AlreadyPresent > 0
}

// NamespaceSkipped reports the number of processes kept outside ResMan-owned
// cgroups because their PID namespace was foreign or unavailable.
func (r ProcessMoveResult) NamespaceSkipped() int {
	return r.PIDNamespaceMismatches + r.PIDNamespaceUnavailable
}

func (r *ProcessMoveResult) add(other ProcessMoveResult) {
	r.Candidates += other.Candidates
	r.Moved += other.Moved
	r.AlreadyPresent += other.AlreadyPresent
	r.PIDNamespaceMismatches += other.PIDNamespaceMismatches
	r.PIDNamespaceUnavailable += other.PIDNamespaceUnavailable
	r.Disappeared += other.Disappeared
	r.Reused += other.Reused
}

type pidNamespaceIdentity struct {
	device uint64
	inode  uint64
}

func readPIDNamespaceIdentity(path string) (pidNamespaceIdentity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return pidNamespaceIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return pidNamespaceIdentity{}, fmt.Errorf("PID namespace %s has unsupported stat metadata", path)
	}
	return pidNamespaceIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func (m *Manager) candidatePIDNamespaceIdentity(pid int) (pidNamespaceIdentity, error) {
	if m.readPIDNamespace != nil {
		return m.readPIDNamespace(pid)
	}
	return readPIDNamespaceIdentity(filepath.Join(m.getProcRoot(), strconv.Itoa(pid), "ns", "pid"))
}

func (m *Manager) filterPIDNamespaceCandidates(pids []int) ([]int, ProcessMoveResult) {
	result := ProcessMoveResult{Candidates: len(pids)}
	allowed := make([]int, 0, len(pids))
	for _, pid := range pids {
		identity, err := m.candidatePIDNamespaceIdentity(pid)
		switch {
		case os.IsNotExist(err):
			result.Disappeared++
		case err != nil:
			result.PIDNamespaceUnavailable++
		case identity != m.pidNamespace:
			result.PIDNamespaceMismatches++
		default:
			allowed = append(allowed, pid)
		}
	}
	return allowed, result
}

func (m *Manager) verifyPIDNamespaceIngress(pid int) (PIDNamespaceSkipReason, error) {
	identity, err := m.candidatePIDNamespaceIdentity(pid)
	if err != nil {
		if os.IsNotExist(err) {
			return "", err
		}
		return PIDNamespaceUnavailable, err
	}
	if identity != m.pidNamespace {
		return PIDNamespaceMismatch, nil
	}
	return "", nil
}

func (m *Manager) logPIDNamespaceSkips(uid int, result ProcessMoveResult) {
	if result.NamespaceSkipped() == 0 {
		return
	}
	m.logger.Warn("Skipped process ingress outside the ResMan PID namespace boundary",
		"uid", uid,
		"reason", "pid_namespace_boundary",
		"pid_namespace_mismatch_count", result.PIDNamespaceMismatches,
		"pid_namespace_unavailable_count", result.PIDNamespaceUnavailable,
	)
}

func mergePIDNamespaceSkip(result *ProcessMoveResult, reason PIDNamespaceSkipReason, err error) {
	switch {
	case reason == PIDNamespaceMismatch:
		result.PIDNamespaceMismatches++
	case reason == PIDNamespaceUnavailable:
		result.PIDNamespaceUnavailable++
	case errors.Is(err, os.ErrNotExist):
		result.Disappeared++
	}
}
