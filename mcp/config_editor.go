/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 */

package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/cpupoints"
)

const maximumConflictFields = 16

type revisionBoundConfigurationReloader interface {
	ReloadEditorRevision(context.Context, config.EditorCompositeRevision) error
}

type getConfigurationEditorArgs struct{}

type updateConfigurationArgs struct {
	Revision config.EditorCompositeRevision `json:"revision"`
	Changes  []config.EditorFieldChange     `json:"changes"`
}

type cpuPointsEditorChange struct {
	Username string  `json:"username"`
	Points   *uint64 `json:"points,omitempty"`
}

type updateCPUPointsArgs struct {
	Revision config.EditorCompositeRevision `json:"revision"`
	Changes  []cpuPointsEditorChange        `json:"changes"`
}

type editorUpdateState string

type editorRefusalReason string

const (
	editorUpdateApplied        editorUpdateState = "applied"
	editorUpdatePendingRestart editorUpdateState = "pending_restart"
	editorUpdateRefused        editorUpdateState = "refused"
	editorUpdateFailed         editorUpdateState = "failed"
)

const (
	editorRefusalRevisionConflict      editorRefusalReason = "revision_conflict"
	editorRefusalWritesDisabled        editorRefusalReason = "write_operations_disabled"
	editorRefusalUpdateInProgress      editorRefusalReason = "update_in_progress"
	editorRefusalEmptyPatch            editorRefusalReason = "empty_patch"
	editorRefusalRemovedOrUnknownKey   editorRefusalReason = "removed_or_unknown_key"
	editorRefusalNonEditableKey        editorRefusalReason = "non_editable_key"
	editorRefusalDuplicateKey          editorRefusalReason = "duplicate_key"
	editorRefusalInvalidValue          editorRefusalReason = "invalid_value"
	editorRefusalInvalidSource         editorRefusalReason = "invalid_source"
	editorRefusalNoChange              editorRefusalReason = "no_change"
	editorRefusalEnvironmentInvalid    editorRefusalReason = "environment_invalid"
	editorRefusalInvalidCandidate      editorRefusalReason = "invalid_candidate"
	editorRefusalInvalidCPUPointsPatch editorRefusalReason = "invalid_cpu_points_patch"
	editorRefusalMalformedCPUPointsMap editorRefusalReason = "malformed_cpu_points_map"
	editorRefusalUnresolvedUsername    editorRefusalReason = "unresolved_username"
	editorRefusalCPUPointsOvercommit   editorRefusalReason = "cpu_points_overcommit"
)

func allEditorRefusalReasons() []editorRefusalReason {
	return []editorRefusalReason{
		editorRefusalRevisionConflict, editorRefusalWritesDisabled, editorRefusalUpdateInProgress,
		editorRefusalEmptyPatch, editorRefusalRemovedOrUnknownKey, editorRefusalNonEditableKey,
		editorRefusalDuplicateKey, editorRefusalInvalidValue, editorRefusalInvalidSource,
		editorRefusalNoChange, editorRefusalEnvironmentInvalid, editorRefusalInvalidCandidate,
		editorRefusalInvalidCPUPointsPatch, editorRefusalMalformedCPUPointsMap,
		editorRefusalUnresolvedUsername, editorRefusalCPUPointsOvercommit,
	}
}

type editorRefusal struct {
	Reason          editorRefusalReason             `json:"reason"`
	Keys            []string                        `json:"keys,omitempty"`
	Usernames       []string                        `json:"usernames,omitempty"`
	UIDs            []int                           `json:"uids,omitempty"`
	ChangedSources  []string                        `json:"changed_sources,omitempty"`
	ChangedFields   []string                        `json:"changed_fields,omitempty"`
	CurrentRevision *config.EditorCompositeRevision `json:"current_revision,omitempty"`
}

type editorUpdateResult struct {
	State                editorUpdateState      `json:"state"`
	RequestedRevision    string                 `json:"requested_revision,omitempty"`
	PersistedRevision    string                 `json:"persisted_revision,omitempty"`
	AppliedRevision      string                 `json:"applied_revision,omitempty"`
	AppliedDynamicFields []string               `json:"applied_dynamic_fields,omitempty"`
	PendingRestartFields []string               `json:"pending_restart_fields,omitempty"`
	Snapshot             *config.EditorSnapshot `json:"snapshot,omitempty"`
	Refusal              *editorRefusal         `json:"refusal,omitempty"`
}

