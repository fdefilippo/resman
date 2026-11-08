package state

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/blockiops"
	"github.com/fdefilippo/resman/internal/systemdunit"
)

type blockIOPSRuntime struct {
	source   blockiops.Source
	previous map[int]blockiops.Sample
}

func needsBlockIOPS(cfg *config.Config) bool {
	p := cfg.GetIODecisionPolicy()
	return p.Enabled && (p.ReadIOPS > 0 || p.WriteIOPS > 0)
}

// WithBlockIOPSRuntime enables mandatory kernel-source preparation in production.
// Tests can supply a Source factory without attaching to the developer kernel.
func WithBlockIOPSRuntime() ManagerOption {
	return func(m *Manager) error {
		m.blockIOPSFactory = func(cfg *config.Config) (blockiops.Source, error) { return blockiops.NewKernelSource(1, cfg) }
		runtime, err := m.newBlockIOPSRuntime(m.cfg)
		if err != nil {
			if m.systemdUnits != nil {
				return errors.Join(err, m.deactivateLimits())
			}
			return err
		}
		m.blockIOPS = runtime
		return nil
	}
}

func (m *Manager) newBlockIOPSRuntime(cfg *config.Config) (*blockIOPSRuntime, error) {
	if !needsBlockIOPS(cfg) || m.blockIOPSFactory == nil {
		return nil, nil
	}
	if m.systemdUnits == nil {
		return nil, fmt.Errorf("IO_READ_IOPS/IO_WRITE_IOPS require authoritative systemd user slices")
	}
	source, err := m.blockIOPSFactory(cfg)
	if err != nil {
		return nil, fmt.Errorf("prepare mandatory block-IOPS source: %w", err)
	}
	return &blockIOPSRuntime{source: source, previous: make(map[int]blockiops.Sample)}, nil
}

// PrepareBlockIOPSConfig proves a reload candidate before any configuration is
// published. The epoch barrier excludes cycles; UpdateConfig selects the staged
// lifetime. Both old and candidate sources remain available for atomic rollback.
func (m *Manager) PrepareBlockIOPSConfig(cfg *config.Config) (func(), error) {
	leave := m.opGate.Enter()
	defer leave()
	oldCfg := m.GetConfig()
	old := m.blockIOPS
	runtime := old
	reuse := needsBlockIOPS(oldCfg) == needsBlockIOPS(cfg) && slices.Equal(oldCfg.GetProcessExcludeList(), cfg.GetProcessExcludeList()) && oldCfg.GetIODeviceFilter() == cfg.GetIODeviceFilter()
	if !reuse {
		var err error
		runtime, err = m.newBlockIOPSRuntime(cfg)
		if err != nil {
			return nil, err
		}
	}
	m.blockIOPSPrepared = map[*config.Config]*blockIOPSRuntime{oldCfg: old, cfg: runtime}
	return func() {
		finish := m.opGate.Enter()
		defer finish()
		for _, r := range []*blockIOPSRuntime{old, runtime} {
			if r != nil && r != m.blockIOPS {
				if err := r.source.Close(); err != nil {
					m.logger.Error("Close retired block-IOPS source", "error", err)
				}
			}
		}
		m.blockIOPSPrepared = nil
		if m.blockIOPS == nil {
			if err := m.restoreBlockIOPSAccounting(); err != nil {
				m.logger.Error("Restore block-IOPS accounting", "error", err)
			}
		}
	}, nil
}

func (m *Manager) restoreBlockIOPSAccounting() error {
	var errs []error
	for uid, id := range m.blockIOPSAccounting {
		result, err := m.systemdUnits.RestoreProperties(context.Background(), id, []systemdunit.PropertyName{systemdunit.PropertyIOAccounting})
		if len(result.Conflicts) > 0 {
			err = errors.Join(err, fmt.Errorf("restore IOPS accounting for UID %d: %d ownership conflicts", uid, len(result.Conflicts)))
		}
		if err != nil {
			errs = append(errs, err)
		} else {
			delete(m.blockIOPSAccounting, uid)
		}
	}
	return errors.Join(errs...)
}

