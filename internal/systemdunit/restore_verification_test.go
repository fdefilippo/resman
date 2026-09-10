package systemdunit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

type fixedIdentityKernelVerifier struct {
	cgroupVerifier
	controlGroupID uint64
}

func (v fixedIdentityKernelVerifier) identity(string) (uint64, error) {
	return v.controlGroupID, nil
}

func TestRestoreWaitsForDeviceKernelConvergenceBeforeJournalCompletion(t *testing.T) {
	for property, field := range map[PropertyName]string{
		PropertyIOReadBandwidthMax: "rbps", PropertyIOWriteBandwidthMax: "wbps",
		PropertyIOReadIOPSMax: "riops", PropertyIOWriteIOPSMax: "wiops",
	} {
		for _, path := range []string{"ordinary", "reclaimed", "interrupted_reload"} {
			t.Run(string(property)+"/"+path, func(t *testing.T) {
				transport := newFakeUnitTransport(1001)
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
				if path == "reclaimed" {
					adapter = mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
				}
				if path == "interrupted_reload" {
					transport.reloadErr = errors.New("crash")
					if _, err := adapter.Restore(context.Background(), identity); err == nil {
						t.Fatal("missing injected failure")
					}
					transport.reloadErr = nil
				}
				reads := 0
				verifier := fixedIdentityKernelVerifier{cgroupVerifier: newCgroupVerifier(t.TempDir()), controlGroupID: identity.ControlGroupID}
				verifier.readFile = func(string) ([]byte, error) {
					if len(store.journal.Units) == 1 && store.journal.Units[0].Phase == leasePhaseApplying {
						return []byte(""), nil // Explicit device reset before revert is already confirmed.
					}
					reads++
					if len(store.journal.Units) != 1 || store.journal.Units[0].Phase != leasePhaseReloading {
						t.Fatal("ownership completed before exact kernel confirmation")
					}
					if len(transport.units[identity.Name].slice[string(property)].([]dbusDeviceLimit)) != 0 {
						t.Fatal("test must model D-Bus cleared before kernel convergence")
					}
					if reads <= 2 {
						return []byte(fmt.Sprintf("8:0 %s=500\n", field)), nil
					}
					return []byte(""), nil
				}
				if path == "interrupted_reload" {
					adapter = mustTestAdapterWithStore(t, transport, verifier, store)
					if report := adapter.RecoveryReport(); len(report) != 1 || report[0].State != LeaseRecoveryReclaimed {
						t.Fatalf("recovery: %+v", report)
					}
				} else {
					adapter.verifier = verifier
					if _, err := adapter.Restore(context.Background(), identity); err != nil {
						t.Fatal(err)
					}
				}
				if reads != 3 || len(store.journal.Units) != 0 {
					t.Fatalf("readbacks=%d journal=%+v", reads, store.journal)
				}
			})
		}
	}
}

func TestRestoreWaitsForScalarKernelConvergenceBeforeJournalCompletion(t *testing.T) {
	transport := newFakeUnitTransport(1001)
	store := newMemoryLeaseJournalStore()
	adapter := mustTestAdapterWithStore(t, transport, &fakeKernelVerifier{}, store)
	identity := identityFor(t, adapter, 1001)
	if _, err := adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 3300)}); err != nil {
		t.Fatal(err)
	}
	reads := 0
	verifier := fixedIdentityKernelVerifier{cgroupVerifier: newCgroupVerifier(t.TempDir()), controlGroupID: identity.ControlGroupID}
	verifier.readFile = func(string) ([]byte, error) {
		if len(store.journal.Units) == 1 && store.journal.Units[0].Phase == leasePhaseApplying {
			return []byte("100\n"), nil // Explicit scalar baseline restore before revert.
		}
		reads++
		if len(store.journal.Units) != 1 || store.journal.Units[0].Phase != leasePhaseReloading {
			t.Fatal("ownership completed before scalar kernel confirmation")
		}
		if reads <= 2 {
			return []byte("3300\n"), nil
		}
		return []byte("100\n"), nil
	}
	adapter.verifier = verifier
	if _, err := adapter.Restore(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if reads != 3 || len(store.journal.Units) != 0 {
		t.Fatalf("readbacks=%d journal=%+v", reads, store.journal)
	}
}

func TestRestoreConvergenceFailsClosedOnDeadlineCancellationAndExternalChange(t *testing.T) {
	for _, outcome := range []string{"deadline", "canceled", "permission", "malformed", "identity", "operator_property", "operator_file"} {
		t.Run(outcome, func(t *testing.T) {
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
			adapter.timeout = 40 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			verifier := fixedIdentityKernelVerifier{cgroupVerifier: newCgroupVerifier(t.TempDir()), controlGroupID: identity.ControlGroupID}
			verifier.readFile = func(string) ([]byte, error) {
				if len(store.journal.Units) == 1 && store.journal.Units[0].Phase == leasePhaseApplying {
					return []byte(""), nil
				}
				reads++
				switch outcome {
				case "canceled":
					cancel()
				case "permission":
					return nil, os.ErrPermission
				case "malformed":
					return []byte("invalid"), nil
				case "identity":
					transport.units[identity.Name].slice["ControlGroupId"] = identity.ControlGroupID + 1
				case "operator_property":
					transport.units[identity.Name].slice[string(PropertyIOReadBandwidthMax)] = []dbusDeviceLimit{{Path: "/dev/vda", Value: 999}}
				case "operator_file":
					transport.diskPaths = map[string][]string{identity.Name: {"/etc/systemd/system/" + identity.Name + ".d/operator.conf"}}
				}
				if reads > 1 && outcome != "deadline" {
					return []byte(""), nil
				}
				return []byte("8:0 rbps=500\n"), nil
			}
			adapter.verifier = verifier
			_, err = adapter.Restore(ctx, identity)
			if err == nil {
				t.Fatal("unverified restore succeeded")
			}
			if outcome == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline lost: %v", err)
			}
			if outcome == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if len(store.journal.Units) != 1 || store.journal.Units[0].Phase != leasePhaseReloading {
				t.Fatal("failed verification discarded ownership")
			}
			if outcome != "deadline" && reads != 1 {
				t.Fatalf("unexpected retry after %s: %d", outcome, reads)
			}
		})
	}
}
