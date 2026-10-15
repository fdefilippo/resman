package systemdunit

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// applyCPUWeights leases CPUWeight on every UID and returns the identities in
// UID order.
func applyCPUWeights(t *testing.T, adapter *Adapter, uids ...uint32) []UnitIdentity {
	t.Helper()
	identities := make([]UnitIdentity, len(uids))
	for index, uid := range uids {
		identities[index] = identityFor(t, adapter, uid)
		if _, err := adapter.Apply(context.Background(), identities[index], []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)}); err != nil {
			t.Fatalf("Apply(%s) error = %v", identities[index].Name, err)
		}
	}
	return identities
}

func unitNames(identities []UnitIdentity) []string {
	names := make([]string, len(identities))
	for index, identity := range identities {
		names[index] = identity.Name
	}
	return names
}

// TestRestoreUnitsSharesOneReloadAcrossUnits pins resman-gya: releasing CPU
// Points on vmgclalpr2400 restored 47 slices with one systemd manager reload
// each and took 19 s. A batch reverts every unit and reloads systemd once,
// with one durable restoring intent, one reloading intent and one lease removal.
func TestRestoreUnitsSharesOneReloadAcrossUnits(t *testing.T) {
	tests := []struct {
		name        string
		uids        []uint32
		wantReloads int
	}{
		{name: "empty batch", uids: nil, wantReloads: 0},
		{name: "single unit", uids: []uint32{1001}, wantReloads: 1},
		{name: "many units", uids: []uint32{1001, 1002, 1003, 1004, 1005}, wantReloads: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := newFakeUnitTransport(tt.uids...)
			store := newMemoryLeaseJournalStore()
			adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
			identities := applyCPUWeights(t, adapter, tt.uids...)
			savesBefore := store.saveCalls

			outcomes := adapter.RestoreUnits(context.Background(), identities)

			if len(outcomes) != len(identities) {
				t.Fatalf("outcomes = %d, want %d", len(outcomes), len(identities))
			}
			for index, outcome := range outcomes {
				if outcome.Identity != identities[index] || outcome.Err != nil {
					t.Fatalf("outcome %d = %+v, want %s without error", index, outcome, identities[index].Name)
				}
				if !reflect.DeepEqual(outcome.Result.Restored, []PropertyName{PropertyCPUWeight}) {
					t.Fatalf("%s restored = %v, want CPUWeight", outcome.Identity.Name, outcome.Result.Restored)
				}
				if got := transport.units[outcome.Identity.Name].slice[string(PropertyCPUWeight)]; got != uint64(SystemdUnset) {
					t.Fatalf("%s CPUWeight = %v, want baseline", outcome.Identity.Name, got)
				}
			}
			if transport.reloadCalls != tt.wantReloads {
				t.Fatalf("manager reloads = %d, want %d", transport.reloadCalls, tt.wantReloads)
			}
			if want := unitNames(identities); len(want) != 0 && !reflect.DeepEqual(transport.revertCalls, want) {
				t.Fatalf("RevertUnitFiles calls = %v, want %v", transport.revertCalls, want)
			}
			// Each unit keeps its own two-save baseline transaction; the
			// restoring intent, the reloading intent and the removal are shared.
			wantSaves := 0
			if len(identities) != 0 {
				wantSaves = 2*len(identities) + 3
			}
			if got := store.saveCalls - savesBefore; got != wantSaves {
				t.Fatalf("journal saves = %d, want %d", got, wantSaves)
			}
			if len(store.journal.Units) != 0 || len(adapter.OwnedUnits()) != 0 {
				t.Fatalf("leases remained after the batch: journal=%+v owned=%v", store.journal.Units, adapter.OwnedUnits())
			}
		})
	}
}

func TestRestoreUnitsRejectsADuplicateWithoutRestoringItTwice(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, newMemoryLeaseJournalStore())
	identity := applyCPUWeights(t, adapter, 1001)[0]

	outcomes := adapter.RestoreUnits(context.Background(), []UnitIdentity{identity, identity})
	if outcomes[0].Err != nil {
		t.Fatalf("first outcome error = %v", outcomes[0].Err)
	}
	if outcomes[1].Err == nil {
		t.Fatal("duplicate unit restored without error")
	}
	if len(transport.revertCalls) != 1 || transport.reloadCalls != 1 {
		t.Fatalf("revert=%v reloads=%d, want one restoration", transport.revertCalls, transport.reloadCalls)
	}
}

