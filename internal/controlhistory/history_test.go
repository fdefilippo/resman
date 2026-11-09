package controlhistory

import (
	"testing"
	"time"
)

func TestMergePrefersMemoryAndSeparatesRestartIdentities(t *testing.T) {
	now := time.Now()
	durable := []Entry{
		{Details: Details{SessionID: "a", CycleID: 1}, Timestamp: now, Reason: "stored"},
		{Details: Details{SessionID: "a", CycleID: 2}, Timestamp: now.Add(time.Second)},
	}
	memory := []Entry{
		{Details: Details{SessionID: "a", CycleID: 1}, Timestamp: now, Reason: "memory"},
		{Details: Details{SessionID: "b", CycleID: 1}, Timestamp: now.Add(2 * time.Second)},
	}
	got := Merge(durable, memory, 100)
	if len(got) != 3 || got[0].Reason != "memory" || got[2].SessionID != "b" {
		t.Fatalf("merged=%+v", got)
	}
	got = Merge(durable, memory, 2)
	if len(got) != 2 || got[0].CycleID != 2 || got[1].SessionID != "b" {
		t.Fatalf("latest window=%+v", got)
	}
	for _, tt := range []struct{ input, want int }{{0, 10}, {-1, 10}, {1, 1}, {100000, 1000}} {
		if got := QueryLimit(tt.input); got != tt.want {
			t.Fatalf("limit(%d)=%d want %d", tt.input, got, tt.want)
		}
	}
}