func (s *Server) registerConfigurationEditorTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_configuration_editor",
		Description: "Get the redacted versioned configuration-editor snapshot",
	}, s.handleGetConfigurationEditor)
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "update_configuration",
		Description: "Persist and synchronously apply a revision-bound partial configuration update",
	}, s.handleUpdateConfiguration)
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "update_cpu_points",
		Description: "Persist and synchronously apply revision-bound CPU Points map changes",
	}, s.handleUpdateCPUPoints)
}

func (s *Server) handleGetConfigurationEditor(_ context.Context, _ *mcp.CallToolRequest, _ getConfigurationEditorArgs) (*mcp.CallToolResult, config.EditorSnapshot, error) {
	if s.stateManager == nil || s.stateManager.GetConfig() == nil {
		return nil, config.EditorSnapshot{}, fmt.Errorf("runtime configuration is not available")
	}
	snapshot, err := config.BuildEditorSnapshot(s.stateManager.GetConfig())
	return &mcp.CallToolResult{}, snapshot, err
}

func (s *Server) handleUpdateConfiguration(ctx context.Context, _ *mcp.CallToolRequest, args updateConfigurationArgs) (_ *mcp.CallToolResult, result editorUpdateResult, err error) {
	defer func() {
		if result.RequestedRevision == "" {
			result.RequestedRevision = args.Revision.Value
		}
		s.recordEditorOperation(ctx, "update_configuration", result)
	}()
	if result, ok := s.beginEditorUpdate(args.Revision); !ok {
		return &mcp.CallToolResult{}, result, nil
	}
	defer s.configWriteActive.Store(false)

	applied := s.stateManager.GetConfig()
	candidate, err := config.PrepareEditorConfigCandidate(applied, args.Revision.Config, args.Changes)
	if err != nil {
		var candidateErr *config.EditorCandidateError
		if errors.As(err, &candidateErr) {
			if candidateErr.Reason == "revision_conflict" {
				return &mcp.CallToolResult{}, s.revisionConflict(args.Revision), nil
			}
			return &mcp.CallToolResult{}, classifyEditorConfigCandidateError(candidateErr), nil
		}
		return &mcp.CallToolResult{}, failedEditorResult(), nil
	}
	_, err = loadEditorPolicy(candidate.Config())
	if err != nil {
		return &mcp.CallToolResult{}, classifyEditorPolicyError(err, configKeys(args.Changes), nil), nil
	}
	persistedConfig, err := candidate.Persist(args.Revision.Config, args.Revision.CPUPoints)
	if err != nil {
		return &mcp.CallToolResult{}, s.revisionOrFailure(args.Revision), nil
	}
	rollback := func() error { return candidate.Rollback(persistedConfig) }
	return &mcp.CallToolResult{}, s.finishEditorPersistence(
		ctx, args.Revision, configKeys(args.Changes), persistedConfig, args.Revision.CPUPoints, rollback,
	), nil
}