// collectEligibleBlockIOPS runs only on the decision stream. Whole-user slice
// authority is required; unknown membership is never substituted with an idle
// device. Each device retains its own bounds, avoiding stacked-device sums.
func (m *Manager) collectEligibleBlockIOPS(sample *SystemMetrics, policy config.IODecisionPolicy) error {
	if !policy.Enabled || (policy.ReadIOPS == 0 && policy.WriteIOPS == 0) {
		return nil
	}
	runtime := m.blockIOPS
	if runtime == nil {
		sample.IOBlockIOPSUnavailableUsers = len(sample.IOEligibleUsers)
		return nil
	}
	if err := runtime.source.Health(); err != nil {
		// A failed mandatory source must not pin previously applied CPU/RAM limits.
		// Restore all owned enforcement instead of fabricating an IOPS zero.
		return errors.Join(fmt.Errorf("mandatory block-IOPS source failed: %w", err), m.deactivateLimits())
	}
	sample.IOBlockIOPSSourceAvailable = true
	devices, err := m.resolveBlockIOPSDevices(m.GetConfig().GetIODeviceFilter())
	if err != nil {
		return errors.Join(fmt.Errorf("resolve IOPS decision devices: %w", err), m.deactivateLimits())
	}
	topology, err := m.systemdUnits.Discover(context.Background())
	if err != nil {
		sample.IOBlockIOPSUnavailableUsers = len(sample.IOEligibleUsers)
		return nil
	}
	units := make(map[int]systemdunit.UnitIdentity, len(topology.Users))
	for _, u := range topology.Users {
		units[int(u.UID)] = u.Unit.Identity
	}
	if m.blockIOPSAccounting == nil {
		m.blockIOPSAccounting = make(map[int]systemdunit.UnitIdentity)
	}
	next := make(map[int]blockiops.Sample)
	sample.IOBlockIOPSDevices = make(map[blockiops.Device]blockIOPSDeviceBounds, len(devices))
	for _, d := range devices {
		sample.IOBlockIOPSDevices[d] = blockIOPSDeviceBounds{}
	}
	assignment, _ := systemdunit.NewPropertyAssignment(systemdunit.PropertyIOAccounting, 1)
	allIntervals := make([]blockiops.Interval, 0, len(devices)*len(sample.IOEligibleUsers))
	for _, uid := range sample.IOEligibleUsers {
		id, exists := units[uid]
		var observation systemdunit.ProcessAuthorityObservation
		if exists && sample.systemdAuthorityInventory != nil {
			observation, exists = sample.systemdAuthorityInventory.Observation(uint32(uid), id)
		} else {
			exists = false
		}
		if !exists || observation.ResourceError != nil || observation.ResourceAuthority.State != systemdunit.ResourceCoverageComplete {
			sample.IOBlockIOPSUnavailableUsers++
			continue
		}
		if _, err := m.systemdUnits.Apply(context.Background(), id, []systemdunit.PropertyAssignment{assignment}); err != nil {
			sample.IOBlockIOPSUnavailableUsers++
			continue
		}
		m.blockIOPSAccounting[uid] = id
		identity, err := runtime.source.Register(uint32(uid), id.InvocationIDString(), id.ControlGroupID)
		if err != nil {
			return errors.Join(err, m.deactivateLimits())
		}
		current, err := runtime.source.Read(identity, devices)
		if err != nil {
			return errors.Join(err, m.deactivateLimits())
		}
		if _, err := m.systemdUnits.ConfirmApplied(context.Background(), id, []systemdunit.PropertyAssignment{assignment}); err != nil {
			sample.IOBlockIOPSUnavailableUsers++
			continue
		}
		next[uid] = current
		previous, exists := runtime.previous[uid]
		if !exists {
			sample.IOBlockIOPSUnavailableUsers++
			continue
		}
		intervals := make([]blockiops.Interval, 0, len(devices))
		for _, d := range devices {
			interval, err := blockiops.Between(previous, current, d)
			if err != nil {
				intervals = nil
				break
			}
			intervals = append(intervals, interval)
		}
		if len(intervals) != len(devices) {
			sample.IOBlockIOPSUnavailableUsers++
			continue
		}
		allIntervals = append(allIntervals, intervals...)
	}
	identities := make([]blockiops.Identity, 0, len(next))
	for _, sample := range next {
		identities = append(identities, sample.Identity)
	}
	if err := runtime.source.Retain(identities); err != nil {
		return errors.Join(err, m.deactivateLimits())
	}
	runtime.previous = next
	aggregate, err := blockiops.Aggregate(allIntervals)
	if err != nil {
		sample.IOBlockIOPSUnavailableUsers = len(sample.IOEligibleUsers)
	} else {
		for device, bounds := range aggregate {
			sample.IOBlockIOPSDevices[device] = blockIOPSDeviceBounds(bounds)
		}
	}
	// Counters for retired units never become another unit's baseline.
	for uid, id := range m.blockIOPSAccounting {
		if _, exists := next[uid]; exists {
			continue
		}
		result, err := m.systemdUnits.RestoreProperties(context.Background(), id, []systemdunit.PropertyName{systemdunit.PropertyIOAccounting})
		if len(result.Conflicts) > 0 {
			err = errors.Join(err, fmt.Errorf("restore retired IOPS accounting: %d ownership conflicts", len(result.Conflicts)))
		}
		if err != nil {
			m.logger.Warn("Restore retired IOPS accounting", "uid", uid, "error", err)
		} else {
			delete(m.blockIOPSAccounting, uid)
		}
	}
	return nil
}

