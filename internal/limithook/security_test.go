package limithook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateScriptPathRejectsSymlinksAndWritableComponents(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := ScriptIdentity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	trusted := filepath.Join(root, "trusted")
	if err := os.Mkdir(trusted, 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(trusted, "hook.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ValidateScriptPath(script, identity); err != nil {
		t.Fatalf("ValidateScriptPath() rejected trusted script: %v", err)
	}

	symlink := filepath.Join(trusted, "hook-link")
	if err := os.Symlink(script, symlink); err != nil {
		t.Fatal(err)
	}
	if err := ValidateScriptPath(symlink, identity); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("ValidateScriptPath() symlink error = %v", err)
	}
	if err := os.Chmod(trusted, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := ValidateScriptPath(script, identity); err == nil || !strings.Contains(err.Error(), "writable by group") {
		t.Fatalf("ValidateScriptPath() writable ancestor error = %v", err)
	}
}

func TestResolveScriptIdentityRejectsRoot(t *testing.T) {
	_, err := ResolveScriptIdentity("root", "root")
	if err == nil || !strings.Contains(err.Error(), "must not use root") {
		t.Fatalf("ResolveScriptIdentity(root, root) error = %v", err)
	}
}
