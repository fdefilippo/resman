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
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
)

func TestNewDatabaseManagerProtectsFileBackedStorageWithPermissiveUmask(t *testing.T) {
	tests := []struct {
		name  string
		path  func(string) string
		setup func(*testing.T, string)
	}{
		{
			name: "new packaged-default-style directory",
			path: func(root string) string {
				return filepath.Join(root, strings.TrimPrefix(config.DefaultMetricsDBPath, string(os.PathSeparator)))
			},
		},
		{
			name: "new custom directory",
			path: func(root string) string {
				return filepath.Join(root, "custom", "private", "history.db")
			},
		},
		{
			name: "existing safe custom directory",
			path: func(root string) string {
				return filepath.Join(root, "custom", "history.db")
			},
			setup: func(t *testing.T, dbPath string) {
				t.Helper()
				if err := os.Mkdir(filepath.Dir(dbPath), 0700); err != nil {
					t.Fatalf("os.Mkdir(%s) error = %v", filepath.Dir(dbPath), err)
				}
			},
		},
		{
			name: "existing safe database file",
			path: func(root string) string {
				return filepath.Join(root, "custom", "history.db")
			},
			setup: func(t *testing.T, dbPath string) {
				t.Helper()
				if err := os.Mkdir(filepath.Dir(dbPath), 0700); err != nil {
					t.Fatalf("os.Mkdir(%s) error = %v", filepath.Dir(dbPath), err)
				}
				writeFileWithExactMode(t, dbPath, nil, 0600)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := privateTempRoot(t)
			dbPath := tt.path(root)
			if tt.setup != nil {
				tt.setup(t, dbPath)
			}

			restoreUmask := setPermissiveUmask(t)
			manager, err := NewDatabaseManager(dbPath)
			restoreUmask()
			if err != nil {
				t.Fatalf("NewDatabaseManager() error = %v", err)
			}
			defer func() { _ = manager.Close() }()

			if err := manager.WriteSystemMetrics(&SystemMetricsRecord{
				Timestamp:  time.Now().UTC(),
				TotalCores: 1,
			}); err != nil {
				t.Fatalf("WriteSystemMetrics() error = %v", err)
			}

			assertMode(t, filepath.Dir(dbPath), 0700)
			for _, artifact := range sqliteArtifacts(dbPath) {
				assertMode(t, artifact.path, 0600)
			}
		})
	}
}

