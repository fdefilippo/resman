package systemdunit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcCoverageInspectorClassifiesCompleteSplitAndRuntimeOwnedWorkloads(t *testing.T) {
	tests := []struct {
		name        string
		outside     bool
		foreignNS   bool
		runtimePath bool
		wantCPU     bool
		wantState   ResourceCoverageState
		wantReason  ResourceCoverageReason
	}{
		{name: "complete", wantCPU: true, wantState: ResourceCoverageComplete, wantReason: ResourceCoverageVerified},
		{name: "authority_split", outside: true, wantState: ResourceCoveragePartial, wantReason: ResourceCoverageAuthoritySplit},
		{name: "runtime_owned_descendant", foreignNS: true, wantCPU: true, wantState: ResourceCoverageRefused, wantReason: ResourceCoverageRuntimeDescendant},
		{name: "rootless_runtime_with_host_pid_namespace", runtimePath: true, wantCPU: true, wantState: ResourceCoverageRefused, wantReason: ResourceCoverageRuntimeDescendant},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			hostNS := filepath.Join(root, "host-pid-ns")
			if err := os.WriteFile(hostNS, []byte("host"), 0600); err != nil {
				t.Fatal(err)
			}
			makeProcFixture(t, root, "1", "/", hostNS)
			path := "/user.slice/user-1000.slice/session-1.scope"
			if test.outside {
				path = "/system.slice/foreign.service"
			}
			if test.runtimePath {
				path = "/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-0123456789abcdef.scope/container"
			}
			processNS := hostNS
			if test.foreignNS {
				processNS = filepath.Join(root, "foreign-pid-ns")
				if err := os.WriteFile(processNS, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			makeProcFixture(t, root, "42", path, processNS)

			inspector := newProcCoverageInspector(root)
			inspector.ownerUID = func(info os.FileInfo) uint32 {
				if info.Name() == "42" {
					return 1000
				}
				return 0
			}
			captured := inspector.capture(context.Background(), []resourceCoverageTarget{{uid: 1000, controlGroup: "/user.slice/user-1000.slice"}}, true)
			if captured.err != nil {
				t.Fatalf("capture() error: %v", captured.err)
			}
			got := captured.resources[0].authority
			if got.State != test.wantState || got.Reason != test.wantReason {
				t.Fatalf("authority = %+v, want state=%s reason=%s", got, test.wantState, test.wantReason)
			}
			if captured.cpuCoverage[1000] != test.wantCPU {
				t.Fatalf("CPU coverage = %t, want %t", captured.cpuCoverage[1000], test.wantCPU)
			}
		})
	}
}

func TestProcCoverageInspectorFailsClosedWhenHostNamespaceCannotBeRead(t *testing.T) {
	root := t.TempDir()
	inspector := newProcCoverageInspector(root)
	got, err := inspector.inspect(context.Background(), uint32(os.Getuid()), "/user.slice/user-1000.slice")
	if got.State != ResourceCoverageRefused || got.Reason != ResourceCoverageInspectionFailed || err == nil {
		t.Fatalf("authority=%+v error=%v, want refused inspection failure", got, err)
	}
}

