package ioweights

import (
	"errors"
	"testing"

	"github.com/fdefilippo/resman/internal/cpupoints"
)

type exactResolver map[string][]cpupoints.ResolvedUserIdentity

func (r exactResolver) ResolveExactUsername(username string) ([]cpupoints.ResolvedUserIdentity, error) {
	identities, ok := r[username]
	if !ok {
		return nil, errors.New("not found")
	}
	return identities, nil
}

func TestPolicyBuildsExactImmutablePlan(t *testing.T) {
	root, _ := NewWeight(200)
	defaultIO, _ := NewWeight(100)
	path, _ := NewPolicyMapPath("/etc/resman/io-weights.map")
	policy, err := NewPolicyLoader().LoadContent(PolicyInputs{Root: root, Default: defaultIO, MapPath: path}, []byte(PolicyMapMarker+"\nalice=700\n"), exactResolver{
		"alice": {{Username: "alice", UID: 1000}},
	})
	if err != nil {
		t.Fatalf("LoadContent() error = %v", err)
	}
	plan, err := Plan(policy, []ActiveUserSlice{{UID: 1001, Eligible: true}, {UID: 0}, {UID: 1000, Eligible: true}, {UID: 1002, Eligible: false}})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	want := []struct {
		uid    int
		class  SliceClass
		weight uint64
	}{{0, SliceClassRoot, 200}, {1000, SliceClassMapped, 700}, {1001, SliceClassDefault, 100}, {1002, SliceClassDefault, 100}}
	for index, expected := range want {
		if plan[index].UID() != expected.uid || plan[index].Class() != expected.class || plan[index].Weight().Value() != expected.weight {
			t.Fatalf("plan[%d] = %+v, want %+v", index, plan[index], expected)
		}
	}
	entries := policy.Entries()
	entries[0].weight = defaultIO
	if mapped, _ := policy.WeightForUID(1000); mapped.Weight().Value() != 700 {
		t.Fatal("mutating Entries() changed immutable policy")
	}
}

func TestPolicyRejectsUnsafeOrAmbiguousContentAtomically(t *testing.T) {
	root, _ := NewWeight(100)
	defaultIO, _ := NewWeight(100)
	path, _ := NewPolicyMapPath("/etc/resman/io-weights.map")
	inputs := PolicyInputs{Root: root, Default: defaultIO, MapPath: path}
	resolver := exactResolver{
		"root":  {{Username: "root", UID: 0}},
		"alice": {{Username: "alice", UID: 1000}},
		"alias": {{Username: "alias", UID: 1000}},
	}
	tests := []string{
		"alice=100\n",
		PolicyMapMarker + "\nalice=0100\n",
		PolicyMapMarker + "\nalice=0\n",
		PolicyMapMarker + "\nroot=100\n",
		PolicyMapMarker + "\nalice=100\nalias=200\n",
	}
	for _, content := range tests {
		if _, err := NewPolicyLoader().LoadContent(inputs, []byte(content), resolver); err == nil {
			t.Fatalf("LoadContent(%q) unexpectedly succeeded", content)
		}
	}
}

func TestPlanRejectsDuplicateParticipants(t *testing.T) {
	weight, _ := NewWeight(100)
	_, err := Plan(NewEmptyPolicySnapshot(weight, weight), []ActiveUserSlice{{UID: 1000}, {UID: 1000}})
	if err == nil {
		t.Fatal("Plan() accepted duplicate UID")
	}
}
