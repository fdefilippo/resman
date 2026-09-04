package systemdunit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestRealSystemdRuntimePropertySurvivesReloadAndRestores is opt-in because it
// changes one pre-created user slice through the system bus. The harness owns
// unit creation and cleanup; the production adapter never gains that capability.
func TestRealSystemdRuntimePropertySurvivesReloadAndRestores(t *testing.T) {
	if os.Getenv("RESMAN_REAL_SYSTEMD_TEST") != "1" {
		t.Skip("set RESMAN_REAL_SYSTEMD_TEST=1 with a disposable active user slice")
	}
	uidValue := os.Getenv("RESMAN_REAL_SYSTEMD_UID")
	uid64, err := strconv.ParseUint(uidValue, 10, 32)
	if err != nil {
		t.Fatalf("RESMAN_REAL_SYSTEMD_UID=%q is not a UID: %v", uidValue, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	transport, err := openDBusTransport(ctx)
	if err != nil {
		t.Fatalf("openDBusTransport() error = %v", err)
	}
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0700); err != nil {
		t.Fatalf("Chmod(state directory) error = %v", err)
	}
	adapter, err := newAdapter(ctx, transport, newCgroupVerifier(defaultCgroupRoot), localUnitFileInspector{}, newFileLeaseJournalStore(filepath.Join(stateDir, "leases.json")), DefaultCallTimeout)
	if err != nil {
		transport.close()
		t.Fatalf("New() error = %v", err)
	}
	defer adapter.Close()

	unit := findUserSlice(t, ctx, adapter, uint32(uid64))
	if paths := mutableUnitFilePaths(unit.unitFiles); len(paths) != 0 {
		t.Fatalf("disposable slice has preexisting mutable unit files: %v", paths)
	}
	baselines := make(map[PropertyName]uint64)
	probeValues := map[PropertyName]uint64{
		PropertyCPUWeight:          321,
		PropertyCPUQuotaPerSecUSec: 500_000,
		PropertyCPUQuotaPeriodUSec: 100_000,
		PropertyMemoryHigh:         512 << 20,
	}
	var assignments []PropertyAssignment
	for property, probeValue := range probeValues {
		baseline, ok := unit.Properties.Value(property)
		if !ok {
			t.Fatalf("%s baseline is absent", property)
		}
		baselines[property] = baseline
		if baseline == probeValue {
			probeValue++
			probeValues[property] = probeValue
		}
		assignments = append(assignments, mustAssignment(t, property, probeValue))
	}
	applied, err := adapter.Apply(ctx, unit.Identity, assignments)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if paths := mutableUnitFilePaths(applied.unitFiles); len(paths) != len(assignments) {
		t.Fatalf("managed runtime drop-ins after Apply() = %v, want %d", paths, len(assignments))
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := adapter.Restore(cleanupCtx, unit.Identity); err != nil {
			t.Errorf("deferred Restore() error = %v", err)
		}
	}()

	command := exec.CommandContext(ctx, "systemctl", "daemon-reload")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("systemctl daemon-reload: %v: %s", err, output)
	}
	afterReload := findUserSlice(t, ctx, adapter, uint32(uid64))
	for property, want := range probeValues {
		if got, _ := afterReload.Properties.Value(property); got != want {
			t.Fatalf("%s after daemon-reload = %d, want %d", property, got, want)
		}
	}

	result, err := adapter.Restore(ctx, unit.Identity)
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if len(result.Restored) != len(assignments) {
		t.Fatalf("Restore() result = %+v", result)
	}
	restored := findUserSlice(t, ctx, adapter, uint32(uid64))
	if paths := mutableUnitFilePaths(restored.unitFiles); len(paths) != 0 {
		t.Fatalf("mutable unit files after Restore() = %v, want none", paths)
	}
	for property, want := range baselines {
		if got, _ := restored.Properties.Value(property); got != want {
			t.Fatalf("restored %s = %d, want baseline %d", property, got, want)
		}
	}
}

func findUserSlice(t *testing.T, ctx context.Context, adapter *Adapter, uid uint32) UnitSnapshot {
	t.Helper()
	topology, err := adapter.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	for _, user := range topology.Users {
		if user.UID == uid {
			return user.Unit
		}
	}
	t.Fatalf("active user-%d.slice was not discovered", uid)
	return UnitSnapshot{Identity: UnitIdentity{Name: fmt.Sprintf("user-%d.slice", uid)}}
}
