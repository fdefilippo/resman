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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"unicode"

	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/internal/operationgate"
)

// EditorApplicationState is the daemon-derived state of one persisted value.
type EditorApplicationState string

const (
	EditorStateEffective           EditorApplicationState = "effective"
	EditorStatePendingRestart      EditorApplicationState = "pending_restart"
	EditorStateEnvironmentShadowed EditorApplicationState = "environment_shadowed"
)

// EditorSourceIdentity identifies one exact source object and its content.
type EditorSourceIdentity struct {
	Path   string `json:"path"`
	Device uint64 `json:"device,string"`
	Inode  uint64 `json:"inode,string"`
	Size   int64  `json:"size,string"`
	SHA256 string `json:"sha256"`
}

// EditorCompositeRevision binds an editor request to both authoritative files.
type EditorCompositeRevision struct {
	Value        string               `json:"value"`
	Config       EditorSourceIdentity `json:"config"`
	CPUPoints    EditorSourceIdentity `json:"cpu_points"`
	PublicValues map[string]string    `json:"public_values"`
}

// EditorFieldSnapshot exposes one redacted field without returning source text.
type EditorFieldSnapshot struct {
	Contract         PublicFieldContract    `json:"contract"`
	AuthoredValue    *string                `json:"authored_value,omitempty"`
	EffectiveValue   *string                `json:"effective_value,omitempty"`
	Source           ConfigSource           `json:"source"`
	ApplicationState EditorApplicationState `json:"application_state"`
	SecretSet        bool                   `json:"secret_set"`
	Remedy           string                 `json:"remedy"`
}

// EditorCPUPointsEntry is one non-sensitive direct guarantee.
type EditorCPUPointsEntry struct {
	Username string `json:"username"`
	UID      int    `json:"uid"`
	Points   uint64 `json:"points"`
}

// EditorSnapshot is the complete versioned, redacted public editor contract.
type EditorSnapshot struct {
	SchemaVersion    string                  `json:"schema_version"`
	SourcePrecedence []ConfigSource          `json:"source_precedence"`
	Revision         EditorCompositeRevision `json:"revision"`
	Fields           []EditorFieldSnapshot   `json:"fields"`
	CPUPoints        []EditorCPUPointsEntry  `json:"cpu_points"`
}

// EditorFieldChange is one explicit serialized public-field replacement.
type EditorFieldChange struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// EditorCandidateError classifies a rejected configuration patch without
// exposing source contents or submitted values.
type EditorCandidateError struct {
	Reason string
	Keys   []string
	cause  error
}

func (e *EditorCandidateError) Error() string { return e.cause.Error() }
func (e *EditorCandidateError) Unwrap() error { return e.cause }

func newEditorCandidateError(reason string, keys []string, err error) error {
	return &EditorCandidateError{Reason: reason, Keys: append([]string(nil), keys...), cause: err}
}

// EditorConfigCandidate is a validated comment-preserving detached update.
type EditorConfigCandidate struct {
	path      string
	content   []byte
	metadata  configFileMetadata
	original  []byte
	existed   bool
	config    *Config
	saveGate  *operationgate.Gate
	saveState *configPersistenceState
	writer    atomicFileWriter
}

// Config returns the fully validated default-file-environment candidate.
func (c EditorConfigCandidate) Config() *Config { return c.config }