// TestRestoreUnitsIsolatesAFailedUnit proves that one unit failing a step of
// the shared stage neither blocks nor fails the other units, and that the
// failed unit keeps a durable lease in the phase startup recovery resolves.
func TestRestoreUnitsIsolatesAFailedUnit(t *testing.T) {
	tests := []struct {
		name      string
		failure   func(*fakeUnitTransport, string)
		wantPhase leasePhase
	}{
		{
			name: "revert fails",
			failure: func(transport *fakeUnitTransport, failing string) {
				transport.onRevert = func(f *fakeUnitTransport, unit string) {
					f.revertErr = nil
					if unit == failing {
						f.revertErr = errors.New("RevertUnitFiles refused")
					}
				}
			},
			wantPhase: leasePhaseRestoring,
		},
		{
			name: "readback after reload differs",
			failure: func(transport *fakeUnitTransport, failing string) {
				transport.onReload = func(f *fakeUnitTransport) {
					f.units[failing].slice[string(PropertyCPUWeight)] = uint64(555)
				}
			},
			wantPhase: leasePhaseReloading,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := newFakeUnitTransport(1001, 1002, 1003)
			store := newMemoryLeaseJournalStore()
			adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
			identities := applyCPUWeights(t, adapter, 1001, 1002, 1003)
			failing := identities[1].Name
			tt.failure(transport, failing)

			outcomes := adapter.RestoreUnits(context.Background(), identities)

			for _, outcome := range outcomes {
				if (outcome.Err != nil) != (outcome.Identity.Name == failing) {
					t.Fatalf("%s error = %v, want an error only for %s", outcome.Identity.Name, outcome.Err, failing)
				}
			}
			if transport.reloadCalls != 1 {
				t.Fatalf("manager reloads = %d, want 1", transport.reloadCalls)
			}
			if len(store.journal.Units) != 1 || store.journal.Units[0].Unit != failing {
				t.Fatalf("durable leases = %+v, want only %s", store.journal.Units, failing)
			}
			if got := store.journal.Units[0].Phase; got != tt.wantPhase {
				t.Fatalf("failed unit phase = %q, want %q", got, tt.wantPhase)
			}
		})
	}
}

// TestRestoreUnitsDoesNotRevertBeforeTheSharedIntentIsDurable keeps the
// write-ahead order of a single-unit restore for the whole batch.
func TestRestoreUnitsDoesNotRevertBeforeTheSharedIntentIsDurable(t *testing.T) {
	transport := newFakeUnitTransport(1001, 1002, 1003)
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identities := applyCPUWeights(t, adapter, 1001, 1002, 1003)
	// Two saves per unit program the baselines; the next one is the shared
	// restoring intent.
	store.saveErr = errors.New("restoring intent unavailable")
	store.failSave = store.saveCalls + 2*len(identities) + 1

	outcomes := adapter.RestoreUnits(context.Background(), identities)

	for _, outcome := range outcomes {
		assertAdapterReason(t, outcome.Err, ReasonLeaseStore)
	}
	if len(transport.revertCalls) != 0 || transport.reloadCalls != 0 {
		t.Fatalf("revert=%v reloads=%d before a durable restoring intent", transport.revertCalls, transport.reloadCalls)
	}
	for _, unit := range store.journal.Units {
		if unit.Phase != leasePhaseApplied {
			t.Fatalf("%s durable phase = %q, want applied", unit.Unit, unit.Phase)
		}
	}
}

