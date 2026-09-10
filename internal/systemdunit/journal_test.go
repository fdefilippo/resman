package systemdunit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileLeaseJournalRoundTripIsPrivateStrictAndDeterministic(t *testing.T) {
	dir := privateLeaseTestDirectory(t)
	path := filepath.Join(dir, "systemd-property-leases.json")
	store := newFileLeaseJournalStore(path)
	journal := testDurableJournal(t)
	journal.Units[0].Properties = append(journal.Units[0].Properties,
		durablePropertyLease{Property: PropertyMemoryHigh, Baseline: SystemdUnset, PreviousApplied: SystemdUnset, LastApplied: 64 << 20},
	)
	journal.Units[0].Footprint = append(journal.Units[0].Footprint,
		durableFileFingerprint{Path: managedRuntimeDropInPath("user-1001.slice", PropertyMemoryHigh), SHA256: strings.Repeat("b", 64)},
	)
	if err := store.Save(journal); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat() error = %v", err)
	}
	if info.Mode().Perm() != 0600 || !info.Mode().IsRegular() {
		t.Fatalf("journal mode = %v, want regular 0600", info.Mode())
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Generation != journal.Generation || len(loaded.Units) != 1 || loaded.Units[0].Properties[0].Property != PropertyCPUWeight {
		t.Fatalf("loaded journal = %+v", loaded)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Index(string(data), `"CPUWeight"`) > strings.Index(string(data), `"MemoryHigh"`) {
		t.Fatalf("journal properties are not sorted: %s", data)
	}
}

func TestFileLeaseJournalRejectsUnsafeOrMalformedState(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{name: "symbolic link", prepare: func(t *testing.T, path string) {
			target := path + ".target"
			writeLeaseTestFile(t, target, `{"version":1,"generation":0,"units":[]}`)
			if err := os.Symlink(target, path); err != nil {
				t.Fatalf("Symlink() error = %v", err)
			}
		}},
		{name: "permissive mode", prepare: func(t *testing.T, path string) {
			writeLeaseTestFile(t, path, `{"version":1,"generation":0,"units":[]}`)
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatalf("Chmod() error = %v", err)
			}
		}},
		{name: "unknown field", prepare: func(t *testing.T, path string) {
			writeLeaseTestFile(t, path, `{"version":1,"generation":0,"units":[],"surprise":true}`)
		}},
		{name: "duplicate field", prepare: func(t *testing.T, path string) {
			writeLeaseTestFile(t, path, `{"version":1,"version":1,"generation":0,"units":[]}`)
		}},
		{name: "unsupported version", prepare: func(t *testing.T, path string) {
			writeLeaseTestFile(t, path, `{"version":2,"generation":0,"units":[]}`)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(privateLeaseTestDirectory(t), "leases.json")
			test.prepare(t, path)
			if _, err := newFileLeaseJournalStore(path).Load(); err == nil {
				t.Fatal("Load() error = nil")
			}
		})
	}
}

func TestFileLeaseJournalEnforcesItsConfiguredOwner(t *testing.T) {
	path := filepath.Join(privateLeaseTestDirectory(t), "leases.json")
	store := newFileLeaseJournalStoreForOwner(path, os.Geteuid()+1)
	if _, err := store.Load(); err == nil {
		t.Fatal("Load() accepted a lease directory owned by a different UID")
	}
}

func TestFileLeaseJournalRemovalSyncsTheEmptyState(t *testing.T) {
	path := filepath.Join(privateLeaseTestDirectory(t), "leases.json")
	store := newFileLeaseJournalStore(path)
	if err := store.Save(testDurableJournal(t)); err != nil {
		t.Fatalf("Save(non-empty) error = %v", err)
	}
	if err := store.Save(durableLeaseJournal{Version: leaseJournalVersion, Generation: 2}); err != nil {
		t.Fatalf("Save(empty) error = %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty journal remained, err = %v", err)
	}
}

func testDurableJournal(t *testing.T) durableLeaseJournal {
	t.Helper()
	identity := fakeUnit("user-1001.slice", "/user.slice/user-1001.slice", 1003)
	parsed, err := parseUnitIdentity("user-1001.slice", identity.listed.objectPath, identity.unit, identity.slice, fakeKernelIdentity("/user.slice/user-1001.slice"))
	if err != nil {
		t.Fatalf("parseUnitIdentity() error = %v", err)
	}
	return durableLeaseJournal{
		Version: leaseJournalVersion, Generation: 1,
		Units: []durableUnitLease{{
			Unit: "user-1001.slice", Identity: identityToDurable(parsed), Phase: leasePhaseApplied,
			Properties: []durablePropertyLease{{Property: PropertyCPUWeight, Baseline: SystemdUnset, PreviousApplied: SystemdUnset, LastApplied: 321}},
			Footprint:  []durableFileFingerprint{{Path: managedRuntimeDropInPath("user-1001.slice", PropertyCPUWeight), SHA256: strings.Repeat("a", 64)}},
		}},
	}
}

func privateLeaseTestDirectory(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatalf("Chmod(test base) error = %v", err)
	}
	dir := filepath.Join(base, "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	return dir
}

func writeLeaseTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