type blockIOPSDeviceBounds struct {
	Read, Write blockiops.RateBounds
	Uncertain   bool
}

// resetBlockIOPSDecisionBaseline is called under the operation gate on a policy
// epoch change. Observation refreshes do not call it or advance source baselines.
func (m *Manager) resetBlockIOPSDecisionBaseline() {
	if m.blockIOPS != nil {
		m.blockIOPS.previous = make(map[int]blockiops.Sample)
	}
	m.blockIOPSLast = nil
}

// blockIOPSDecisionProjection is cached evidence, never an observation baseline.
// All accesses are ordered by the manager operation gate.
type blockIOPSDecisionProjection struct {
	available   bool
	observedAt  time.Time
	unavailable int
	devices     map[blockiops.Device]blockIOPSDeviceBounds
}

func (m *Manager) rememberBlockIOPSDecision(sample *SystemMetrics) {
	sample.IOBlockIOPSDecisionAt = sample.Timestamp
	if sample.IOBlockIOPSDecisionAt.IsZero() {
		sample.IOBlockIOPSDecisionAt = time.Now()
	}
	m.blockIOPSLast = &blockIOPSDecisionProjection{available: sample.IOBlockIOPSSourceAvailable, observedAt: sample.IOBlockIOPSDecisionAt, unavailable: sample.IOBlockIOPSUnavailableUsers, devices: cloneBlockIOPSBounds(sample.IOBlockIOPSDevices)}
}
func (m *Manager) projectBlockIOPSDecision(sample *SystemMetrics) {
	if m.blockIOPSLast == nil {
		if needsBlockIOPS(m.GetConfig()) {
			sample.IOBlockIOPSUnavailableUsers = len(sample.IOEligibleUsers)
		}
		return
	}
	sample.IOBlockIOPSSourceAvailable = m.blockIOPSLast.available
	sample.IOBlockIOPSDecisionAt = m.blockIOPSLast.observedAt
	sample.IOBlockIOPSUnavailableUsers = m.blockIOPSLast.unavailable
	sample.IOBlockIOPSDevices = cloneBlockIOPSBounds(m.blockIOPSLast.devices)
}
func cloneBlockIOPSBounds(input map[blockiops.Device]blockIOPSDeviceBounds) map[blockiops.Device]blockIOPSDeviceBounds {
	if input == nil {
		return nil
	}
	result := make(map[blockiops.Device]blockIOPSDeviceBounds, len(input))
	for d, b := range input {
		result[d] = b
	}
	return result
}