// PrepareEditorConfigCandidate reconfirms the expected source, merges only
// named values, preserves every other byte-oriented line, and runs complete
// daemon validation under current environment overrides.
func PrepareEditorConfigCandidate(applied *Config, expected EditorSourceIdentity, changes []EditorFieldChange) (EditorConfigCandidate, error) {
	if len(changes) == 0 {
		return EditorConfigCandidate{}, newEditorCandidateError("empty_patch", nil, fmt.Errorf("configuration patch must contain at least one change"))
	}
	if applied == nil {
		return EditorConfigCandidate{}, fmt.Errorf("applied configuration is required")
	}
	path, _ := applied.editorValues()
	saveGate, saveState := applied.persistenceCoordinator()
	current, original, err := readEditorSource(path)
	if err != nil {
		return EditorConfigCandidate{}, err
	}
	if current != expected {
		return EditorConfigCandidate{}, newEditorCandidateError("revision_conflict", configChangeKeys(changes), fmt.Errorf("configuration source revision changed"))
	}
	metadata, _, existed, err := readConfigFile(path)
	if err != nil {
		return EditorConfigCandidate{}, err
	}
	if metadata.mode.Perm() != defaultConfigMode {
		return EditorConfigCandidate{}, fmt.Errorf("configuration file %s has mode %04o; set mode 0600", path, metadata.mode.Perm())
	}
	updates := make(map[string]string, len(changes))
	for _, change := range changes {
		contract, ok := publicFieldContractByKey(change.Key)
		if !ok {
			return EditorConfigCandidate{}, newEditorCandidateError("removed_or_unknown_key", []string{change.Key}, fmt.Errorf("unknown or removed configuration key %q", change.Key))
		}
		if !contract.Editable {
			return EditorConfigCandidate{}, newEditorCandidateError("non_editable_key", []string{change.Key}, fmt.Errorf("configuration key %s is not editable: %s", change.Key, contract.Remedy))
		}
		if _, duplicate := updates[change.Key]; duplicate {
			return EditorConfigCandidate{}, newEditorCandidateError("duplicate_key", []string{change.Key}, fmt.Errorf("duplicate configuration patch key %s", change.Key))
		}
		if strings.ContainsAny(change.Value, "\r\n\x00") {
			return EditorConfigCandidate{}, newEditorCandidateError("invalid_value", []string{change.Key}, fmt.Errorf("configuration value for %s contains a line or NUL delimiter", change.Key))
		}
		updates[change.Key] = change.Value
	}
	content, err := mergePublicConfigValues(original, updates)
	if err != nil {
		return EditorConfigCandidate{}, newEditorCandidateError("invalid_source", configChangeKeys(changes), err)
	}
	if string(content) == string(original) {
		return EditorConfigCandidate{}, newEditorCandidateError("no_change", configChangeKeys(changes), fmt.Errorf("configuration patch does not change the authored source"))
	}
	candidate := DefaultConfig()
	if err := loadFromData(path, content, candidate); err != nil {
		return EditorConfigCandidate{}, newEditorCandidateError("invalid_value", configChangeKeys(changes), fmt.Errorf("parse configuration candidate: %w", err))
	}
	if err := loadFromEnvironment(candidate); err != nil {
		return EditorConfigCandidate{}, newEditorCandidateError("environment_invalid", configChangeKeys(changes), fmt.Errorf("load environment overrides for candidate: %w", err))
	}
	candidate.ConfigFile = path
	if err := validateConfig(candidate); err != nil {
		return EditorConfigCandidate{}, newEditorCandidateError("invalid_candidate", configChangeKeys(changes), fmt.Errorf("validate configuration candidate: %w", err))
	}
	return EditorConfigCandidate{
		path: path, content: content, metadata: metadata, original: original, existed: existed,
		config: candidate, saveGate: saveGate, saveState: saveState, writer: writeFileAtomically,
	}, nil
}

func configChangeKeys(changes []EditorFieldChange) []string {
	keys := make([]string, 0, len(changes))
	for _, change := range changes {
		keys = append(keys, change.Key)
	}
	return keys
}

