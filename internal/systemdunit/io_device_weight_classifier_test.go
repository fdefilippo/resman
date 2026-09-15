package systemdunit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestParseIODeviceWeightDevicesRequiresCanonicalUniqueExplicitNumbers(t *testing.T) {
	got, err := ParseIODeviceWeightDevices("8:16, 8:0")
	if err != nil {
		t.Fatal(err)
	}
	want := []IODeviceWeightDeviceNumber{{Major: 8, Minor: 0}, {Major: 8, Minor: 16}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseIODeviceWeightDevices() = %+v, want %+v", got, want)
	}
	tests := []struct {
		name     string
		selector string
		reason   IODeviceWeightCapabilityReason
	}{
		{name: "empty", reason: IODeviceWeightReasonEmptySelector},
		{name: "all", selector: "all", reason: IODeviceWeightReasonInvalidSelector},
		{name: "path", selector: "/dev/vda", reason: IODeviceWeightReasonInvalidSelector},
		{name: "glob", selector: "8:*", reason: IODeviceWeightReasonInvalidSelector},
		{name: "zero", selector: "0:0", reason: IODeviceWeightReasonInvalidSelector},
		{name: "leading zero", selector: "08:0", reason: IODeviceWeightReasonInvalidSelector},
		{name: "duplicate", selector: "8:0,8:0", reason: IODeviceWeightReasonDuplicateDevice},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseIODeviceWeightDevices(test.selector)
			var capabilityErr *IODeviceWeightCapabilityError
			if !errors.As(err, &capabilityErr) || capabilityErr.Reason != test.reason {
				t.Fatalf("error = %v, want reason %s", err, test.reason)
			}
		})
	}
}

func TestIODeviceWeightCapabilityOutcomeNamesPreservePhaseBoundaries(t *testing.T) {
	if got := string(IODeviceWeightProbeCandidate); got != "probe_candidate" {
		t.Fatalf("probe candidate outcome = %q", got)
	}
	if got := string(IODeviceWeightMechanismInactive); got != "mechanism_inactive" {
		t.Fatalf("inactive mechanism outcome = %q", got)
	}
}

func TestIODeviceWeightClassifierUsesSoftwareIdentityOnlyAsDiagnostics(t *testing.T) {
	tests := []struct {
		name          string
		osRelease     string
		kernelRelease string
	}{
		{name: "characterized OL9 RHCK", osRelease: "ID=ol\nVERSION_ID=9.8\n", kernelRelease: "5.14.0-687.el9.x86_64"},
		{name: "previously unclaimed distribution", osRelease: "ID=rocky\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=9.8\n", kernelRelease: "5.14.0-687.el9.x86_64"},
		{name: "previously unclaimed UEK", osRelease: "ID=ol\nVERSION_ID=9.8\n", kernelRelease: "6.12.0-204.el9uek.x86_64"},
		{name: "previously unsupported line", osRelease: "ID=ol\nVERSION_ID=8.10\n", kernelRelease: "4.18.0-553.el8.x86_64"},
		{name: "malformed diagnostics", osRelease: "not-os-release\n", kernelRelease: "custom"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newIODeviceWeightClassifierFixture(t)
			fixture.write(fixture.classifier.io.osReleasePath, test.osRelease)
			fixture.classifier.io.kernelRelease = func() (string, error) { return test.kernelRelease, nil }
			snapshot, err := fixture.classifier.Classify(context.Background(), "8:0")
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Outcome() != IODeviceWeightProbeCandidate {
				t.Fatalf("outcome/reason = %s/%s, want probe candidate", snapshot.Outcome(), snapshot.Reason())
			}
		})
	}
}

func TestIODeviceWeightClassifierDoesNotRequireDiagnosticIdentitySources(t *testing.T) {
	fixture := newIODeviceWeightClassifierFixture(t)
	readFile := fixture.classifier.io.readFile
	fixture.classifier.io.readFile = func(path string) ([]byte, error) {
		if path == fixture.classifier.io.osReleasePath {
			return nil, os.ErrPermission
		}
		return readFile(path)
	}
	fixture.classifier.io.kernelRelease = func() (string, error) { return "", os.ErrPermission }
	snapshot := fixture.classify(t)
	if snapshot.Platform() != (IODeviceWeightPlatformIdentity{}) {
		t.Fatalf("Platform() = %+v, want empty best-effort diagnostics", snapshot.Platform())
	}
}

