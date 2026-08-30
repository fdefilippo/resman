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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestPersistUserFilterReleasesConfigLockBeforeFilesystemIO(t *testing.T) {
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
		_, err := cfg.persistUserFilterWithWriter(
			[]string{"^saved$"}, path, userFilterInclude, writer,
		)
		done <- err
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
		t.Fatalf("persistUserFilterWithWriter() error = %v", err)
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

func TestPersistUserFilterRejectsConfigSymlinks(t *testing.T) {
	tests := []struct {
		name         string
		createTarget bool
	}{
		{name: "regular target", createTarget: true},
		{name: "dangling target"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "managed-target.conf")
			path := filepath.Join(dir, "resman.conf")
			original := []byte("MCP_AUTH_TOKEN=target-secret\nUSER_INCLUDE_LIST=^old$\n")
			if tt.createTarget {
				if err := os.WriteFile(target, original, 0600); err != nil {
					t.Fatalf("os.WriteFile(target) error = %v", err)
				}
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatalf("os.Symlink() error = %v", err)
			}

			cfg := DefaultConfig()
			_, err := cfg.PersistUserIncludeList([]string{"^new$"}, path)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("PersistUserIncludeList() error = %v, want named symbolic-link rejection", err)
			}

			info, lstatErr := os.Lstat(path)
			if lstatErr != nil {
				t.Fatalf("os.Lstat(config symlink) error = %v", lstatErr)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("config path mode = %v, want original symbolic link", info.Mode())
			}
			if tt.createTarget {
				content, readErr := os.ReadFile(target)
				if readErr != nil || string(content) != string(original) {
					t.Fatalf("symlink target changed: content=%q error=%v", content, readErr)
				}
			}
			if _, statErr := os.Lstat(path + configBackupSuffix); !os.IsNotExist(statErr) {
				t.Fatalf("backup created for rejected symlink: %v", statErr)
			}
			assertNoAtomicTemps(t, path)
		})
	}
}

func TestApplyConfigFileMetadataOwnershipFailureIsActionable(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "metadata-*")
	if err != nil {
		t.Fatalf("os.CreateTemp() error = %v", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("file.Close() error = %v", err)
		}
	}()

	uid, gid := fileOwnership(t, file.Name())
	requiredUID := int(uid) + 1
	targetPath := filepath.Join(filepath.Dir(file.Name()), "resman.conf")
	injectedErr := errors.New("injected chown failure")
	var requestedUID, requestedGID int
	err = applyConfigFileMetadataWithChown(
		file,
		targetPath,
		configFileMetadata{
			mode:         0600,
			uid:          requiredUID,
			gid:          int(gid),
			hasOwnership: true,
		},
		func(uid, gid int) error {
			requestedUID = uid
			requestedGID = gid
			return injectedErr
		},
	)
	if !errors.Is(err, injectedErr) {
		t.Fatalf("applyConfigFileMetadataWithChown() error = %v, want injected error", err)
	}
	if requestedUID != requiredUID || requestedGID != int(gid) {
		t.Fatalf("requested ownership = %d:%d, want %d:%d", requestedUID, requestedGID, requiredUID, gid)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		t.Fatalf("file.Stat() after ownership failure error = %v", statErr)
	}
	if info.Size() != 0 {
		t.Fatalf("temporary file size after ownership failure = %d, want 0 before secret content", info.Size())
	}
	for _, fragment := range []string{
		targetPath,
		fmt.Sprintf("%d:%d", requiredUID, gid),
		"grant resman permission to chown",
		"change the source ownership",
	} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("ownership error %q does not contain %q", err, fragment)
		}
	}
}

