package systemdunit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// ResourceKind names one independently authorized resource.
type ResourceKind string

const (
	ResourceMemory   ResourceKind = "memory"
	ResourceIO       ResourceKind = "io"
	ResourceIOWeight ResourceKind = "io_weight"
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
	ResourceCoverageTopologyChanged   ResourceCoverageReason = "sample_topology_changed"
)

// ResourceAuthority records one complete, partial or refused authority decision.
type ResourceAuthority struct {
	Resource ResourceKind
	State    ResourceCoverageState
	Reason   ResourceCoverageReason
}

// ProcessAuthorityObservation freezes the CPU and optional RAM/I/O authority
// classification for one user slice captured during a decision sample.
type ProcessAuthorityObservation struct {
	UID               uint32
	Identity          UnitIdentity
	CPUCoverage       bool
	ResourceAuthority ResourceAuthority
	ResourceError     error
}

// ProcessAuthorityInventory is one sample-scoped process-membership view. Its
// observations are collected over one /proc traversal and are frozen afterward.
type ProcessAuthorityInventory struct {
	sampleEpochID       int64
	topologyFingerprint string
	resourceDetail      bool
	observations        []ProcessAuthorityObservation
}

// NewProcessAuthorityInventory builds a defensive copy of a captured inventory.
func NewProcessAuthorityInventory(sampleEpochID int64, topologyFingerprint string, resourceDetail bool, observations []ProcessAuthorityObservation) ProcessAuthorityInventory {
	return ProcessAuthorityInventory{
		sampleEpochID:       sampleEpochID,
		topologyFingerprint: topologyFingerprint,
		resourceDetail:      resourceDetail,
		observations:        append([]ProcessAuthorityObservation(nil), observations...),
	}
}

// SampleEpochID returns the decision sample that owns the inventory.
func (i ProcessAuthorityInventory) SampleEpochID() int64 { return i.sampleEpochID }

// TopologyFingerprint returns capture provenance for diagnostics. Authority is
// validated per observed UID and exact unit identity, not as one global gate.
func (i ProcessAuthorityInventory) TopologyFingerprint() string { return i.topologyFingerprint }

// HasResourceDetail reports whether RAM/I/O membership data was captured.
func (i ProcessAuthorityInventory) HasResourceDetail() bool { return i.resourceDetail }

// CPUCoverage returns a detached UID-wide CPU coverage projection.
func (i ProcessAuthorityInventory) CPUCoverage() map[uint32]bool {
	result := make(map[uint32]bool, len(i.observations))
	for _, observation := range i.observations {
		result[observation.UID] = observation.CPUCoverage
	}
	return result
}

// Observation returns the classification captured for an exact UID and unit lifetime.
func (i ProcessAuthorityInventory) Observation(uid uint32, identity UnitIdentity) (ProcessAuthorityObservation, bool) {
	for _, observation := range i.observations {
		if observation.UID == uid && observation.Identity == identity {
			return observation, true
		}
	}
	return ProcessAuthorityObservation{}, false
}