func TestIODeviceWeightClassifierRequiresActiveMechanismEvidence(t *testing.T) {
	tests := []struct {
		name      string
		scheduler string
		qos       string
		outcome   IODeviceWeightCapabilityOutcome
		reason    IODeviceWeightCapabilityReason
		mechanism IODeviceWeightMechanism
	}{
		{name: "BFQ active", scheduler: "mq-deadline [bfq] none", qos: "8:0 enable=0 ctrl=user\n", outcome: IODeviceWeightProbeCandidate, mechanism: IODeviceWeightMechanismBFQ},
		{name: "io cost active", scheduler: "[mq-deadline] bfq none", qos: "8:0 enable=1 ctrl=user\n", outcome: IODeviceWeightProbeCandidate, mechanism: IODeviceWeightMechanismIOCost},
		{name: "both active", scheduler: "mq-deadline [bfq] none", qos: "8:0 enable=1 ctrl=user\n", outcome: IODeviceWeightMechanismAmbiguous, reason: IODeviceWeightReasonMechanismAmbiguous},
		{name: "both inactive", scheduler: "[mq-deadline] bfq none", qos: "8:0 enable=0 ctrl=user\n", outcome: IODeviceWeightMechanismInactive, reason: IODeviceWeightReasonNoActiveMechanism},
		{name: "io cost enabled only for another device", scheduler: "[mq-deadline] bfq none", qos: "8:16 enable=1 ctrl=user\n", outcome: IODeviceWeightMechanismInactive, reason: IODeviceWeightReasonNoActiveMechanism},
		{name: "invalid io cost evidence", scheduler: "[mq-deadline] bfq none", qos: "8:0 enable=maybe ctrl=user\n", outcome: IODeviceWeightEvidenceUnavailable, reason: IODeviceWeightReasonEvidenceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newIODeviceWeightClassifierFixture(t)
			fixture.write(fixture.schedulerPath, test.scheduler+"\n")
			fixture.write(fixture.ioCostQOSPath, test.qos)
			snapshot, err := fixture.classifier.Classify(context.Background(), "8:0")
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Outcome() != test.outcome || snapshot.Reason() != test.reason {
				t.Fatalf("outcome/reason = %s/%s, want %s/%s", snapshot.Outcome(), snapshot.Reason(), test.outcome, test.reason)
			}
			targets := snapshot.ProbeTargets()
			if test.outcome != IODeviceWeightProbeCandidate {
				if targets != nil {
					t.Fatalf("ProbeTargets() = %+v for negative outcome", targets)
				}
				return
			}
			if len(targets) != 1 || targets[0].Mechanism != test.mechanism || targets[0].Identity.Number.String() != "8:0" {
				t.Fatalf("ProbeTargets() = %+v, want mechanism %s on 8:0", targets, test.mechanism)
			}
		})
	}
}

func TestIODeviceWeightCapabilityAggregationKeepsTheDeviceSetAtomic(t *testing.T) {
	active := IODeviceWeightDeviceCapability{Outcome: IODeviceWeightProbeCandidate}
	tests := []struct {
		name    string
		devices []IODeviceWeightDeviceCapability
		outcome IODeviceWeightCapabilityOutcome
		reason  IODeviceWeightCapabilityReason
	}{
		{name: "inactive member", devices: []IODeviceWeightDeviceCapability{active, {Outcome: IODeviceWeightMechanismInactive, Reason: IODeviceWeightReasonNoActiveMechanism}}, outcome: IODeviceWeightMechanismInactive, reason: IODeviceWeightReasonNoActiveMechanism},
		{name: "ambiguous member", devices: []IODeviceWeightDeviceCapability{active, {Outcome: IODeviceWeightMechanismAmbiguous, Reason: IODeviceWeightReasonMechanismAmbiguous}}, outcome: IODeviceWeightMechanismAmbiguous, reason: IODeviceWeightReasonMechanismAmbiguous},
		{name: "definitive ambiguity dominates unavailable", devices: []IODeviceWeightDeviceCapability{{Outcome: IODeviceWeightMechanismAmbiguous, Reason: IODeviceWeightReasonMechanismAmbiguous}, {Outcome: IODeviceWeightEvidenceUnavailable, Reason: IODeviceWeightReasonDeviceMissing}}, outcome: IODeviceWeightMechanismAmbiguous, reason: IODeviceWeightReasonMechanismAmbiguous},
		{name: "definitive unsupported dominates unavailable", devices: []IODeviceWeightDeviceCapability{{Outcome: IODeviceWeightUnsupportedMechanism, Reason: IODeviceWeightReasonMechanismUnsupported}, {Outcome: IODeviceWeightEvidenceUnavailable, Reason: IODeviceWeightReasonEvidenceUnavailable}}, outcome: IODeviceWeightUnsupportedMechanism, reason: IODeviceWeightReasonMechanismUnsupported},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome, reason, _ := aggregateIODeviceWeightCapability(test.devices)
			if outcome != test.outcome || reason != test.reason {
				t.Fatalf("aggregate outcome/reason = %s/%s, want %s/%s", outcome, reason, test.outcome, test.reason)
			}
		})
	}
}

