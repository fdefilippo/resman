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

// PSIStats contiene le statistiche Pressure Stall Information per IO.
type PSIStats struct {
	SomeAvg10  float64 // % di tempo con almeno un task stallato (media 10s)
	SomeAvg60  float64 // % di tempo con almeno un task stallato (media 60s)
	SomeAvg300 float64 // % di tempo con almeno un task stallato (media 300s)
	SomeTotal  uint64  // Microsecondi totali di stall (some)
	FullAvg10  float64 // % di tempo con tutti i task stallati (media 10s)
	FullAvg60  float64 // % di tempo con tutti i task stallati (media 60s)
	FullAvg300 float64 // % di tempo con tutti i task stallati (media 300s)
	FullTotal  uint64  // Microsecondi totali di stall (full)
}

// GetPSIStats legge le statistiche PSI per IO dal cgroup di un utente.
// Restituisce un errore se il file io.pressure non esiste o non e' leggibile.
// Se il kernel non supporta PSI (CONFIG_PSI=n), restituisce errore.
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

// parsePSI analizza il contenuto di un file io.pressure.
// Formato atteso:
//
//	some avg10=25.00 avg60=18.50 avg300=12.30 total=1234567
//	full avg10=10.00 avg60=8.20 avg300=5.10 total=567890
func parsePSI(content string) (PSIStats, error) {
	var stats PSIStats

	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) < 2 {
		return stats, fmt.Errorf("invalid PSI format: expected 2 lines, got %d", len(lines))
	}

	some, err := parsePSILine(lines[0])
	if err != nil {
		return stats, fmt.Errorf("failed to parse 'some' PSI line: %w", err)
	}
	stats.SomeAvg10 = some.avg10
	stats.SomeAvg60 = some.avg60
	stats.SomeAvg300 = some.avg300
	stats.SomeTotal = some.total

	full, err := parsePSILine(lines[1])
	if err != nil {
		return stats, fmt.Errorf("failed to parse 'full' PSI line: %w", err)
	}
	stats.FullAvg10 = full.avg10
	stats.FullAvg60 = full.avg60
	stats.FullAvg300 = full.avg300
	stats.FullTotal = full.total

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
