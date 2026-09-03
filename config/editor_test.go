package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEditorFixture(t *testing.T, configBody string) (*Config, string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatalf("chmod fixture directory: %v", err)
	}
	mapPath := filepath.Join(directory, "cpu-points.map")
	if err := os.WriteFile(mapPath, []byte("[resman-cpu-points-map-v1]\nroot=100\n"), 0600); err != nil {
		t.Fatalf("write CPU Points fixture: %v", err)
	}
	configPath := filepath.Join(directory, "resman.conf")
	body := "CPU_POINTS_FILE=" + mapPath + "\n" + configBody
	if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	applied := DefaultConfig()
	applied.ConfigFile = configPath
	applied.CPUPointsFile = mapPath
	return applied, configPath
}

func TestEditorSnapshotRedactsSecretsAndDerivesPersistentApplicationState(t *testing.T) {
	t.Setenv("CPU_THRESHOLD", "80")
	applied, _ := writeEditorFixture(t, "# operator note\nCPU_THRESHOLD=70\nLOG_FILE=/persisted/new.log\nMCP_AUTH_TOKEN=top-secret\nLIMIT_HOOK_URL=https://user:secret@example.test/hook\n")
	applied.CPUThreshold = 80
	applied.MCPAuthToken = "top-secret"
	applied.LimitHookURL = "https://user:secret@example.test/hook"

	snapshot, err := BuildEditorSnapshot(applied)
	if err != nil {
		t.Fatalf("BuildEditorSnapshot() error = %v", err)
	}
	if got, want := snapshot.SourcePrecedence, []ConfigSource{ConfigSourceDefault, ConfigSourceFile, ConfigSourceEnvironment}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("source precedence = %v, want %v", got, want)
	}
	fields := make(map[string]EditorFieldSnapshot, len(snapshot.Fields))
	for _, field := range snapshot.Fields {
		fields[field.Contract.Key] = field
	}
	if fields["CPU_THRESHOLD"].ApplicationState != EditorStateEnvironmentShadowed {
		t.Fatalf("CPU_THRESHOLD state = %s", fields["CPU_THRESHOLD"].ApplicationState)
	}
	if fields["LOG_FILE"].ApplicationState != EditorStatePendingRestart {
		t.Fatalf("LOG_FILE state = %s", fields["LOG_FILE"].ApplicationState)
	}
	for _, key := range []string{"MCP_AUTH_TOKEN", "LIMIT_HOOK_URL"} {
		field := fields[key]
		if !field.SecretSet || field.AuthoredValue != nil || field.EffectiveValue != nil {
			t.Errorf("%s redaction = %+v", key, field)
		}
	}
	if snapshot.Revision.PublicValues["CPU_THRESHOLD"] != "70" {
		t.Fatalf("revision authored CPU_THRESHOLD = %q, want 70", snapshot.Revision.PublicValues["CPU_THRESHOLD"])
	}
	for _, key := range []string{"MCP_AUTH_TOKEN", "MCP_EDITOR_AUTH_TOKEN", "LIMIT_HOOK_URL"} {
		if _, exists := snapshot.Revision.PublicValues[key]; exists {
			t.Errorf("revision public values expose sensitive key %s", key)
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	for _, forbidden := range []string{"top-secret", "user:secret", "# operator note", "MCP_AUTH_TOKEN=top-secret"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("snapshot leaked %q", forbidden)
		}
	}
}

func TestEditorSourceIdentityUsesLosslessJSONIntegersForBrowserClients(t *testing.T) {
	want := EditorSourceIdentity{Path: "/etc/resman/resman.conf", Device: ^uint64(0), Inode: 1<<63 + 7, Size: 1<<62 + 9, SHA256: strings.Repeat("a", 64)}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, quoted := range []string{`"device":"18446744073709551615"`, `"inode":"9223372036854775815"`, `"size":"4611686018427387913"`} {
		if !strings.Contains(string(encoded), quoted) {
			t.Fatalf("identity JSON = %s, want lossless %s", encoded, quoted)
		}
	}
	var got EditorSourceIdentity
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("identity round trip = %+v, want %+v", got, want)
	}
}

