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
package database

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	privateDatabaseDirectoryMode = os.FileMode(0700)
	privateDatabaseFileMode      = os.FileMode(0600)
)

type sqliteStorageBoundary struct {
	dbPath string
}

type sqliteArtifact struct {
	path string
	kind string
}

func prepareSQLiteStorage(dbPath string) (*sqliteStorageBoundary, error) {
	if dbPath == ":memory:" {
		return nil, nil
	}

	dir := filepath.Dir(dbPath)
	if err := ensurePrivateDatabaseDirectory(dir); err != nil {
		return nil, err
	}

	artifacts := sqliteArtifacts(dbPath)
	for _, artifact := range artifacts {
		if err := validateExistingSQLiteArtifact(artifact); err != nil {
			return nil, err
		}
	}
	if err := ensureMainDatabaseFile(artifacts[0]); err != nil {
		return nil, err
	}

	return &sqliteStorageBoundary{dbPath: dbPath}, nil
}

func ensurePrivateDatabaseDirectory(dir string) error {
	if _, err := os.Lstat(dir); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to inspect database directory %s: %w", dir, err)
		}
		if err := os.MkdirAll(dir, privateDatabaseDirectoryMode); err != nil {
			return fmt.Errorf("failed to create database directory %s: %w", dir, err)
		}
	}
	if err := validatePrivateDatabaseDirectory(dir); err != nil {
		return err
	}
	return validateDatabasePathHierarchy(dir)
}

func validateDatabasePathHierarchy(dir string) error {
	current, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("failed to resolve database directory %s: %w", dir, err)
	}
	childUID := os.Geteuid()
	for {
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}

		info, err := os.Lstat(parent)
		if err != nil {
			return fmt.Errorf("failed to inspect database path ancestor %s: %w", parent, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf(
				"database path ancestor %s is a symbolic link; use a path below a stable, non-symlink directory hierarchy before restarting resman",
				parent,
			)
		}
		if !info.IsDir() {
			return fmt.Errorf("database path ancestor %s is not a directory", parent)
		}
		uid, err := fileOwnerUID(info)
		if err != nil {
			return fmt.Errorf("failed to inspect database path ancestor ownership for %s: %w", parent, err)
		}

		mode := info.Mode()
		untrustedOwnerCanReplace := uid != os.Geteuid() && uid != 0 && mode.Perm()&0200 != 0
		writableByGroupOrOther := mode.Perm()&0022 != 0
		stickyProtectsChild := mode&os.ModeSticky != 0 && (childUID == os.Geteuid() || childUID == 0)
		if untrustedOwnerCanReplace || (writableByGroupOrOther && !stickyProtectsChild) {
			return fmt.Errorf(
				"database path ancestor %s can be replaced by an untrusted user; move METRICS_DB_PATH below a stable hierarchy or remove untrusted write access before restarting resman",
				parent,
			)
		}

		childUID = uid
		current = parent
	}
}

func validatePrivateDatabaseDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("failed to inspect database directory %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(
			"database directory %s is a symbolic link; replace it with a directory owned by UID %d with mode 0700 before restarting resman",
			dir,
			os.Geteuid(),
		)
	}
	if !info.IsDir() {
		return fmt.Errorf(
			"database parent path %s is not a directory; replace it with a directory owned by UID %d with mode 0700 before restarting resman",
			dir,
			os.Geteuid(),
		)
	}
	if info.Mode().Perm() != privateDatabaseDirectoryMode {
		return fmt.Errorf(
			"database directory %s has unsafe mode %04o; set its ownership to UID %d and mode to 0700 before restarting resman",
			dir,
			info.Mode().Perm(),
			os.Geteuid(),
		)
	}
	uid, err := fileOwnerUID(info)
	if err != nil {
		return fmt.Errorf("failed to inspect database directory ownership for %s: %w", dir, err)
	}
	if uid != os.Geteuid() {
		return fmt.Errorf(
			"database directory %s is owned by UID %d while resman runs as UID %d; change its ownership to UID %d and keep mode 0700 before restarting resman",
			dir,
			uid,
			os.Geteuid(),
			os.Geteuid(),
		)
	}
	return nil
}

