/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <https://www.gnu.org/licenses/>.
 */
package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSaveToFileReleasesConfigLockBeforeFilesystemIO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resman.conf")
	if err := os.WriteFile(path, []byte("USER_INCLUDE_LIST=^old$\nUSER_EXCLUDE_LIST=^old$\n"), 0600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	cfg := DefaultConfig()
	cfg.UserIncludeList = []string{"^saved$"}
	enteredIO := make(chan struct{})
	releaseIO := make(chan struct{})
	release := newTestGateRelease(releaseIO)
	defer release()
	var blockOnce sync.Once
	writer := func(target string, content []byte, metadata configFileMetadata) (bool, error) {
		shouldBlock := false
		blockOnce.Do(func() {
			shouldBlock = true
			close(enteredIO)
		})
		if shouldBlock {
			<-releaseIO
		}
		return writeFileAtomically(target, content, metadata)
	}

	done := make(chan error, 1)
	go func() {
		done <- cfg.saveToFileWithWriter(path, writer)
	}()
	waitForTestSignal(t, enteredIO, "filesystem I/O to start")

	configUpdated := make(chan struct{})
	go func() {
		cfg.mu.Lock()
		cfg.CPUThreshold = 88
		cfg.mu.Unlock()
		close(configUpdated)
	}()
	select {
	case <-configUpdated:
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("Config.mu remained locked while filesystem I/O was blocked")
	}
	if got := cfg.GetCPUThreshold(); got != 88 {
		release()
		t.Fatalf("GetCPUThreshold() = %d, want concurrent update 88", got)
	}

	release()
	if err := <-done; err != nil {
		t.Fatalf("saveToFileWithWriter() error = %v", err)
	}
}

func TestPersistenceCoordinatorSerializesReloadEpochsWithoutLostFilterUpdates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resman.conf")
	if err := os.WriteFile(path, []byte("USER_INCLUDE_LIST=^old-include$\nUSER_EXCLUDE_LIST=^old-exclude$\n"), 0600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	effective := DefaultConfig()
	effective.UserIncludeList = []string{"^old-include$"}
	effective.UserExcludeList = []string{"^old-exclude$"}
	requested := DefaultConfig()
	requested.UserIncludeList = []string{"^old-include$"}
	requested.UserExcludeList = []string{"^old-exclude$"}
	if _, err := ApplyReloadLifecycle(effective, requested); err != nil {
		t.Fatalf("ApplyReloadLifecycle() error = %v", err)
	}

	includeWriteEntered := make(chan struct{})
	releaseIncludeWrite := make(chan struct{})
	releaseInclude := newTestGateRelease(releaseIncludeWrite)
	defer releaseInclude()
	var blockOnce sync.Once
	writer := func(target string, content []byte, metadata configFileMetadata) (bool, error) {
		body := string(content)
		if target == path && strings.Contains(body, "USER_INCLUDE_LIST=^new-include$") {
			shouldBlock := false
			blockOnce.Do(func() {
				shouldBlock = true
				close(includeWriteEntered)
			})
			if shouldBlock {
				<-releaseIncludeWrite
			}
		}
		return writeFileAtomically(target, content, metadata)
	}

	includeDone := make(chan error, 1)
	go func() {
		_, err := effective.persistUserFilterWithWriter(
			[]string{"^new-include$"}, path, userFilterInclude, writer,
		)
		includeDone <- err
	}()
	waitForTestSignal(t, includeWriteEntered, "include persistence to reach the active file")
	sharedMu := requested.persistenceMutex()
	if sharedMu.TryLock() {
		sharedMu.Unlock()
		releaseInclude()
		t.Fatal("reloaded configuration did not share the locked persistence coordinator")
	}

	excludeDone := make(chan error, 1)
	go func() {
		_, err := requested.persistUserFilterWithWriter(
			[]string{"^new-exclude$"}, path, userFilterExclude, writer,
		)
		excludeDone <- err
	}()

	releaseInclude()
	if err := <-includeDone; err != nil {
		t.Fatalf("include persistence error = %v", err)
	}
	if err := <-excludeDone; err != nil {
		t.Fatalf("exclude persistence error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if !strings.Contains(string(content), "USER_INCLUDE_LIST=^new-include$") ||
		!strings.Contains(string(content), "USER_EXCLUDE_LIST=^new-exclude$") {
		t.Fatalf("persisted filters = %q, want both concurrent updates", content)
	}
}

func TestFailedPersistenceCannotRollbackNewerRuntimeSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resman.conf")
	if err := os.WriteFile(path, []byte("USER_INCLUDE_LIST=^old$\n"), 0600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	cfg := DefaultConfig()
	cfg.UserIncludeList = []string{"^old$"}
	enteredIO := make(chan struct{})
	releaseIO := make(chan struct{})
	release := newTestGateRelease(releaseIO)
	defer release()
	writer := func(string, []byte, configFileMetadata) (bool, error) {
		close(enteredIO)
		<-releaseIO
		return false, errors.New("injected persistence failure")
	}

	done := make(chan error, 1)
	go func() {
		_, err := cfg.persistUserFilterWithWriter(
			[]string{"^stale-request$"}, path, userFilterInclude, writer,
		)
		done <- err
	}()
	waitForTestSignal(t, enteredIO, "failed persistence to enter filesystem I/O")

	runtimeUpdated := make(chan struct{})
	go func() {
		cfg.mu.Lock()
		cfg.UserIncludeList = []string{"^newer-runtime$"}
		cfg.mu.Unlock()
		close(runtimeUpdated)
	}()
	waitForTestSignal(t, runtimeUpdated, "newer runtime update while persistence was blocked")
	release()

	if err := <-done; err == nil || !strings.Contains(err.Error(), "injected persistence failure") {
		t.Fatalf("persistUserFilterWithWriter() error = %v, want injected failure", err)
	}
	if got := cfg.GetUserIncludeList(); len(got) != 1 || got[0] != "^newer-runtime$" {
		t.Fatalf("runtime include list = %v, want newer update preserved", got)
	}
}

func waitForTestSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func newTestGateRelease(gate chan struct{}) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			close(gate)
		})
	}
}

