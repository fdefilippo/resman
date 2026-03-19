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

// ParseTimeRange converte vari formati temporali in time.Time
// Supporta: ISO 8601, relative (now-24h), predefined (today, yesterday, etc.)
func ParseTimeRange(input string, defaultEnd time.Time) (time.Time, time.Time, error) {
    if input == "" {
        // Default: ultime 24 ore
        return defaultEnd.Add(-24 * time.Hour), defaultEnd, nil
    }

    // Controllo formati predefiniti
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
        // Lunedì della settimana corrente
        daysSinceMonday := int(defaultEnd.Weekday())
        if daysSinceMonday == 0 {
            daysSinceMonday = 7 // Domenica -> 7 giorni fa
        }
        start := time.Date(defaultEnd.Year(), defaultEnd.Month(), defaultEnd.Day()-daysSinceMonday+1, 0, 0, 0, 0, defaultEnd.Location())
        return start, defaultEnd, nil
    case "this_month":
        // Primo giorno del mese corrente
        start := time.Date(defaultEnd.Year(), defaultEnd.Month(), 1, 0, 0, 0, 0, defaultEnd.Location())
        return start, defaultEnd, nil
    }

    // Controllo formato relative (now-24h, now-7d, etc.)
    relativeMatch, _ := regexp.MatchString(`^now-\d+[hd]$`, input)
    if relativeMatch {
        duration, err := ParseDuration(strings.TrimPrefix(input, "now-"))
        if err != nil {
            return time.Time{}, time.Time{}, fmt.Errorf("invalid relative time format: %s", input)
        }
        return defaultEnd.Add(-duration), defaultEnd, nil
    }

    // Controllo formato ISO 8601
    t, err := time.Parse(time.RFC3339, input)
    if err == nil {
        return t, defaultEnd, nil
    }

    // Controllo formato ISO 8601 con timezone
    t, err = time.Parse("2006-01-02T15:04:05Z07:00", input)
    if err == nil {
        return t, defaultEnd, nil
    }

    // Controllo formato date-only (YYYY-MM-DD)
    t, err = time.Parse("2006-01-02", input)
    if err == nil {
        // Usa la timezone di defaultEnd
        start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, defaultEnd.Location())
        end := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, defaultEnd.Location())
        return start, end, nil
    }

    return time.Time{}, time.Time{}, fmt.Errorf("unrecognized time format: %s (use ISO 8601, 'today', 'yesterday', 'last_24_hours', etc.)", input)
}

// ParseDuration converte stringhe come "24h", "7d", "30d" in time.Duration
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

// FormatISO8601 formatta un time.Time in ISO 8601
func FormatISO8601(t time.Time) string {
    return t.Format(time.RFC3339)
}
