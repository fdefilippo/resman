/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 */

package cpupoints

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// PolicyEditorChange sets one username guarantee, or removes it when Points is nil.
type PolicyEditorChange struct {
	Username string
	Points   *uint64
}

// PolicyEditorError classifies a rejected partial policy update without
// exposing source contents or submitted point values.
type PolicyEditorError struct {
	Reason    string
	Usernames []string
	Cause     error
}

func (e *PolicyEditorError) Error() string { return e.Cause.Error() }
func (e *PolicyEditorError) Unwrap() error { return e.Cause }

func newPolicyEditorError(reason string, usernames []string, err error) error {
	return &PolicyEditorError{Reason: reason, Usernames: append([]string(nil), usernames...), Cause: err}
}

// PolicyEditorCandidate is one detached, validated, comment-preserving update.
type PolicyEditorCandidate struct {
	loader   *PolicyLoader
	path     PolicyMapPath
	expected PolicySource
	content  []byte
	original []byte
	uid      int
	gid      int
	policy   PolicySnapshot
}

// Policy returns the fully parsed and NSS-resolved candidate.
func (c PolicyEditorCandidate) Policy() PolicySnapshot { return c.policy }

// PreparePolicyEditorCandidate validates a partial map update without writing it.
func PreparePolicyEditorCandidate(inputs PolicyInputs, expected PolicySource, changes []PolicyEditorChange, resolver ExactIdentityResolver) (PolicyEditorCandidate, error) {
	if len(changes) == 0 {
		return PolicyEditorCandidate{}, newPolicyEditorError("empty_patch", nil, fmt.Errorf("CPU Points patch must contain at least one change"))
	}
	loader := NewPolicyLoader()
	data, current, err := loader.readSafePolicyFile(inputs.MapPath)
	if err != nil {
		return PolicyEditorCandidate{}, err
	}
	if !samePolicySource(current, expected) {
		return PolicyEditorCandidate{}, newPolicyEditorError("revision_conflict", policyEditorUsernames(changes), fmt.Errorf("CPU Points source revision changed"))
	}
	info, err := os.Lstat(inputs.MapPath.String())
	if err != nil {
		return PolicyEditorCandidate{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return PolicyEditorCandidate{}, fmt.Errorf("CPU Points source has unsupported metadata type %T", info.Sys())
	}
	content, err := mergePolicyMap(data, changes)
	if err != nil {
		return PolicyEditorCandidate{}, newPolicyEditorError("invalid_cpu_points_patch", policyEditorUsernames(changes), err)
	}
	if string(content) == string(data) {
		return PolicyEditorCandidate{}, newPolicyEditorError("no_change", policyEditorUsernames(changes), fmt.Errorf("CPU Points patch does not change the authored source"))
	}
	policy, err := loader.LoadContent(inputs, content, resolver)
	if err != nil {
		return PolicyEditorCandidate{}, err
	}
	return PolicyEditorCandidate{
		loader: loader, path: inputs.MapPath, expected: expected, content: content,
		original: data, uid: int(stat.Uid), gid: int(stat.Gid), policy: policy,
	}, nil
}

func policyEditorUsernames(changes []PolicyEditorChange) []string {
	usernames := make([]string, 0, len(changes))
	for _, change := range changes {
		usernames = append(usernames, change.Username)
	}
	return usernames
}

// Persist atomically replaces the map after reconfirming its complete identity.
func (c PolicyEditorCandidate) Persist(confirmOtherSource func() error) (PolicySource, error) {
	if err := c.loader.ConfirmSource(c.expected); err != nil {
		return PolicySource{}, err
	}
	if confirmOtherSource == nil {
		return PolicySource{}, fmt.Errorf("other composite source confirmation is required")
	}
	if err := confirmOtherSource(); err != nil {
		return PolicySource{}, fmt.Errorf("confirm other composite source: %w", err)
	}
	if err := writePolicyEditorFile(c.path.String(), c.content, c.uid, c.gid); err != nil {
		return PolicySource{}, err
	}
	content, persisted, err := c.loader.readSafePolicyFile(c.path)
	if err != nil {
		return PolicySource{}, fmt.Errorf("confirm persisted CPU Points map: %w", err)
	}
	if string(content) != string(c.content) {
		return PolicySource{}, fmt.Errorf("persisted CPU Points map was replaced before confirmation")
	}
	return persisted, nil
}

// Rollback restores the exact bytes captured before persistence only while the
// path still identifies the exact object committed by this request.
func (c PolicyEditorCandidate) Rollback(expectedPersisted PolicySource) error {
	if err := c.loader.ConfirmSource(expectedPersisted); err != nil {
		return fmt.Errorf("CPU Points map changed after persistence; rollback refused: %w", err)
	}
	return writePolicyEditorFile(c.path.String(), c.original, c.uid, c.gid)
}

func samePolicySource(left, right PolicySource) bool {
	return left.path == right.path && left.dev == right.dev && left.inode == right.inode && left.size == right.size && left.digest == right.digest
}

func mergePolicyMap(original []byte, changes []PolicyEditorChange) ([]byte, error) {
	updates := make(map[string]*uint64, len(changes))
	for _, change := range changes {
		if change.Username == "" || strings.TrimSpace(change.Username) != change.Username || strings.ContainsAny(change.Username, "=\r\n\x00") {
			return nil, fmt.Errorf("invalid CPU Points username %q", change.Username)
		}
		if _, duplicate := updates[change.Username]; duplicate {
			return nil, fmt.Errorf("duplicate CPU Points patch username %q", change.Username)
		}
		updates[change.Username] = change.Points
	}
	lines := strings.Split(string(original), "\n")
	seen := make(map[string]bool, len(updates))
	merged := make([]string, 0, len(lines)+len(changes))
	for index, line := range lines {
		if index == 0 || line == "" || strings.HasPrefix(line, "#") {
			merged = append(merged, line)
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed CPU Points map line %d", index+1)
		}
		points, update := updates[parts[0]]
		if !update {
			merged = append(merged, line)
			continue
		}
		if seen[parts[0]] {
			return nil, fmt.Errorf("CPU Points map duplicates username %q", parts[0])
		}
		seen[parts[0]] = true
		if points != nil {
			merged = append(merged, parts[0]+"="+strconv.FormatUint(*points, 10))
		}
	}
	names := make([]string, 0, len(updates))
	for name, points := range updates {
		if !seen[name] && points != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		merged = append(merged, name+"="+strconv.FormatUint(*updates[name], 10))
	}
	return []byte(strings.Join(merged, "\n")), nil
}

func writePolicyEditorFile(path string, content []byte, uid, gid int) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".editor-*")
	if err != nil {
		return fmt.Errorf("create CPU Points temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chown(uid, gid); err != nil {
		return fmt.Errorf("preserve CPU Points ownership: %w", err)
	}
	if err := temporary.Chmod(policyMapFileMode); err != nil {
		return fmt.Errorf("set CPU Points mode: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		return fmt.Errorf("write CPU Points temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync CPU Points temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close CPU Points temporary file: %w", err)
	}
	closed = true
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace CPU Points map: %w", err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open CPU Points parent after replacement: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return fmt.Errorf("sync CPU Points parent after replacement: %w", errors.Join(syncErr, closeErr))
	}
	return nil
}