// Persist writes the exact candidate atomically after reconfirming both the
// expected configuration object and the caller-supplied CPU Points source.
func (c EditorConfigCandidate) Persist(expectedConfig, expectedCPU EditorSourceIdentity) (EditorSourceIdentity, error) {
	if c.saveGate == nil || c.saveState == nil {
		return EditorSourceIdentity{}, fmt.Errorf("configuration persistence coordinator is unavailable")
	}
	leavePersistence := c.saveGate.Enter()
	defer leavePersistence()
	if c.saveState.unusableErr != nil {
		return EditorSourceIdentity{}, fmt.Errorf("configuration persistence is unavailable until resman restarts after operator recovery: %w", c.saveState.unusableErr)
	}
	current, _, err := readEditorSource(c.path)
	if err != nil {
		return EditorSourceIdentity{}, err
	}
	currentCPU, _, err := readEditorSource(expectedCPU.Path)
	if err != nil {
		return EditorSourceIdentity{}, err
	}
	if current != expectedConfig || currentCPU != expectedCPU {
		return EditorSourceIdentity{}, fmt.Errorf("composite source revision changed before persistence")
	}
	if c.existed {
		if _, err := c.writer(c.path+configBackupSuffix, c.original, c.metadata); err != nil {
			return EditorSourceIdentity{}, fmt.Errorf("create secure editor configuration backup: %w", err)
		}
	}
	current, _, err = readEditorSource(c.path)
	if err != nil {
		return EditorSourceIdentity{}, err
	}
	currentCPU, _, err = readEditorSource(expectedCPU.Path)
	if err != nil {
		return EditorSourceIdentity{}, err
	}
	if current != expectedConfig || currentCPU != expectedCPU {
		return EditorSourceIdentity{}, fmt.Errorf("composite source revision changed immediately before persistence")
	}
	committed, err := c.writer(c.path, c.content, c.metadata)
	if err != nil {
		if committed {
			if _, restoreErr := c.writer(c.path, c.original, c.metadata); restoreErr != nil {
				unusable := &configPersistenceUnusableError{
					detail:   "failed to restore configuration after an unconfirmed editor write",
					recovery: fmt.Sprintf("stop resman, restore %s, and restart before accepting further configuration writes", c.path+configBackupSuffix),
					cause:    restoreErr,
				}
				c.saveState.unusableErr = unusable
				return EditorSourceIdentity{}, errors.Join(err, unusable)
			}
		}
		return EditorSourceIdentity{}, fmt.Errorf("persist editor configuration (committed=%t): %w", committed, err)
	}
	persisted, content, err := readEditorSource(c.path)
	if err != nil {
		return EditorSourceIdentity{}, fmt.Errorf("confirm persisted editor configuration: %w", err)
	}
	if sha256.Sum256(content) != sha256.Sum256(c.content) {
		return EditorSourceIdentity{}, fmt.Errorf("persisted editor configuration was replaced before confirmation")
	}
	return persisted, nil
}

// Rollback restores the source bytes captured before persistence only while
// the path still identifies the exact object committed by this request.
func (c EditorConfigCandidate) Rollback(expectedPersisted EditorSourceIdentity) error {
	if c.saveGate == nil || c.saveState == nil {
		return fmt.Errorf("configuration persistence coordinator is unavailable")
	}
	leavePersistence := c.saveGate.Enter()
	defer leavePersistence()
	current, _, err := readEditorSource(c.path)
	if err != nil {
		return fmt.Errorf("confirm editor configuration before rollback: %w", err)
	}
	if current != expectedPersisted {
		return fmt.Errorf("editor configuration changed after persistence; rollback refused")
	}
	if !c.existed {
		return os.Remove(c.path)
	}
	committed, err := c.writer(c.path, c.original, c.metadata)
	if err == nil {
		return nil
	}
	if committed {
		unusable := &configPersistenceUnusableError{
			detail:   "editor rollback durability could not be confirmed",
			recovery: fmt.Sprintf("stop resman, restore %s, and restart before accepting further configuration writes", c.path+configBackupSuffix),
			cause:    err,
		}
		c.saveState.unusableErr = unusable
		return unusable
	}
	return err
}