func TestEditorConfigCandidatePreservesUnmentionedSecretBytesAndInlineComments(t *testing.T) {
	applied, configPath := writeEditorFixture(t, "# keep this comment\nCPU_THRESHOLD=70 # preserve inline explanation\nMCP_AUTH_TOKEN=exact-secret-bytes\n")
	snapshot, err := BuildEditorSnapshot(applied)
	if err != nil {
		t.Fatalf("BuildEditorSnapshot() error = %v", err)
	}
	candidate, err := PrepareEditorConfigCandidate(applied, snapshot.Revision.Config, []EditorFieldChange{{Key: "CPU_THRESHOLD", Value: "65"}})
	if err != nil {
		t.Fatalf("PrepareEditorConfigCandidate() error = %v", err)
	}
	persisted, err := candidate.Persist(snapshot.Revision.Config, snapshot.Revision.CPUPoints)
	if err != nil {
		t.Fatalf("Persist() error = %v", err)
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	for _, required := range []string{"# keep this comment", "CPU_THRESHOLD=65 # preserve inline explanation", "MCP_AUTH_TOKEN=exact-secret-bytes"} {
		if !strings.Contains(string(content), required) {
			t.Errorf("persisted config omits %q: %s", required, content)
		}
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("persisted mode = %04o, want 0600", info.Mode().Perm())
	}
	backup, err := os.ReadFile(configPath + configBackupSuffix)
	if err != nil {
		t.Fatalf("read secure editor backup: %v", err)
	}
	if !strings.Contains(string(backup), "CPU_THRESHOLD=70") || strings.Contains(string(backup), "CPU_THRESHOLD=65") {
		t.Fatalf("editor backup content = %q", backup)
	}
	backupInfo, err := os.Stat(configPath + configBackupSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if backupInfo.Mode().Perm() != 0600 {
		t.Fatalf("editor backup mode = %04o, want 0600", backupInfo.Mode().Perm())
	}
	if err := candidate.Rollback(persisted); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	restored, _ := os.ReadFile(configPath)
	if strings.Contains(string(restored), "CPU_THRESHOLD=65") || !strings.Contains(string(restored), "CPU_THRESHOLD=70") {
		t.Fatalf("rollback content = %s", restored)
	}
}

func TestEditorConfigCandidateRejectsRemovedDuplicateAndStaleInput(t *testing.T) {
	applied, configPath := writeEditorFixture(t, "CPU_THRESHOLD=70\n")
	snapshot, err := BuildEditorSnapshot(applied)
	if err != nil {
		t.Fatal(err)
	}
	removedKey := "MIN_SYSTEM_" + "CORES"
	if _, err := PrepareEditorConfigCandidate(applied, snapshot.Revision.Config, []EditorFieldChange{{Key: removedKey, Value: "1"}}); err == nil {
		t.Fatal("removed key accepted")
	} else {
		assertEditorCandidateError(t, err, "removed_or_unknown_key", removedKey)
	}
	if _, err := PrepareEditorConfigCandidate(applied, snapshot.Revision.Config, []EditorFieldChange{{Key: "CPU_THRESHOLD", Value: "70"}, {Key: "CPU_THRESHOLD", Value: "60"}}); err == nil {
		t.Fatal("duplicate patch key accepted")
	} else {
		assertEditorCandidateError(t, err, "duplicate_key", "CPU_THRESHOLD")
	}
	if err := os.WriteFile(configPath, []byte("CPU_POINTS_FILE="+applied.CPUPointsFile+"\nCPU_THRESHOLD=71\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareEditorConfigCandidate(applied, snapshot.Revision.Config, []EditorFieldChange{{Key: "CPU_THRESHOLD", Value: "60"}}); err == nil {
		t.Fatal("stale source revision accepted")
	} else {
		assertEditorCandidateError(t, err, "revision_conflict", "CPU_THRESHOLD")
	}
}

func assertEditorCandidateError(t *testing.T, err error, reason, key string) {
	t.Helper()
	var typed *EditorCandidateError
	if !errors.As(err, &typed) || typed.Reason != reason || len(typed.Keys) != 1 || typed.Keys[0] != key {
		t.Fatalf("error = %+v, want %s for %s", typed, reason, key)
	}
}

func TestEditorConfigCandidateRollbackRefusesToOverwriteAConcurrentWriter(t *testing.T) {
	applied, configPath := writeEditorFixture(t, "CPU_THRESHOLD=70\n")
	snapshot, err := BuildEditorSnapshot(applied)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := PrepareEditorConfigCandidate(applied, snapshot.Revision.Config, []EditorFieldChange{{Key: "CPU_THRESHOLD", Value: "65"}})
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := candidate.Persist(snapshot.Revision.Config, snapshot.Revision.CPUPoints)
	if err != nil {
		t.Fatal(err)
	}
	concurrent := []byte("CPU_POINTS_FILE=" + applied.CPUPointsFile + "\nCPU_THRESHOLD=62\n")
	if err := os.WriteFile(configPath, concurrent, 0600); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Rollback(persisted); err == nil {
		t.Fatal("Rollback() overwrote a source changed after editor persistence")
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(concurrent) {
		t.Fatalf("concurrent content was overwritten: %q", content)
	}
}

func TestEditorConfigCandidateReconfirmsBothSourcesAfterWritingTheBackup(t *testing.T) {
	applied, configPath := writeEditorFixture(t, "CPU_THRESHOLD=70\n")
	snapshot, err := BuildEditorSnapshot(applied)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := PrepareEditorConfigCandidate(applied, snapshot.Revision.Config, []EditorFieldChange{{Key: "CPU_THRESHOLD", Value: "65"}})
	if err != nil {
		t.Fatal(err)
	}
	concurrent := []byte("CPU_POINTS_FILE=" + applied.CPUPointsFile + "\nCPU_THRESHOLD=62\n")
	candidate.writer = func(path string, content []byte, metadata configFileMetadata) (bool, error) {
		committed, writeErr := writeFileAtomically(path, content, metadata)
		if writeErr == nil && path == configPath+configBackupSuffix {
			if err := os.WriteFile(configPath, concurrent, 0600); err != nil {
				return committed, err
			}
		}
		return committed, writeErr
	}
	if _, err := candidate.Persist(snapshot.Revision.Config, snapshot.Revision.CPUPoints); err == nil || !strings.Contains(err.Error(), "immediately before persistence") {
		t.Fatalf("Persist() error = %v, want final composite-source refusal", err)
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(concurrent) {
		t.Fatalf("concurrent content was overwritten: %q", content)
	}
}
