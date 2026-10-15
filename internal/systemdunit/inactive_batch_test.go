package systemdunit

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// departUnits makes systemd forget the given units while their recorded
// runtime drop-ins stay on disk, as when every process of a slice exits.
func departUnits(transport *fakeUnitTransport, identities ...UnitIdentity) {
	if transport.diskPaths == nil {
		transport.diskPaths = make(map[string][]string)
	}
	for _, identity := range identities {
		paths, _ := transport.units[identity.Name].unit["DropInPaths"].([]string)
		transport.diskPaths[identity.Name] = append([]string(nil), paths...)
		delete(transport.units, identity.Name)
	}
}

func journalPhases(store *memoryLeaseJournalStore) map[string]leasePhase {
	phases := make(map[string]leasePhase, len(store.journal.Units))
	for _, unit := range store.journal.Units {
		phases[unit.Unit] = unit.Phase
	}
	return phases
}

// TestReconcileOwnedCleansDepartedUnitsWithOneReload pins resman-2bj: when
// several leased slices disappear together, as in a burst of logouts,
// ReconcileOwned reverts each recorded footprint and reloads systemd once,
// with one restoring intent, one reloading intent and one lease removal.
func TestReconcileOwnedCleansDepartedUnitsWithOneReload(t *testing.T) {
	tests := []struct {
		name     string
		departed int
	}{
		{name: "no departed unit", departed: 0},
		{name: "one departed unit", departed: 1},
		{name: "burst of departed units", departed: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uids := []uint32{1001, 1002, 1003, 1004, 1005}
			transport := newFakeUnitTransport(uids...)
			store := newMemoryLeaseJournalStore()
			adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
			identities := applyCPUWeights(t, adapter, uids...)
			departed := identities[:tt.departed]
			departUnits(transport, departed...)
			savesBefore := store.saveCalls

			if err := adapter.ReconcileOwned(context.Background()); err != nil {
				t.Fatalf("ReconcileOwned() error = %v", err)
			}

			wantReloads, wantSaves := 0, 0
			if tt.departed != 0 {
				wantReloads, wantSaves = 1, 3
			}
			if transport.reloadCalls != wantReloads {
				t.Fatalf("manager reloads = %d, want %d", transport.reloadCalls, wantReloads)
			}
			if got := store.saveCalls - savesBefore; got != wantSaves {
				t.Fatalf("journal saves = %d, want %d", got, wantSaves)
			}
			if want := unitNames(departed); len(want) != 0 && !reflect.DeepEqual(transport.revertCalls, want) {
				t.Fatalf("RevertUnitFiles calls = %v, want %v", transport.revertCalls, want)
			}
			if got, want := len(store.journal.Units), len(identities)-tt.departed; got != want {
				t.Fatalf("durable leases = %d, want %d remaining", got, want)
			}
			for _, identity := range departed {
				if paths := transport.diskPaths[identity.Name]; len(paths) != 0 {
					t.Fatalf("%s footprint remained: %v", identity.Name, paths)
				}
			}
		})
	}
}

// TestReconcileOwnedIsolatesAFailedDepartedUnit proves that one departed unit
// failing the shared cleanup neither blocks nor fails the others, and keeps a
// durable lease in the phase the next pass resolves.
func TestReconcileOwnedIsolatesAFailedDepartedUnit(t *testing.T) {
	tests := []struct {
		name      string
		failure   func(*fakeUnitTransport, string)
		wantPhase leasePhase
		wantState LeaseRecoveryState
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
			wantState: LeaseRecoveryPending,
		},
		{
			name: "operator file appears before the reload",
			failure: func(transport *fakeUnitTransport, failing string) {
				transport.onReload = func(f *fakeUnitTransport) {
					f.diskPaths[failing] = []string{"/etc/systemd/system/" + failing + ".d/override.conf"}
				}
			},
			wantPhase: leasePhaseReloading,
			wantState: LeaseRecoveryConflict,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uids := []uint32{1001, 1002, 1003}
			transport := newFakeUnitTransport(uids...)
			store := newMemoryLeaseJournalStore()
			adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
			identities := applyCPUWeights(t, adapter, uids...)
			departUnits(transport, identities...)
			failing := identities[1].Name
			tt.failure(transport, failing)

			if err := adapter.ReconcileOwned(context.Background()); err == nil {
				t.Fatal("ReconcileOwned() error = nil, want the failed unit")
			}

			if transport.reloadCalls != 1 {
				t.Fatalf("manager reloads = %d, want 1", transport.reloadCalls)
			}
			if want := map[string]leasePhase{failing: tt.wantPhase}; !reflect.DeepEqual(journalPhases(store), want) {
				t.Fatalf("durable phases = %v, want %v", journalPhases(store), want)
			}
			var failed []LeaseRecoveryOutcome
			for _, outcome := range adapter.RecoveryReport() {
				if outcome.Unit == failing {
					failed = append(failed, outcome)
				}
			}
			if len(failed) != 1 || failed[0].State != tt.wantState {
				t.Fatalf("recovery outcomes for %s = %+v, want %s", failing, failed, tt.wantState)
			}
		})
	}
}

