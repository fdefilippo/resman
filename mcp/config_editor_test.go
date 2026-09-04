package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/cpupoints"
	"github.com/fdefilippo/resman/state"
)

type editorTestReloader struct {
	path      string
	manager   *state.Manager
	reloadErr error
	reloads   int
}

func (r *editorTestReloader) Reload(context.Context) error {
	r.reloads++
	if r.reloadErr != nil {
		return r.reloadErr
	}
	loaded, err := config.LoadAndValidate(r.path)
	if err != nil {
		return err
	}
	r.manager.UpdateConfig(loaded)
	return nil
}

func TestCPUPointsEditorReconfirmsCompositeRevisionAfterPreflightBeforePersistence(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(directory, "cpu-points.map")
	originalMap := []byte("[resman-cpu-points-map-v1]\nnobody=100\n")
	if err := os.WriteFile(mapPath, originalMap, 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "resman.conf")
	if err := os.WriteFile(configPath, []byte("CPU_POINTS_FILE="+mapPath+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	applied, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(applied, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reloader := &editorTestReloader{path: configPath, manager: manager}
	server := &Server{
		cfg: &config.MCPServerConfig{AllowWriteOps: true}, stateManager: manager, configReloader: reloader,
	}

	request, err := config.BuildEditorSnapshot(manager.GetConfig())
	if err != nil {
		t.Fatal(err)
	}
	server.revisionConfirm = func(current *config.Config) (config.EditorSnapshot, error) {
		if err := os.WriteFile(mapPath, []byte("[resman-cpu-points-map-v1]\nnobody=101\n"), 0600); err != nil {
			return config.EditorSnapshot{}, err
		}
		fresh, snapshotErr := config.BuildEditorSnapshot(current)
		if err := os.WriteFile(mapPath, originalMap, 0600); err != nil {
			return config.EditorSnapshot{}, err
		}
		return fresh, snapshotErr
	}

	points := uint64(120)
	_, result, err := server.handleUpdateCPUPoints(context.Background(), nil, updateCPUPointsArgs{
		Revision: request.Revision, Changes: []cpuPointsEditorChange{{Username: "nobody", Points: &points}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != editorUpdateRefused || result.Refusal == nil || result.Refusal.Reason != editorRefusalRevisionConflict {
		t.Fatalf("update result = %+v, want revision conflict after preflight", result)
	}
	if reloader.reloads != 0 {
		t.Fatalf("reload calls = %d, want none after pre-persistence conflict", reloader.reloads)
	}
	content, err := os.ReadFile(mapPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(originalMap) {
		t.Fatalf("CPU Points map = %q, want original source unchanged", content)
	}
}

func (r *editorTestReloader) ReloadEditorRevision(ctx context.Context, expected config.EditorCompositeRevision) error {
	current, err := config.BuildEditorSnapshot(r.manager.GetConfig())
	if err != nil {
		return err
	}
	if !config.SameEditorRevision(current.Revision, expected) {
		return fmt.Errorf("revision-bound test reload received a stale source")
	}
	return r.Reload(ctx)
}

func TestConfigurationEditorWritesFailClosedWhenWriteOperationsAreDisabled(t *testing.T) {
	server := &Server{cfg: &config.MCPServerConfig{AllowWriteOps: false}}
	_, configuration, err := server.handleUpdateConfiguration(context.Background(), nil, updateConfigurationArgs{Revision: config.EditorCompositeRevision{Value: "config-request"}})
	if err != nil || configuration.State != editorUpdateRefused || configuration.RequestedRevision != "config-request" || configuration.Refusal == nil || configuration.Refusal.Reason != editorRefusalWritesDisabled {
		t.Fatalf("configuration result = (%+v, %v)", configuration, err)
	}
	_, points, err := server.handleUpdateCPUPoints(context.Background(), nil, updateCPUPointsArgs{Revision: config.EditorCompositeRevision{Value: "points-request"}})
	if err != nil || points.State != editorUpdateRefused || points.RequestedRevision != "points-request" || points.Refusal == nil || points.Refusal.Reason != editorRefusalWritesDisabled {
		t.Fatalf("CPU Points result = (%+v, %v)", points, err)
	}
}

func TestConfigurationEditorCompatibilityFixturesAreVersionedRedactedAndPermissivelyLicensed(t *testing.T) {
	directory := filepath.Join("..", "protocol", "config-editor")
	license, err := os.ReadFile(filepath.Join(directory, "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(license), "Apache License, Version 2.0") {
		t.Fatal("configuration-editor fixtures lack the explicit permissive grant")
	}
	schemaContent, err := os.ReadFile(filepath.Join(directory, "schema-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertEditorRefusalSchemaMatchesProduction(t, schemaContent)
	for _, name := range []string{"schema-v1.json", "snapshot-v1.json", "update-request-v1.json", "update-result-v1.json"} {
		content, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if err := json.Unmarshal(content, &value); err != nil {
			t.Fatalf("%s is not valid JSON: %v", name, err)
		}
		for _, forbidden := range []string{"top-secret", "bearer-token", "complete_source"} {
			if strings.Contains(string(content), forbidden) {
				t.Errorf("%s contains forbidden fixture material %q", name, forbidden)
			}
		}
	}
	fixtureContent, _ := os.ReadFile(filepath.Join(directory, "snapshot-v1.json"))
	var fixture config.EditorSnapshot
	if err := json.Unmarshal(fixtureContent, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SchemaVersion != "resman.config-editor.v1" {
		t.Fatalf("fixture schema version = %q", fixture.SchemaVersion)
	}
	for _, field := range fixture.Fields {
		if field.Contract.Sensitive && (field.AuthoredValue != nil || field.EffectiveValue != nil) {
			t.Fatalf("sensitive fixture field exposes a value: %+v", field)
		}
	}
	requestContent, _ := os.ReadFile(filepath.Join(directory, "update-request-v1.json"))
	var request updateConfigurationArgs
	if err := json.Unmarshal(requestContent, &request); err != nil {
		t.Fatal(err)
	}
	if request.Revision.Value == "" || len(request.Changes) != 1 || request.Changes[0].Key != "CPU_THRESHOLD" {
		t.Fatalf("update request fixture = %+v", request)
	}
	resultContent, _ := os.ReadFile(filepath.Join(directory, "update-result-v1.json"))
	var result editorUpdateResult
	if err := json.Unmarshal(resultContent, &result); err != nil {
		t.Fatal(err)
	}
	if result.State != editorUpdateApplied || result.RequestedRevision == "" || result.PersistedRevision == result.RequestedRevision {
		t.Fatalf("update result fixture = %+v", result)
	}
}

func assertEditorRefusalSchemaMatchesProduction(t *testing.T, content []byte) {
	t.Helper()
	var schema struct {
		Definitions map[string]struct {
			Properties map[string]struct {
				Properties map[string]struct {
					Enum []string `json:"enum"`
				} `json:"properties"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(content, &schema); err != nil {
		t.Fatal(err)
	}
	actual := schema.Definitions["updateResult"].Properties["refusal"].Properties["reason"].Enum
	wantReasons := allEditorRefusalReasons()
	want := make([]string, 0, len(wantReasons))
	for _, reason := range wantReasons {
		want = append(want, string(reason))
	}
	slices.Sort(actual)
	slices.Sort(want)
	if !slices.Equal(actual, want) {
		t.Fatalf("schema refusal reasons = %v, production reasons = %v", actual, want)
	}
}

func TestRevisionConflictIsBoundedAndContainsNoSensitiveValues(t *testing.T) {
	expected := config.EditorCompositeRevision{
		Value: "old", Config: config.EditorSourceIdentity{Path: "/etc/resman/resman.conf", SHA256: strings.Repeat("a", 64)},
		CPUPoints:    config.EditorSourceIdentity{Path: "/etc/resman/cpu-points.map", SHA256: strings.Repeat("b", 64)},
		PublicValues: map[string]string{"CPU_THRESHOLD": "70", "LOG_LEVEL": "INFO"},
	}
	current := config.EditorCompositeRevision{
		Value: "new", Config: config.EditorSourceIdentity{Path: "/etc/resman/resman.conf", SHA256: strings.Repeat("c", 64)},
		CPUPoints:    expected.CPUPoints,
		PublicValues: map[string]string{"CPU_THRESHOLD": "80", "LOG_LEVEL": "INFO"},
	}
	result := revisionConflictResult(expected, current)
	if result.State != editorUpdateRefused || result.Refusal == nil || result.Refusal.Reason != "revision_conflict" {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Refusal.ChangedSources) != 1 || result.Refusal.ChangedSources[0] != "configuration" {
		t.Fatalf("changed sources = %v", result.Refusal.ChangedSources)
	}
	if len(result.Refusal.ChangedFields) != 1 || result.Refusal.ChangedFields[0] != "CPU_THRESHOLD" {
		t.Fatalf("changed fields = %v", result.Refusal.ChangedFields)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "MCP_AUTH_TOKEN") || strings.Contains(string(encoded), "secret") {
		t.Fatalf("conflict response exposed sensitive material: %s", encoded)
	}
}

func TestConfigurationEditorClassifiesRefusalsWithoutReturningSubmittedValues(t *testing.T) {
	configResult := classifyEditorPolicyError(
		fmt.Errorf("candidate policy: %w", &cpupoints.PolicyOvercommitError{Reserve: 100, Pool: 900, Guarantees: 750, Root: 100, BestEffort: 100, Total: 950}),
		[]string{"CPU_BEST_EFFORT_POINTS"}, nil,
	)
	if configResult.Refusal == nil || configResult.Refusal.Reason != "cpu_points_overcommit" || len(configResult.Refusal.Keys) != 1 || configResult.Refusal.Keys[0] != "CPU_BEST_EFFORT_POINTS" {
		t.Fatalf("overcommit result = %+v", configResult)
	}

	identityResult := classifyEditorPolicyError(
		fmt.Errorf("candidate policy: %w", &cpupoints.PolicyIdentityError{Username: "unknown", Cause: errors.New("not resolved")}),
		nil, []string{"unknown"},
	)
	if identityResult.Refusal == nil || identityResult.Refusal.Reason != "unresolved_username" || len(identityResult.Refusal.Usernames) != 1 || identityResult.Refusal.Usernames[0] != "unknown" {
		t.Fatalf("identity result = %+v", identityResult)
	}

	syntaxResult := classifyEditorPolicyError(
		fmt.Errorf("candidate policy: %w", &cpupoints.PolicySyntaxError{Cause: errors.New("bad marker")}),
		nil, []string{"alice"},
	)
	if syntaxResult.Refusal == nil || syntaxResult.Refusal.Reason != "malformed_cpu_points_map" {
		t.Fatalf("syntax result = %+v", syntaxResult)
	}
	encoded, _ := json.Marshal([]editorUpdateResult{configResult, identityResult, syntaxResult})
	for _, forbidden := range []string{"850", "100", "bad marker", "not resolved"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("typed refusal exposed submitted or internal detail %q: %s", forbidden, encoded)
		}
	}
}

func TestConfigurationEditorPersistsAndSynchronouslyPublishesBothSourceKinds(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(directory, "cpu-points.map")
	if err := os.WriteFile(mapPath, []byte("[resman-cpu-points-map-v1]\n# keep map comment\nnobody=100\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "resman.conf")
	configBody := "# keep config comment\nCPU_POINTS_FILE=" + mapPath + "\nCPU_THRESHOLD=70\nMCP_AUTH_TOKEN=exact-secret\n"
	if err := os.WriteFile(configPath, []byte(configBody), 0600); err != nil {
		t.Fatal(err)
	}
	applied, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(applied, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reloader := &editorTestReloader{path: configPath, manager: manager}
	server := &Server{
		cfg: &config.MCPServerConfig{AllowWriteOps: true}, stateManager: manager, configReloader: reloader,
	}

	first, err := config.BuildEditorSnapshot(manager.GetConfig())
	if err != nil {
		t.Fatal(err)
	}
	_, configResult, err := server.handleUpdateConfiguration(context.Background(), nil, updateConfigurationArgs{
		Revision: first.Revision, Changes: []config.EditorFieldChange{{Key: "CPU_THRESHOLD", Value: "65"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if configResult.State != editorUpdateApplied || configResult.RequestedRevision != first.Revision.Value || configResult.PersistedRevision == first.Revision.Value || configResult.AppliedRevision != configResult.PersistedRevision {
		t.Fatalf("configuration result = %+v", configResult)
	}
	if manager.GetConfig().GetCPUThreshold() != 65 {
		t.Fatalf("runtime CPU threshold = %d, want 65", manager.GetConfig().GetCPUThreshold())
	}
	configContent, _ := os.ReadFile(configPath)
	for _, required := range []string{"# keep config comment", "CPU_THRESHOLD=65", "MCP_AUTH_TOKEN=exact-secret"} {
		if !strings.Contains(string(configContent), required) {
			t.Errorf("persisted configuration omits %q", required)
		}
	}

	second, err := config.BuildEditorSnapshot(manager.GetConfig())
	if err != nil {
		t.Fatal(err)
	}
	points := uint64(120)
	_, pointsResult, err := server.handleUpdateCPUPoints(context.Background(), nil, updateCPUPointsArgs{
		Revision: second.Revision, Changes: []cpuPointsEditorChange{{Username: "nobody", Points: &points}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pointsResult.State != editorUpdateApplied || pointsResult.RequestedRevision != second.Revision.Value || pointsResult.PersistedRevision == second.Revision.Value || pointsResult.AppliedRevision != pointsResult.PersistedRevision {
		t.Fatalf("CPU Points result = %+v", pointsResult)
	}
	mapContent, _ := os.ReadFile(mapPath)
	for _, required := range []string{"# keep map comment", "nobody=120"} {
		if !strings.Contains(string(mapContent), required) {
			t.Errorf("persisted CPU Points map omits %q", required)
		}
	}

	third, err := config.BuildEditorSnapshot(manager.GetConfig())
	if err != nil {
		t.Fatal(err)
	}
	reloader.reloadErr = &config.RestartRequiredError{Fields: []string{"LOG_FILE"}}
	_, pendingResult, err := server.handleUpdateConfiguration(context.Background(), nil, updateConfigurationArgs{
		Revision: third.Revision, Changes: []config.EditorFieldChange{{Key: "LOG_FILE", Value: "/tmp/pending-resman.log"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pendingResult.State != editorUpdatePendingRestart || pendingResult.AppliedRevision != third.Revision.Value || len(pendingResult.PendingRestartFields) != 1 || pendingResult.PendingRestartFields[0] != "LOG_FILE" {
		t.Fatalf("pending restart result = %+v", pendingResult)
	}
	if pendingResult.Snapshot == nil {
		t.Fatal("pending restart result omitted canonical snapshot")
	}
	foundPending := false
	for _, field := range pendingResult.Snapshot.Fields {
		if field.Contract.Key == "LOG_FILE" && field.ApplicationState == config.EditorStatePendingRestart {
			foundPending = true
		}
	}
	if !foundPending {
		t.Fatal("persisted restart-required field did not remain pending in canonical snapshot")
	}
}
