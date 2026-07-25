package cgroup

import (
	"strings"
	"testing"
)

func TestParsePSIAcceptsSomeOnly(t *testing.T) {
	stats, err := parsePSI("some avg10=2.50 avg60=1.25 avg300=0.50 total=12345\n")
	if err != nil {
		t.Fatalf("parsePSI() error: %v", err)
	}
	if stats.SomeAvg10 != 2.50 || stats.SomeAvg60 != 1.25 || stats.SomeAvg300 != 0.50 {
		t.Fatalf("some averages = %f/%f/%f, want 2.50/1.25/0.50",
			stats.SomeAvg10, stats.SomeAvg60, stats.SomeAvg300)
	}
	if stats.SomeTotal != 12345 {
		t.Fatalf("SomeTotal = %d, want 12345", stats.SomeTotal)
	}
	if stats.FullAvg10 != 0 || stats.FullAvg60 != 0 || stats.FullAvg300 != 0 || stats.FullTotal != 0 {
		t.Fatalf("optional full statistics were not zero: %+v", stats)
	}
}

func TestParsePSIAcceptsSomeAndFull(t *testing.T) {
	content := `some avg10=2.50 avg60=1.25 avg300=0.50 total=12345
full avg10=1.50 avg60=0.75 avg300=0.25 total=6789`
	stats, err := parsePSI(content)
	if err != nil {
		t.Fatalf("parsePSI() error: %v", err)
	}
	if stats.FullAvg10 != 1.50 || stats.FullAvg60 != 0.75 || stats.FullAvg300 != 0.25 {
		t.Fatalf("full averages = %f/%f/%f, want 1.50/0.75/0.25",
			stats.FullAvg10, stats.FullAvg60, stats.FullAvg300)
	}
	if stats.FullTotal != 6789 {
		t.Fatalf("FullTotal = %d, want 6789", stats.FullTotal)
	}
}

func TestParsePSIRejectsContentWithoutSome(t *testing.T) {
	_, err := parsePSI("full avg10=1.50 avg60=0.75 avg300=0.25 total=6789")
	if err == nil || !strings.Contains(err.Error(), "missing 'some' line") {
		t.Fatalf("parsePSI() error = %v, want missing some error", err)
	}
}
