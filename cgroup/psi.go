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

import "github.com/fdefilippo/resman/internal/pressure"

// PSIStats contains pressure observations with explicit optional full coverage.
type PSIStats = pressure.Stats

// parsePSI uses the shared validated kernel pressure parser.
func parsePSI(content string) (PSIStats, error) {
	return pressure.Parse(content)
}
