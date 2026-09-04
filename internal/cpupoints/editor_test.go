package cpupoints

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type editorIdentityResolver map[string]int

func (r editorIdentityResolver) ResolveExactUsername(username string) ([]ResolvedUserIdentity, error) {
	uid, ok := r[username]
	if !ok {
		return nil, nil
	}
	return []ResolvedUserIdentity{{Username: username, UID: uid}}, nil
}

func editorPoints(value uint64) *uint64 { return &value }

func TestPolicyEditorCandidatePreservesCommentsAndRejectsSourceRaces(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "cpu-points.map")
	original := PolicyMapMarker + "\n# preserve policy note\nalice=300\nbob=200\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	mapPath, _ := NewPolicyMapPath(path)
	reserve, _ := NewReservePoints(100)
	root, _ := NewRootPoints(100)
	bestEffort, _ := NewBestEffortPoints(100)
	inputs := PolicyInputs{Reserve: reserve, Root: root, BestEffort: bestEffort, MapPath: mapPath}
	resolver := editorIdentityResolver{"alice": 1001, "bob": 1002, "carol": 1003}
	current, err := NewPolicyLoader().Load(inputs, resolver)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := PreparePolicyEditorCandidate(inputs, current.Source(), []PolicyEditorChange{
		{Username: "alice", Points: editorPoints(400)},
		{Username: "bob", Points: nil},
		{Username: "carol", Points: editorPoints(100)},
	}, resolver)
	if err != nil {
		t.Fatalf("PreparePolicyEditorCandidate() error = %v", err)
	}
	if len(candidate.Policy().Entries()) != 2 {
		t.Fatalf("candidate entries = %d, want 2", len(candidate.Policy().Entries()))
	}
	persisted, err := candidate.Persist(func() error { return nil })
	if err != nil {
		t.Fatalf("Persist() error = %v", err)
	}
	content, _ := os.ReadFile(path)
	for _, required := range []string{"# preserve policy note", "alice=400", "carol=100"} {
		if !strings.Contains(string(content), required) {
			t.Errorf("persisted map omits %q: %s", required, content)
		}
	}
	if strings.Contains(string(content), "bob=") {
		t.Errorf("persisted map retained removed user: %s", content)
	}
	if err := candidate.Rollback(persisted); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	current, err = NewPolicyLoader().Load(inputs, resolver)
	if err != nil {
		t.Fatalf("reload restored source: %v", err)
	}
	stale, err := PreparePolicyEditorCandidate(inputs, current.Source(), []PolicyEditorChange{{Username: "alice", Points: editorPoints(301)}}, resolver)
	if err != nil {
		t.Fatalf("prepare after rollback: %v", err)
	}
	if err := os.WriteFile(path, []byte(PolicyMapMarker+"\nalice=301\nbob=200\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Persist(func() error { return nil }); err == nil {
		t.Fatal("Persist() accepted a map changed after candidate preparation")
	}
}

func TestPolicyEditorCandidateRejectsOvercommitAndUnresolvedUsers(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "cpu-points.map")
	if err := os.WriteFile(path, []byte(PolicyMapMarker+"\nalice=300\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mapPath, _ := NewPolicyMapPath(path)
	reserve, _ := NewReservePoints(100)
	root, _ := NewRootPoints(100)
	bestEffort, _ := NewBestEffortPoints(100)
	inputs := PolicyInputs{Reserve: reserve, Root: root, BestEffort: bestEffort, MapPath: mapPath}
	resolver := editorIdentityResolver{"alice": 1001}
	current, err := NewPolicyLoader().Load(inputs, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PreparePolicyEditorCandidate(inputs, current.Source(), []PolicyEditorChange{{Username: "alice", Points: editorPoints(900)}}, resolver); err == nil {
		t.Fatal("overcommitted policy accepted")
	} else {
		var typed *PolicyOvercommitError
		if !errors.As(err, &typed) {
			t.Fatalf("overcommit error = %T %v, want PolicyOvercommitError", err, err)
		}
	}
	if _, err := PreparePolicyEditorCandidate(inputs, current.Source(), []PolicyEditorChange{{Username: "unknown", Points: editorPoints(1)}}, resolver); err == nil {
		t.Fatal("unresolved username accepted")
	} else {
		var typed *PolicyIdentityError
		if !errors.As(err, &typed) || typed.Username != "unknown" {
			t.Fatalf("identity error = %T %v, want PolicyIdentityError for unknown", err, err)
		}
	}
	if _, err := PreparePolicyEditorCandidate(inputs, current.Source(), []PolicyEditorChange{{Username: "bad\nname", Points: editorPoints(1)}}, resolver); err == nil {
		t.Fatal("invalid username accepted")
	} else {
		var typed *PolicyEditorError
		if !errors.As(err, &typed) || typed.Reason != "invalid_cpu_points_patch" || len(typed.Usernames) != 1 || typed.Usernames[0] != "bad\nname" {
			t.Fatalf("patch error = %+v, want typed invalid_cpu_points_patch", typed)
		}
	}
}

func TestPolicyEditorCandidateRollbackRefusesToOverwriteAConcurrentWriter(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "cpu-points.map")
	original := PolicyMapMarker + "\nalice=300\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	mapPath, _ := NewPolicyMapPath(path)
	reserve, _ := NewReservePoints(100)
	root, _ := NewRootPoints(100)
	bestEffort, _ := NewBestEffortPoints(100)
	inputs := PolicyInputs{Reserve: reserve, Root: root, BestEffort: bestEffort, MapPath: mapPath}
	resolver := editorIdentityResolver{"alice": 1001}
	current, err := NewPolicyLoader().Load(inputs, resolver)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := PreparePolicyEditorCandidate(inputs, current.Source(), []PolicyEditorChange{{Username: "alice", Points: editorPoints(400)}}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := candidate.Persist(func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	concurrent := []byte(PolicyMapMarker + "\nalice=350\n")
	if err := os.WriteFile(path, concurrent, 0600); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Rollback(persisted); err == nil {
		t.Fatal("Rollback() overwrote a source changed after editor persistence")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(concurrent) {
		t.Fatalf("concurrent content was overwritten: %q", content)
	}
}

func FuzzMergePolicyMap(f *testing.F) {
	f.Add("alice", uint64(300), false)
	f.Add("bob", uint64(1), true)
	f.Fuzz(func(t *testing.T, username string, points uint64, remove bool) {
		var value *uint64
		if !remove {
			value = &points
		}
		_, _ = mergePolicyMap([]byte(PolicyMapMarker+"\n# preserved\nalice=100\n"), []PolicyEditorChange{{Username: username, Points: value}})
	})
}
