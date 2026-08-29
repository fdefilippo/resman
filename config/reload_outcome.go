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

package config

import (
	"sort"
	"strings"
)

// RestartRequiredError reports requested fields that were not applied. It
// deliberately contains key names only, never configuration values.
type RestartRequiredError struct {
	Fields []string
}

func (e *RestartRequiredError) Error() string {
	return "restart-required configuration changes rejected: " + strings.Join(e.Fields, ", ")
}

// ReloadErrorClassification separates expected restart-required rejections
// from genuine reload failures without losing either side of a joined error.
type ReloadErrorClassification struct {
	RestartRequiredFields []string
	OnlyRestartRequired   bool
}

// ClassifyReloadError walks every wrapped and joined error branch. The
// presence of RestartRequiredError alone is not enough to downgrade an error:
// every leaf must be a well-formed restart-required rejection.
func ClassifyReloadError(err error) ReloadErrorClassification {
	fields := make(map[string]struct{})
	hasRestartRequired, hasGenuineFailure := classifyReloadError(err, fields)

	result := ReloadErrorClassification{
		RestartRequiredFields: make([]string, 0, len(fields)),
		OnlyRestartRequired:   hasRestartRequired && !hasGenuineFailure,
	}
	for field := range fields {
		result.RestartRequiredFields = append(result.RestartRequiredFields, field)
	}
	sort.Strings(result.RestartRequiredFields)
	return result
}

func classifyReloadError(err error, fields map[string]struct{}) (bool, bool) {
	if err == nil {
		return false, false
	}
	if restartErr, ok := err.(*RestartRequiredError); ok {
		hasField := false
		for _, field := range restartErr.Fields {
			field = strings.TrimSpace(field)
			if field == "" {
				continue
			}
			fields[field] = struct{}{}
			hasField = true
		}
		return hasField, !hasField
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var hasRestartRequired, hasGenuineFailure bool
		for _, child := range joined.Unwrap() {
			childRestartRequired, childGenuineFailure := classifyReloadError(child, fields)
			hasRestartRequired = hasRestartRequired || childRestartRequired
			hasGenuineFailure = hasGenuineFailure || childGenuineFailure
		}
		return hasRestartRequired, hasGenuineFailure
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return classifyReloadError(wrapped.Unwrap(), fields)
	}
	return false, true
}
