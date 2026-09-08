package systemdunit

import (
	"context"
	"errors"
	"testing"
)

func TestRestoreResetsDeviceLimitsBeforeRevertWhilePeerKeepsController(t *testing.T) {
	for _, reclaimed := range []bool{false, true} {
		for property := range approvedDeviceProperties {
			t.Run(string(property)+map[bool]string{false: "/ordinary", true: "/reclaimed"}[reclaimed], func(t *testing.T) {
				transport := newFakeUnitTransport(1001, 1002)
				store := newMemoryLeaseJournalStore()
				adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
				identity := identityFor(t, adapter, 1001)
				assignment, err := NewDevicePropertyAssignment(property, []DeviceLimit{{Path: "/dev/vda", Value: 500}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{assignment}); err != nil {
					t.Fatal(err)
				}
				if reclaimed {
					adapter = mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
				}
				reset := false
				transport.onSet = func(_ *fakeUnitTransport, unit string, assignments []PropertyAssignment) {
					if unit != identity.Name || len(assignments) != 1 || assignments[0].name != property || len(assignments[0].value.devices) != 0 {
						t.Fatalf("reset unrelated properties: %+v", assignments)
					}
					if len(store.journal.Units) != 1 || store.journal.Units[0].Phase != leasePhaseApplying {
						t.Fatal("device reset lacks durable intent")
					}
					reset = true
				}
				transport.onRevert = func(*fakeUnitTransport, string) {
					// systemd Reload forgets the device list; it does not clear an
					// old kernel limit while a sibling retains the controller.
					if !reset {
						t.Fatal("revert forgot device list before kernel reset")
					}
					if store.journal.Units[0].Phase != leasePhaseRestoring {
						t.Fatal("revert lacks durable intent")
					}
				}
				if _, err := adapter.Restore(context.Background(), identity); err != nil {
					t.Fatal(err)
				}
				if !reset || len(store.journal.Units) != 0 {
					t.Fatal("device reset or ownership completion missing")
				}
			})
		}
	}
}

func TestRestoreResetsScalarBaselineBeforeRevertWhileUnitRemainsActive(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, adapter, 1001)
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 3300)}); err != nil {
		t.Fatal(err)
	}

	reset := false
	transport.onSet = func(_ *fakeUnitTransport, unit string, assignments []PropertyAssignment) {
		if unit != identity.Name || len(assignments) != 1 || assignments[0].name != PropertyCPUWeight || assignments[0].value.scalar != uint64(SystemdUnset) {
			t.Fatalf("unexpected scalar baseline restore: %+v", assignments)
		}
		if len(store.journal.Units) != 1 || store.journal.Units[0].Phase != leasePhaseApplying {
			t.Fatal("scalar baseline restore lacks durable intent")
		}
		reset = true
	}
	transport.onRevert = func(*fakeUnitTransport, string) {
		if !reset {
			t.Fatal("runtime files reverted before the scalar baseline was restored")
		}
		if store.journal.Units[0].Phase != leasePhaseRestoring {
			t.Fatal("revert lacks durable intent")
		}
	}

	if _, err := adapter.Restore(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if !reset || len(store.journal.Units) != 0 {
		t.Fatal("scalar baseline restore or ownership completion missing")
	}
}

func TestStartupFinishesDeviceResetInterruptedBeforeRestoreIntent(t *testing.T) {
	for _, failedSave := range []int{2, 3} {
		t.Run(map[int]string{2: "reset_acknowledgement", 3: "restore_intent"}[failedSave], func(t *testing.T) {
			transport := newFakeUnitTransport(1001)
			store := newMemoryLeaseJournalStore()
			adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
			identity := identityFor(t, adapter, 1001)
			assignment, err := NewDevicePropertyAssignment(PropertyIOReadBandwidthMax, []DeviceLimit{{Path: "/dev/vda", Value: 500}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{assignment}); err != nil {
				t.Fatal(err)
			}
			store.saveErr = errors.New("crash boundary")
			store.failSave = store.saveCalls + failedSave
			if _, err := adapter.Restore(context.Background(), identity); err == nil {
				t.Fatal("restore succeeded across failed durable boundary")
			}
			if len(store.journal.Units) != 1 {
				t.Fatal("crash lost ownership")
			}
			store.saveErr = nil
			_ = mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
			if len(store.journal.Units) != 0 || len(transport.units[identity.Name].unit["DropInPaths"].([]string)) != 0 {
				t.Fatal("restart left baseline-only ownership or runtime drop-ins")
			}
		})
	}
}

func TestDeviceResetNeverRevertsAnOperatorFileAddedDuringTheReset(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, adapter, 1001)
	assignment, err := NewDevicePropertyAssignment(PropertyIOReadBandwidthMax, []DeviceLimit{{Path: "/dev/vda", Value: 500}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{assignment}); err != nil {
		t.Fatal(err)
	}
	transport.onSet = func(f *fakeUnitTransport, unit string, _ []PropertyAssignment) {
		paths := f.units[unit].unit["DropInPaths"].([]string)
		f.units[unit].unit["DropInPaths"] = append(paths, "/etc/systemd/system/"+unit+".d/10-operator.conf")
	}
	if _, err := adapter.Restore(context.Background(), identity); err == nil {
		t.Fatal("restore accepted the operator file")
	}
	adapter = mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	if len(transport.revertCalls) != 0 || len(store.journal.Units) != 1 {
		t.Fatal("restore or recovery reverted unowned files or forgot ownership")
	}
	if report := adapter.RecoveryReport(); len(report) != 1 || report[0].State == LeaseRecoveryReclaimed {
		t.Fatalf("recovery did not report the conflict: %+v", report)
	}
}
