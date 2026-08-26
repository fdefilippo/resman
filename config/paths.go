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
	legacyBackupExampleLimit = 3

	// DefaultConfigPath is the authoritative operator-authored configuration path.
	DefaultConfigPath = "/etc/resman/resman.conf"
	// LegacyConfigPath is rejected at the default-path startup boundary.
	LegacyConfigPath = "/etc/resman.conf"
	// LegacyConfigSavedPath is where RPM preserves a modified legacy configuration.
	LegacyConfigSavedPath = "/etc/resman.conf.rpmsave"
	// LegacyConfigBackupPath contains secret-bearing configuration from the old layout.
	LegacyConfigBackupPath = "/etc/resman.conf.backup"
	// LegacyConfigTempPath is the fixed-name temporary file used by older releases.
	LegacyConfigTempPath = "/etc/resman.conf.tmp"
	// LegacyConfigBackupPrefix identifies potential configuration copies in the old layout.
	LegacyConfigBackupPrefix = "/etc/resman.conf.backup_"
	// DefaultMetricsDBPath is the authoritative mutable metrics-state path.
	DefaultMetricsDBPath = "/var/lib/resman/metrics.db"
	// LegacyMetricsDBPath is rejected when default metrics persistence is enabled.
	LegacyMetricsDBPath = "/etc/resman/metrics.db"
	// DefaultCreatedCgroupsPath is boot-scoped runtime state under /run.
	DefaultCreatedCgroupsPath = "/run/resman-cgroups.txt"
)

type diskLayout struct {
	defaultConfigPath  string
	legacyConfigPath   string
	legacySavedPath    string
	legacyBackupPath   string
	legacyTempPath     string
	legacyBackupPrefix string
	defaultDBPath      string
	legacyDBPath       string
}

var defaultDiskLayout = diskLayout{
	defaultConfigPath:  DefaultConfigPath,
	legacyConfigPath:   LegacyConfigPath,
	legacySavedPath:    LegacyConfigSavedPath,
	legacyBackupPath:   LegacyConfigBackupPath,
	legacyTempPath:     LegacyConfigTempPath,
	legacyBackupPrefix: LegacyConfigBackupPrefix,
	defaultDBPath:      DefaultMetricsDBPath,
	legacyDBPath:       LegacyMetricsDBPath,
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
	tempExists, err := pathEntryExists(layout.legacyTempPath)
	if err != nil {
		return fmt.Errorf("inspecting legacy configuration temporary path %s: %w", layout.legacyTempPath, err)
	}
	backupCandidates, err := matchingLegacyPaths(layout.legacyBackupPrefix)
	if err != nil {
		return fmt.Errorf("inspecting legacy configuration backup candidates %s*: %w", layout.legacyBackupPrefix, err)
	}
	if !configExists && !savedExists && !backupExists && !tempExists && len(backupCandidates) == 0 {
		return nil
	}
	legacyPaths := make([]string, 0, 5)
	actions := make([]string, 0, 2)
	authoredSources := make([]string, 0, 2)
	fixedSecretArtifactsDetected := false
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
		fixedSecretArtifactsDetected = true
	}
	if tempExists {
		legacyPaths = append(legacyPaths, layout.legacyTempPath)
		fixedSecretArtifactsDetected = true
	}
	if len(backupCandidates) > 0 {
		legacyPaths = append(legacyPaths, summarizeLegacyBackupCandidates(layout.legacyBackupPrefix, backupCandidates))
	}
	if fixedSecretArtifactsDetected {
		actions = append(actions, "after recovering any needed configuration, securely remove the detected fixed-name orphaned backup and temporary artifacts")
	}
	if len(backupCandidates) > 0 {
		actions = append(actions, fmt.Sprintf(
			"move any needed operator-managed copies matching %s* to a protected archive outside the legacy path, and securely remove generated or unneeded matching copies",
			layout.legacyBackupPrefix,
		))
	}
	return fmt.Errorf(
		"legacy configuration artifacts exist at %s while the default path is %s; stop resman, %s, and restart",
		strings.Join(legacyPaths, ", "),
		layout.defaultConfigPath,
		strings.Join(actions, "; "),
	)
}

func summarizeLegacyBackupCandidates(prefix string, paths []string) string {
	examples := paths
	if len(examples) > legacyBackupExampleLimit {
		examples = examples[:legacyBackupExampleLimit]
	}
	summary := fmt.Sprintf(
		"%d potential configuration-copy entries matching %s* (first %d: %s",
		len(paths),
		prefix,
		len(examples),
		strings.Join(examples, ", "),
	)
	if remaining := len(paths) - len(examples); remaining > 0 {
		summary += fmt.Sprintf("; %d more", remaining)
	}
	return summary + ")"
}

func matchingLegacyPaths(prefix string) ([]string, error) {
	dir := filepath.Dir(prefix)
	basePrefix := filepath.Base(prefix)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	paths := make([]string, 0)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), basePrefix) {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	return paths, nil
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