// TestStartupRecoversABatchInterruptedBeforeItsReload covers the journal states
// a batch can leave behind that a single-unit restore never did all at once:
// some units reverted and waiting for the reload, others still holding their
// drop-ins. A restarted adapter must reclaim every one of them.
func TestStartupRecoversABatchInterruptedBeforeItsReload(t *testing.T) {
	uids := []uint32{1001, 1002, 1003}
	transport := newFakeUnitTransport(uids...)
	store := newMemoryLeaseJournalStore()
	first := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identities := applyCPUWeights(t, first, uids...)
	reverted := identities[0].Name

	transport.skipRevertEffect = true
	transport.diskPaths = map[string][]string{}
	transport.onRevert = func(f *fakeUnitTransport, unit string) {
		// Real systemd removes the files but keeps DropInPaths and normalized
		// property values stale until Reload completes.
		f.revertErr = nil
		if unit == reverted {
			f.diskPaths[unit] = nil
			return
		}
		f.revertErr = errors.New("simulated crash during the revert stage")
	}
	transport.onReload = func(f *fakeUnitTransport) {
		for _, identity := range identities {
			if paths, removed := f.diskPaths[identity.Name]; removed && len(paths) == 0 {
				f.units[identity.Name].unit["DropInPaths"] = []string{}
				f.units[identity.Name].slice[string(PropertyCPUWeight)] = uint64(SystemdUnset)
			}
		}
	}
	transport.reloadErr = errors.New("simulated crash before Reload completes")
	for _, outcome := range first.RestoreUnits(context.Background(), identities) {
		if outcome.Err == nil {
			t.Fatalf("%s restored despite the interrupted batch", outcome.Identity.Name)
		}
	}
	phases := make(map[string]leasePhase, len(store.journal.Units))
	for _, unit := range store.journal.Units {
		phases[unit.Unit] = unit.Phase
	}
	want := map[string]leasePhase{
		identities[0].Name: leasePhaseReloading,
		identities[1].Name: leasePhaseRestoring,
		identities[2].Name: leasePhaseRestoring,
	}
	if !reflect.DeepEqual(phases, want) {
		t.Fatalf("durable phases = %v, want %v", phases, want)
	}

	transport.reloadErr = nil
	transport.onRevert = func(f *fakeUnitTransport, unit string) {
		f.revertErr = nil
		f.diskPaths[unit] = nil
	}
	restarted := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	if len(store.journal.Units) != 0 {
		t.Fatalf("leases remained after startup recovery: %+v", store.journal.Units)
	}
	for _, identity := range identities {
		if got := transport.units[identity.Name].slice[string(PropertyCPUWeight)]; got != uint64(SystemdUnset) {
			t.Fatalf("%s recovered CPUWeight = %v, want baseline", identity.Name, got)
		}
	}
	report := restarted.RecoveryReport()
	if len(report) != len(identities) {
		t.Fatalf("RecoveryReport() = %+v, want one outcome per unit", report)
	}
	for _, outcome := range report {
		if outcome.State != LeaseRecoveryReclaimed {
			t.Fatalf("RecoveryReport() = %+v, want every unit reclaimed", report)
		}
	}
}

// TestReleaseOwnedLeasesReleasesEveryUnitDespiteOneFailure proves that the
// release before observation shares one reload and that a unit which cannot be
// restored keeps its lease without stranding the limits of the other units.
func TestReleaseOwnedLeasesReleasesEveryUnitDespiteOneFailure(t *testing.T) {
	transport := newFakeUnitTransport(1001, 1002, 1003)
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identities := applyCPUWeights(t, adapter, 1001, 1002, 1003)
	failing := identities[0].Name
	transport.onRevert = func(f *fakeUnitTransport, unit string) {
		f.revertErr = nil
		if unit == failing {
			f.revertErr = errors.New("RevertUnitFiles refused")
		}
	}

	report, err := adapter.ReleaseOwnedLeases(context.Background())

	if err == nil || !strings.Contains(err.Error(), failing) {
		t.Fatalf("ReleaseOwnedLeases() error = %v, want the failure of %s", err, failing)
	}
	// The failed unit's baseline was programmed before its revert failed, so
	// its property counts as restored while its drop-in keeps the lease.
	if report.Units != len(identities) || report.Properties != len(identities) {
		t.Fatalf("report = %+v, want %d units and properties", report, len(identities))
	}
	if transport.reloadCalls != 1 {
		t.Fatalf("manager reloads = %d, want 1", transport.reloadCalls)
	}
	if len(store.journal.Units) != 1 || store.journal.Units[0].Unit != failing {
		t.Fatalf("durable leases = %+v, want only %s", store.journal.Units, failing)
	}
}
