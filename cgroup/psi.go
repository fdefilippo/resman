/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <https://www.gnu.org/licenses/>.
 */
// cgroup/psi.go
package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PSIStats contains Pressure Stall Information statistics.
type PSIStats struct {
	SomeAvg10  float64 // Percentage of time with at least one stalled task over 10 seconds
	SomeAvg60  float64 // Percentage of time with at least one stalled task over 60 seconds
	SomeAvg300 float64 // Percentage of time with at least one stalled task over 300 seconds
	SomeTotal  uint64  // Total microseconds stalled in some mode
	FullAvg10  float64 // Percentage of time with all tasks stalled over 10 seconds
	FullAvg60  float64 // Percentage of time with all tasks stalled over 60 seconds
	FullAvg300 float64 // Percentage of time with all tasks stalled over 300 seconds
	FullTotal  uint64  // Total microseconds stalled in full mode
}

// GetPSIStats reads I/O PSI statistics from a user's cgroup.
// It returns an error when io.pressure does not exist or cannot be read.
// It also returns an error when the kernel does not support PSI (CONFIG_PSI=n).
func (m *Manager) GetPSIStats(uid int) (PSIStats, error) {
	cgroupPath, exists := m.getCgroupPath(uid)
	if !exists {
		return PSIStats{}, fmt.Errorf("cgroup for UID %d not found", uid)
	}

	psiFile := filepath.Join(cgroupPath, "io.pressure")
	data, err := os.ReadFile(psiFile)
	if err != nil {
		return PSIStats{}, fmt.Errorf("failed to read io.pressure for UID %d: %w", uid, err)
	}

	return parsePSI(string(data))
}

// parsePSI parses a pressure file. The full line is optional because older
// kernels expose only some pressure for CPU.
//
//	some avg10=25.00 avg60=18.50 avg300=12.30 total=1234567
//	full avg10=10.00 avg60=8.20 avg300=5.10 total=567890
func parsePSI(content string) (PSIStats, error) {
	var stats PSIStats

	lines := strings.Split(strings.TrimSpace(content), "\n")
	hasSome := false
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "some":
			some, err := parsePSILine(line)
			if err != nil {
				return stats, fmt.Errorf("failed to parse 'some' PSI line: %w", err)
			}
			stats.SomeAvg10 = some.avg10
			stats.SomeAvg60 = some.avg60
			stats.SomeAvg300 = some.avg300
			stats.SomeTotal = some.total
			hasSome = true
		case "full":
			full, err := parsePSILine(line)
			if err != nil {
				return stats, fmt.Errorf("failed to parse 'full' PSI line: %w", err)
			}
			stats.FullAvg10 = full.avg10
			stats.FullAvg60 = full.avg60
			stats.FullAvg300 = full.avg300
			stats.FullTotal = full.total
		}
	}
	if !hasSome {
		return stats, fmt.Errorf("invalid PSI format: missing 'some' line")
	}

	return stats, nil
}

type psiLine struct {
	avg10  float64
	avg60  float64
	avg300 float64
	total  uint64
}

func parsePSILine(line string) (psiLine, error) {
	var result psiLine

	// Skip prefix ("some" o "full")
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return result, fmt.Errorf("invalid PSI line: %s", line)
	}

	for _, field := range fields[1:] {
		kv := strings.SplitN(field, "=", 2)
		if len(kv) != 2 {
			continue
		}
		val, err := strconv.ParseFloat(kv[1], 64)
		if err != nil {
			continue
		}
		switch kv[0] {
		case "avg10":
			result.avg10 = val
		case "avg60":
			result.avg60 = val
		case "avg300":
			result.avg300 = val
		case "total":
			result.total = uint64(val)
		}
	}

	return result, nil
}