func TestPersistUserFilterDurabilityFailureReportsDeterministicRuntimeAndDiskState(t *testing.T) {
	tests := []struct {
		name                     string
		failRollbackSync         bool
		failRollbackBeforeRename bool
		wantRollbackMessage      bool
		wantDivergenceMessage    bool
	}{
		{name: "rollback sync succeeds"},
		{name: "rollback sync also fails", failRollbackSync: true, wantRollbackMessage: true},
		{
			name:                     "rollback fails before rename",
			failRollbackBeforeRename: true,
			wantDivergenceMessage:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
			cfg.UserIncludeList = []string{"^old$"}
			activeWrites := 0
			writer := func(target string, content []byte, metadata configFileMetadata) (bool, error) {
				if target != path {
					return writeFileAtomically(target, content, metadata)
				}
				activeWrites++
				if activeWrites == 2 && tt.failRollbackBeforeRename {
					return false, errors.New("injected rollback failure before rename")
				}
				failSync := activeWrites == 1 || (activeWrites == 2 && tt.failRollbackSync)
				if failSync {
					return writeFileAtomicallyWithSync(target, content, metadata, func(string) error {
						return fmt.Errorf("injected parent sync failure %d", activeWrites)
					})
				}
				return writeFileAtomically(target, content, metadata)
			}

			previous, err := cfg.persistUserFilterWithWriter(
				[]string{"^new$"}, path, userFilterInclude, writer,
			)
			if err == nil || !strings.Contains(err.Error(), "injected parent sync failure 1") {
				t.Fatalf("persistUserFilterWithWriter() error = %v, want replacement sync failure", err)
			}
			if got := strings.Contains(err.Error(), "failed to confirm rollback durability"); got != tt.wantRollbackMessage {
				t.Fatalf("rollback durability message present = %t, want %t; error=%v", got, tt.wantRollbackMessage, err)
			}
			if got := strings.Contains(err.Error(), "stop resman"); got != tt.wantDivergenceMessage {
				t.Fatalf("divergence remedy present = %t, want %t; error=%v", got, tt.wantDivergenceMessage, err)
			}
			if activeWrites != 2 {
				t.Fatalf("active-file writes = %d, want replacement and rollback", activeWrites)
			}
			if len(previous.PreviousValue) != 1 || previous.PreviousValue[0] != "^old$" {
				t.Fatalf("previous filter = %v, want [^old$]", previous.PreviousValue)
			}
			if got := cfg.GetUserIncludeList(); len(got) != 1 || got[0] != "^old$" {
				t.Fatalf("runtime filter = %v, want unchanged [^old$]", got)
			}

			readable, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("os.ReadFile(active config) error = %v", readErr)
			}
			if tt.wantDivergenceMessage {
				if !strings.Contains(string(readable), "USER_INCLUDE_LIST=^new$") {
					t.Fatalf("active config = %q, want requested content after pre-rename rollback failure", readable)
				}
			} else if string(readable) != string(original) {
				t.Fatalf("readable config diverged: content=%q, want %q", readable, original)
			}
			backup, backupErr := os.ReadFile(path + configBackupSuffix)
			if backupErr != nil || string(backup) != string(original) {
				t.Fatalf("backup = %q error=%v, want %q", backup, backupErr, original)
			}
			if tt.wantDivergenceMessage {
				reloaded := DefaultConfig()
				if _, lifecycleErr := ApplyReloadLifecycle(cfg, reloaded); lifecycleErr != nil {
					t.Fatalf("ApplyReloadLifecycle() error = %v", lifecycleErr)
				}
				writerCalled := false
				_, retryErr := reloaded.persistUserFilterWithWriter(
					[]string{"^retry$"},
					path,
					userFilterInclude,
					func(string, []byte, configFileMetadata) (bool, error) {
						writerCalled = true
						return false, errors.New("writer must remain blocked")
					},
				)
				if retryErr == nil || !strings.Contains(retryErr.Error(), "persistence is unavailable") ||
					!strings.Contains(retryErr.Error(), path+configBackupSuffix) {
					t.Fatalf("retry error = %v, want shared unusable state and recovery path", retryErr)
				}
				if writerCalled {
					t.Fatal("persistence writer ran after the coordinator entered an unusable state")
				}
			}
			assertFileMode(t, path, 0400)
			assertNoAtomicTemps(t, path)
		})
	}
}