// PersistCPUPointsEditorCandidate serializes a policy-map replacement with
// every configuration-file writer and reconfirms the other composite source
// while that shared persistence gate is held.
func PersistCPUPointsEditorCandidate(applied *Config, candidate cpupoints.PolicyEditorCandidate, expectedConfig EditorSourceIdentity) (cpupoints.PolicySource, error) {
	if applied == nil {
		return cpupoints.PolicySource{}, fmt.Errorf("applied configuration is required")
	}
	saveGate, saveState := applied.persistenceCoordinator()
	leavePersistence := saveGate.Enter()
	defer leavePersistence()
	if saveState.unusableErr != nil {
		return cpupoints.PolicySource{}, fmt.Errorf("configuration persistence is unavailable until resman restarts after operator recovery: %w", saveState.unusableErr)
	}
	return candidate.Persist(func() error {
		current, err := ReadEditorSourceIdentity(expectedConfig.Path)
		if err != nil {
			return err
		}
		if current != expectedConfig {
			return fmt.Errorf("configuration source revision changed")
		}
		return nil
	})
}

// RollbackCPUPointsEditorCandidate serializes conditional policy-map rollback
// with every configuration source writer.
func RollbackCPUPointsEditorCandidate(applied *Config, candidate cpupoints.PolicyEditorCandidate, expected cpupoints.PolicySource) error {
	if applied == nil {
		return fmt.Errorf("applied configuration is required")
	}
	saveGate, _ := applied.persistenceCoordinator()
	leavePersistence := saveGate.Enter()
	defer leavePersistence()
	return candidate.Rollback(expected)
}

func mergePublicConfigValues(original []byte, updates map[string]string) ([]byte, error) {
	if len(updates) == 0 {
		return append([]byte(nil), original...), nil
	}
	seen := make(map[string]bool, len(updates))
	lines := strings.Split(string(original), "\n")
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed configuration line %d", index+1)
		}
		key := strings.TrimSpace(parts[0])
		value, update := updates[key]
		if !update {
			continue
		}
		if seen[key] {
			return nil, fmt.Errorf("configuration contains duplicate key %s", key)
		}
		seen[key] = true
		lines[index] = key + "=" + value + editorInlineCommentSuffix(parts[1])
	}
	keys := make([]string, 0, len(updates))
	for key := range updates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !seen[key] {
			lines = append(lines, key+"="+updates[key])
		}
	}
	return []byte(strings.Join(lines, "\n")), nil
}

func editorInlineCommentSuffix(value string) string {
	var quote rune
	escaped := false
	previousWhitespace := false
	for index, current := range value {
		if escaped {
			escaped = false
			previousWhitespace = unicode.IsSpace(current)
			continue
		}
		if quote != 0 {
			switch current {
			case '\\':
				escaped = true
			case quote:
				quote = 0
			}
			previousWhitespace = unicode.IsSpace(current)
			continue
		}
		switch current {
		case '\'', '"':
			quote = current
		case '#':
			if previousWhitespace {
				return " " + value[index:]
			}
		}
		previousWhitespace = unicode.IsSpace(current)
	}
	return ""
}