func (s *Server) handleUpdateCPUPoints(ctx context.Context, _ *mcp.CallToolRequest, args updateCPUPointsArgs) (_ *mcp.CallToolResult, result editorUpdateResult, err error) {
	defer func() {
		if result.RequestedRevision == "" {
			result.RequestedRevision = args.Revision.Value
		}
		s.recordEditorOperation(ctx, "update_cpu_points", result)
	}()
	if result, ok := s.beginEditorUpdate(args.Revision); !ok {
		return &mcp.CallToolResult{}, result, nil
	}
	defer s.configWriteActive.Store(false)

	applied := s.stateManager.GetConfig()
	persisted, err := config.LoadAndValidate(applied.ConfigFile)
	if err != nil {
		return &mcp.CallToolResult{}, failedEditorResult(), nil
	}
	currentPolicy, err := loadEditorPolicy(persisted)
	if err != nil {
		return &mcp.CallToolResult{}, failedEditorResult(), nil
	}
	if editorIdentityFromPolicy(currentPolicy.Source()) != args.Revision.CPUPoints {
		return &mcp.CallToolResult{}, s.revisionConflict(args.Revision), nil
	}
	changes := make([]cpupoints.PolicyEditorChange, 0, len(args.Changes))
	for _, change := range args.Changes {
		changes = append(changes, cpupoints.PolicyEditorChange{Username: change.Username, Points: change.Points})
	}
	inputs, err := policyInputs(persisted)
	if err != nil {
		return &mcp.CallToolResult{}, failedEditorResult(), nil
	}
	candidate, err := cpupoints.PreparePolicyEditorCandidate(inputs, currentPolicy.Source(), changes, cpupoints.NSSIdentityResolver{})
	if err != nil {
		var candidateErr *cpupoints.PolicyEditorError
		if errors.As(err, &candidateErr) {
			if candidateErr.Reason == "revision_conflict" {
				return &mcp.CallToolResult{}, s.revisionConflict(args.Revision), nil
			}
			return &mcp.CallToolResult{}, classifyPolicyEditorCandidateError(candidateErr), nil
		}
		return &mcp.CallToolResult{}, classifyEditorPolicyError(err, nil, cpuPointsUsernames(args.Changes)), nil
	}
	fresh, err := s.buildEditorPrePersistSnapshot(applied)
	if err != nil || fresh.Revision.Value != args.Revision.Value {
		return &mcp.CallToolResult{}, s.revisionConflict(args.Revision), nil
	}
	persistedPolicy, err := config.PersistCPUPointsEditorCandidate(applied, candidate, args.Revision.Config)
	if err != nil {
		return &mcp.CallToolResult{}, s.revisionOrFailure(args.Revision), nil
	}
	persistedCPU := editorIdentityFromPolicy(persistedPolicy)
	rollback := func() error { return config.RollbackCPUPointsEditorCandidate(applied, candidate, persistedPolicy) }
	return &mcp.CallToolResult{}, s.finishEditorPersistence(
		ctx, args.Revision, nil, args.Revision.Config, persistedCPU, rollback,
	), nil
}

func (s *Server) buildEditorPrePersistSnapshot(applied *config.Config) (config.EditorSnapshot, error) {
	if s.revisionConfirm != nil {
		return s.revisionConfirm(applied)
	}
	return config.BuildEditorSnapshot(applied)
}

func (s *Server) beginEditorUpdate(expected config.EditorCompositeRevision) (editorUpdateResult, bool) {
	if !s.cfg.AllowWriteOps {
		return refusedEditorResult(editorRefusalWritesDisabled, nil, nil), false
	}
	if s.stateManager == nil || s.configReloader == nil || s.stateManager.GetConfig() == nil {
		return failedEditorResult(), false
	}
	if _, ok := s.configReloader.(revisionBoundConfigurationReloader); !ok {
		return failedEditorResult(), false
	}
	if !s.configWriteActive.CompareAndSwap(false, true) {
		return refusedEditorResult(editorRefusalUpdateInProgress, nil, nil), false
	}
	current, err := config.BuildEditorSnapshot(s.stateManager.GetConfig())
	if err != nil {
		s.configWriteActive.Store(false)
		return failedEditorResult(), false
	}
	if expected.Value == "" || !config.SameEditorRevision(current.Revision, expected) {
		s.configWriteActive.Store(false)
		return revisionConflictResult(expected, current.Revision), false
	}
	return editorUpdateResult{}, true
}

