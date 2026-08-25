/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 */

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// DefaultConfigPath is the authoritative operator-authored configuration path.
	DefaultConfigPath = "/etc/resman/resman.conf"
	// LegacyConfigPath is rejected at the default-path startup boundary.
	LegacyConfigPath = "/etc/resman.conf"
	// LegacyConfigSavedPath is where RPM preserves a modified legacy configuration.
	LegacyConfigSavedPath = "/etc/resman.conf.rpmsave"
	// LegacyConfigBackupPath contains secret-bearing configuration from the old layout.
	LegacyConfigBackupPath = "/etc/resman.conf.backup"
	// DefaultMetricsDBPath is the authoritative mutable metrics-state path.
	DefaultMetricsDBPath = "/var/lib/resman/metrics.db"
	// LegacyMetricsDBPath is rejected when default metrics persistence is enabled.
	LegacyMetricsDBPath = "/etc/resman/metrics.db"
	// DefaultCreatedCgroupsPath is boot-scoped runtime state under /run.
	DefaultCreatedCgroupsPath = "/run/resman-cgroups.txt"
)

type diskLayout struct {
	defaultConfigPath string
	legacyConfigPath  string
	legacySavedPath   string
	legacyBackupPath  string
	defaultDBPath     string
	legacyDBPath      string
}

var defaultDiskLayout = diskLayout{
	defaultConfigPath: DefaultConfigPath,
	legacyConfigPath:  LegacyConfigPath,
	legacySavedPath:   LegacyConfigSavedPath,
	legacyBackupPath:  LegacyConfigBackupPath,
	defaultDBPath:     DefaultMetricsDBPath,
	legacyDBPath:      LegacyMetricsDBPath,
}

func rejectLegacyConfigAtDefault(selectedPath string, layout diskLayout) error {
	if filepath.Clean(selectedPath) != filepath.Clean(layout.defaultConfigPath) {
		return nil
	}
	configExists, err := pathEntryExists(layout.legacyConfigPath)
	if err != nil {
		return fmt.Errorf("inspecting legacy configuration path %s: %w", layout.legacyConfigPath, err)
	}
	savedExists, err := pathEntryExists(layout.legacySavedPath)
	if err != nil {
		return fmt.Errorf("inspecting RPM-saved legacy configuration path %s: %w", layout.legacySavedPath, err)
	}
	backupExists, err := pathEntryExists(layout.legacyBackupPath)
	if err != nil {
		return fmt.Errorf("inspecting legacy configuration backup path %s: %w", layout.legacyBackupPath, err)
	}
	if !configExists && !savedExists && !backupExists {
		return nil
	}
	legacyPaths := make([]string, 0, 3)
	actions := make([]string, 0, 2)
	authoredSources := make([]string, 0, 2)
	if configExists {
		legacyPaths = append(legacyPaths, layout.legacyConfigPath)
		authoredSources = append(authoredSources, layout.legacyConfigPath)
	}
	if savedExists {
		legacyPaths = append(legacyPaths, layout.legacySavedPath)
		authoredSources = append(authoredSources, layout.legacySavedPath)
	}
	if len(authoredSources) > 0 {
		actions = append(actions, fmt.Sprintf(
			"choose the authoritative authored contents from %s, install them as a regular file at %s, and remove the legacy source files",
			strings.Join(authoredSources, ", "),
			layout.defaultConfigPath,
		))
	}
	if backupExists {
		legacyPaths = append(legacyPaths, layout.legacyBackupPath)
		actions = append(actions, fmt.Sprintf("securely remove the orphaned secret-bearing backup %s", layout.legacyBackupPath))
	}
	return fmt.Errorf(
		"legacy configuration artifacts exist at %s while the default path is %s; stop resman, %s, and restart",
		strings.Join(legacyPaths, ", "),
		layout.defaultConfigPath,
		strings.Join(actions, "; "),
	)
}

func rejectLegacyMetricsDBAtDefault(cfg *Config, layout diskLayout) error {
	if !cfg.MetricsDBEnabled || filepath.Clean(cfg.MetricsDBPath) != filepath.Clean(layout.defaultDBPath) {
		return nil
	}
	exists, err := pathEntryExists(layout.legacyDBPath)
	if err != nil {
		return fmt.Errorf("inspecting legacy metrics database path %s: %w", layout.legacyDBPath, err)
	}
	if !exists {
		return nil
	}
	return fmt.Errorf(
		"legacy metrics database exists at %s while the default path is %s; archive or delete the legacy database and restart so resman can create the current schema at %s",
		layout.legacyDBPath,
		layout.defaultDBPath,
		layout.defaultDBPath,
	)
}

func pathEntryExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
