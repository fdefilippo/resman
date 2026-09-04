package systemdunit

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

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
