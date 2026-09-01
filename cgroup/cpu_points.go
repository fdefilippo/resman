package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fdefilippo/resman/internal/cpupoints"
)

const (
	cpuPointsGuaranteedDomain = "guaranteed"
	cpuPointsBestEffortDomain = "best_effort"
)

// CPUPointsHierarchy names the three-level CPU Points hierarchy owned by ResMan.
type CPUPointsHierarchy struct {
	Parent     string
	Guaranteed string
	BestEffort string
}

// DomainPath returns the internal domain for one allocation class.
func (h CPUPointsHierarchy) DomainPath(class cpupoints.AllocationClass) (string, error) {
	switch class {
	case cpupoints.AllocationClassGuaranteed:
		return h.Guaranteed, nil
	case cpupoints.AllocationClassBestEffort:
		return h.BestEffort, nil
	default:
		return "", fmt.Errorf("unknown CPU Points allocation class %q", class)
	}
}

// EnsureCPUPointsHierarchy creates and verifies the finite parent and its two
// internal scheduling domains. No process is ever placed in an internal node.
func (m *Manager) EnsureCPUPointsHierarchy(parentQuota cpupoints.ParentQuota, bestEffortWeight cpupoints.KernelCPUWeight) (CPUPointsHierarchy, error) {
	parent, err := m.CreateSharedCgroup()
	if err != nil {
		return CPUPointsHierarchy{}, err
	}
	hierarchy := CPUPointsHierarchy{
		Parent:     parent,
		Guaranteed: filepath.Join(parent, cpuPointsGuaranteedDomain),
		BestEffort: filepath.Join(parent, cpuPointsBestEffortDomain),
	}

	if err := writeCPUPointsValue(filepath.Join(parent, "cpu.max"), parentQuota.CPUmax()); err != nil {
		return CPUPointsHierarchy{}, fmt.Errorf("apply CPU Points parent quota: %w", err)
	}

	requirements := enabledControllerInterfaces(m.getConfig())
	candidates := m.managedHierarchyControllerCandidates(requirements)
	for _, domain := range []string{hierarchy.Guaranteed, hierarchy.BestEffort} {
		createManagedCgroup := m.createManagedCgroup
		if createManagedCgroup == nil {
			createManagedCgroup = func(path string) error { return os.Mkdir(path, 0755) }
		}
		if err := createManagedCgroup(domain); err != nil && !os.IsExist(err) {
			return CPUPointsHierarchy{}, fmt.Errorf("create CPU Points domain %s: %w", domain, err)
		}
		if _, err := m.enableControllerInterfaces(filepath.Join(domain, "cgroup.subtree_control"), candidates, requirements); err != nil {
			return CPUPointsHierarchy{}, fmt.Errorf("enable controllers in CPU Points domain %s: %w", domain, err)
		}
		if pids, err := m.readPidsFromFile(filepath.Join(domain, "cgroup.procs")); err != nil {
			return CPUPointsHierarchy{}, fmt.Errorf("verify empty CPU Points domain %s: %w", domain, err)
		} else if len(pids) != 0 {
			return CPUPointsHierarchy{}, fmt.Errorf("CPU Points internal domain %s contains %d processes", domain, len(pids))
		}
	}

	// A zero active guarantee has no participating children. Weight one is the
	// legal dormant representation until admission raises it before ingress.
	if err := writeCPUPointsValue(filepath.Join(hierarchy.Guaranteed, "cpu.weight"), "1"); err != nil {
		return CPUPointsHierarchy{}, fmt.Errorf("initialize guaranteed domain weight: %w", err)
	}
	if err := writeCPUPointsValue(filepath.Join(hierarchy.BestEffort, "cpu.weight"), strconv.Itoa(bestEffortWeight.Value())); err != nil {
		return CPUPointsHierarchy{}, fmt.Errorf("apply best-effort domain weight: %w", err)
	}
	return hierarchy, nil
}