// BuildEditorSnapshot reads and validates both persisted sources and compares
// them with the currently effective daemon configuration.
func BuildEditorSnapshot(applied *Config) (EditorSnapshot, error) {
	if applied == nil {
		return EditorSnapshot{}, fmt.Errorf("applied configuration is required")
	}
	configPath, appliedValues := applied.editorValues()
	configSource, configContent, err := readEditorSource(configPath)
	if err != nil {
		return EditorSnapshot{}, fmt.Errorf("read editor configuration source: %w", err)
	}
	persisted, err := LoadAndValidate(configPath)
	if err != nil {
		return EditorSnapshot{}, fmt.Errorf("load persisted editor configuration: %w", err)
	}
	_, persistedValues := persisted.editorValues()
	authored, err := parseAuthoredPublicValues(configContent)
	if err != nil {
		return EditorSnapshot{}, err
	}

	reserve, err := cpupoints.NewReservePoints(uint64(persisted.GetCPUReservePoints()))
	if err != nil {
		return EditorSnapshot{}, err
	}
	bestEffort, err := cpupoints.NewBestEffortPoints(uint64(persisted.GetCPUBestEffortPoints()))
	if err != nil {
		return EditorSnapshot{}, err
	}
	mapPath, err := cpupoints.NewPolicyMapPath(persisted.GetCPUPointsFile())
	if err != nil {
		return EditorSnapshot{}, err
	}
	policy, err := cpupoints.NewPolicyLoader().Load(cpupoints.PolicyInputs{
		Reserve: reserve, BestEffort: bestEffort, MapPath: mapPath,
	}, cpupoints.NSSIdentityResolver{})
	if err != nil {
		return EditorSnapshot{}, fmt.Errorf("load editor CPU Points source: %w", err)
	}
	mapSource := policy.Source()
	mapDigest := mapSource.Digest()
	cpuSource := EditorSourceIdentity{
		Path: mapSource.Path().String(), Device: mapSource.Device(), Inode: mapSource.Inode(),
		Size: mapSource.Size(), SHA256: hex.EncodeToString(mapDigest[:]),
	}
	publicValues := make(map[string]string, len(authored))
	for _, contract := range PublicFieldContracts() {
		if value, ok := authored[contract.Key]; ok && !contract.Sensitive {
			publicValues[contract.Key] = value
		}
	}
	revision := newEditorCompositeRevision(configSource, cpuSource, publicValues)

	fields := make([]EditorFieldSnapshot, 0, len(appliedValues))
	for _, contract := range PublicFieldContracts() {
		authoredValue, hasAuthored := authored[contract.Key]
		effectiveValue := appliedValues[contract.Key]
		persistedValue := persistedValues[contract.Key]
		_, environmentSet := os.LookupEnv(contract.Key)
		source := ConfigSourceDefault
		if hasAuthored {
			source = ConfigSourceFile
		}
		if environmentSet {
			source = ConfigSourceEnvironment
		}
		state := EditorStateEffective
		remedy := contract.Remedy
		if environmentSet && hasAuthored {
			state = EditorStateEnvironmentShadowed
			remedy = EnvironmentShadowingRemedy()
		} else if contract.Lifecycle == LifecycleRestartRequired && persistedValue != effectiveValue {
			state = EditorStatePendingRestart
		}
		field := EditorFieldSnapshot{Contract: contract, Source: source, ApplicationState: state, Remedy: remedy}
		if contract.Sensitive {
			field.SecretSet = effectiveValue != "" || authoredValue != ""
		} else {
			if hasAuthored {
				value := authoredValue
				field.AuthoredValue = &value
			}
			value := effectiveValue
			field.EffectiveValue = &value
		}
		fields = append(fields, field)
	}
	entries := make([]EditorCPUPointsEntry, 0, len(policy.Entries()))
	for _, entry := range policy.Entries() {
		entries = append(entries, EditorCPUPointsEntry{Username: entry.Username(), UID: entry.UID(), Points: entry.Points().Value()})
	}
	return EditorSnapshot{
		SchemaVersion: "resman.config-editor.v1", SourcePrecedence: PublicConfigSourcePrecedence(),
		Revision: revision, Fields: fields, CPUPoints: entries,
	}, nil
}

func (c *Config) editorValues() (string, map[string]string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	typeOfConfig := reflect.TypeOf(c).Elem()
	valueOfConfig := reflect.ValueOf(c).Elem()
	values := make(map[string]string, len(configFieldLifecycles))
	for index := 0; index < typeOfConfig.NumField(); index++ {
		key := typeOfConfig.Field(index).Tag.Get("config")
		if key == "" || key == "-" {
			continue
		}
		values[key] = formatPublicDefault(valueOfConfig.Field(index))
	}
	return c.ConfigFile, values
}