func (s *Server) finishEditorPersistence(
	ctx context.Context,
	previous config.EditorCompositeRevision,
	fields []string,
	persistedConfig config.EditorSourceIdentity,
	persistedCPU config.EditorSourceIdentity,
	rollback func() error,
) editorUpdateResult {
	persistedSnapshot, err := config.BuildEditorSnapshot(s.stateManager.GetConfig())
	if err != nil || persistedSnapshot.Revision.Config != persistedConfig || persistedSnapshot.Revision.CPUPoints != persistedCPU {
		_ = rollback()
		return s.revisionConflict(previous)
	}
	reloader := s.configReloader.(revisionBoundConfigurationReloader)
	reloadErr := reloader.ReloadEditorRevision(ctx, persistedSnapshot.Revision)
	classification := config.ClassifyReloadError(reloadErr)
	if reloadErr != nil && !classification.OnlyRestartRequired {
		var revisionConflict interface{ EditorRevisionConflict() }
		if rollback() == nil {
			_ = s.configReloader.Reload(context.Background())
		}
		if errors.As(reloadErr, &revisionConflict) {
			return s.revisionConflict(previous)
		}
		return failedEditorResult()
	}
	snapshot, err := config.BuildEditorSnapshot(s.stateManager.GetConfig())
	if err != nil {
		return failedEditorResult()
	}
	state := editorUpdateApplied
	appliedRevision := snapshot.Revision.Value
	if classification.OnlyRestartRequired {
		state = editorUpdatePendingRestart
		appliedRevision = previous.Value
	}
	result := editorUpdateResult{
		State: state, RequestedRevision: previous.Value, PersistedRevision: snapshot.Revision.Value,
		AppliedRevision: appliedRevision, PendingRestartFields: classification.RestartRequiredFields, Snapshot: &snapshot,
	}
	pending := make(map[string]bool, len(classification.RestartRequiredFields))
	for _, field := range classification.RestartRequiredFields {
		pending[field] = true
	}
	for _, field := range fields {
		if !pending[field] {
			result.AppliedDynamicFields = append(result.AppliedDynamicFields, field)
		}
	}
	return result
}

func (s *Server) revisionOrFailure(expected config.EditorCompositeRevision) editorUpdateResult {
	current, err := config.BuildEditorSnapshot(s.stateManager.GetConfig())
	if err == nil && current.Revision.Value != expected.Value {
		return revisionConflictResult(expected, current.Revision)
	}
	return failedEditorResult()
}

func (s *Server) revisionConflict(expected config.EditorCompositeRevision) editorUpdateResult {
	current, err := config.BuildEditorSnapshot(s.stateManager.GetConfig())
	if err != nil {
		return failedEditorResult()
	}
	return revisionConflictResult(expected, current.Revision)
}

func revisionConflictResult(expected, current config.EditorCompositeRevision) editorUpdateResult {
	return editorUpdateResult{State: editorUpdateRefused, RequestedRevision: expected.Value, Refusal: &editorRefusal{
		Reason: editorRefusalRevisionConflict, ChangedSources: config.ChangedEditorSources(expected, current),
		ChangedFields: config.ChangedEditorFields(expected, current, maximumConflictFields), CurrentRevision: &current,
	}}
}

func refusedEditorResult(reason editorRefusalReason, keys []string, uids []int) editorUpdateResult {
	return editorUpdateResult{State: editorUpdateRefused, Refusal: &editorRefusal{Reason: reason, Keys: keys, UIDs: uids}}
}

func refusedEditorResultForUsers(reason editorRefusalReason, usernames []string) editorUpdateResult {
	return editorUpdateResult{State: editorUpdateRefused, Refusal: &editorRefusal{Reason: reason, Usernames: usernames}}
}

func failedEditorResult() editorUpdateResult { return editorUpdateResult{State: editorUpdateFailed} }

func (s *Server) recordEditorOperation(ctx context.Context, operation string, result editorUpdateResult) {
	if s.logger == nil {
		return
	}
	principal, _ := ctx.Value(principalContextKey{}).(principalKind)
	principalName := "local"
	switch principal {
	case principalOperator:
		principalName = "operator"
	case principalEditor:
		principalName = "editor"
	}
	s.logger.Info("MCP configuration editor operation completed",
		"operation", operation,
		"principal", principalName,
		"state", result.State,
		"requested_revision", result.RequestedRevision,
		"persisted_revision", result.PersistedRevision,
		"applied_revision", result.AppliedRevision,
	)
}