func TestIODeviceWeightClassifierKeepsUnavailableEvidenceDistinct(t *testing.T) {
	fixture := newIODeviceWeightClassifierFixture(t)
	readFile := fixture.classifier.io.readFile
	fixture.classifier.io.readFile = func(path string) ([]byte, error) {
		if path == fixture.schedulerPath {
			return nil, os.ErrPermission
		}
		return readFile(path)
	}
	snapshot, err := fixture.classifier.Classify(context.Background(), "8:0")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Outcome() != IODeviceWeightEvidenceUnavailable || snapshot.Reason() != IODeviceWeightReasonEvidenceUnavailable {
		t.Fatalf("outcome/reason = %s/%s, want evidence unavailable", snapshot.Outcome(), snapshot.Reason())
	}
}

func TestIODeviceWeightClassifierTreatsMissingMechanismFactsAsUnsupported(t *testing.T) {
	fixture := newIODeviceWeightClassifierFixture(t)
	fixture.write(fixture.schedulerPath, "[mq-deadline] none\n")
	if err := os.Remove(fixture.ioCostQOSPath); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.classifier.Classify(context.Background(), "8:0")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Outcome() != IODeviceWeightUnsupportedMechanism || snapshot.Reason() != IODeviceWeightReasonMechanismUnsupported {
		t.Fatalf("outcome/reason = %s/%s, want unsupported mechanism", snapshot.Outcome(), snapshot.Reason())
	}
}

func TestIODeviceWeightClassifierSelectsMechanismBeforeControllerMaterialization(t *testing.T) {
	tests := []struct {
		name      string
		scheduler string
		qos       string
		mechanism IODeviceWeightMechanism
	}{
		{name: "BFQ", scheduler: "mq-deadline [bfq] none\n", qos: "8:0 enable=0 ctrl=user\n", mechanism: IODeviceWeightMechanismBFQ},
		{name: "io cost", scheduler: "[mq-deadline] bfq none\n", qos: "8:0 enable=1 ctrl=user\n", mechanism: IODeviceWeightMechanismIOCost},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newIODeviceWeightClassifierFixture(t)
			fixture.write(fixture.schedulerPath, test.scheduler)
			fixture.write(fixture.ioCostQOSPath, test.qos)
			fixture.write(filepath.Join(fixture.classifier.io.cgroupRoot, "cgroup.subtree_control"), "memory pids\n")
			readFile := fixture.classifier.io.readFile
			fixture.classifier.io.readFile = func(path string) ([]byte, error) {
				if filepath.Base(path) == "io.weight" || filepath.Base(path) == "io.bfq.weight" {
					t.Fatalf("classifier read weight interface before probe materialization: %s", path)
				}
				return readFile(path)
			}
			snapshot := fixture.classify(t)
			targets := snapshot.ProbeTargets()
			if len(targets) != 1 || targets[0].Mechanism != test.mechanism {
				t.Fatalf("ProbeTargets() = %+v, want %s candidate", targets, test.mechanism)
			}
		})
	}
}

