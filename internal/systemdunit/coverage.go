package systemdunit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ResourceKind names one independently authorized resource.
type ResourceKind string

const (
	ResourceMemory ResourceKind = "memory"
	ResourceIO     ResourceKind = "io"
)

// ResourceCoverageState describes whether one user slice covers its workload.
type ResourceCoverageState string

const (
	ResourceCoverageComplete ResourceCoverageState = "complete"
	ResourceCoveragePartial  ResourceCoverageState = "partial"
	ResourceCoverageRefused  ResourceCoverageState = "refused"
)

// ResourceCoverageReason is a bounded resource-authority outcome.
type ResourceCoverageReason string

const (
	ResourceCoverageVerified          ResourceCoverageReason = "verified"
	ResourceCoverageAuthoritySplit    ResourceCoverageReason = "authority_split"
	ResourceCoverageRuntimeDescendant ResourceCoverageReason = "runtime_owned_descendant"
	ResourceCoverageInspectionFailed  ResourceCoverageReason = "inspection_unavailable"
	ResourceCoverageControllerMissing ResourceCoverageReason = "controller_unavailable"
	ResourceCoverageApplyFailed       ResourceCoverageReason = "apply_failed"
)

// ResourceAuthority records one complete, partial or refused authority decision.
type ResourceAuthority struct {
	Resource ResourceKind
	State    ResourceCoverageState
	Reason   ResourceCoverageReason
}

// ObserveCPUCoverage checks UID-wide membership without excluding rootless
// descendants: CPU authority covers the entire user slice, including containers.
func (a *Adapter) ObserveCPUCoverage(ctx context.Context, topology TopologySnapshot) (map[uint32]bool, error) {
	leave := a.opGate.Enter()
	defer leave()
	if err := a.requireOpen("observe_cpu_coverage"); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	inspector, ok := a.coverage.(procCoverageInspector)
	if !ok {
		return nil, fmt.Errorf("CPU coverage inspector is unavailable")
	}
	targets := make([]resourceCoverageTarget, 0, len(topology.Users))
	for _, user := range topology.Users {
		targets = append(targets, coverageTargetFor(user.UID, user.Unit))
	}
	return inspector.observeCPU(ctx, targets)
}

func (i procCoverageInspector) observeCPU(ctx context.Context, targets []resourceCoverageTarget) (map[uint32]bool, error) {
	result := make(map[uint32]bool, len(targets))
	paths := make(map[uint32]string, len(targets))
	for _, target := range targets {
		result[target.uid] = true
		paths[target.uid] = target.controlGroup
	}
	entries, err := i.readDir(i.root)
	if err != nil {
		return nil, fmt.Errorf("scan CPU authority: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := strconv.ParseUint(entry.Name(), 10, 32); err != nil {
			continue
		}
		path := filepath.Join(i.root, entry.Name())
		info, err := i.stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect CPU authority process: %w", err)
		}
		uid := i.ownerUID(info)
		parent, tracked := paths[uid]
		if !tracked {
			continue
		}
		data, err := i.readFile(filepath.Join(path, "cgroup"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read CPU authority process: %w", err)
		}
		member := ""
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "0::") {
				member = strings.TrimPrefix(line, "0::")
			}
		}
		if member == "" {
			return nil, fmt.Errorf("CPU authority process has no unified cgroup")
		}
		if member != parent && !strings.HasPrefix(member, strings.TrimSuffix(parent, "/")+"/") {
			result[uid] = false
		}
	}
	return result, nil
}

// ResourceAuthorityRequest describes one resource-specific authority check.
type ResourceAuthorityRequest struct {
	Identity    UnitIdentity
	UID         uint32
	Resource    ResourceKind
	Assignments []PropertyAssignment
}

// ResourceAuthorityResult is one result from a batched authority inspection.
type ResourceAuthorityResult struct {
	Authority ResourceAuthority
	Err       error
}

// ResourceAuthorityError reports a refusal at either resource-authority confirmation boundary.
type ResourceAuthorityError struct {
	UID       uint32
	Authority ResourceAuthority
	Err       error
}

func (e *ResourceAuthorityError) Error() string {
	message := fmt.Sprintf("systemd resource authority for UID %d %s is %s (%s)", e.UID, e.Authority.Resource, e.Authority.State, e.Authority.Reason)
	if e.Err != nil {
		return message + ": " + e.Err.Error()
	}
	return message
}

// Unwrap exposes the bounded inspection or controller failure.
func (e *ResourceAuthorityError) Unwrap() error { return e.Err }

type resourceCoverageInspector interface {
	inspectMany(context.Context, []resourceCoverageTarget) []resourceCoverageInspection
}

type resourceCoverageTarget struct {
	uid          uint32
	controlGroup string
}

type resourceCoverageInspection struct {
	authority ResourceAuthority
	err       error
}

func coverageTargetFor(uid uint32, snapshot UnitSnapshot) resourceCoverageTarget {
	return resourceCoverageTarget{uid: uid, controlGroup: snapshot.ControlGroup}
}

type procCoverageInspector struct {
	root     string
	readDir  func(string) ([]os.DirEntry, error)
	readFile func(string) ([]byte, error)
	stat     func(string) (os.FileInfo, error)
	ownerUID func(os.FileInfo) uint32
}