func TestUserFilterSnapshotPersistenceSecurityContract(t *testing.T) {
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

			snapshot := userFilterPersistenceSnapshot{
				include:      []string{"^service$"},
				exclude:      []string{"^blocked$"},
				writeInclude: true,
				writeExclude: true,
			}
			if _, err := saveUserFilterSnapshotWithWriter(path, snapshot, writeFileAtomically); err != nil {
				t.Fatalf("saveUserFilterSnapshotWithWriter() error = %v", err)
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

func TestSaveToCustomPathKeepsOneRollingBackupAndPrunesAdjacentLegacyArtifacts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom", "resman.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("os.MkdirAll(custom config parent) error = %v", err)
	}
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
	operatorPaths := []string{
		path + ".backup_prima_della_migrazione",
		path + ".backup_20260821_030303_manual",
		path + ".backup_20261321_030303",
	}
	for _, operatorPath := range operatorPaths {
		if err := os.WriteFile(operatorPath, []byte("operator-owned backup\n"), 0600); err != nil {
			t.Fatalf("os.WriteFile(%s) error = %v", operatorPath, err)
		}
	}

	cfg := DefaultConfig()
	result, err := cfg.PersistUserIncludeList([]string{"^second$"}, path)
	if err != nil {
		t.Fatalf("first PersistUserIncludeList() error = %v", err)
	}
	wantRemoved := []string{
		"resman.conf.backup_20260821_010101",
		"resman.conf.backup_20260821_020202",
		"resman.conf.tmp",
	}
	if !slices.Equal(result.RemovedLegacyArtifacts, wantRemoved) {
		t.Fatalf("removed legacy artifacts = %v, want %v", result.RemovedLegacyArtifacts, wantRemoved)
	}
	if strings.Contains(strings.Join(result.RemovedLegacyArtifacts, ","), "legacy-secret") {
		t.Fatalf("persistence result exposed file contents: %v", result.RemovedLegacyArtifacts)
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
	for _, operatorPath := range operatorPaths {
		if _, err := os.Lstat(operatorPath); err != nil {
			t.Errorf("operator-owned backup %s was removed: %v", operatorPath, err)
		}
	}

	result, err = cfg.PersistUserIncludeList([]string{"^third$"}, path)
	if err != nil {
		t.Fatalf("second PersistUserIncludeList() error = %v", err)
	}
	if len(result.RemovedLegacyArtifacts) != 0 {
		t.Fatalf("second persistence removed legacy artifacts = %v, want none", result.RemovedLegacyArtifacts)
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
	wantBackupCount := 1 + len(operatorPaths)
	if len(backups) != wantBackupCount {
		t.Fatalf("backup set = %v, want rolling backup plus %d operator-owned files", backups, len(operatorPaths))
	}
	assertNoAtomicTemps(t, path)
}

func TestGeneratedLegacyConfigArtifactMatchingIsExact(t *testing.T) {
	tests := []struct {
		name      string
		candidate string
		want      bool
	}{
		{name: "historical timestamp backup", candidate: "resman.conf.backup_20260821_010101", want: true},
		{name: "historical temporary file", candidate: "resman.conf.tmp", want: true},
		{name: "operator named backup", candidate: "resman.conf.backup_prima_della_migrazione"},
		{name: "timestamp with suffix", candidate: "resman.conf.backup_20260821_010101_manual"},
		{name: "invalid timestamp", candidate: "resman.conf.backup_20261321_010101"},
		{name: "different config basename", candidate: "other.conf.backup_20260821_010101"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isGeneratedLegacyConfigArtifact("resman.conf", tt.candidate); got != tt.want {
				t.Fatalf("isGeneratedLegacyConfigArtifact() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestLegacyConfigArtifactCleanupReportsPartialFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resman.conf")
	names := []string{
		"resman.conf.backup_20260821_010101",
		"resman.conf.backup_20260821_020202",
		"resman.conf.tmp",
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("MCP_AUTH_TOKEN=must-not-appear\n"), 0600); err != nil {
			t.Fatalf("os.WriteFile(%s) error = %v", name, err)
		}
	}
	injectedErr := errors.New("injected removal failure")
	syncCalled := false
	result, err := removeLegacyConfigArtifactsBesideWith(
		path,
		func(candidate string) error {
			if filepath.Base(candidate) == names[1] {
				return injectedErr
			}
			return os.Remove(candidate)
		},
		func(string) error {
			syncCalled = true
			return nil
		},
	)
	if !errors.Is(err, injectedErr) {
		t.Fatalf("cleanup error = %v, want injected removal failure", err)
	}
	if !slices.Equal(result.RemovedLegacyArtifacts, names[:1]) {
		t.Fatalf("removed artifacts = %v, want %v", result.RemovedLegacyArtifacts, names[:1])
	}
	if strings.Contains(err.Error()+strings.Join(result.RemovedLegacyArtifacts, ","), "must-not-appear") {
		t.Fatalf("partial cleanup result exposed configuration contents: result=%v error=%v", result, err)
	}
	if syncCalled {
		t.Fatal("parent directory sync ran after partial removal failure")
	}
	if _, statErr := os.Lstat(filepath.Join(dir, names[0])); !os.IsNotExist(statErr) {
		t.Fatalf("first artifact was not removed: %v", statErr)
	}
	for _, name := range names[1:] {
		if _, statErr := os.Lstat(filepath.Join(dir, name)); statErr != nil {
			t.Fatalf("unprocessed artifact %s was removed: %v", name, statErr)
		}
	}
}

func TestUserFilterPersistenceReportsArtifactsRemovedBeforeCleanupFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resman.conf")
	original := []byte("MCP_AUTH_TOKEN=active-secret\nUSER_INCLUDE_LIST=^old$\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatalf("os.WriteFile(config) error = %v", err)
	}
	removedName := "resman.conf.backup_20260821_010101"
	blockedName := "resman.conf.backup_20260821_020202"
	if err := os.WriteFile(filepath.Join(dir, removedName), []byte("MCP_AUTH_TOKEN=legacy-secret\n"), 0600); err != nil {
		t.Fatalf("os.WriteFile(legacy backup) error = %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, blockedName), 0700); err != nil {
		t.Fatalf("os.Mkdir(blocking artifact) error = %v", err)
	}

	cfg := DefaultConfig()
	result, err := cfg.PersistUserIncludeList([]string{"^new$"}, path)
	if err == nil || !strings.Contains(err.Error(), blockedName) || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("PersistUserIncludeList() error = %v, want named directory cleanup failure", err)
	}
	if !slices.Equal(result.RemovedLegacyArtifacts, []string{removedName}) {
		t.Fatalf("PersistUserIncludeList() removed artifacts = %v, want [%s]", result.RemovedLegacyArtifacts, removedName)
	}
	if strings.Contains(err.Error()+strings.Join(result.RemovedLegacyArtifacts, ","), "active-secret") {
		t.Fatalf("partial persistence result exposed configuration contents: result=%v error=%v", result, err)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil || string(content) != string(original) {
		t.Fatalf("active config after cleanup failure = %q error=%v, want original", content, readErr)
	}
}

func TestLegacyConfigArtifactCleanupNoOpDoesNotMutateOperatorBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resman.conf")
	operatorBackup := path + ".backup_keep_for_operator"
	if err := os.WriteFile(operatorBackup, []byte("operator-owned\n"), 0600); err != nil {
		t.Fatalf("os.WriteFile(operator backup) error = %v", err)
	}
	result, err := removeLegacyConfigArtifactsBesideWith(
		path,
		func(string) error {
			t.Fatal("cleanup attempted to remove a non-generated backup")
			return nil
		},
		func(string) error {
			t.Fatal("cleanup synced a directory after removing nothing")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("cleanup no-op error = %v", err)
	}
	if len(result.RemovedLegacyArtifacts) != 0 {
		t.Fatalf("cleanup no-op result = %v, want no removed artifacts", result.RemovedLegacyArtifacts)
	}
	if _, err := os.Lstat(operatorBackup); err != nil {
		t.Fatalf("operator backup changed during cleanup no-op: %v", err)
	}
}

func TestUserFilterPersistenceRestoresOriginalAfterPostRenameSyncFailure(t *testing.T) {
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

	_, err := cfg.persistUserFilterWithWriter(
		[]string{"^new$"}, path, userFilterInclude, writer,
	)
	if err == nil || !strings.Contains(err.Error(), "injected parent sync failure") {
		t.Fatalf("persistUserFilterWithWriter() error = %v, want injected sync failure", err)
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

func TestUserFilterPersistenceRemovesNewFileAfterPostRenameSyncFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resman.conf")
	cfg := DefaultConfig()
	cfg.UserIncludeList = []string{"^new$"}

	writer := func(target string, content []byte, metadata configFileMetadata) (bool, error) {
		return writeFileAtomicallyWithSync(target, content, metadata, func(string) error {
			return errors.New("injected parent sync failure")
		})
	}

	_, err := cfg.persistUserFilterWithWriter(
		[]string{"^new$"}, path, userFilterInclude, writer,
	)
	if err == nil || !strings.Contains(err.Error(), "injected parent sync failure") {
		t.Fatalf("persistUserFilterWithWriter() error = %v, want injected sync failure", err)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatalf("new config remains after durability failure: %v", statErr)
	}
	if _, statErr := os.Lstat(path + configBackupSuffix); !os.IsNotExist(statErr) {
		t.Fatalf("backup exists for failed new config creation: %v", statErr)
	}
	assertNoAtomicTemps(t, path)
}

func TestNewFileCompensationFailureReportsDeterministicState(t *testing.T) {
	tests := []struct {
		name         string
		removed      bool
		wantUnusable bool
	}{
		{name: "file removed but removal durability fails", removed: true},
		{name: "file removal fails", wantUnusable: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "resman.conf")
			snapshot := userFilterPersistenceSnapshot{
				include:      []string{"^new$"},
				writeInclude: true,
				writeExclude: true,
			}
			writer := func(target string, content []byte, metadata configFileMetadata) (bool, error) {
				return writeFileAtomicallyWithSync(target, content, metadata, func(string) error {
					return errors.New("injected creation sync failure")
				})
			}
			remover := func(target string) (bool, error) {
				if tt.removed {
					if err := os.Remove(target); err != nil {
						t.Fatalf("os.Remove(compensated config) error = %v", err)
					}
					return true, errors.New("injected removal sync failure")
				}
				return false, errors.New("injected removal failure")
			}

			_, err := saveUserFilterSnapshot(path, snapshot, writer, remover)
			if err == nil || !strings.Contains(err.Error(), "injected creation sync failure") {
				t.Fatalf("saveUserFilterSnapshot() error = %v, want creation sync failure", err)
			}
			var unusableErr *configPersistenceUnusableError
			if got := errors.As(err, &unusableErr); got != tt.wantUnusable {
				t.Fatalf("unusable persistence state = %t, want %t; error=%v", got, tt.wantUnusable, err)
			}
			if tt.removed {
				if !strings.Contains(err.Error(), "failed to confirm removal durability") {
					t.Fatalf("compensation error = %v, want unconfirmed removal durability", err)
				}
				if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
					t.Fatalf("new config remains after successful removal: %v", statErr)
				}
			} else {
				if !strings.Contains(err.Error(), "stop resman") || !strings.Contains(err.Error(), "remove "+path) {
					t.Fatalf("unusable-state error = %v, want removal and restart remedy", err)
				}
				content, readErr := os.ReadFile(path)
				if readErr != nil || !strings.Contains(string(content), "USER_INCLUDE_LIST=^new$") {
					t.Fatalf("active new config = %q error=%v, want requested content", content, readErr)
				}
			}
			assertNoAtomicTemps(t, path)
		})
	}
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
