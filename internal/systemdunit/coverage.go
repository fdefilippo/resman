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
)

// ResourceAuthority records one complete, partial or refused authority decision.
type ResourceAuthority struct {
	Resource ResourceKind
	State    ResourceCoverageState
	Reason   ResourceCoverageReason
}

// ResourceAuthorityError reports a refusal made before resource mutation.
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
	inspect(context.Context, uint32, string) (ResourceAuthority, error)
}

func inspectResourceCoverage(inspector resourceCoverageInspector, ctx context.Context, uid uint32, snapshot UnitSnapshot) (ResourceAuthority, error) {
	return inspector.inspect(ctx, uid, snapshot.ControlGroup)
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
	complete := ResourceAuthority{State: ResourceCoverageComplete, Reason: ResourceCoverageVerified}
	entries, err := i.readDir(i.root)
	if err != nil {
		return refusedAuthority(ResourceCoverageInspectionFailed, err)
	}
	hostNamespace, err := i.stat(filepath.Join(i.root, "1", "ns", "pid"))
	if err != nil {
		return refusedAuthority(ResourceCoverageInspectionFailed, fmt.Errorf("inspect host PID namespace: %w", err))
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return refusedAuthority(ResourceCoverageInspectionFailed, err)
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
			return refusedAuthority(ResourceCoverageInspectionFailed, fmt.Errorf("inspect process %d: %w", pid, err))
		}
		processCgroup, err := readUnifiedProcessCgroup(i.readFile, filepath.Join(processRoot, "cgroup"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return refusedAuthority(ResourceCoverageInspectionFailed, fmt.Errorf("inspect process %d cgroup: %w", pid, err))
		}
		inside := controlGroupContains(controlGroup, processCgroup)
		if !inside {
			ownerUID := i.ownerUID
			if ownerUID == nil {
				ownerUID = processOwnerUID
			}
			if ownerUID(processInfo) == uid {
				return ResourceAuthority{State: ResourceCoveragePartial, Reason: ResourceCoverageAuthoritySplit}, nil
			}
			continue
		}
		if runtimeOwnedCgroupPath(processCgroup) {
			return refusedAuthority(ResourceCoverageRuntimeDescendant, nil)
		}
		processNamespace, err := i.stat(filepath.Join(processRoot, "ns", "pid"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return refusedAuthority(ResourceCoverageInspectionFailed, fmt.Errorf("inspect process %d PID namespace: %w", pid, err))
		}
		if !os.SameFile(hostNamespace, processNamespace) {
			return refusedAuthority(ResourceCoverageRuntimeDescendant, nil)
		}
	}
	return complete, nil
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