func classifyEditorPolicyError(err error, keys, usernames []string) editorUpdateResult {
	var syntax *cpupoints.PolicySyntaxError
	if errors.As(err, &syntax) {
		return editorUpdateResult{State: editorUpdateRefused, Refusal: &editorRefusal{
			Reason: editorRefusalMalformedCPUPointsMap, Keys: append([]string(nil), keys...), Usernames: append([]string(nil), usernames...),
		}}
	}
	var identity *cpupoints.PolicyIdentityError
	if errors.As(err, &identity) {
		return editorUpdateResult{State: editorUpdateRefused, Refusal: &editorRefusal{
			Reason: editorRefusalUnresolvedUsername, Keys: append([]string(nil), keys...), Usernames: []string{identity.Username},
		}}
	}
	var overcommit *cpupoints.PolicyOvercommitError
	if errors.As(err, &overcommit) {
		return editorUpdateResult{State: editorUpdateRefused, Refusal: &editorRefusal{
			Reason: editorRefusalCPUPointsOvercommit, Keys: append([]string(nil), keys...), Usernames: append([]string(nil), usernames...),
		}}
	}
	return failedEditorResult()
}

func classifyEditorConfigCandidateError(err *config.EditorCandidateError) editorUpdateResult {
	reasons := map[string]editorRefusalReason{
		"empty_patch": editorRefusalEmptyPatch, "removed_or_unknown_key": editorRefusalRemovedOrUnknownKey,
		"non_editable_key": editorRefusalNonEditableKey, "duplicate_key": editorRefusalDuplicateKey,
		"invalid_value": editorRefusalInvalidValue, "invalid_source": editorRefusalInvalidSource,
		"no_change": editorRefusalNoChange, "environment_invalid": editorRefusalEnvironmentInvalid,
		"invalid_candidate": editorRefusalInvalidCandidate,
	}
	reason, ok := reasons[err.Reason]
	if !ok {
		return failedEditorResult()
	}
	return refusedEditorResult(reason, err.Keys, nil)
}

func classifyPolicyEditorCandidateError(err *cpupoints.PolicyEditorError) editorUpdateResult {
	reasons := map[string]editorRefusalReason{
		"empty_patch": editorRefusalEmptyPatch, "invalid_cpu_points_patch": editorRefusalInvalidCPUPointsPatch,
		"no_change": editorRefusalNoChange,
	}
	reason, ok := reasons[err.Reason]
	if !ok {
		return failedEditorResult()
	}
	return refusedEditorResultForUsers(reason, err.Usernames)
}

func configKeys(changes []config.EditorFieldChange) []string {
	keys := make([]string, 0, len(changes))
	for _, change := range changes {
		keys = append(keys, change.Key)
	}
	return keys
}

func cpuPointsUsernames(changes []cpuPointsEditorChange) []string {
	usernames := make([]string, 0, len(changes))
	for _, change := range changes {
		usernames = append(usernames, change.Username)
	}
	return usernames
}

func policyInputs(cfg *config.Config) (cpupoints.PolicyInputs, error) {
	reserve, err := cpupoints.NewReservePoints(uint64(cfg.GetCPUReservePoints()))
	if err != nil {
		return cpupoints.PolicyInputs{}, err
	}
	root, err := cpupoints.NewRootPoints(uint64(cfg.GetCPURootPoints()))
	if err != nil {
		return cpupoints.PolicyInputs{}, err
	}
	bestEffort, err := cpupoints.NewBestEffortPoints(uint64(cfg.GetCPUBestEffortPoints()))
	if err != nil {
		return cpupoints.PolicyInputs{}, err
	}
	path, err := cpupoints.NewPolicyMapPath(cfg.GetCPUPointsFile())
	if err != nil {
		return cpupoints.PolicyInputs{}, err
	}
	return cpupoints.PolicyInputs{Reserve: reserve, Root: root, BestEffort: bestEffort, MapPath: path}, nil
}

func loadEditorPolicy(cfg *config.Config) (cpupoints.PolicySnapshot, error) {
	inputs, err := policyInputs(cfg)
	if err != nil {
		return cpupoints.PolicySnapshot{}, err
	}
	return cpupoints.NewPolicyLoader().Load(inputs, cpupoints.NSSIdentityResolver{})
}

func editorIdentityFromPolicy(source cpupoints.PolicySource) config.EditorSourceIdentity {
	digest := source.Digest()
	return config.EditorSourceIdentity{
		Path: source.Path().String(), Device: source.Device(), Inode: source.Inode(), Size: source.Size(),
		SHA256: fmt.Sprintf("%x", digest[:]),
	}
}
