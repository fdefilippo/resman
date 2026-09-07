package systemdunit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// realRecoveryCrashTransport terminates only this opt-in probe process, after
// the real RevertUnitFiles and durable reloading intent, before the real Reload.
// No production crash hook or alternate systemd writer is introduced.
type realRecoveryCrashTransport struct {
	unitTransport
	armed   bool
	unit    string
	journal string
	output  string
}

func (r *realRecoveryCrashTransport) reload(ctx context.Context) error {
	if !r.armed {
		return r.unitTransport.reload(ctx)
	}
	journal, err := newFileLeaseJournalStore(r.journal).Load()
	if err != nil {
		return fmt.Errorf("read crash-window journal: %w", err)
	}
	if len(journal.Units) != 1 || journal.Units[0].Unit != r.unit || journal.Units[0].Phase != leasePhaseReloading {
		return fmt.Errorf("crash boundary lacks exact durable reloading intent: %+v", journal)
	}
	properties, err := r.unitProperties(ctx, r.unit)
	if err != nil {
		return fmt.Errorf("read pre-reload unit: %w", err)
	}
	slice, err := r.sliceProperties(ctx, r.unit)
	if err != nil {
		return fmt.Errorf("read pre-reload weight: %w", err)
	}
	if err := requireRealRecoveryFilesAbsent(journal.Units[0].Footprint); err != nil {
		return err
	}
	if err := writeRealRecoveryOutput(r.output, map[string]any{
		"phase": "reloading", "journal": journal, "disk_footprint_absent": true,
		"drop_in_paths_before_reload": properties["DropInPaths"],
		"cpu_weight_before_reload":    slice[string(PropertyCPUWeight)],
		"reload_executed":             false,
	}); err != nil {
		return err
	}
	return syscall.Kill(os.Getpid(), syscall.SIGKILL)
}

func requireRealRecoveryFilesAbsent(files []durableFileFingerprint) error {
	if len(files) == 0 {
		return fmt.Errorf("crash boundary has no recorded footprint to verify")
	}
	for _, fingerprint := range files {
		if _, err := os.Lstat(fingerprint.Path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("reverted path is not absent: %s: %v", fingerprint.Path, err)
		}
	}
	return nil
}

func TestRealRecoveryCrashRequiresActualDiskRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned.conf")
	if err := requireRealRecoveryFilesAbsent(nil); err == nil {
		t.Fatal("empty footprint is a vacuous crash proof")
	}
	files := []durableFileFingerprint{{Path: path}}
	if err := requireRealRecoveryFilesAbsent(files); err != nil {
		t.Fatalf("absent file rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte("[Slice]\nCPUWeight=321\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := requireRealRecoveryFilesAbsent(files); err == nil {
		t.Fatal("surviving file accepted as post-revert crash")
	}
}

// TestRealRecoveryGateProbe is the source-bound subprocess used by the native
// recovery fixture. The caller owns one disposable active PAM user slice, all
// operator-conflict files, the private journal and teardown. Normal unit tests
// never touch the system bus or systemd files.
func TestRealRecoveryGateProbe(t *testing.T) {
	if os.Getenv("RESMAN_REAL_RECOVERY_GATE") != "1" {
		t.Skip("opt-in real-systemd recovery probe")
	}
	journal := os.Getenv("RESMAN_REAL_RECOVERY_JOURNAL")
	output := os.Getenv("RESMAN_REAL_RECOVERY_OUTPUT")
	uid, err := validateRealRecoveryTarget(os.Getenv("RESMAN_REAL_SYSTEMD_UID"), journal, output)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	transport, err := openDBusTransport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	crash := &realRecoveryCrashTransport{unitTransport: transport,
		unit: fmt.Sprintf("user-%d.slice", uid), journal: journal, output: output}
	adapter, err := newAdapter(ctx, crash, newCgroupVerifier(defaultCgroupRoot),
		localUnitFileInspector{}, newFileLeaseJournalStore(journal), DefaultCallTimeout)
	if err != nil {
		transport.close()
		t.Fatal(err)
	}
	defer adapter.Close()
	unit := findUserSlice(t, ctx, adapter, uint32(uid))
	operation := os.Getenv("RESMAN_REAL_RECOVERY_OPERATION")
	var operationErr error
	switch operation {
	case "apply", "conflict-apply":
		_, operationErr = adapter.Apply(ctx, unit.Identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)})
	case "restore", "conflict-restore":
		_, operationErr = adapter.Restore(ctx, unit.Identity)
	case "crash-restore":
		crash.armed = true
		_, operationErr = adapter.Restore(ctx, unit.Identity)
		if operationErr == nil {
			t.Fatal("crash restore returned without reaching the crash boundary")
		}
	case "recover":
		// Construction above performs the real startup reconciliation.
	default:
		t.Fatalf("unknown recovery operation %q", operation)
	}
	wantConflict := operation == "conflict-apply" || operation == "conflict-restore"
	var adapterErr *AdapterError
	if wantConflict {
		if !errors.As(operationErr, &adapterErr) || adapterErr.Reason != ReasonExternalConflict {
			t.Fatalf("operation %s = %v, want typed external conflict", operation, operationErr)
		}
	} else if operationErr != nil {
		t.Fatalf("operation %s: %v", operation, operationErr)
	}
	current := findUserSlice(t, ctx, adapter, uint32(uid))
	weight, ok := current.Properties.Value(PropertyCPUWeight)
	if !ok {
		t.Fatal("CPUWeight absent from typed snapshot")
	}
	ioWeight, ok := current.Properties.Value(PropertyIOWeight)
	if !ok {
		t.Fatal("IOWeight absent from typed snapshot")
	}
	saved, err := newFileLeaseJournalStore(journal).Load()
	if err != nil {
		t.Fatal(err)
	}
	record := map[string]any{"operation": operation, "identity": current.Identity,
		"cpu_weight": weight, "io_weight": ioWeight, "mutable_paths": mutableUnitFilePaths(current.unitFiles),
		"recovery": adapter.RecoveryReport(), "journal": saved, "conflict": wantConflict}
	if operationErr != nil {
		record["error"] = operationErr.Error()
	}
	if err := writeRealRecoveryOutput(output, record); err != nil {
		t.Fatal(err)
	}
}

