package pressure

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePreservesTotalsAndOptionalCoverage(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		full       bool
	}{
		{"some only", "some avg10=0 avg60=1.25 avg300=100 total=9007199254740993", false},
		{"some and full", "some avg10=0 avg60=1.25 avg300=100 total=9007199254740993\nfull avg10=0 avg60=0 avg300=0 total=18446744073709551615", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stats, err := Parse(tt.body)
			if err != nil {
				t.Fatal(err)
			}
			if stats.SomeTotal != 9007199254740993 || stats.FullAvailable != tt.full {
				t.Fatalf("stats=%+v", stats)
			}
			if tt.full && stats.FullTotal != ^uint64(0) {
				t.Fatalf("full total=%d", stats.FullTotal)
			}
		})
	}
}

func TestParseRejectsInvalidRecognizedFields(t *testing.T) {
	valid := "some avg10=1 avg60=2 avg300=3 total=4"
	for _, tt := range []struct{ name, body string }{
		{"empty", ""}, {"no some", "full avg10=0 avg60=0 avg300=0 total=0"},
		{"missing field", "some avg10=1 avg60=2 total=4 extra=5"},
		{"duplicate field", valid + " avg10=2"}, {"duplicate line", valid + "\n" + valid},
		{"NaN", strings.Replace(valid, "avg10=1", "avg10=NaN", 1)},
		{"infinity", strings.Replace(valid, "avg60=2", "avg60=Inf", 1)},
		{"negative", strings.Replace(valid, "avg300=3", "avg300=-1", 1)},
		{"over100", strings.Replace(valid, "avg300=3", "avg300=100.1", 1)},
		{"fractional total", strings.Replace(valid, "total=4", "total=4.5", 1)},
		{"negative total", strings.Replace(valid, "total=4", "total=-1", 1)},
		{"overflow total", strings.Replace(valid, "total=4", "total=18446744073709551616", 1)},
		{"invalid number", strings.Replace(valid, "avg10=1", "avg10=no", 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse(tt.body); err == nil {
				t.Fatal("invalid pressure accepted")
			}
		})
	}
}

func TestReadSystemReportsUnavailableAndUndefinedModes(t *testing.T) {
	dir := t.TempDir()
	valid := []byte("some avg10=1 avg60=2 avg300=3 total=4\nfull avg10=0 avg60=0 avg300=0 total=0\n")
	if err := os.WriteFile(filepath.Join(dir, "cpu"), valid, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory"), valid, 0600); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1700000000, 0)
	got := ReadSystem(dir, at)
	if !got.At.Equal(at) || !got.CPU.Available || got.CPU.Stats.FullAvailable || !got.Memory.Stats.FullAvailable || got.IO.Available || got.IO.UnavailableReason != ReasonRead {
		t.Fatalf("snapshot=%+v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory"), []byte("some broken"), 0600); err != nil {
		t.Fatal(err)
	}
	got = ReadSystem(dir, at.Add(time.Second))
	if got.Memory.Available || got.Memory.UnavailableReason != ReasonInvalid {
		t.Fatalf("invalid memory=%+v", got.Memory)
	}
}
