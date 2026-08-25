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
	"strings"
	"syscall"
	"time"
)

const (
	configBackupSuffix = ".backup"
	legacyBackupMarker = ".backup_"
	legacyTempSuffix   = ".tmp"
	defaultConfigMode  = os.FileMode(0600)
)

type configFileMetadata struct {
	mode         os.FileMode
	uid          int
	gid          int
	hasOwnership bool
}

type atomicFileWriter func(string, []byte, configFileMetadata) (bool, error)
type committedFileRemover func(string) (bool, error)
type legacyArtifactRemover func(string) error
type parentDirectorySyncer func(string) error

// PersistenceResult reports filesystem cleanup performed while persisting a
// configuration. RemovedLegacyArtifacts contains basenames only and never file
// contents or configuration values.
type PersistenceResult struct {
	RemovedLegacyArtifacts []string
}

type configPersistenceState struct {
	unusableErr error
}

type configPersistenceUnusableError struct {
	detail   string
	recovery string
	cause    error
}

func (e *configPersistenceUnusableError) Error() string {
	return fmt.Sprintf(
		"%s; active file content may differ from the unchanged runtime configuration; %s: %v",
		e.detail,
		e.recovery,
		e.cause,
	)
}

func (e *configPersistenceUnusableError) Unwrap() error {
	return e.cause
}

func readConfigFile(path string) (configFileMetadata, []byte, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return configFileMetadata{mode: defaultConfigMode}, nil, false, nil
		}
		return configFileMetadata{}, nil, false, fmt.Errorf("failed to inspect config path %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return configFileMetadata{}, nil, false, fmt.Errorf(
			"config path %s is a symbolic link; replace it with a regular file or select its target with --config",
			path,
		)
	}
	if !info.Mode().IsRegular() {
		return configFileMetadata{}, nil, false, fmt.Errorf("config path %s is not a regular file", path)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return configFileMetadata{}, nil, false, fmt.Errorf("failed to read config file: %w", err)
	}

	metadata := configFileMetadata{mode: info.Mode().Perm()}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		metadata.uid = int(stat.Uid)
		metadata.gid = int(stat.Gid)
		metadata.hasOwnership = true
	}
	return metadata, content, true, nil
}

// writeFileAtomically writes and syncs a temporary file before replacing path.
// The committed result is true when rename succeeded but parent sync failed.
func writeFileAtomically(path string, content []byte, metadata configFileMetadata) (committed bool, err error) {
	return writeFileAtomicallyWithSync(path, content, metadata, syncParentDirectory)
}

func writeFileAtomicallyWithSync(path string, content []byte, metadata configFileMetadata, syncDir func(string) error) (committed bool, err error) {
	dir := filepath.Dir(path)
	prefix := "." + filepath.Base(path) + ".tmp-"
	tmp, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return false, fmt.Errorf("failed to create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()

	// Apply final metadata before writing secret-bearing content. CreateTemp
	// starts at 0600, so the empty file is never exposed with broad access.
	if err := applyConfigFileMetadata(tmp, path, metadata); err != nil {
		return false, err
	}
	if _, err := tmp.Write(content); err != nil {
		return false, fmt.Errorf("failed to write temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return false, fmt.Errorf("failed to sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("failed to close temporary file: %w", err)
	}
	closed = true

	if err := os.Rename(tmpPath, path); err != nil {
		return false, fmt.Errorf("failed to replace %s: %w", path, err)
	}
	committed = true
	if err := syncDir(path); err != nil {
		return true, err
	}
	return true, nil
}

func applyConfigFileMetadata(file *os.File, targetPath string, metadata configFileMetadata) error {
	return applyConfigFileMetadataWithChown(file, targetPath, metadata, file.Chown)
}

func applyConfigFileMetadataWithChown(
	file *os.File,
	targetPath string,
	metadata configFileMetadata,
	chown func(int, int) error,
) error {
	if metadata.hasOwnership {
		info, err := file.Stat()
		if err != nil {
			return fmt.Errorf("failed to inspect temporary file ownership: %w", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != metadata.uid || int(stat.Gid) != metadata.gid {
			if err := chown(metadata.uid, metadata.gid); err != nil {
				return fmt.Errorf(
					"cannot preserve required ownership %d:%d for %s while running as %d:%d; grant resman permission to chown the configuration or change the source ownership before retrying: %w",
					metadata.uid,
					metadata.gid,
					targetPath,
					os.Geteuid(),
					os.Getegid(),
					err,
				)
			}
		}
	}
	if err := file.Chmod(metadata.mode.Perm()); err != nil {
		return fmt.Errorf("failed to preserve file permissions: %w", err)
	}
	return nil
}

// removeLegacyConfigArtifactsBeside removes artifacts written beside a selected
// configuration by version 1.25.1 and earlier. It remains necessary for custom
// paths; default-layout legacy artifacts are refused at startup and never
// deleted automatically.
func removeLegacyConfigArtifactsBeside(path string) (PersistenceResult, error) {
	return removeLegacyConfigArtifactsBesideWith(path, os.Remove, syncParentDirectory)
}

func removeLegacyConfigArtifactsBesideWith(
	path string,
	remove legacyArtifactRemover,
	syncParent parentDirectorySyncer,
) (PersistenceResult, error) {
	result := PersistenceResult{}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return result, fmt.Errorf("failed to inspect config directory for legacy artifacts: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if !isGeneratedLegacyConfigArtifact(base, name) {
			continue
		}
		if entry.IsDir() {
			return result, fmt.Errorf("legacy config artifact %s is a directory", filepath.Join(dir, name))
		}
		if err := remove(filepath.Join(dir, name)); err != nil {
			return result, fmt.Errorf("failed to remove legacy config artifact %s: %w", name, err)
		}
		result.RemovedLegacyArtifacts = append(result.RemovedLegacyArtifacts, name)
	}
	if len(result.RemovedLegacyArtifacts) > 0 {
		if err := syncParent(path); err != nil {
			return result, err
		}
	}
	return result, nil
}

func isGeneratedLegacyConfigArtifact(base, name string) bool {
	if name == base+legacyTempSuffix {
		return true
	}
	timestamp, found := strings.CutPrefix(name, base+legacyBackupMarker)
	if !found {
		return false
	}
	parsed, err := time.Parse("20060102_150405", timestamp)
	return err == nil && parsed.Format("20060102_150405") == timestamp
}

func removeCommittedFile(path string) (bool, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return true, syncParentDirectory(path)
}

func syncParentDirectory(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("failed to open parent directory: %w", err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil || closeErr != nil {
		return errors.Join(
			wrapOptionalError("failed to sync parent directory", syncErr),
			wrapOptionalError("failed to close parent directory", closeErr),
		)
	}
	return nil
}

func wrapOptionalError(message string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", message, err)
}