func TestIODeviceWeightClassifierRequiresRootIOControllerAvailability(t *testing.T) {
	fixture := newIODeviceWeightClassifierFixture(t)
	fixture.write(filepath.Join(fixture.classifier.io.cgroupRoot, "cgroup.controllers"), "cpu memory pids\n")
	snapshot, err := fixture.classifier.Classify(context.Background(), "8:0")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Outcome() != IODeviceWeightUnsupportedMechanism || snapshot.Reason() != IODeviceWeightReasonMechanismUnsupported {
		t.Fatalf("outcome/reason = %s/%s, want unsupported without root io controller", snapshot.Outcome(), snapshot.Reason())
	}
}

func TestIODeviceWeightClassifierRejectsUnsupportedTopologyAndMissingDevices(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ioDeviceWeightClassifierFixture)
		outcome IODeviceWeightCapabilityOutcome
		reason  IODeviceWeightCapabilityReason
	}{
		{name: "partition", mutate: func(f *ioDeviceWeightClassifierFixture) { f.write(filepath.Join(f.sysfsDevice, "partition"), "1\n") }, outcome: IODeviceWeightAmbiguousTopology, reason: IODeviceWeightReasonAmbiguousTopology},
		{name: "device mapper", mutate: func(f *ioDeviceWeightClassifierFixture) { f.mkdir(filepath.Join(f.sysfsDevice, "dm")) }, outcome: IODeviceWeightAmbiguousTopology, reason: IODeviceWeightReasonAmbiguousTopology},
		{name: "multipath", mutate: func(f *ioDeviceWeightClassifierFixture) {
			f.mkdir(filepath.Join(f.sysfsDevice, "dm"))
			f.write(filepath.Join(f.sysfsDevice, "dm", "uuid"), "mpath-test\n")
		}, outcome: IODeviceWeightAmbiguousTopology, reason: IODeviceWeightReasonAmbiguousTopology},
		{name: "RAID", mutate: func(f *ioDeviceWeightClassifierFixture) { f.mkdir(filepath.Join(f.sysfsDevice, "md")) }, outcome: IODeviceWeightAmbiguousTopology, reason: IODeviceWeightReasonAmbiguousTopology},
		{name: "unapproved simple mapping", mutate: func(f *ioDeviceWeightClassifierFixture) { f.write(filepath.Join(f.sysfsDevice, "slaves", "vdb"), "") }, outcome: IODeviceWeightAmbiguousTopology, reason: IODeviceWeightReasonAmbiguousTopology},
		{name: "fan out", mutate: func(f *ioDeviceWeightClassifierFixture) {
			f.write(filepath.Join(f.sysfsDevice, "slaves", "vdb"), "")
			f.write(filepath.Join(f.sysfsDevice, "slaves", "vdc"), "")
		}, outcome: IODeviceWeightAmbiguousTopology, reason: IODeviceWeightReasonAmbiguousTopology},
		{name: "unknown queue", mutate: func(f *ioDeviceWeightClassifierFixture) {
			if err := os.RemoveAll(filepath.Join(f.sysfsDevice, "queue")); err != nil {
				f.t.Fatal(err)
			}
		}, outcome: IODeviceWeightAmbiguousTopology, reason: IODeviceWeightReasonAmbiguousTopology},
		{name: "missing holders topology", mutate: func(f *ioDeviceWeightClassifierFixture) {
			if err := os.Remove(filepath.Join(f.sysfsDevice, "holders")); err != nil {
				f.t.Fatal(err)
			}
		}, outcome: IODeviceWeightAmbiguousTopology, reason: IODeviceWeightReasonAmbiguousTopology},
		{name: "unknown device class", mutate: func(f *ioDeviceWeightClassifierFixture) {
			loop := filepath.Join(f.classifier.io.sysRoot, "devices", "virtual", "block", "loop0")
			f.mkdir(filepath.Join(loop, "queue"))
			f.mkdir(filepath.Join(loop, "slaves"))
			f.mkdir(filepath.Join(loop, "holders"))
			f.write(filepath.Join(loop, "dev"), "8:0\n")
			f.write(filepath.Join(loop, "uevent"), "DEVNAME=loop0\n")
			f.write(filepath.Join(loop, "queue", "scheduler"), "[none]\n")
			f.classifier.io.evalSymlinks = func(string) (string, error) { return loop, nil }
		}, outcome: IODeviceWeightAmbiguousTopology, reason: IODeviceWeightReasonAmbiguousTopology},
		{name: "missing", mutate: func(f *ioDeviceWeightClassifierFixture) {
			if err := os.Remove(f.sysfsLink); err != nil {
				f.t.Fatal(err)
			}
		}, outcome: IODeviceWeightEvidenceUnavailable, reason: IODeviceWeightReasonDeviceMissing},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newIODeviceWeightClassifierFixture(t)
			test.mutate(fixture)
			snapshot, err := fixture.classifier.Classify(context.Background(), "8:0")
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Outcome() != test.outcome || snapshot.Reason() != test.reason {
				t.Fatalf("outcome/reason = %s/%s, want %s/%s", snapshot.Outcome(), snapshot.Reason(), test.outcome, test.reason)
			}
		})
	}
}

