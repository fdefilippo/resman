package systemdunit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRealIOWeightGateProbe exercises only the production adapter's programmed
// weight contract. The owning fixture separately proves device identity, BFQ
// scheduling and delivered I/O; a cgroup readback cannot prove those properties.
func TestRealIOWeightGateProbe(t *testing.T) {
	if os.Getenv("RESMAN_REAL_IO_WEIGHT_GATE") != "1" {
		t.Skip("opt-in disposable-slice I/O weight probe")
	}
	journal := os.Getenv("RESMAN_REAL_IO_WEIGHT_JOURNAL")
	output := os.Getenv("RESMAN_REAL_IO_WEIGHT_OUTPUT")
	uid, err := validateRealRecoveryTarget(os.Getenv("RESMAN_REAL_SYSTEMD_UID"), journal, output)
	if err != nil {
		t.Fatal(err)
	}
	operation := os.Getenv("RESMAN_REAL_IO_WEIGHT_OPERATION")
	weight, err := validateRealIOWeightOperation(operation, os.Getenv("RESMAN_REAL_IO_WEIGHT"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("probe output must not already exist: %v", err)
	}
	store := newFileLeaseJournalStore(journal)
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	unitName := fmt.Sprintf("user-%d.slice", uid)
	if err := validateRealIOWeightJournal(saved, unitName); err != nil {
		t.Fatal(err)
	}
	journalBefore := saved
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	transport, err := openDBusTransport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	verifier := newCgroupVerifier(defaultCgroupRoot)
	adapter, err := newAdapter(ctx, transport, verifier, localUnitFileInspector{}, store, DefaultCallTimeout)
	if err != nil {
		transport.close()
		t.Fatal(err)
	}
	defer adapter.Close()
	before := findUserSlice(t, ctx, adapter, uint32(uid))
	beforeWeight, ok := before.Properties.Value(PropertyIOWeight)
	if !ok {
		t.Fatal("IOWeight absent from typed pre-operation snapshot")
	}
	beforeKernel := readRealIOWeightFiles(t, verifier, before)
	switch operation {
	case "apply":
		_, err = adapter.Apply(ctx, before.Identity, []PropertyAssignment{mustAssignment(t, PropertyIOWeight, weight)})
	case "restore":
		_, err = adapter.Restore(ctx, before.Identity)
	case "recover":
		// Adapter construction already performed startup reconciliation.
	}
	if err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
	after := findUserSlice(t, ctx, adapter, uint32(uid))
	afterWeight, ok := after.Properties.Value(PropertyIOWeight)
	if !ok {
		t.Fatal("IOWeight absent from typed post-operation snapshot")
	}
	if err := requireSameIdentity("io_weight_probe", before.Identity, after.Identity); err != nil {
		t.Fatal(err)
	}
	afterKernel := readRealIOWeightFiles(t, verifier, after)
	confirmed := findUserSlice(t, ctx, adapter, uint32(uid))
	if err := requireSameIdentity("io_weight_probe_readback", after.Identity, confirmed.Identity); err != nil {
		t.Fatal(err)
	}
	if err := verifier.verify(after, []PropertyAssignment{mustAssignment(t, PropertyIOWeight, 100)}); err != nil {
		t.Fatal(err)
	}
	saved, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRealIOWeightJournal(saved, unitName); err != nil {
		t.Fatal(err)
	}
	if operation == "apply" {
		if afterWeight != weight || len(saved.Units) != 1 || saved.Units[0].Phase != leasePhaseApplied ||
			len(saved.Units[0].Properties) != 1 || saved.Units[0].Properties[0].LastApplied != weight ||
			saved.Units[0].Properties[0].Uncertain {
			t.Fatalf("weight not durably verified: readback=%d journal=%+v", afterWeight, saved)
		}
	}
	if operation == "restore" && (len(saved.Units) != 0 || len(mutableUnitFilePaths(after.unitFiles)) != 0) {
		t.Fatalf("restore retained ownership or mutable files: %+v", saved)
	}
	if operation == "restore" && len(journalBefore.Units) == 1 && len(journalBefore.Units[0].Properties) == 1 {
		baseline := journalBefore.Units[0].Properties[0].Baseline
		if afterWeight != baseline {
			t.Fatalf("restored IOWeight = %d, want durable baseline %d", afterWeight, baseline)
		}
	}
	normalizedWeight := afterWeight
	if normalizedWeight == SystemdUnset {
		normalizedWeight = 100
	}
	if err := writeRealRecoveryOutput(output, map[string]any{
		"scope":     "adapter-programmed-weight-only; not daemon or device-delivery evidence",
		"operation": operation, "identity": after.Identity, "requested_io_weight": weight,
		"before_io_weight": beforeWeight, "after_io_weight": afterWeight,
		"expected_bfq_weight": bfqWeight(normalizedWeight),
		"before_kernel":       beforeKernel, "after_kernel": afterKernel,
		"mutable_paths":        mutableUnitFilePaths(after.unitFiles),
		"before_mutable_paths": mutableUnitFilePaths(before.unitFiles),
		"journal_before":       journalBefore, "journal": saved, "recovery": adapter.RecoveryReport(),
	}); err != nil {
		t.Fatal(err)
	}
}

func validateRealIOWeightOperation(operation, value string) (uint64, error) {
	switch operation {
	case "apply":
		switch value {
		case "100":
			return 100, nil
		case "2300":
			return 2300, nil
		}
	case "restore", "recover":
		if value == "" {
			return 0, nil
		}
	}
	return 0, fmt.Errorf("probe requires apply weight 100 or 2300, or restore/recover without weight")
}

func validateRealIOWeightJournal(journal durableLeaseJournal, unit string) error {
	for _, lease := range journal.Units {
		if lease.Unit != unit {
			return fmt.Errorf("private I/O probe journal contains unrelated unit %s", lease.Unit)
		}
		for _, property := range lease.Properties {
			if property.Property != PropertyIOWeight {
				return fmt.Errorf("private I/O probe journal contains unrelated property %s", property.Property)
			}
		}
	}
	return nil
}

func readRealIOWeightFiles(t *testing.T, verifier cgroupVerifier, unit UnitSnapshot) map[string]*string {
	t.Helper()
	path, err := verifier.controlGroupPath(unit.ControlGroup)
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]*string)
	for _, name := range []string{"io.weight", "io.bfq.weight"} {
		data, err := os.ReadFile(filepath.Join(path, name))
		if errors.Is(err, os.ErrNotExist) {
			result[name] = nil
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		value := string(data)
		result[name] = &value
	}
	return result
}

