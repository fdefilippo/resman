package state

import (
	"context"

	"github.com/fdefilippo/resman/internal/ioweights"
	"github.com/fdefilippo/resman/internal/systemdunit"
)

type systemdManagerVersionProvider interface {
	ManagerVersion(context.Context) (string, error)
}

type ioDeviceWeightQualificationCoordinate struct {
	distributionID      string
	distributionVersion string
	systemdManager      string
	kernelRelease       string
	mechanism           systemdunit.IODeviceWeightMechanism
	provenance          ioweights.EffectQualificationProvenance
}

var ioDeviceWeightQualificationCoordinates = []ioDeviceWeightQualificationCoordinate{
	{
		distributionID:      "ol",
		distributionVersion: "9.8",
		systemdManager:      "252 (252-67.0.1.el9_8.2)",
		kernelRelease:       "5.14.0-687.46.1.el9_8.x86_64",
		mechanism:           systemdunit.IODeviceWeightMechanismBFQ,
		provenance:          ioweights.EffectQualificationOL9RHCK20260915,
	},
	{
		distributionID:      "ol",
		distributionVersion: "9.8",
		systemdManager:      "252 (252-67.0.1.el9_8.2)",
		kernelRelease:       "5.14.0-687.46.1.el9_8.x86_64",
		mechanism:           systemdunit.IODeviceWeightMechanismIOCost,
		provenance:          ioweights.EffectQualificationOL9RHCK20260915,
	},
}

func ioDeviceWeightEffectQualification(snapshot systemdunit.IODeviceWeightCapabilitySnapshot, systemdManager string) ioweights.EffectQualificationProvenance {
	platform := snapshot.Platform()
	provenance := ioweights.EffectQualificationNone
	devices := snapshot.Devices()
	if len(devices) == 0 {
		return provenance
	}
	for _, device := range devices {
		matched := ioweights.EffectQualificationNone
		for _, coordinate := range ioDeviceWeightQualificationCoordinates {
			if platform.DistributionID == coordinate.distributionID &&
				platform.DistributionVersion == coordinate.distributionVersion &&
				systemdManager == coordinate.systemdManager &&
				platform.KernelRelease == coordinate.kernelRelease &&
				device.Mechanism == coordinate.mechanism {
				matched = coordinate.provenance
				break
			}
		}
		if matched == ioweights.EffectQualificationNone ||
			(provenance != ioweights.EffectQualificationNone && provenance != matched) {
			return ioweights.EffectQualificationNone
		}
		provenance = matched
	}
	return provenance
}

func (m *Manager) publishIODeviceWeightEffectQualification(ctx context.Context, snapshot systemdunit.IODeviceWeightCapabilitySnapshot) {
	provenance := ioweights.EffectQualificationNone
	systemdManager := ""
	provider, ok := m.systemdIOWeights.(systemdManagerVersionProvider)
	if ok {
		var err error
		if systemdManager, err = provider.ManagerVersion(ctx); err == nil {
			provenance = ioDeviceWeightEffectQualification(snapshot, systemdManager)
		} else if m.logger != nil {
			m.logger.Debug("Weighted I/O effect qualification provenance unavailable", "error", err)
		}
	}
	m.mu.Lock()
	changed := m.ioWeightStatus.EffectQualificationProvenance != provenance
	m.ioWeightStatus.EffectQualificationProvenance = provenance
	m.ioWeightStatus.EffectQualified = provenance != ioweights.EffectQualificationNone
	m.mu.Unlock()
	if changed && m.logger != nil {
		platform := snapshot.Platform()
		m.logger.Info("Weighted I/O effect qualification changed",
			"effect_qualified", provenance != ioweights.EffectQualificationNone,
			"provenance", provenance,
			"distribution_id", platform.DistributionID,
			"distribution_version", platform.DistributionVersion,
			"systemd_manager", systemdManager,
			"kernel_release", platform.KernelRelease,
		)
	}
}