func validateRealRecoveryTarget(rawUID, journal, output string) (uint64, error) {
	uid, err := strconv.ParseUint(rawUID, 10, 32)
	if err != nil || uid == 0 {
		return 0, fmt.Errorf("a non-root disposable UID is required: %q", rawUID)
	}
	if !filepath.IsAbs(journal) || !filepath.IsAbs(output) ||
		filepath.Clean(journal) == DefaultLeaseJournalPath || filepath.Clean(output) == DefaultLeaseJournalPath ||
		filepath.Clean(journal) == filepath.Clean(output) {
		return 0, fmt.Errorf("distinct absolute private journal/output paths are required; the daemon journal is forbidden")
	}
	return uid, nil
}

func TestRealRecoveryProbeRejectsUnsafeTargetsBeforeOpeningBus(t *testing.T) {
	for _, tc := range []struct{ name, uid, journal, output string }{
		{"root", "0", "/tmp/private/leases", "/tmp/private/output"},
		{"invalid UID", "no", "/tmp/private/leases", "/tmp/private/output"},
		{"relative journal", "1006", "leases", "/tmp/private/output"},
		{"relative output", "1006", "/tmp/private/leases", "output"},
		{"daemon journal", "1006", DefaultLeaseJournalPath, "/tmp/private/output"},
		{"normalized daemon journal", "1006", "/var/lib/resman/../resman/systemd-property-leases.json", "/tmp/private/output"},
		{"daemon output", "1006", "/tmp/private/leases", DefaultLeaseJournalPath},
		{"same file", "1006", "/tmp/private/leases", "/tmp/private/leases"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateRealRecoveryTarget(tc.uid, tc.journal, tc.output); err == nil {
				t.Fatal("unsafe real probe target accepted")
			}
		})
	}
	if uid, err := validateRealRecoveryTarget("1006", "/tmp/private/leases", "/tmp/private/output"); err != nil || uid != 1006 {
		t.Fatalf("private target = %d, %v", uid, err)
	}
}

func TestRealRecoveryCrashHookIsDisarmedDuringConstruction(t *testing.T) {
	transport := &fakeUnitTransport{}
	probe := &realRecoveryCrashTransport{unitTransport: transport}
	if err := probe.reload(context.Background()); err != nil || transport.reloadCalls != 1 {
		t.Fatalf("disarmed reload = %v, calls = %d", err, transport.reloadCalls)
	}
	probe.armed = true
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	probe.journal = filepath.Join(directory, "absent.json")
	if err := probe.reload(context.Background()); err == nil || !strings.Contains(err.Error(), "durable reloading intent") {
		t.Fatalf("armed crash without durable intent = %v", err)
	}
	if transport.reloadCalls != 1 {
		t.Fatal("armed crash executed Reload despite missing durable intent")
	}
}

func writeRealRecoveryOutput(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode real recovery evidence: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create real recovery evidence: %w", err)
	}
	_, writeErr := file.Write(append(data, '\n'))
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}