func readEditorSource(path string) (EditorSourceIdentity, []byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return EditorSourceIdentity{}, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return EditorSourceIdentity{}, nil, fmt.Errorf("editor source %s is not a non-symlink regular file", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return EditorSourceIdentity{}, nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return EditorSourceIdentity{}, nil, err
	}
	if after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) || int64(len(content)) != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return EditorSourceIdentity{}, nil, fmt.Errorf("editor source %s changed while it was read", path)
	}
	stat, ok := after.Sys().(*syscall.Stat_t)
	if !ok {
		return EditorSourceIdentity{}, nil, fmt.Errorf("editor source %s has unsupported metadata type %T", path, after.Sys())
	}
	digest := sha256.Sum256(content)
	return EditorSourceIdentity{
		Path: path, Device: uint64(stat.Dev), Inode: stat.Ino, Size: after.Size(), SHA256: hex.EncodeToString(digest[:]),
	}, content, nil
}

// ReadEditorSourceIdentity reads one source through the editor identity guard
// without exposing its content across the package boundary.
func ReadEditorSourceIdentity(path string) (EditorSourceIdentity, error) {
	identity, _, err := readEditorSource(path)
	return identity, err
}

func newEditorCompositeRevision(configSource, cpuSource EditorSourceIdentity, publicValues map[string]string) EditorCompositeRevision {
	serialized := fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s",
		configSource.Path, configSource.Device, configSource.Inode, configSource.Size, configSource.SHA256,
		cpuSource.Path, cpuSource.Device, cpuSource.Inode, cpuSource.Size, cpuSource.SHA256)
	digest := sha256.Sum256([]byte(serialized))
	return EditorCompositeRevision{Value: hex.EncodeToString(digest[:]), Config: configSource, CPUPoints: cpuSource, PublicValues: publicValues}
}

// SameEditorRevision compares the complete source-bound public revision rather
// than trusting its compact digest in isolation.
func SameEditorRevision(left, right EditorCompositeRevision) bool {
	return left.Value == right.Value && left.Config == right.Config && left.CPUPoints == right.CPUPoints && maps.Equal(left.PublicValues, right.PublicValues)
}

func parseAuthoredPublicValues(content []byte) (map[string]string, error) {
	values := make(map[string]string)
	for index, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed configuration line %d", index+1)
		}
		key := strings.TrimSpace(parts[0])
		if _, ok := publicFieldContractByKey(key); !ok {
			return nil, fmt.Errorf("configuration line %d contains non-public key %s", index+1, key)
		}
		if _, duplicate := values[key]; duplicate {
			return nil, fmt.Errorf("configuration line %d duplicates key %s", index+1, key)
		}
		value := strings.TrimSpace(stripInlineComment(parts[1]))
		values[key] = strings.TrimSpace(strings.Trim(value, `"'`))
	}
	return values, nil
}

// ChangedEditorSources returns a bounded source-only conflict description.
func ChangedEditorSources(expected, current EditorCompositeRevision) []string {
	changed := make([]string, 0, 2)
	if expected.Config != current.Config {
		changed = append(changed, "configuration")
	}
	if expected.CPUPoints != current.CPUPoints {
		changed = append(changed, "cpu_points")
	}
	sort.Strings(changed)
	return changed
}

// ChangedEditorFields returns at most limit non-sensitive changed public keys.
func ChangedEditorFields(expected, current EditorCompositeRevision, limit int) []string {
	if limit < 1 {
		return nil
	}
	changed := make([]string, 0)
	for key, previous := range expected.PublicValues {
		if value, ok := current.PublicValues[key]; !ok || value != previous {
			changed = append(changed, key)
		}
	}
	for key := range current.PublicValues {
		if _, ok := expected.PublicValues[key]; !ok {
			changed = append(changed, key)
		}
	}
	sort.Strings(changed)
	if len(changed) > limit {
		changed = changed[:limit]
	}
	return changed
}