func TestRealIOWeightProbeValidatesOperationsBeforeOpeningBus(t *testing.T) {
	for _, tc := range []struct {
		operation, value string
		want             uint64
		valid            bool
	}{
		{"apply", "100", 100, true}, {"apply", "2300", 2300, true},
		{"restore", "", 0, true}, {"recover", "", 0, true},
		{"apply", "", 0, false}, {"apply", "0", 0, false},
		{"apply", "10000", 0, false}, {"apply", "+100", 0, false},
		{"apply", "0100", 0, false}, {"apply", "bad", 0, false},
		{"restore", "100", 0, false}, {"recover", "2300", 0, false},
		{"", "100", 0, false}, {"other", "100", 0, false},
	} {
		t.Run(tc.operation+"/"+tc.value, func(t *testing.T) {
			got, err := validateRealIOWeightOperation(tc.operation, tc.value)
			if (err == nil) != tc.valid || got != tc.want {
				t.Fatalf("validation = %d, %v; want %d valid=%v", got, err, tc.want, tc.valid)
			}
		})
	}
}

func TestRealIOWeightProbeRejectsUnrelatedDurableOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, unit string
		property   PropertyName
		valid      bool
	}{
		{"owned weight", "user-1006.slice", PropertyIOWeight, true},
		{"root", "user-0.slice", PropertyIOWeight, false},
		{"peer", "user-1007.slice", PropertyIOWeight, false},
		{"CPU property", "user-1006.slice", PropertyCPUWeight, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			journal := durableLeaseJournal{Units: []durableUnitLease{{Unit: tc.unit,
				Properties: []durablePropertyLease{{Property: tc.property}}}}}
			if err := validateRealIOWeightJournal(journal, "user-1006.slice"); (err == nil) != tc.valid {
				t.Fatalf("journal validation = %v; want valid=%v", err, tc.valid)
			}
		})
	}
}