func TestSaveToFileSecurityContract(t *testing.T) {
	tests := []struct {
		name          string
		createSource  bool
		sourceMode    os.FileMode
		wantMode      os.FileMode
		wantBackup    bool
		wantBackupRaw string
	}{
		{
			name:          "existing restrictive file preserves metadata",
			createSource:  true,
			sourceMode:    0400,
			wantMode:      0400,
			wantBackup:    true,
			wantBackupRaw: "MCP_AUTH_TOKEN=top-secret\nUSER_INCLUDE_LIST=^old$\n",
		},
		{
			name:       "new file defaults to owner only",
			wantMode:   0600,
			wantBackup: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "resman.conf")
			var sourceUID, sourceGID uint32
			if tt.createSource {
				if err := os.WriteFile(path, []byte(tt.wantBackupRaw), tt.sourceMode); err != nil {
					t.Fatalf("os.WriteFile() error = %v", err)
				}
				if err := os.Chmod(path, tt.sourceMode); err != nil {
					t.Fatalf("os.Chmod() error = %v", err)
				}
				sourceUID, sourceGID = fileOwnership(t, path)
			}

			cfg := DefaultConfig()
			cfg.UserIncludeList = []string{"^service$"}
			cfg.UserExcludeList = []string{"^blocked$"}
			if err := cfg.SaveToFile(path); err != nil {
				t.Fatalf("SaveToFile() error = %v", err)
			}

			assertFileMode(t, path, tt.wantMode)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("os.ReadFile(config) error = %v", err)
			}
			if !strings.Contains(string(content), "USER_INCLUDE_LIST=^service$") ||
				!strings.Contains(string(content), "USER_EXCLUDE_LIST=^blocked$") {
				t.Fatalf("saved config does not contain updated filters: %q", content)
			}
			assertNoAtomicTemps(t, path)

			backupPath := path + configBackupSuffix
			if !tt.wantBackup {
				if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
					t.Fatalf("backup exists for a new config: %v", err)
				}
				return
			}

			assertFileMode(t, backupPath, tt.wantMode)
			backup, err := os.ReadFile(backupPath)
			if err != nil {
				t.Fatalf("os.ReadFile(backup) error = %v", err)
			}
			if string(backup) != tt.wantBackupRaw {
				t.Fatalf("backup = %q, want %q", backup, tt.wantBackupRaw)
			}
			configUID, configGID := fileOwnership(t, path)
			backupUID, backupGID := fileOwnership(t, backupPath)
			if configUID != sourceUID || configGID != sourceGID {
				t.Errorf("config ownership = %d:%d, want %d:%d", configUID, configGID, sourceUID, sourceGID)
			}
			if backupUID != sourceUID || backupGID != sourceGID {
				t.Errorf("backup ownership = %d:%d, want %d:%d", backupUID, backupGID, sourceUID, sourceGID)
			}
		})
	}
}

func TestSaveToFileKeepsOneRollingBackupAndPrunesLegacyArtifacts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resman.conf")
	original := "MCP_AUTH_TOKEN=first-secret\nUSER_INCLUDE_LIST=^first$\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatalf("os.WriteFile(config) error = %v", err)
	}

	legacyPaths := []string{
		path + ".backup_20260821_010101",
		path + ".backup_20260821_020202",
		path + legacyTempSuffix,
	}
	for _, legacyPath := range legacyPaths {
		if err := os.WriteFile(legacyPath, []byte("MCP_AUTH_TOKEN=legacy-secret\n"), 0644); err != nil {
			t.Fatalf("os.WriteFile(%s) error = %v", legacyPath, err)
		}
	}

	cfg := DefaultConfig()
	cfg.UserIncludeList = []string{"^second$"}
	if err := cfg.SaveToFile(path); err != nil {
		t.Fatalf("first SaveToFile() error = %v", err)
	}
	firstSaved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(first saved config) error = %v", err)
	}

	for _, legacyPath := range legacyPaths {
		if _, err := os.Lstat(legacyPath); !os.IsNotExist(err) {
			t.Errorf("legacy artifact %s was not removed: %v", legacyPath, err)
		}
	}

	cfg.UserIncludeList = []string{"^third$"}
	if err := cfg.SaveToFile(path); err != nil {
		t.Fatalf("second SaveToFile() error = %v", err)
	}
	backup, err := os.ReadFile(path + configBackupSuffix)
	if err != nil {
		t.Fatalf("os.ReadFile(rolling backup) error = %v", err)
	}
	if string(backup) != string(firstSaved) {
		t.Fatalf("rolling backup = %q, want previous config %q", backup, firstSaved)
	}
	assertFileMode(t, path+configBackupSuffix, 0600)

	backups, err := filepath.Glob(path + ".backup*")
	if err != nil {
		t.Fatalf("filepath.Glob() error = %v", err)
	}
	if len(backups) != 1 || backups[0] != path+configBackupSuffix {
		t.Fatalf("backup set = %v, want only %s", backups, path+configBackupSuffix)
	}
	assertNoAtomicTemps(t, path)
}

