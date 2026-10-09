package systemdunit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// CapabilityRefusal is the typed outcome of a host that cannot provide a
// mandatory enforcement capability at all. It is produced only for a
// structural absence, never for a probe that could not run, and it reports
// what ResMan released before giving up enforcement.
type CapabilityRefusal struct {
	Capability MissingCapability
	Released   ReleaseReport
	Err        error
}

func (r *CapabilityRefusal) Error() string {
	return fmt.Sprintf("mandatory capability unavailable: %v", r.Err)
}

// Unwrap exposes the refusal behind the typed outcome.
func (r *CapabilityRefusal) Unwrap() error { return r.Err }

// ReleaseReport counts what one release of owned properties restored.
type ReleaseReport struct {
	Units      int
	Properties int
}

// ReleaseOwnedLeases restores every property ResMan still owns, unit by unit,
// with the same compare-before-restore discipline used by a normal release. A
// property changed outside ResMan is preserved and reported as a conflict,
// because overwriting an operator's value is never a recovery.
//
// It exists so that a daemon which is about to stop enforcing does not leave
// applied limits behind with no owner.
func (a *Adapter) ReleaseOwnedLeases(ctx context.Context) (ReleaseReport, error) {
	var report ReleaseReport
	identities := a.OwnedUnits()
	sort.Slice(identities, func(left, right int) bool {
		return identities[left].Name < identities[right].Name
	})
	for _, identity := range identities {
		owned := false
		for _, lease := range a.Leases(identity) {
			if lease.Active() {
				owned = true
				break
			}
		}
		if !owned {
			continue
		}
		result, err := a.Restore(ctx, identity)
		report.Units++
		report.Properties += len(result.Restored)
		if err != nil {
			return report, fmt.Errorf("release owned properties of %s: %w", identity.Name, err)
		}
	}
	return report, nil
}

// NewOrObserve opens the adapter when every mandatory capability is available.
//
// On a structural refusal it releases every property ResMan still owns and
// returns a typed CapabilityRefusal with no adapter and no error, so that the
// caller may publish a truthful observation-only state instead of enforcing.
// Every other failure, including a probe that could not run, an unavailable bus
// and a timeout, is returned as an error: a transient fault must never remove
// enforcement. A refusal whose owned properties cannot be released safely is
// also an error, because applied limits with no owner are worse than a daemon
// that refuses to start.
func NewOrObserve(ctx context.Context, cgroupRoot string, timeout time.Duration, requirements StartupRequirements) (*Adapter, *CapabilityRefusal, error) {
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	adapter, err := newSystemAdapter(ctx, cgroupRoot, timeout)
	if err != nil {
		return nil, nil, err
	}
	refusal, err := adapter.resolveStartupCapabilities(ctx, requirements)
	if err != nil || refusal != nil {
		adapter.Close()
		return nil, refusal, err
	}
	return adapter, nil, nil
}

// resolveStartupCapabilities requires every mandatory capability and classifies
// what the host answered. A structural refusal releases owned properties and is
// reported as a typed outcome; every other failure stays an error.
func (a *Adapter) resolveStartupCapabilities(ctx context.Context, requirements StartupRequirements) (*CapabilityRefusal, error) {
	capabilityErr := a.requireStartupCapabilities(ctx, requirements)
	if capabilityErr == nil {
		return nil, nil
	}
	capability, definitive := MissingCapabilityFromError(capabilityErr)
	if !definitive {
		return nil, capabilityErr
	}
	released, releaseErr := a.ReleaseOwnedLeases(ctx)
	if releaseErr != nil {
		return nil, fmt.Errorf(
			"release owned properties before observation after %w: %w", capabilityErr, releaseErr)
	}
	return &CapabilityRefusal{Capability: capability, Released: released, Err: capabilityErr}, nil
}

// DurableLeasesPresent reports whether an earlier enforcing run left durable
// property ownership on this host. It reads the journal only, so a host that
// never enforced needs no system bus to answer, and an unreadable journal is an
// error rather than a silent no.
func DurableLeasesPresent() (bool, error) {
	return durableLeasesPresent(newFileLeaseJournalStoreForOwner(DefaultLeaseJournalPath, 0))
}

func durableLeasesPresent(store leaseJournalStore) (bool, error) {
	journal, err := store.Load()
	if err != nil {
		return false, err
	}
	for _, unit := range journal.Units {
		if len(unit.Properties) != 0 {
			return true, nil
		}
	}
	return false, nil
}

// ReleaseForObservation releases every property an earlier enforcing run still
// owns, without requiring any enforcement capability. A host declared
// observation-only calls it so that limits applied before the declaration do
// not remain applied with no owner. It installs nothing and applies no limit.
func ReleaseForObservation(ctx context.Context, cgroupRoot string, timeout time.Duration) (ReleaseReport, error) {
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	adapter, err := newSystemAdapter(ctx, cgroupRoot, timeout)
	if err != nil {
		return ReleaseReport{}, err
	}
	defer adapter.Close()
	return adapter.ReleaseOwnedLeases(ctx)
}

// IsCapabilityRefusal reports whether err carries a typed structural refusal of
// a mandatory enforcement capability.
func IsCapabilityRefusal(err error) bool {
	var refusal *CapabilityRefusal
	return errors.As(err, &refusal)
}