func ensureMainDatabaseFile(artifact sqliteArtifact) error {
	info, err := os.Lstat(artifact.path)
	if err == nil {
		return validateSQLiteArtifactInfo(artifact, info)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("failed to inspect %s %s: %w", artifact.kind, artifact.path, err)
	}

	flags := os.O_CREATE | os.O_EXCL | os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	file, err := os.OpenFile(artifact.path, flags, privateDatabaseFileMode)
	if err != nil {
		return fmt.Errorf("failed to create %s %s with mode 0600: %w", artifact.kind, artifact.path, err)
	}
	if err := file.Chmod(privateDatabaseFileMode); err != nil {
		_ = file.Close()
		return fmt.Errorf("failed to set mode 0600 on %s %s: %w", artifact.kind, artifact.path, err)
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return fmt.Errorf("failed to inspect newly created %s %s: %w", artifact.kind, artifact.path, statErr)
	}
	if closeErr != nil {
		return fmt.Errorf("failed to close newly created %s %s: %w", artifact.kind, artifact.path, closeErr)
	}
	return validateSQLiteArtifactInfo(artifact, info)
}

func validateExistingSQLiteArtifact(artifact sqliteArtifact) error {
	info, err := os.Lstat(artifact.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to inspect %s %s: %w", artifact.kind, artifact.path, err)
	}
	return validateSQLiteArtifactInfo(artifact, info)
}

func validateSQLiteArtifactInfo(artifact sqliteArtifact, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(
			"%s %s is a symbolic link; replace it with a regular file owned by UID %d with mode 0600 before restarting resman",
			artifact.kind,
			artifact.path,
			os.Geteuid(),
		)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf(
			"%s %s is not a regular file; replace it with a regular file owned by UID %d with mode 0600 before restarting resman",
			artifact.kind,
			artifact.path,
			os.Geteuid(),
		)
	}
	if info.Mode().Perm() != privateDatabaseFileMode {
		return fmt.Errorf(
			"%s %s has unsafe mode %04o; set its ownership to UID %d and mode to 0600 before restarting resman",
			artifact.kind,
			artifact.path,
			info.Mode().Perm(),
			os.Geteuid(),
		)
	}
	uid, err := fileOwnerUID(info)
	if err != nil {
		return fmt.Errorf("failed to inspect %s ownership for %s: %w", artifact.kind, artifact.path, err)
	}
	if uid != os.Geteuid() {
		return fmt.Errorf(
			"%s %s is owned by UID %d while resman runs as UID %d; change its ownership to UID %d and keep mode 0600 before restarting resman",
			artifact.kind,
			artifact.path,
			uid,
			os.Geteuid(),
			os.Geteuid(),
		)
	}
	return nil
}

func fileOwnerUID(info os.FileInfo) (int, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unsupported file metadata type %T", info.Sys())
	}
	return int(stat.Uid), nil
}

func sqliteArtifacts(dbPath string) []sqliteArtifact {
	return []sqliteArtifact{
		{path: dbPath, kind: "SQLite database file"},
		{path: dbPath + "-wal", kind: "SQLite WAL sidecar"},
		{path: dbPath + "-shm", kind: "SQLite shared-memory sidecar"},
	}
}

// secureRuntimeArtifacts normalizes only artifacts created after the initial
// validation. Pre-existing unsafe artifacts are rejected before SQLite opens.
// The private, process-owned parent directory prevents broader SQLite creation
// modes from becoming observable before this normalization runs.
func (s *sqliteStorageBoundary) secureRuntimeArtifacts() error {
	if s == nil {
		return nil
	}
	for _, artifact := range sqliteArtifacts(s.dbPath) {
		info, err := os.Lstat(artifact.path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("failed to inspect runtime %s %s: %w", artifact.kind, artifact.path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return validateSQLiteArtifactInfo(artifact, info)
		}
		uid, err := fileOwnerUID(info)
		if err != nil {
			return fmt.Errorf("failed to inspect runtime %s ownership for %s: %w", artifact.kind, artifact.path, err)
		}
		if uid != os.Geteuid() {
			return validateSQLiteArtifactInfo(artifact, info)
		}

		flags := os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
		file, err := os.OpenFile(artifact.path, flags, 0)
		if err != nil {
			return fmt.Errorf("failed to open runtime %s %s for permission enforcement: %w", artifact.kind, artifact.path, err)
		}
		if err := file.Chmod(privateDatabaseFileMode); err != nil {
			_ = file.Close()
			return fmt.Errorf("failed to set mode 0600 on runtime %s %s: %w", artifact.kind, artifact.path, err)
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil {
			return fmt.Errorf("failed to verify runtime %s %s: %w", artifact.kind, artifact.path, statErr)
		}
		if closeErr != nil {
			return fmt.Errorf("failed to close runtime %s %s after permission enforcement: %w", artifact.kind, artifact.path, closeErr)
		}
		if err := validateSQLiteArtifactInfo(artifact, info); err != nil {
			return err
		}
	}
	return nil
}