func TestApprovedDirectIODeviceFailsClosedForUnknownAndStackedClasses(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		dev   string
		allow bool
	}{
		{name: "virtio", path: "/sys/devices/pci0000:00/0000:00:05.0/virtio2/block/vda", dev: "vda", allow: true},
		{name: "SCSI", path: "/sys/devices/pci0000:00/host0/target0:0:0/0:0:0:0/block/sda", dev: "sda", allow: true},
		{name: "direct NVMe", path: "/sys/devices/pci0000:00/0000:00:04.0/nvme/nvme0/nvme0n1", dev: "nvme0n1", allow: true},
		{name: "loop", path: "/sys/devices/virtual/block/loop0", dev: "loop0"},
		{name: "zram", path: "/sys/devices/virtual/block/zram0", dev: "zram0"},
		{name: "network block", path: "/sys/devices/virtual/block/nbd0", dev: "nbd0"},
		{name: "RBD", path: "/sys/devices/rbd/0/block/rbd0", dev: "rbd0"},
		{name: "native NVMe multipath head", path: "/sys/devices/virtual/nvme-subsystem/nvme-subsys0/nvme0n1", dev: "nvme0n1"},
		{name: "name and path mismatch", path: "/sys/devices/pci0000:00/virtio2/block/vda", dev: "vdb"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := approvedDirectIODevice(test.path, test.dev); got != test.allow {
				t.Fatalf("approvedDirectIODevice(%q, %q) = %t, want %t", test.path, test.dev, got, test.allow)
			}
		})
	}
}

func TestIODeviceWeightClassifierRejectsMultipathAndRAIDHoldersButAcceptsLVM(t *testing.T) {
	tests := []struct {
		name    string
		holder  string
		uuid    string
		raid    bool
		outcome IODeviceWeightCapabilityOutcome
	}{
		{name: "multipath member", holder: "dm-0", uuid: "mpath-test-wwid", outcome: IODeviceWeightAmbiguousTopology},
		{name: "LVM member", holder: "dm-0", uuid: "LVM-test-volume", outcome: IODeviceWeightProbeCandidate},
		{name: "unmodeled device mapper member", holder: "dm-0", uuid: "CRYPT-test-volume", outcome: IODeviceWeightAmbiguousTopology},
		{name: "unmodeled holder", holder: "bcache0", outcome: IODeviceWeightAmbiguousTopology},
		{name: "RAID member", holder: "md0", raid: true, outcome: IODeviceWeightAmbiguousTopology},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newIODeviceWeightClassifierFixture(t)
			holderPath := filepath.Join(fixture.classifier.io.sysRoot, "devices", "virtual", "block", test.holder)
			fixture.mkdir(holderPath)
			if test.raid {
				fixture.mkdir(filepath.Join(holderPath, "md"))
			} else if test.uuid != "" {
				fixture.mkdir(filepath.Join(holderPath, "dm"))
				fixture.write(filepath.Join(holderPath, "dm", "uuid"), test.uuid+"\n")
			}
			if err := os.Symlink(holderPath, filepath.Join(fixture.sysfsDevice, "holders", test.holder)); err != nil {
				t.Fatal(err)
			}
			snapshot, err := fixture.classifier.Classify(context.Background(), "8:0")
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Outcome() != test.outcome {
				t.Fatalf("Outcome() = %s/%s, want %s", snapshot.Outcome(), snapshot.Reason(), test.outcome)
			}
		})
	}
}