func TestNewDatabaseManagerRejectsUnsafeOrAmbiguousFileBackedStorage(t *testing.T) {
	const sensitiveMarker = "stored-history-must-not-appear-in-errors"
	tests := []struct {
		name               string
		setup              func(*testing.T, string) string
		wantPath           func(string) string
		wantRemedy         string
		wantDatabaseAbsent bool
	}{
		{
			name: "existing unsafe custom directory",
			setup: func(t *testing.T, root string) string {
				dir := filepath.Join(root, "shared")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatalf("os.Mkdir(%s) error = %v", dir, err)
				}
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatalf("os.Chmod(%s) error = %v", dir, err)
				}
				return filepath.Join(dir, "metrics.db")
			},
			wantPath:   func(dbPath string) string { return filepath.Dir(dbPath) },
			wantRemedy: "mode to 0700",
		},
		{
			name: "symbolic link database directory",
			setup: func(t *testing.T, root string) string {
				target := filepath.Join(root, "target")
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatalf("os.Mkdir(%s) error = %v", target, err)
				}
				link := filepath.Join(root, "linked")
				if err := os.Symlink(target, link); err != nil {
					t.Fatalf("os.Symlink(%s, %s) error = %v", target, link, err)
				}
				return filepath.Join(link, "metrics.db")
			},
			wantPath:   func(dbPath string) string { return filepath.Dir(dbPath) },
			wantRemedy: "symbolic link",
		},
		{
			name: "non-directory database parent",
			setup: func(t *testing.T, root string) string {
				parent := filepath.Join(root, "database-parent")
				writeFileWithExactMode(t, parent, nil, 0600)
				return filepath.Join(parent, "metrics.db")
			},
			wantPath:   func(dbPath string) string { return filepath.Dir(dbPath) },
			wantRemedy: "not a directory",
		},
		{
			name: "group-writable database path ancestor",
			setup: func(t *testing.T, root string) string {
				ancestor := filepath.Join(root, "shared")
				if err := os.Mkdir(ancestor, 0770); err != nil {
					t.Fatalf("os.Mkdir(%s) error = %v", ancestor, err)
				}
				if err := os.Chmod(ancestor, 0770); err != nil {
					t.Fatalf("os.Chmod(%s) error = %v", ancestor, err)
				}
				dir := filepath.Join(ancestor, "private")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatalf("os.Mkdir(%s) error = %v", dir, err)
				}
				return filepath.Join(dir, "metrics.db")
			},
			wantPath:   func(dbPath string) string { return filepath.Dir(filepath.Dir(dbPath)) },
			wantRemedy: "can be replaced by an untrusted user",
		},
		{
			name: "symbolic link database path ancestor",
			setup: func(t *testing.T, root string) string {
				target := filepath.Join(root, "target")
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatalf("os.Mkdir(%s) error = %v", target, err)
				}
				dir := filepath.Join(target, "private")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatalf("os.Mkdir(%s) error = %v", dir, err)
				}
				link := filepath.Join(root, "linked")
				if err := os.Symlink(target, link); err != nil {
					t.Fatalf("os.Symlink(%s, %s) error = %v", target, link, err)
				}
				return filepath.Join(link, "private", "metrics.db")
			},
			wantPath:   func(dbPath string) string { return filepath.Dir(filepath.Dir(dbPath)) },
			wantRemedy: "symbolic link",
		},
		{
			name: "existing database with unsafe mode",
			setup: func(t *testing.T, root string) string {
				dbPath := safeDatabasePath(t, root)
				writeFileWithExactMode(t, dbPath, []byte(sensitiveMarker), 0644)
				return dbPath
			},
			wantPath:   func(dbPath string) string { return dbPath },
			wantRemedy: "mode to 0600",
		},
		{
			name: "symbolic link database file",
			setup: func(t *testing.T, root string) string {
				dbPath := safeDatabasePath(t, root)
				target := filepath.Join(root, "target.db")
				writeFileWithExactMode(t, target, []byte(sensitiveMarker), 0600)
				if err := os.Symlink(target, dbPath); err != nil {
					t.Fatalf("os.Symlink(%s, %s) error = %v", target, dbPath, err)
				}
				return dbPath
			},
			wantPath:   func(dbPath string) string { return dbPath },
			wantRemedy: "regular file",
		},
		{
			name: "non-regular database file",
			setup: func(t *testing.T, root string) string {
				dbPath := safeDatabasePath(t, root)
				if err := os.Mkdir(dbPath, 0700); err != nil {
					t.Fatalf("os.Mkdir(%s) error = %v", dbPath, err)
				}
				return dbPath
			},
			wantPath:   func(dbPath string) string { return dbPath },
			wantRemedy: "regular file",
		},
		{
			name: "orphaned WAL sidecar with unsafe mode",
			setup: func(t *testing.T, root string) string {
				dbPath := safeDatabasePath(t, root)
				writeFileWithExactMode(t, dbPath+"-wal", []byte(sensitiveMarker), 0644)
				return dbPath
			},
			wantPath:           func(dbPath string) string { return dbPath + "-wal" },
			wantRemedy:         "mode to 0600",
			wantDatabaseAbsent: true,
		},
		{
			name: "symbolic link shared-memory sidecar",
			setup: func(t *testing.T, root string) string {
				dbPath := safeDatabasePath(t, root)
				writeFileWithExactMode(t, dbPath, nil, 0600)
				target := filepath.Join(root, "target.shm")
				writeFileWithExactMode(t, target, []byte(sensitiveMarker), 0600)
				if err := os.Symlink(target, dbPath+"-shm"); err != nil {
					t.Fatalf("os.Symlink(%s, %s) error = %v", target, dbPath+"-shm", err)
				}
				return dbPath
			},
			wantPath:   func(dbPath string) string { return dbPath + "-shm" },
			wantRemedy: "regular file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := privateTempRoot(t)
			dbPath := tt.setup(t, root)
			restoreUmask := setPermissiveUmask(t)
			manager, err := NewDatabaseManager(dbPath)
			restoreUmask()
			if manager != nil {
				_ = manager.Close()
				t.Fatal("NewDatabaseManager() returned a manager for unsafe storage")
			}
			if err == nil {
				t.Fatal("NewDatabaseManager() accepted unsafe storage")
			}
			for _, fragment := range []string{tt.wantPath(dbPath), tt.wantRemedy} {
				if !strings.Contains(err.Error(), fragment) {
					t.Fatalf("NewDatabaseManager() error = %q, want fragment %q", err, fragment)
				}
			}
			if strings.Contains(err.Error(), sensitiveMarker) {
				t.Fatalf("NewDatabaseManager() exposed stored data in error: %q", err)
			}
			if tt.wantDatabaseAbsent {
				if _, statErr := os.Lstat(dbPath); !os.IsNotExist(statErr) {
					t.Fatalf("database path after rejected sidecar = %v, want absent", statErr)
				}
			}
		})
	}
}

func privateTempRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatalf("os.Chmod(%s) error = %v", root, err)
	}
	return root
}

func safeDatabasePath(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatalf("os.Mkdir(%s) error = %v", dir, err)
	}
	return filepath.Join(dir, "metrics.db")
}

func writeFileWithExactMode(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatalf("os.WriteFile(%s) error = %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("os.Chmod(%s) error = %v", path, err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("os.Lstat(%s) error = %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}

func setPermissiveUmask(t *testing.T) func() {
	t.Helper()
	previous := syscall.Umask(0)
	restored := false
	restore := func() {
		if restored {
			return
		}
		syscall.Umask(previous)
		restored = true
	}
	t.Cleanup(restore)
	return restore
}