func newProcCoverageInspector(root string) procCoverageInspector {
	if root == "" {
		root = "/proc"
	}
	return procCoverageInspector{root: root, readDir: os.ReadDir, readFile: os.ReadFile, stat: os.Stat, ownerUID: processOwnerUID}
}

func (i procCoverageInspector) inspect(ctx context.Context, uid uint32, controlGroup string) (ResourceAuthority, error) {
	results := i.inspectMany(ctx, []resourceCoverageTarget{{uid: uid, controlGroup: controlGroup}})
	return results[0].authority, results[0].err
}

func (i procCoverageInspector) inspectMany(ctx context.Context, targets []resourceCoverageTarget) []resourceCoverageInspection {
	results := make([]resourceCoverageInspection, len(targets))
	for index := range results {
		results[index].authority = ResourceAuthority{State: ResourceCoverageComplete, Reason: ResourceCoverageVerified}
	}
	refuseAll := func(reason ResourceCoverageReason, err error) []resourceCoverageInspection {
		for index := range results {
			results[index].authority, results[index].err = refusedAuthority(reason, err)
		}
		return results
	}
	if len(targets) == 0 {
		return results
	}
	entries, err := i.readDir(i.root)
	if err != nil {
		return refuseAll(ResourceCoverageInspectionFailed, err)
	}
	hostNamespace, err := i.stat(filepath.Join(i.root, "1", "ns", "pid"))
	if err != nil {
		return refuseAll(ResourceCoverageInspectionFailed, fmt.Errorf("inspect host PID namespace: %w", err))
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return refuseAll(ResourceCoverageInspectionFailed, err)
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || !entry.IsDir() {
			continue
		}
		processRoot := filepath.Join(i.root, entry.Name())
		processInfo, err := i.stat(processRoot)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return refuseAll(ResourceCoverageInspectionFailed, fmt.Errorf("inspect process %d: %w", pid, err))
		}
		processCgroup, err := readUnifiedProcessCgroup(i.readFile, filepath.Join(processRoot, "cgroup"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return refuseAll(ResourceCoverageInspectionFailed, fmt.Errorf("inspect process %d cgroup: %w", pid, err))
		}
		ownerUID := i.ownerUID
		if ownerUID == nil {
			ownerUID = processOwnerUID
		}
		uid := ownerUID(processInfo)
		var processNamespace os.FileInfo
		var namespaceErr error
		namespaceRead := false
		for index, target := range targets {
			if results[index].authority.State != ResourceCoverageComplete {
				continue
			}
			if !controlGroupContains(target.controlGroup, processCgroup) {
				if uid == target.uid {
					results[index].authority = ResourceAuthority{State: ResourceCoveragePartial, Reason: ResourceCoverageAuthoritySplit}
				}
				continue
			}
			if runtimeOwnedCgroupPath(processCgroup) {
				results[index].authority, results[index].err = refusedAuthority(ResourceCoverageRuntimeDescendant, nil)
				continue
			}
			if !namespaceRead {
				processNamespace, namespaceErr = i.stat(filepath.Join(processRoot, "ns", "pid"))
				namespaceRead = true
			}
			if errors.Is(namespaceErr, os.ErrNotExist) {
				continue
			}
			if namespaceErr != nil {
				results[index].authority, results[index].err = refusedAuthority(ResourceCoverageInspectionFailed, fmt.Errorf("inspect process %d PID namespace: %w", pid, namespaceErr))
				continue
			}
			if !os.SameFile(hostNamespace, processNamespace) {
				results[index].authority, results[index].err = refusedAuthority(ResourceCoverageRuntimeDescendant, nil)
			}
		}
	}
	return results
}

func runtimeOwnedCgroupPath(path string) bool {
	for _, component := range strings.Split(filepath.Clean(path), "/") {
		switch {
		case runtimeScopeComponent(component, "libpod-"), runtimeScopeComponent(component, "docker-"), runtimeScopeComponent(component, "cri-containerd-"):
			return true
		case component == "docker", component == "kubepods.slice", component == "kubepods":
			return true
		case strings.HasPrefix(component, "kubepods-") || strings.HasPrefix(component, "kubepods_"):
			return true
		}
	}
	return false
}

func runtimeScopeComponent(component, prefix string) bool {
	if !strings.HasPrefix(component, prefix) || !strings.HasSuffix(component, ".scope") {
		return false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(component, prefix), ".scope")
	if len(id) < 12 || len(id) > 64 {
		return false
	}
	for _, character := range id {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') && (character < 'A' || character > 'F') {
			return false
		}
	}
	return true
}

func refusedAuthority(reason ResourceCoverageReason, err error) (ResourceAuthority, error) {
	return ResourceAuthority{State: ResourceCoverageRefused, Reason: reason}, err
}

func readUnifiedProcessCgroup(readFile func(string) ([]byte, error), path string) (string, error) {
	data, err := readFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && parts[0] == "0" && parts[1] == "" && validControlGroup(parts[2]) {
			return filepath.Clean(parts[2]), nil
		}
	}
	return "", fmt.Errorf("unified cgroup v2 entry is absent")
}

func controlGroupContains(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	return child == parent || strings.HasPrefix(child, parent+"/")
}

func processOwnerUID(info os.FileInfo) uint32 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Uid
	}
	return ^uint32(0)
}