func TestIODeviceWeightClassifierConfirmationDetectsRuntimeChanges(t *testing.T) {
	t.Run("unchanged", func(t *testing.T) {
		fixture := newIODeviceWeightClassifierFixture(t)
		before := fixture.classify(t)
		if _, err := fixture.classifier.Confirm(context.Background(), before); err != nil {
			t.Fatalf("Confirm() error = %v", err)
		}
	})
	tests := []struct {
		name   string
		mutate func(*ioDeviceWeightClassifierFixture)
		reason IODeviceWeightCapabilityReason
	}{
		{name: "device number reused", mutate: func(f *ioDeviceWeightClassifierFixture) { f.sysfsInode++ }, reason: IODeviceWeightReasonDeviceIdentityChanged},
		{name: "scheduler changed", mutate: func(f *ioDeviceWeightClassifierFixture) { f.write(f.schedulerPath, "[mq-deadline] bfq none\n") }, reason: IODeviceWeightReasonSchedulerChanged},
		{name: "io cost changed", mutate: func(f *ioDeviceWeightClassifierFixture) {
			f.write(f.ioCostQOSPath, "8:0 enable=1 ctrl=user\n")
		}, reason: IODeviceWeightReasonIOCostChanged},
		{name: "hot unplug", mutate: func(f *ioDeviceWeightClassifierFixture) {
			if err := os.Remove(f.sysfsLink); err != nil {
				f.t.Fatal(err)
			}
		}, reason: IODeviceWeightReasonDeviceMissing},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newIODeviceWeightClassifierFixture(t)
			before := fixture.classify(t)
			test.mutate(fixture)
			_, err := fixture.classifier.Confirm(context.Background(), before)
			var capabilityErr *IODeviceWeightCapabilityError
			if !errors.As(err, &capabilityErr) || capabilityErr.Reason != test.reason {
				t.Fatalf("Confirm() error = %v, want reason %s", err, test.reason)
			}
		})
	}
}

func TestIODeviceWeightClassifierConfirmationIgnoresIrrelevantDiagnostics(t *testing.T) {
	fixture := newIODeviceWeightClassifierFixture(t)
	fixture.write(fixture.schedulerPath, "[mq-deadline] bfq none\n")
	fixture.write(fixture.ioCostQOSPath, "8:0 enable=1 ctrl=user\n")
	before := fixture.classify(t)
	fixture.write(fixture.schedulerPath, "[none] bfq mq-deadline\n")
	fixture.write(fixture.classifier.io.osReleasePath, "ID=custom\nVERSION_ID=99\n")
	fixture.classifier.io.kernelRelease = func() (string, error) { return "99.1-custom", nil }
	if _, err := fixture.classifier.Confirm(context.Background(), before); err != nil {
		t.Fatalf("Confirm() rejected diagnostics irrelevant to active io.cost: %v", err)
	}
}

func TestIODeviceWeightCapabilitySnapshotAccessorsAreDefensive(t *testing.T) {
	fixture := newIODeviceWeightClassifierFixture(t)
	snapshot := fixture.classify(t)
	devices := snapshot.Devices()
	devices[0].Identity.DeviceNode = "/dev/changed"
	targets := snapshot.ProbeTargets()
	targets[0].Identity.DeviceNode = "/dev/changed"
	if snapshot.Devices()[0].Identity.DeviceNode != fixture.deviceNode || snapshot.ProbeTargets()[0].Identity.DeviceNode != fixture.deviceNode {
		t.Fatal("snapshot changed through a returned slice")
	}
}

const defaultIODeviceWeightKernelRelease = "5.14.0-687.el9.x86_64"

type ioDeviceWeightClassifierFixture struct {
	t             *testing.T
	root          string
	classifier    *IODeviceWeightCapabilityClassifier
	sysfsLink     string
	sysfsDevice   string
	deviceNode    string
	schedulerPath string
	ioCostQOSPath string
	sysfsInode    uint64
	deviceMajor   uint32
	deviceMinor   uint32
}