// TopologyFingerprint identifies the complete authoritative systemd topology.
func TopologyFingerprint(topology TopologySnapshot) string {
	identityKey := func(identity UnitIdentity) string {
		return fmt.Sprintf("%s:%s:%d", identity.Name, identity.InvocationIDString(), identity.ControlGroupID)
	}
	parts := make([]string, 0, len(topology.Users)+1)
	parts = append(parts, "parent:"+identityKey(topology.Parent.Identity))
	for _, user := range topology.Users {
		parts = append(parts, fmt.Sprintf("uid:%d:%s", user.UID, identityKey(user.Unit.Identity)))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
}

// CaptureProcessAuthorityInventory observes CPU coverage and optional RAM/I/O
// authority from one process-population traversal for a decision sample.
func (a *Adapter) CaptureProcessAuthorityInventory(ctx context.Context, topology TopologySnapshot, sampleEpochID int64, includeResourceDetail bool) (ProcessAuthorityInventory, error) {
	leave := a.opGate.Enter()
	defer leave()
	if err := a.requireOpen("capture_process_authority_inventory"); err != nil {
		return ProcessAuthorityInventory{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	targets := make([]resourceCoverageTarget, 0, len(topology.Users))
	for _, user := range topology.Users {
		targets = append(targets, coverageTargetFor(user.UID, user.Unit))
	}
	captured := a.coverage.capture(callCtx, targets, includeResourceDetail)
	if captured.err != nil {
		return ProcessAuthorityInventory{}, captured.err
	}
	observations := make([]ProcessAuthorityObservation, len(targets))
	for index, target := range targets {
		observations[index] = ProcessAuthorityObservation{UID: target.uid, Identity: topology.Users[index].Unit.Identity, CPUCoverage: captured.cpuCoverage[target.uid]}
		if includeResourceDetail {
			observations[index].ResourceAuthority = captured.resources[index].authority
			observations[index].ResourceError = captured.resources[index].err
		}
	}
	return NewProcessAuthorityInventory(sampleEpochID, TopologyFingerprint(topology), includeResourceDetail, observations), nil
}

// ObserveCPUCoverage checks UID-wide membership without excluding rootless
// descendants: CPU authority covers the entire user slice, including containers.
func (a *Adapter) ObserveCPUCoverage(ctx context.Context, topology TopologySnapshot) (map[uint32]bool, error) {
	inventory, err := a.CaptureProcessAuthorityInventory(ctx, topology, 0, false)
	if err != nil {
		return nil, err
	}
	return inventory.CPUCoverage(), nil
}

func (i procCoverageInspector) observeCPU(ctx context.Context, targets []resourceCoverageTarget) (map[uint32]bool, error) {
	captured := i.capture(ctx, targets, false)
	return captured.cpuCoverage, captured.err
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
	capture(context.Context, []resourceCoverageTarget, bool) processAuthorityCapture
}

type resourceCoverageTarget struct {
	uid          uint32
	controlGroup string
}

type resourceCoverageInspection struct {
	authority ResourceAuthority
	err       error
}

type processAuthorityCapture struct {
	cpuCoverage map[uint32]bool
	resources   []resourceCoverageInspection
	err         error
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
	return i.capture(ctx, targets, true).resources
}

func (i procCoverageInspector) capture(ctx context.Context, targets []resourceCoverageTarget, includeResourceDetail bool) processAuthorityCapture {
	if !includeResourceDetail {
		return i.captureCPUOnly(ctx, targets)
	}
	captured := processAuthorityCapture{
		cpuCoverage: make(map[uint32]bool, len(targets)),
		resources:   make([]resourceCoverageInspection, len(targets)),
	}
	paths := make(map[uint32]string, len(targets))
	for index, target := range targets {
		captured.cpuCoverage[target.uid] = true
		paths[target.uid] = target.controlGroup
		captured.resources[index].authority = ResourceAuthority{State: ResourceCoverageComplete, Reason: ResourceCoverageVerified}
	}
	refuseAll := func(reason ResourceCoverageReason, err error) processAuthorityCapture {
		captured.err = err
		for index := range captured.resources {
			captured.resources[index].authority, captured.resources[index].err = refusedAuthority(reason, err)
		}
		return captured
	}
	if len(targets) == 0 {
		return captured
	}
	entries, err := i.readDir(i.root)
	if err != nil {
		return refuseAll(ResourceCoverageInspectionFailed, fmt.Errorf("scan process authority: %w", err))
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
		ownerUID := i.ownerUID
		if ownerUID == nil {
			ownerUID = processOwnerUID
		}
		uid := ownerUID(processInfo)
		parent, tracked := paths[uid]
		processCgroup, err := readUnifiedProcessCgroup(i.readFile, filepath.Join(processRoot, "cgroup"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return refuseAll(ResourceCoverageInspectionFailed, fmt.Errorf("inspect process %d cgroup: %w", pid, err))
		}
		if tracked && !controlGroupContains(parent, processCgroup) {
			captured.cpuCoverage[uid] = false
		}
		var processNamespace os.FileInfo
		var namespaceErr error
		namespaceRead := false
		for index, target := range targets {
			if captured.resources[index].authority.State != ResourceCoverageComplete {
				continue
			}
			if !controlGroupContains(target.controlGroup, processCgroup) {
				if uid == target.uid {
					captured.resources[index].authority = ResourceAuthority{State: ResourceCoveragePartial, Reason: ResourceCoverageAuthoritySplit}
				}
				continue
			}
			if runtimeOwnedCgroupPath(processCgroup) {
				captured.resources[index].authority, captured.resources[index].err = refusedAuthority(ResourceCoverageRuntimeDescendant, nil)
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
				captured.resources[index].authority, captured.resources[index].err = refusedAuthority(ResourceCoverageInspectionFailed, fmt.Errorf("inspect process %d PID namespace: %w", pid, namespaceErr))
				continue
			}
			if !os.SameFile(hostNamespace, processNamespace) {
				captured.resources[index].authority, captured.resources[index].err = refusedAuthority(ResourceCoverageRuntimeDescendant, nil)
			}
		}
	}
	return captured
}

func (i procCoverageInspector) captureCPUOnly(ctx context.Context, targets []resourceCoverageTarget) processAuthorityCapture {
	result := make(map[uint32]bool, len(targets))
	paths := make(map[uint32]string, len(targets))
	for _, target := range targets {
		result[target.uid] = true
		paths[target.uid] = target.controlGroup
	}
	entries, err := i.readDir(i.root)
	if err != nil {
		return processAuthorityCapture{cpuCoverage: result, err: fmt.Errorf("scan CPU authority: %w", err)}
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return processAuthorityCapture{cpuCoverage: result, err: err}
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
			return processAuthorityCapture{cpuCoverage: result, err: fmt.Errorf("inspect CPU authority process: %w", err)}
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
			return processAuthorityCapture{cpuCoverage: result, err: fmt.Errorf("read CPU authority process: %w", err)}
		}
		member := ""
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "0::") {
				member = strings.TrimPrefix(line, "0::")
			}
		}
		if member == "" {
			return processAuthorityCapture{cpuCoverage: result, err: fmt.Errorf("CPU authority process has no unified cgroup")}
		}
		if member != parent && !strings.HasPrefix(member, strings.TrimSuffix(parent, "/")+"/") {
			result[uid] = false
		}
	}
	return processAuthorityCapture{cpuCoverage: result}
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
