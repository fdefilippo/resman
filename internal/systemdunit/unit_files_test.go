package systemdunit

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDefaultMutableUnitFileRootsIncludePersistentOperatorConfiguration(t *testing.T) {
	want := []string{
		"/etc/systemd/system",
		"/run/systemd/system",
		"/etc/systemd/system.control",
		"/run/systemd/system.control",
		"/run/systemd/transient",
	}
	if got := defaultMutableUnitFileRoots(); !reflect.DeepEqual(got, want) {
		t.Fatalf("default mutable unit-file roots = %v, want %v", got, want)
	}
}

func TestLocalUnitFileInspectorSeesFilesNotYetLoadedBySystemd(t *testing.T) {
	root := t.TempDir()
	unitRoot := filepath.Join(root, "etc", "systemd", "system")
	dropInDirectory := filepath.Join(unitRoot, "user-1001.slice.d")
	if err := os.MkdirAll(dropInDirectory, 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	operatorPath := filepath.Join(dropInDirectory, "10-not-yet-loaded.conf")
	if err := os.WriteFile(operatorPath, []byte("[Slice]\nCPUWeight=500\n"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	paths, err := (localUnitFileInspector{roots: []string{unitRoot}}).mutablePaths("user-1001.slice")
	if err != nil {
		t.Fatalf("mutablePaths() error = %v", err)
	}
	if !reflect.DeepEqual(paths, []string{operatorPath}) {
		t.Fatalf("mutable paths = %v, want %s", paths, operatorPath)
	}
}

func TestLocalUnitFileInspectorRejectsNonCanonicalUnits(t *testing.T) {
	if _, err := (localUnitFileInspector{roots: []string{t.TempDir()}}).mutablePaths("ssh.service"); err == nil {
		t.Fatal("mutablePaths() error = nil, want non-canonical unit rejection")
	}
}

func TestAdapterRejectsOperatorDropInVisibleOnlyToTheLocalInspector(t *testing.T) {
	root := t.TempDir()
	unitRoot := filepath.Join(root, "etc", "systemd", "system")
	dropInDirectory := filepath.Join(unitRoot, "user-1001.slice.d")
	if err := os.MkdirAll(dropInDirectory, 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	operatorPath := filepath.Join(dropInDirectory, "10-operator.conf")
	if err := os.WriteFile(operatorPath, []byte("[Slice]\nCPUWeight=500\n"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	transport := newFakeUnitTransport(1001)
	if paths, ok := transport.units["user-1001.slice"].unit["DropInPaths"].([]string); !ok || len(paths) != 0 {
		t.Fatalf("D-Bus fixture unexpectedly exposes drop-ins: %#v", transport.units["user-1001.slice"].unit["DropInPaths"])
	}
	adapter, err := newAdapter(
		context.Background(),
		transport,
		&fakeKernelVerifier{},
		localUnitFileInspector{roots: []string{unitRoot}},
		newMemoryLeaseJournalStore(),
		time.Second,
	)
	if err != nil {
		t.Fatalf("newAdapter() error = %v", err)
	}
	identity := identityFor(t, adapter, 1001)
	_, err = adapter.Apply(context.Background(), identity, []PropertyAssignment{mustAssignment(t, PropertyCPUWeight, 321)})
	assertAdapterReason(t, err, ReasonExternalConflict)
	if len(transport.setCalls) != 0 {
		t.Fatalf("operator drop-in was detected after mutation: %+v", transport.setCalls)
	}
}