// ApplyCPUPointsParentQuota updates the finite parent from one live-capacity plan.
func (m *Manager) ApplyCPUPointsParentQuota(hierarchy CPUPointsHierarchy, quota cpupoints.ParentQuota) error {
	if err := writeCPUPointsValue(filepath.Join(hierarchy.Parent, "cpu.max"), quota.CPUmax()); err != nil {
		return fmt.Errorf("apply CPU Points parent quota: %w", err)
	}
	return nil
}

// ApplyCPUPointsGuaranteedWeight publishes the aggregate acquired guarantee.
func (m *Manager) ApplyCPUPointsGuaranteedWeight(hierarchy CPUPointsHierarchy, weight cpupoints.KernelCPUWeight) error {
	if err := writeCPUPointsValue(filepath.Join(hierarchy.Guaranteed, "cpu.weight"), strconv.Itoa(weight.Value())); err != nil {
		return fmt.Errorf("apply CPU Points guaranteed domain weight: %w", err)
	}
	return nil
}

// ApplyCPUPointsBestEffortWeight publishes the aggregate best-effort entitlement.
func (m *Manager) ApplyCPUPointsBestEffortWeight(hierarchy CPUPointsHierarchy, weight cpupoints.KernelCPUWeight) error {
	if err := writeCPUPointsValue(filepath.Join(hierarchy.BestEffort, "cpu.weight"), strconv.Itoa(weight.Value())); err != nil {
		return fmt.Errorf("apply CPU Points best-effort domain weight: %w", err)
	}
	return nil
}

// ApplyCPUPointsUserWeight updates and verifies one existing leaf without moving processes.
func (m *Manager) ApplyCPUPointsUserWeight(leafPath string, weight cpupoints.KernelCPUWeight) error {
	if err := writeCPUPointsValue(filepath.Join(leafPath, "cpu.weight"), strconv.Itoa(weight.Value())); err != nil {
		return fmt.Errorf("apply CPU Points leaf weight: %w", err)
	}
	if err := writeCPUPointsValue(filepath.Join(leafPath, "cpu.max"), normalCPUQuota); err != nil {
		return fmt.Errorf("verify CPU Points leaf unlimited quota: %w", err)
	}
	return nil
}

// EnsureCPUPointsUserPlacement configures and verifies a leaf before any PID
// may enter it. The caller must raise the guaranteed-domain weight first.
func (m *Manager) EnsureCPUPointsUserPlacement(uid int, domainPath string, weight cpupoints.KernelCPUWeight) (string, ProcessMoveResult, error) {
	return m.ensureUserCgroupPlacement(uid, domainPath, normalCPUQuota, &weight)
}

// ReleaseCPUPointsUser restores one leaf's processes and removes the leaf.
func (m *Manager) ReleaseCPUPointsUser(uid int, domainPath string) error {
	return m.ReleaseUserFromSharedCgroup(uid, domainPath, normalCPUQuota)
}

// RemoveCPUPointsHierarchy removes empty domains and then the empty parent.
func (m *Manager) RemoveCPUPointsHierarchy(hierarchy CPUPointsHierarchy) error {
	for _, path := range []string{hierarchy.Guaranteed, hierarchy.BestEffort, hierarchy.Parent} {
		if err := m.removeManagedCgroupPath(path); err != nil {
			return fmt.Errorf("remove CPU Points hierarchy node %s: %w", path, err)
		}
	}
	return nil
}

func writeCPUPointsValue(path, value string) error {
	if err := os.WriteFile(path, []byte(value), defaultFilePerm); err != nil {
		return fmt.Errorf("write %s to %s: %w", value, path, err)
	}
	readback, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read back %s: %w", path, err)
	}
	if actual := strings.TrimSpace(string(readback)); actual != value {
		return fmt.Errorf("read back %s as %q after writing %q", path, actual, value)
	}
	return nil
}