func TestCheckResourceAuthorityRejectsMissingControllerBeforeMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.slice", "user-1000.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	transport := newFakeUnitTransport(1000)
	for _, unit := range transport.units {
		delete(unit.slice, "ControlGroupId")
	}
	verifier := newCgroupVerifier(root)
	adapter := mustTestAdapter(t, transport, verifier)
	adapter.coverage = staticCoverageInspector{authority: ResourceAuthority{State: ResourceCoverageComplete, Reason: ResourceCoverageVerified}}
	assignment := mustAssignment(t, PropertyMemoryHigh, 64<<20)
	snapshot, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := adapter.CaptureProcessAuthorityInventory(context.Background(), snapshot, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	results, err := adapter.CheckCapturedResourceAuthorities(context.Background(), inventory, []ResourceAuthorityRequest{{Identity: snapshot.Users[0].Unit.Identity, UID: 1000, Resource: ResourceMemory, Assignments: []PropertyAssignment{assignment}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := results[0].Authority, results[0].Err
	var authorityErr *ResourceAuthorityError
	if !errors.As(err, &authorityErr) || got.Reason != ResourceCoverageControllerMissing {
		t.Fatalf("authority=%+v error=%v, want controller refusal", got, err)
	}
	if len(transport.setCalls) != 0 {
		t.Fatalf("controller refusal performed %d mutations", len(transport.setCalls))
	}
}

func TestCheckResourceAuthorityAcceptsIOThatSystemdWillMaterializeOnApply(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.slice", "user-1000.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu io memory\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "user.slice", "cgroup.controllers"), []byte("cpu memory\n"), 0600); err != nil {
		t.Fatal(err)
	}
	transport := newFakeUnitTransport(1000)
	for _, unit := range transport.units {
		delete(unit.slice, "ControlGroupId")
	}
	verifier := newCgroupVerifier(root)
	adapter := mustTestAdapter(t, transport, verifier)
	adapter.coverage = staticCoverageInspector{authority: ResourceAuthority{State: ResourceCoverageComplete, Reason: ResourceCoverageVerified}}
	assignment, err := NewDevicePropertyAssignment(PropertyIOReadBandwidthMax, []DeviceLimit{{Path: "/dev/vda", Value: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	topology, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := adapter.CaptureProcessAuthorityInventory(context.Background(), topology, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	results, err := adapter.CheckCapturedResourceAuthorities(context.Background(), inventory, []ResourceAuthorityRequest{{Identity: topology.Users[0].Unit.Identity, UID: 1000, Resource: ResourceIO, Assignments: []PropertyAssignment{assignment}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := results[0].Authority, results[0].Err
	if err != nil || got.State != ResourceCoverageComplete || got.Reason != ResourceCoverageVerified {
		t.Fatalf("authority=%+v error=%v, want complete authority before transactional materialization", got, err)
	}
	if len(transport.setCalls) != 0 {
		t.Fatalf("authority inspection performed %d mutations", len(transport.setCalls))
	}
}

func TestCapturedInventoryUsesOneProcSnapshotForCPUAndAllResources(t *testing.T) {
	root := t.TempDir()
	hostNS := filepath.Join(root, "host-pid-ns")
	if err := os.WriteFile(hostNS, []byte("host"), 0600); err != nil {
		t.Fatal(err)
	}
	makeProcFixture(t, root, "1", "/", hostNS)
	inspector := newProcCoverageInspector(root)
	inspector.ownerUID = func(os.FileInfo) uint32 { return 0 }
	readDir := inspector.readDir
	readDirCalls := 0
	inspector.readDir = func(path string) ([]os.DirEntry, error) {
		readDirCalls++
		return readDir(path)
	}

	transport := newFakeUnitTransport(1000, 1001)
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	adapter.coverage = inspector
	topology, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := adapter.CaptureProcessAuthorityInventory(context.Background(), topology, 42, true)
	if err != nil {
		t.Fatal(err)
	}
	if inventory.SampleEpochID() != 42 || inventory.TopologyFingerprint() != TopologyFingerprint(topology) || !inventory.HasResourceDetail() {
		t.Fatalf("inventory metadata = epoch %d fingerprint %q detail %t", inventory.SampleEpochID(), inventory.TopologyFingerprint(), inventory.HasResourceDetail())
	}
	if coverage := inventory.CPUCoverage(); !coverage[1000] || !coverage[1001] {
		t.Fatalf("CPU coverage = %v, want both user slices complete", coverage)
	}
	identities := map[uint32]UnitIdentity{}
	for _, user := range topology.Users {
		identities[user.UID] = user.Unit.Identity
	}
	memory := mustAssignment(t, PropertyMemoryHigh, 64<<20)
	ioWeight := mustAssignment(t, PropertyIOWeight, 100)
	requests := []ResourceAuthorityRequest{
		{Identity: identities[1000], UID: 1000, Resource: ResourceMemory, Assignments: []PropertyAssignment{memory}},
		{Identity: identities[1000], UID: 1000, Resource: ResourceIO, Assignments: []PropertyAssignment{ioWeight}},
		{Identity: identities[1001], UID: 1001, Resource: ResourceMemory, Assignments: []PropertyAssignment{memory}},
		{Identity: identities[1001], UID: 1001, Resource: ResourceIO, Assignments: []PropertyAssignment{ioWeight}},
	}
	results, err := adapter.CheckCapturedResourceAuthorities(context.Background(), inventory, requests)
	if err != nil {
		t.Fatalf("CheckCapturedResourceAuthorities() error: %v", err)
	}
	for index, result := range results {
		if result.Err != nil || result.Authority.State != ResourceCoverageComplete {
			t.Fatalf("result %d = %+v", index, result)
		}
	}
	if readDirCalls != 1 {
		t.Fatalf("/proc snapshots = %d, want one shared by CPU and all authority requests", readDirCalls)
	}
}

func TestCapturedInventoryRefusesTargetAbsentFromCapturedTopology(t *testing.T) {
	adapter := mustTestAdapter(t, newFakeUnitTransport(1000, 1001), &fakeKernelVerifier{})
	topology, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	capturedTopology := topology
	capturedTopology.Users = capturedTopology.Users[:1]
	inventory := NewProcessAuthorityInventory(7, TopologyFingerprint(capturedTopology), true, []ProcessAuthorityObservation{{
		UID:               capturedTopology.Users[0].UID,
		Identity:          capturedTopology.Users[0].Unit.Identity,
		CPUCoverage:       true,
		ResourceAuthority: ResourceAuthority{State: ResourceCoverageComplete, Reason: ResourceCoverageVerified},
	}})
	assignment := mustAssignment(t, PropertyMemoryHigh, 64<<20)
	request := ResourceAuthorityRequest{Identity: topology.Users[1].Unit.Identity, UID: topology.Users[1].UID, Resource: ResourceMemory, Assignments: []PropertyAssignment{assignment}}
	results, err := adapter.CheckCapturedResourceAuthorities(context.Background(), inventory, []ResourceAuthorityRequest{request})
	if err != nil {
		t.Fatal(err)
	}
	var authorityErr *ResourceAuthorityError
	if len(results) != 1 || !errors.As(results[0].Err, &authorityErr) || results[0].Authority.Reason != ResourceCoverageTopologyChanged {
		t.Fatalf("absent target result = %+v, want typed sample-topology refusal", results)
	}
}

func TestCapturedInventoryDefersLaterMembershipToTheNextSample(t *testing.T) {
	root := t.TempDir()
	hostNS := filepath.Join(root, "host-pid-ns")
	if err := os.WriteFile(hostNS, []byte("host"), 0600); err != nil {
		t.Fatal(err)
	}
	makeProcFixture(t, root, "1", "/", hostNS)
	inspector := newProcCoverageInspector(root)
	inspector.ownerUID = func(info os.FileInfo) uint32 {
		if info.Name() == "42" {
			return 1000
		}
		return 0
	}
	adapter := mustTestAdapter(t, newFakeUnitTransport(1000), &fakeKernelVerifier{})
	adapter.coverage = inspector
	topology, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first, err := adapter.CaptureProcessAuthorityInventory(context.Background(), topology, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	assignment := mustAssignment(t, PropertyMemoryHigh, 64<<20)
	request := ResourceAuthorityRequest{Identity: topology.Users[0].Unit.Identity, UID: 1000, Resource: ResourceMemory, Assignments: []PropertyAssignment{assignment}}

	makeProcFixture(t, root, "42", "/system.slice/late.service", hostNS)
	oldResults, err := adapter.CheckCapturedResourceAuthorities(context.Background(), first, []ResourceAuthorityRequest{request})
	if err != nil || len(oldResults) != 1 || oldResults[0].Err != nil || oldResults[0].Authority.State != ResourceCoverageComplete {
		t.Fatalf("frozen first inventory changed after later membership: results=%+v error=%v", oldResults, err)
	}
	second, err := adapter.CaptureProcessAuthorityInventory(context.Background(), topology, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	secondResults, err := adapter.CheckCapturedResourceAuthorities(context.Background(), second, []ResourceAuthorityRequest{request})
	if err != nil || len(secondResults) != 1 || secondResults[0].Authority.Reason != ResourceCoverageAuthoritySplit {
		t.Fatalf("next sample did not observe late membership: results=%+v error=%v", secondResults, err)
	}

	if err := os.RemoveAll(filepath.Join(root, "42")); err != nil {
		t.Fatal(err)
	}
	third, err := adapter.CaptureProcessAuthorityInventory(context.Background(), topology, 3, true)
	if err != nil {
		t.Fatal(err)
	}
	thirdResults, err := adapter.CheckCapturedResourceAuthorities(context.Background(), third, []ResourceAuthorityRequest{request})
	if err != nil || len(thirdResults) != 1 || thirdResults[0].Err != nil || thirdResults[0].Authority.State != ResourceCoverageComplete {
		t.Fatalf("process absent for the complete sample remained classified: results=%+v error=%v", thirdResults, err)
	}
}

type staticCoverageInspector struct {
	authority ResourceAuthority
	err       error
}

func (i staticCoverageInspector) capture(_ context.Context, targets []resourceCoverageTarget, includeResourceDetail bool) processAuthorityCapture {
	result := processAuthorityCapture{cpuCoverage: make(map[uint32]bool, len(targets)), resources: make([]resourceCoverageInspection, len(targets))}
	for index, target := range targets {
		result.cpuCoverage[target.uid] = true
		if includeResourceDetail {
			result.resources[index] = resourceCoverageInspection(i)
		}
	}
	return result
}

func makeProcFixture(t *testing.T, root, pid, cgroup, namespaceTarget string) {
	t.Helper()
	processRoot := filepath.Join(root, pid)
	if err := os.MkdirAll(filepath.Join(processRoot, "ns"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processRoot, "cgroup"), []byte("0::"+cgroup+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(namespaceTarget, filepath.Join(processRoot, "ns", "pid")); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkProcCoverageInspectorRepresentativeHost(b *testing.B) {
	const (
		processCount = 10_000
		hostUIDCount = 500
		trackedUIDs  = 100
	)
	entries := make([]os.DirEntry, 0, processCount)
	for pid := 1; pid <= processCount; pid++ {
		entries = append(entries, benchmarkProcEntry{name: strconv.Itoa(pid)})
	}
	targets := make([]resourceCoverageTarget, 0, trackedUIDs)
	for offset := 0; offset < trackedUIDs; offset++ {
		uid := uint32(1000 + offset)
		targets = append(targets, resourceCoverageTarget{uid: uid, controlGroup: "/user.slice/user-" + strconv.FormatUint(uint64(uid), 10) + ".slice"})
	}
	resourceTargets := make([]resourceCoverageTarget, 0, len(targets)*2)
	resourceTargets = append(resourceTargets, targets...)
	resourceTargets = append(resourceTargets, targets...)

	b.Run("cpu_only", func(b *testing.B) {
		inspector, counts := benchmarkProcInspector(entries, hostUIDCount)
		b.ResetTimer()
		for iteration := 0; iteration < b.N; iteration++ {
			if _, err := inspector.observeCPU(context.Background(), targets); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		b.ReportMetric(float64(counts.cgroupReads)/float64(b.N), "cgroup_reads/op")
		b.ReportMetric(float64(counts.namespaceStats)/float64(b.N), "pidns_stats/op")
	})

	b.Run("ram_io", func(b *testing.B) {
		inspector, counts := benchmarkProcInspector(entries, hostUIDCount)
		b.ResetTimer()
		for iteration := 0; iteration < b.N; iteration++ {
			results := inspector.inspectMany(context.Background(), resourceTargets)
			if len(results) != len(resourceTargets) {
				b.Fatalf("results = %d, want %d", len(results), len(resourceTargets))
			}
		}
		b.StopTimer()
		b.ReportMetric(float64(counts.cgroupReads)/float64(b.N), "cgroup_reads/op")
		b.ReportMetric(float64(counts.namespaceStats)/float64(b.N), "pidns_stats/op")
	})
}

func TestCPUOnlyCaptureRetainsTheCheapObservationShape(t *testing.T) {
	const (
		processCount = 1_000
		hostUIDCount = 500
		trackedUIDs  = 100
	)
	entries := make([]os.DirEntry, 0, processCount)
	for pid := 1; pid <= processCount; pid++ {
		entries = append(entries, benchmarkProcEntry{name: strconv.Itoa(pid)})
	}
	targets := make([]resourceCoverageTarget, 0, trackedUIDs)
	for offset := 0; offset < trackedUIDs; offset++ {
		uid := uint32(1000 + offset)
		targets = append(targets, resourceCoverageTarget{uid: uid, controlGroup: "/user.slice/user-" + strconv.FormatUint(uint64(uid), 10) + ".slice"})
	}
	cpuInspector, cpuCounts := benchmarkProcInspector(entries, hostUIDCount)
	if captured := cpuInspector.capture(context.Background(), targets, false); captured.err != nil {
		t.Fatal(captured.err)
	}
	if cpuCounts.cgroupReads != 200 || cpuCounts.namespaceStats != 0 {
		t.Fatalf("CPU-only reads = cgroup:%d pidns:%d, want cgroup:200 pidns:0", cpuCounts.cgroupReads, cpuCounts.namespaceStats)
	}

	resourceInspector, resourceCounts := benchmarkProcInspector(entries, hostUIDCount)
	if captured := resourceInspector.capture(context.Background(), targets, true); captured.err != nil {
		t.Fatal(captured.err)
	}
	if resourceCounts.cgroupReads != processCount || resourceCounts.namespaceStats <= 0 {
		t.Fatalf("resource reads = cgroup:%d pidns:%d, want all %d cgroups and relevant namespaces", resourceCounts.cgroupReads, resourceCounts.namespaceStats, processCount)
	}
}

type benchmarkProcCounts struct {
	cgroupReads    int
	namespaceStats int
}

type benchmarkProcEntry struct {
	name string
	uid  uint32
	dev  uint64
	ino  uint64
}

func (e benchmarkProcEntry) Name() string               { return e.name }
func (e benchmarkProcEntry) IsDir() bool                { return true }
func (e benchmarkProcEntry) Type() os.FileMode          { return os.ModeDir }
func (e benchmarkProcEntry) Info() (os.FileInfo, error) { return e, nil }
func (e benchmarkProcEntry) Size() int64                { return 0 }
func (e benchmarkProcEntry) Mode() os.FileMode          { return os.ModeDir | 0500 }
func (e benchmarkProcEntry) ModTime() time.Time         { return time.Time{} }
func (e benchmarkProcEntry) Sys() interface{} {
	return &syscall.Stat_t{Uid: e.uid, Dev: e.dev, Ino: e.ino}
}

func benchmarkProcInspector(entries []os.DirEntry, hostUIDCount int) (procCoverageInspector, *benchmarkProcCounts) {
	counts := &benchmarkProcCounts{}
	inspector := newProcCoverageInspector("/synthetic-proc")
	inspector.readDir = func(string) ([]os.DirEntry, error) { return entries, nil }
	inspector.stat = func(path string) (os.FileInfo, error) {
		if strings.HasSuffix(path, "/ns/pid") {
			counts.namespaceStats++
			return benchmarkProcEntry{name: "pid", dev: 1, ino: 1}, nil
		}
		pid, err := strconv.Atoi(filepath.Base(path))
		if err != nil {
			return nil, err
		}
		return benchmarkProcEntry{name: strconv.Itoa(pid), uid: uint32(1000 + pid%hostUIDCount)}, nil
	}
	inspector.readFile = func(path string) ([]byte, error) {
		counts.cgroupReads++
		pid, err := strconv.Atoi(filepath.Base(filepath.Dir(path)))
		if err != nil {
			return nil, err
		}
		uid := 1000 + pid%hostUIDCount
		return []byte("0::/user.slice/user-" + strconv.Itoa(uid) + ".slice/session.scope\n"), nil
	}
	return inspector, counts
}