// TestReconcileOwnedDoesNotRevertDepartedUnitsBeforeTheIntentIsDurable keeps
// the write-ahead order of a single-unit cleanup for the whole batch.
func TestReconcileOwnedDoesNotRevertDepartedUnitsBeforeTheIntentIsDurable(t *testing.T) {
	uids := []uint32{1001, 1002, 1003}
	transport := newFakeUnitTransport(uids...)
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identities := applyCPUWeights(t, adapter, uids...)
	departUnits(transport, identities...)
	store.saveErr = errors.New("restoring intent unavailable")
	store.failSave = store.saveCalls + 1

	if err := adapter.ReconcileOwned(context.Background()); err == nil {
		t.Fatal("ReconcileOwned() error = nil, want the persistence failure")
	}
	if len(transport.revertCalls) != 0 || transport.reloadCalls != 0 {
		t.Fatalf("revert=%v reloads=%d before a durable restoring intent", transport.revertCalls, transport.reloadCalls)
	}
	for unit, phase := range journalPhases(store) {
		if phase != leasePhaseApplied {
			t.Fatalf("%s durable phase = %q, want applied", unit, phase)
		}
	}
}

// TestDepartedUnitsInterruptedBeforeTheReloadRecoverWithOneReload covers the
// mixed journal a batch can leave: reverted units waiting for the reload and
// units whose revert never happened. Both the next ReconcileOwned and a
// restarted adapter finish every unit with one reload.
func TestDepartedUnitsInterruptedBeforeTheReloadRecoverWithOneReload(t *testing.T) {
	for _, path := range []string{"next reconciliation", "startup"} {
		t.Run(path, func(t *testing.T) {
			uids := []uint32{1001, 1002, 1003}
			transport := newFakeUnitTransport(uids...)
			store := newMemoryLeaseJournalStore()
			adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
			identities := applyCPUWeights(t, adapter, uids...)
			departUnits(transport, identities...)
			notReverted := identities[2].Name
			transport.onRevert = func(f *fakeUnitTransport, unit string) {
				f.revertErr = nil
				if unit == notReverted {
					f.revertErr = errors.New("simulated crash during the revert stage")
				}
			}
			transport.reloadErr = errors.New("simulated crash before Reload completes")
			if err := adapter.ReconcileOwned(context.Background()); err == nil {
				t.Fatal("ReconcileOwned() error = nil, want the interrupted batch")
			}
			want := map[string]leasePhase{
				identities[0].Name: leasePhaseReloading,
				identities[1].Name: leasePhaseReloading,
				notReverted:        leasePhaseRestoring,
			}
			if !reflect.DeepEqual(journalPhases(store), want) {
				t.Fatalf("durable phases = %v, want %v", journalPhases(store), want)
			}

			transport.reloadErr = nil
			transport.onRevert = nil
			transport.revertErr = nil
			reloadsBefore := transport.reloadCalls
			switch path {
			case "next reconciliation":
				if err := adapter.ReconcileOwned(context.Background()); err != nil {
					t.Fatalf("second ReconcileOwned() error = %v", err)
				}
			case "startup":
				mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
			}
			if got := transport.reloadCalls - reloadsBefore; got != 1 {
				t.Fatalf("recovery reloads = %d, want 1", got)
			}
			if len(store.journal.Units) != 0 {
				t.Fatalf("leases remained after recovery: %+v", store.journal.Units)
			}
		})
	}
}

// TestStartupCleansDepartedUnitsWithOneReload covers the startup boundary: a
// daemon restarted after many leased slices disappeared reloads systemd once.
func TestStartupCleansDepartedUnitsWithOneReload(t *testing.T) {
	uids := []uint32{1001, 1002, 1003, 1004}
	transport := newFakeUnitTransport(uids...)
	store := newMemoryLeaseJournalStore()
	first := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identities := applyCPUWeights(t, first, uids...)
	departUnits(transport, identities...)

	restarted := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)

	if transport.reloadCalls != 1 {
		t.Fatalf("startup reloads = %d, want 1", transport.reloadCalls)
	}
	if len(store.journal.Units) != 0 {
		t.Fatalf("leases remained after startup: %+v", store.journal.Units)
	}
	report := restarted.RecoveryReport()
	if len(report) != len(identities) {
		t.Fatalf("RecoveryReport() = %+v, want one outcome per unit", report)
	}
	for index, outcome := range report {
		if outcome != (LeaseRecoveryOutcome{Unit: fmt.Sprintf("user-%d.slice", uids[index]), State: LeaseRecoveryInactive}) {
			t.Fatalf("RecoveryReport() = %+v, want every unit inactive", report)
		}
	}
}