func newIODeviceWeightClassifierFixture(t *testing.T) *ioDeviceWeightClassifierFixture {
	t.Helper()
	root := t.TempDir()
	sysRoot := filepath.Join(root, "sys")
	sysDevBlockRoot := filepath.Join(sysRoot, "dev", "block")
	sysfsDevice := filepath.Join(sysRoot, "devices", "pci0000:00", "0000:00:05.0", "virtio2", "block", "vda")
	devRoot := filepath.Join(root, "dev")
	cgroupRoot := filepath.Join(root, "cgroup")
	fixture := &ioDeviceWeightClassifierFixture{
		t: t, root: root, sysfsLink: filepath.Join(sysDevBlockRoot, "8:0"), sysfsDevice: sysfsDevice,
		deviceNode: filepath.Join(devRoot, "vda"), schedulerPath: filepath.Join(sysfsDevice, "queue", "scheduler"),
		ioCostQOSPath: filepath.Join(cgroupRoot, "io.cost.qos"), sysfsInode: 1001, deviceMajor: 8,
	}
	for _, directory := range []string{sysDevBlockRoot, filepath.Join(sysfsDevice, "queue"), filepath.Join(sysfsDevice, "slaves"), filepath.Join(sysfsDevice, "holders"), devRoot, cgroupRoot, filepath.Join(root, "etc")} {
		fixture.mkdir(directory)
	}
	if err := os.Symlink(filepath.Join("..", "..", "devices", "pci0000:00", "0000:00:05.0", "virtio2", "block", "vda"), fixture.sysfsLink); err != nil {
		t.Fatal(err)
	}
	fixture.write(filepath.Join(sysfsDevice, "dev"), "8:0\n")
	fixture.write(filepath.Join(sysfsDevice, "uevent"), "DEVNAME=vda\n")
	fixture.write(fixture.schedulerPath, "mq-deadline [bfq] none\n")
	fixture.write(filepath.Join(cgroupRoot, "cgroup.controllers"), "cpu io memory\n")
	fixture.write(fixture.ioCostQOSPath, "8:0 enable=0 ctrl=user\n")
	fixture.write(filepath.Join(root, "etc", "os-release"), "ID=ol\nVERSION_ID=9.8\n")
	classifier := NewIODeviceWeightCapabilityClassifier()
	classifier.io.osReleasePath = filepath.Join(root, "etc", "os-release")
	classifier.io.sysRoot = sysRoot
	classifier.io.sysDevBlockRoot = sysDevBlockRoot
	classifier.io.cgroupRoot = cgroupRoot
	classifier.io.devRoot = devRoot
	classifier.io.kernelRelease = func() (string, error) { return defaultIODeviceWeightKernelRelease, nil }
	classifier.io.stat = func(path string) (os.FileInfo, error) {
		if path == fixture.deviceNode {
			return classifierFileInfo{name: "vda", mode: os.ModeDevice, rdev: uint64(unix.Mkdev(fixture.deviceMajor, fixture.deviceMinor))}, nil
		}
		if path == fixture.sysfsDevice {
			return classifierFileInfo{name: "vda", mode: os.ModeDir, inode: fixture.sysfsInode}, nil
		}
		return os.Stat(path)
	}
	fixture.classifier = classifier
	return fixture
}

func (f *ioDeviceWeightClassifierFixture) classify(t *testing.T) IODeviceWeightCapabilitySnapshot {
	t.Helper()
	snapshot, err := f.classifier.Classify(context.Background(), "8:0")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Outcome() != IODeviceWeightProbeCandidate {
		t.Fatalf("Classify() outcome = %s/%s, want probe_candidate", snapshot.Outcome(), snapshot.Reason())
	}
	return snapshot
}

func (f *ioDeviceWeightClassifierFixture) write(path, value string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *ioDeviceWeightClassifierFixture) mkdir(path string) {
	f.t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		f.t.Fatal(err)
	}
}

type classifierFileInfo struct {
	name  string
	mode  os.FileMode
	inode uint64
	rdev  uint64
}

func (i classifierFileInfo) Name() string       { return i.name }
func (i classifierFileInfo) Size() int64        { return 0 }
func (i classifierFileInfo) Mode() os.FileMode  { return i.mode }
func (i classifierFileInfo) ModTime() time.Time { return time.Time{} }
func (i classifierFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i classifierFileInfo) Sys() any           { return &syscall.Stat_t{Ino: i.inode, Rdev: i.rdev} }
