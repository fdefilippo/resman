package systemdunit

import (
	"context"
	"testing"
)

func TestIOAccountingBooleanLeaseSurvivesRecoveryAndRestores(t *testing.T) {
	transport := newFakeUnitTransport(1000)
	store := newMemoryLeaseJournalStore()
	adapter, err := newAdapter(context.Background(), transport, &fakeKernelVerifier{}, transport, store, DefaultCallTimeout)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id := topology.Users[0].Unit.Identity
	on, err := NewPropertyAssignment(PropertyIOAccounting, 1)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := dbusPropertyValue(on).(bool); !ok || !value {
		t.Fatal("IOAccounting did not encode as D-Bus boolean")
	}
	if _, err := adapter.Apply(context.Background(), id, []PropertyAssignment{on}); err != nil {
		t.Fatal(err)
	}
	recovered, err := newAdapter(context.Background(), transport, &fakeKernelVerifier{}, transport, store, DefaultCallTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Leases(id)) != 1 {
		t.Fatal("accounting lease was not recovered")
	}
	if _, err := recovered.RestoreProperties(context.Background(), id, []PropertyName{PropertyIOAccounting}); err != nil {
		t.Fatal(err)
	}
	if value := transport.units[id.Name].slice[string(PropertyIOAccounting)]; value != false {
		t.Fatalf("restored accounting=%v", value)
	}
	for _, lease := range recovered.Leases(id) {
		if lease.Active() {
			t.Fatal("accounting remained active")
		}
	}
	if _, err := recovered.Restore(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if len(recovered.OwnedUnits()) != 0 {
		t.Fatal("accounting ownership leaked")
	}
	if _, err := NewPropertyAssignment(PropertyIOAccounting, 2); err == nil {
		t.Fatal("invalid boolean accepted")
	}
}
