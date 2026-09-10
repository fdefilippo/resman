package systemdunit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRealStartupCapabilityProbeUsesProductionTransactions is opt-in because
// it creates short-lived units and runtime properties on a disposable host.
func TestRealStartupCapabilityProbeUsesProductionTransactions(t *testing.T) {
	if os.Getenv("RESMAN_REAL_STARTUP_CAPABILITY_TEST") != "1" {
		t.Skip("set RESMAN_REAL_STARTUP_CAPABILITY_TEST=1 on a disposable systemd host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	transport, err := openDBusTransport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0700); err != nil {
		transport.close()
		t.Fatal(err)
	}
	store := newFileLeaseJournalStore(filepath.Join(stateDir, "leases.json"))
	adapter, err := newAdapter(ctx, transport, newCgroupVerifier(defaultCgroupRoot), localUnitFileInspector{}, store, DefaultCallTimeout)
	if err != nil {
		transport.close()
		t.Fatal(err)
	}
	defer adapter.Close()

	if err := adapter.requireStartupCapabilities(ctx, StartupRequirements{Memory: true, IO: true}); err != nil {
		t.Fatalf("requireStartupCapabilities() error = %v", err)
	}
	if owned := adapter.OwnedUnits(); len(owned) != 0 {
		t.Fatalf("startup probe left property leases: %+v", owned)
	}
	journal, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(journal.Units) != 0 {
		t.Fatalf("startup probe left a durable journal: %+v", journal)
	}
	listed, err := transport.listUserSlices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range listed {
		if isCapabilityProbeUnit(unit.name) {
			t.Fatalf("startup probe left active unit %s", unit.name)
		}
	}
	paths, err := filepath.Glob("/run/systemd/system.control/" + capabilityProbeUnitPrefix + "*.slice.d")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("startup probe left runtime drop-in directories: %v", paths)
	}
}
