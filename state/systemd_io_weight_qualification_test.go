package state

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/fdefilippo/resman/internal/ioweights"
	"github.com/fdefilippo/resman/internal/systemdunit"
)

func qualifiedIODeviceWeightCandidate(t *testing.T, mechanisms ...systemdunit.IODeviceWeightMechanism) systemdunit.IODeviceWeightCapabilitySnapshot {
	t.Helper()
	devices := make([]systemdunit.IODeviceWeightDeviceCapability, 0, len(mechanisms))
	selector := "8:0"
	for index, mechanism := range mechanisms {
		number := systemdunit.IODeviceWeightDeviceNumber{Major: 8, Minor: uint32(index)}
		if index > 0 {
			selector += ",8:" + string(rune('0'+index))
		}
		devices = append(devices, systemdunit.IODeviceWeightDeviceCapability{
			Identity: systemdunit.IODeviceWeightDeviceIdentity{Number: number, DeviceNode: "/dev/vd" + string(rune('a'+index)), SysfsPath: "/sys/devices/vd" + string(rune('a'+index)), SysfsInode: uint64(10 + index)},
			Outcome:  systemdunit.IODeviceWeightProbeCandidate, Mechanism: mechanism,
		})
	}
	snapshot, err := systemdunit.NewIODeviceWeightProbeCandidateSnapshot(selector, systemdunit.IODeviceWeightPlatformIdentity{
		DistributionID: "ol", DistributionVersion: "9.8", DistributionMajor: 9,
		KernelSeries: "5.14", KernelRelease: "5.14.0-687.46.1.el9_8.x86_64",
	}, devices)
	if err != nil {
		t.Fatalf("NewIODeviceWeightProbeCandidateSnapshot() error = %v", err)
	}
	return snapshot
}

func TestIODeviceWeightEffectQualificationRequiresEveryExactCoordinate(t *testing.T) {
	managerVersion := "252-67.0.1.el9_8.2"
	for _, tt := range []struct {
		name       string
		mechanisms []systemdunit.IODeviceWeightMechanism
		want       ioweights.EffectQualificationProvenance
	}{
		{name: "retained BFQ coordinate", mechanisms: []systemdunit.IODeviceWeightMechanism{systemdunit.IODeviceWeightMechanismBFQ}, want: ioweights.EffectQualificationOL9RHCK20260915},
		{name: "unqualified io.cost", mechanisms: []systemdunit.IODeviceWeightMechanism{systemdunit.IODeviceWeightMechanismIOCost}, want: ioweights.EffectQualificationNone},
		{name: "mixed qualified and unqualified mechanisms", mechanisms: []systemdunit.IODeviceWeightMechanism{systemdunit.IODeviceWeightMechanismBFQ, systemdunit.IODeviceWeightMechanismIOCost}, want: ioweights.EffectQualificationNone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ioDeviceWeightEffectQualification(qualifiedIODeviceWeightCandidate(t, tt.mechanisms...), managerVersion); got != tt.want {
				t.Fatalf("qualification for %v = %q, want %q", tt.mechanisms, got, tt.want)
			}
		})
	}

	snapshot := qualifiedIODeviceWeightCandidate(t, systemdunit.IODeviceWeightMechanismBFQ)
	if got := ioDeviceWeightEffectQualification(snapshot, "252 (252-67.0.1.el9_8.2)"); got != ioweights.EffectQualificationNone {
		t.Fatalf("mismatched Manager version qualified as %q", got)
	}
	platform := snapshot.Platform()
	platform.KernelRelease = "5.14.0-other"
	device := snapshot.Devices()[0]
	unmatched, err := systemdunit.NewIODeviceWeightProbeCandidateSnapshot("8:0", platform, []systemdunit.IODeviceWeightDeviceCapability{device})
	if err != nil {
		t.Fatal(err)
	}
	if got := ioDeviceWeightEffectQualification(unmatched, managerVersion); got != ioweights.EffectQualificationNone {
		t.Fatalf("mismatched kernel qualified as %q", got)
	}
}

func TestIODeviceWeightQualificationCoordinatesHaveValidatedEvidence(t *testing.T) {
	repositoryRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	validator := filepath.Join(repositoryRoot, "test", "functional", "io-device-weight-effect", "validate_evidence.py")
	for _, coordinate := range ioDeviceWeightQualificationCoordinates {
		evidence := filepath.Join(repositoryRoot, "test", "functional", "io-device-weight-effect", "evidence", string(coordinate.provenance), "remote", "guest")
		command := exec.Command("python3", validator, evidence,
			"--mechanism", string(coordinate.mechanism),
			"--provenance", string(coordinate.provenance),
			"--distribution-id", coordinate.distributionID,
			"--distribution-version", coordinate.distributionVersion,
			"--manager-version", coordinate.systemdManager,
			"--kernel-release", coordinate.kernelRelease,
			"--revision", coordinate.sourceRevision,
			"--source-tree", coordinate.sourceTree,
			"--package-identity", coordinate.packageIdentity,
			"--package-sha", coordinate.packageSHA256)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("qualification coordinate %q/%q lacks accepted retained evidence: %v\n%s", coordinate.provenance, coordinate.mechanism, err, output)
		}
	}
}

func TestIODeviceWeightFunctionalAcceptancePublishesExactQualificationProvenance(t *testing.T) {
	manager := testSystemdCPUPointsManager(t, testCPUPointsPolicy(t, nil), &fakeSystemdCPUUnitAdapter{}, &forbiddenSystemdNativeCgroupManager{}, 4)
	adapter := &fakeSystemdCPUUnitAdapter{
		topology:              testSystemdTopology(1000),
		systemdManagerVersion: "252-67.0.1.el9_8.2",
	}
	manager.cfg.IOWeightDevices = "8:0"
	classifier := &fakeIODeviceWeightClassifier{snapshot: qualifiedIODeviceWeightCandidate(t, systemdunit.IODeviceWeightMechanismBFQ)}
	if err := WithSystemdIODeviceWeights(adapter, classifier, testIODeviceWeightPolicy(t))(manager); err != nil {
		t.Fatal(err)
	}

	result := manager.AttemptIODeviceWeightCapability(context.Background())
	if !result.Status.EffectQualified || result.Status.EffectQualificationProvenance != ioweights.EffectQualificationOL9RHCK20260915 {
		t.Fatalf("qualification status = %+v", result.Status)
	}

	classifier.snapshot = testIODeviceWeightObservation(t, systemdunit.IODeviceWeightMechanismAmbiguous, systemdunit.IODeviceWeightReasonMechanismAmbiguous)
	classifier.confirmErr = &systemdunit.IODeviceWeightCapabilityError{Reason: systemdunit.IODeviceWeightReasonMechanismAmbiguous}
	result = manager.AttemptIODeviceWeightCapability(context.Background())
	if result.Status.EffectQualified || result.Status.EffectQualificationProvenance != ioweights.EffectQualificationNone {
		t.Fatalf("refused status retained qualification = %+v", result.Status)
	}
}
