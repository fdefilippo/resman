// Package pressure provides typed, read-only kernel pressure observations.
package pressure

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Resource is one fixed kernel pressure resource.
type Resource string

const (
	CPU    Resource = "cpu"
	IO     Resource = "io"
	Memory Resource = "memory"
)

// UnavailableReason describes a bounded pressure-source failure.
type UnavailableReason string

const (
	ReasonNone    UnavailableReason = "none"
	ReasonRead    UnavailableReason = "read_error"
	ReasonInvalid UnavailableReason = "invalid_data"
)

// Stats contains kernel pressure averages in percent and exact stall totals in us.
// FullAvailable distinguishes an absent optional line from a measured zero.
type Stats struct {
	SomeAvg10     float64
	SomeAvg60     float64
	SomeAvg300    float64
	SomeTotal     uint64
	FullAvg10     float64
	FullAvg60     float64
	FullAvg300    float64
	FullTotal     uint64
	FullAvailable bool
}

// Measurement is one resource observation; unavailable averages are not zeros.
type Measurement struct {
	Stats             Stats
	Available         bool
	UnavailableReason UnavailableReason
}

// Snapshot carries a decision-owned observation of the three system resources.
// It is not an enforcement input or an event-driven trigger observation.
type Snapshot struct {
	At     time.Time
	CPU    Measurement
	IO     Measurement
	Memory Measurement
}

// ReadSystem samples fixed pressure files under root without changing controls.
// At is the owning decision sample timestamp, not a promise of simultaneous reads.
func ReadSystem(root string, at time.Time) Snapshot {
	snapshot := Snapshot{At: at}
	for _, target := range []struct {
		resource Resource
		sample   *Measurement
	}{
		{CPU, &snapshot.CPU}, {IO, &snapshot.IO}, {Memory, &snapshot.Memory},
	} {
		body, err := os.ReadFile(filepath.Join(root, string(target.resource)))
		if err != nil {
			target.sample.UnavailableReason = ReasonRead
			continue
		}
		stats, err := Parse(string(body))
		if err != nil {
			target.sample.UnavailableReason = ReasonInvalid
			continue
		}
		// System CPU full pressure is undefined; newer kernels report a
		// compatibility zero. Do not publish it as meaningful measured pressure.
		// https://docs.kernel.org/accounting/psi.html
		if target.resource == CPU {
			stats.FullAvailable = false
		}
		*target.sample = Measurement{Stats: stats, Available: true, UnavailableReason: ReasonNone}
	}
	return snapshot
}

// Parse validates the shared PSI file format, retaining exact uint64 totals.
// The some line is required; older CPU sources may omit full entirely.
func Parse(content string) (Stats, error) {
	var stats Stats
	hasSome, hasFull := false, false
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] != "some" && fields[0] != "full" {
			continue
		}
		if (fields[0] == "some" && hasSome) || (fields[0] == "full" && hasFull) {
			return Stats{}, fmt.Errorf("duplicate %s PSI line", fields[0])
		}
		avg10, avg60, avg300, total, err := parseLine(fields)
		if err != nil {
			return Stats{}, fmt.Errorf("parse %s PSI line: %w", fields[0], err)
		}
		if fields[0] == "some" {
			stats.SomeAvg10, stats.SomeAvg60, stats.SomeAvg300, stats.SomeTotal = avg10, avg60, avg300, total
			hasSome = true
		} else {
			stats.FullAvg10, stats.FullAvg60, stats.FullAvg300, stats.FullTotal = avg10, avg60, avg300, total
			hasFull = true
		}
	}
	if !hasSome {
		return Stats{}, fmt.Errorf("invalid PSI format: missing 'some' line")
	}
	stats.FullAvailable = hasFull
	return stats, nil
}

func parseLine(fields []string) (float64, float64, float64, uint64, error) {
	values := map[string]float64{}
	seen := map[string]bool{}
	var total uint64
	for _, field := range fields[1:] {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return 0, 0, 0, 0, fmt.Errorf("invalid PSI field")
		}
		switch key {
		case "avg10", "avg60", "avg300", "total":
			if seen[key] {
				return 0, 0, 0, 0, fmt.Errorf("duplicate PSI field %s", key)
			}
			seen[key] = true
		default:
			continue
		}
		if key == "total" {
			var err error
			total, err = strconv.ParseUint(value, 10, 64)
			if err != nil {
				return 0, 0, 0, 0, fmt.Errorf("invalid PSI total: %w", err)
			}
			continue
		}
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > 100 {
			return 0, 0, 0, 0, fmt.Errorf("invalid PSI average %s", key)
		}
		values[key] = parsed
	}
	for _, key := range []string{"avg10", "avg60", "avg300", "total"} {
		if !seen[key] {
			return 0, 0, 0, 0, fmt.Errorf("missing PSI field %s", key)
		}
	}
	return values["avg10"], values["avg60"], values["avg300"], total, nil
}