func TestSaveToFileRestoresOriginalAfterPostRenameSyncFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resman.conf")
	original := []byte("MCP_AUTH_TOKEN=original-secret\nUSER_INCLUDE_LIST=^old$\n")
	if err := os.WriteFile(path, original, 0400); err != nil {
		t.Fatalf("os.WriteFile(config) error = %v", err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatalf("os.Chmod(config) error = %v", err)
	}

	cfg := DefaultConfig()
	cfg.UserIncludeList = []string{"^new$"}
	writeCalls := 0
	writer := func(target string, content []byte, metadata configFileMetadata) (bool, error) {
		writeCalls++
		if writeCalls == 2 {
			return writeFileAtomicallyWithSync(target, content, metadata, func(string) error {
				return errors.New("injected parent sync failure")
			})
		}
		return writeFileAtomically(target, content, metadata)
	}

	err := cfg.saveToFileWithWriter(path, writer)
	if err == nil || !strings.Contains(err.Error(), "injected parent sync failure") {
		t.Fatalf("saveToFileWithWriter() error = %v, want injected sync failure", err)
	}
	if writeCalls != 3 {
		t.Fatalf("atomic write calls = %d, want backup, replacement, and restore", writeCalls)
	}
	restored, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("os.ReadFile(restored config) error = %v", readErr)
	}
	if string(restored) != string(original) {
		t.Fatalf("restored config = %q, want %q", restored, original)
	}
	assertFileMode(t, path, 0400)
	assertNoAtomicTemps(t, path)
}

func TestSaveToFileRemovesNewFileAfterPostRenameSyncFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resman.conf")
	cfg := DefaultConfig()
	cfg.UserIncludeList = []string{"^new$"}

	writer := func(target string, content []byte, metadata configFileMetadata) (bool, error) {
		return writeFileAtomicallyWithSync(target, content, metadata, func(string) error {
			return errors.New("injected parent sync failure")
		})
	}

	err := cfg.saveToFileWithWriter(path, writer)
	if err == nil || !strings.Contains(err.Error(), "injected parent sync failure") {
		t.Fatalf("saveToFileWithWriter() error = %v, want injected sync failure", err)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatalf("new config remains after durability failure: %v", statErr)
	}
	if _, statErr := os.Lstat(path + configBackupSuffix); !os.IsNotExist(statErr) {
		t.Fatalf("backup exists for failed new config creation: %v", statErr)
	}
	assertNoAtomicTemps(t, path)
}

func TestWriteFileAtomicallyCleansTemporaryFileAfterRenameFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "occupied")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "child"), []byte("keep"), 0600); err != nil {
		t.Fatalf("os.WriteFile(child) error = %v", err)
	}

	committed, err := writeFileAtomically(target, []byte("MCP_AUTH_TOKEN=secret\n"), configFileMetadata{mode: 0600})
	if err == nil {
		t.Fatal("writeFileAtomically() succeeded over a non-empty directory")
	}
	if committed {
		t.Fatal("writeFileAtomically() reported committed after rename failure")
	}
	assertNoAtomicTemps(t, target)
	if content, readErr := os.ReadFile(filepath.Join(target, "child")); readErr != nil || string(content) != "keep" {
		t.Fatalf("target directory changed after failure: content=%q error=%v", content, readErr)
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("os.Stat(%s) error = %v", path, err)
	}
	if got := info.Mode().Perm(); got != want.Perm() {
		t.Errorf("mode(%s) = %04o, want %04o", path, got, want.Perm())
	}
}

func fileOwnership(t *testing.T, path string) (uint32, uint32) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("os.Stat(%s) error = %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("os.Stat(%s) returned unsupported ownership metadata", path)
	}
	return stat.Uid, stat.Gid
}

func assertNoAtomicTemps(t *testing.T, path string) {
	t.Helper()
	pattern := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	temps, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("filepath.Glob(%s) error = %v", pattern, err)
	}
	if len(temps) != 0 {
		t.Errorf("temporary files remain: %v", temps)
	}
}
