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
// database/time_parser.go
package database

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ParseTimeRange converts supported time expressions into time.Time values.
// It accepts ISO 8601, relative expressions such as now-24h, and predefined ranges.
func ParseTimeRange(input string, defaultEnd time.Time) (time.Time, time.Time, error) {
	if input == "" {
		// Default to the last 24 hours.
		return defaultEnd.Add(-24 * time.Hour), defaultEnd, nil
	}

	// Check predefined ranges.
	switch strings.ToLower(input) {
	case "today":
		start := time.Date(defaultEnd.Year(), defaultEnd.Month(), defaultEnd.Day(), 0, 0, 0, 0, defaultEnd.Location())
		return start, defaultEnd, nil
	case "yesterday":
		yesterday := defaultEnd.AddDate(0, 0, -1)
		start := time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 0, 0, 0, 0, yesterday.Location())
		end := time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 23, 59, 59, 0, yesterday.Location())
		return start, end, nil
	case "last_24_hours", "last24h":
		return defaultEnd.Add(-24 * time.Hour), defaultEnd, nil
	case "last_7_days", "last7d", "last_week":
		return defaultEnd.Add(-7 * 24 * time.Hour), defaultEnd, nil
	case "last_30_days", "last30d", "last_month":
		return defaultEnd.Add(-30 * 24 * time.Hour), defaultEnd, nil
	case "this_week":
		// Start at Monday of the current week.
		daysSinceMonday := int(defaultEnd.Weekday())
		if daysSinceMonday == 0 {
			daysSinceMonday = 7 // Sunday maps to seven days ago.
		}
		start := time.Date(defaultEnd.Year(), defaultEnd.Month(), defaultEnd.Day()-daysSinceMonday+1, 0, 0, 0, 0, defaultEnd.Location())
		return start, defaultEnd, nil
	case "this_month":
		// Start at the first day of the current month.
		start := time.Date(defaultEnd.Year(), defaultEnd.Month(), 1, 0, 0, 0, 0, defaultEnd.Location())
		return start, defaultEnd, nil
	}

	// Check relative formats such as now-24h and now-7d.
	relativeMatch, err := regexp.MatchString(`^now-\d+[hd]$`, input)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("regexp error: %w", err)
	}
	if relativeMatch {
		duration, err := ParseDuration(strings.TrimPrefix(input, "now-"))
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid relative time format: %s", input)
		}
		return defaultEnd.Add(-duration), defaultEnd, nil
	}

	// Check the ISO 8601 format.
	t, err := time.Parse(time.RFC3339, input)
	if err == nil {
		return t, defaultEnd, nil
	}

	// Check the ISO 8601 format with a timezone.
	t, err = time.Parse("2006-01-02T15:04:05Z07:00", input)
	if err == nil {
		return t, defaultEnd, nil
	}

	// Check the date-only format (YYYY-MM-DD).
	t, err = time.Parse("2006-01-02", input)
	if err == nil {
		// Use the defaultEnd timezone.
		start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, defaultEnd.Location())
		end := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, defaultEnd.Location())
		return start, end, nil
	}

	return time.Time{}, time.Time{}, fmt.Errorf("unrecognized time format: %s (use ISO 8601, 'today', 'yesterday', 'last_24_hours', etc.)", input)
}

// ParseDuration converts values such as "24h", "7d", and "30d" into time.Duration.
func ParseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid duration format: %s", s)
	}

	unit := s[len(s)-1:]
	valueStr := s[:len(s)-1]

	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return 0, fmt.Errorf("invalid duration value: %s", valueStr)
	}

	switch unit {
	case "h":
		return time.Duration(value) * time.Hour, nil
	case "d":
		return time.Duration(value) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("invalid duration unit: %s (use 'h' for hours or 'd' for days)", unit)
	}
}
