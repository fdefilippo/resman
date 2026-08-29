/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 */

package config

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestClassifyReloadErrorDistinguishesExpectedAndGenuineFailures(t *testing.T) {
	genuineErr := errors.New("apply failed")
	tests := []struct {
		name       string
		err        error
		wantFields []string
		wantOnly   bool
	}{
		{name: "nil"},
		{
			name:       "one restart-required field",
			err:        &RestartRequiredError{Fields: []string{"SERVER_ROLE"}},
			wantFields: []string{"SERVER_ROLE"},
			wantOnly:   true,
		},
		{
			name: "wrapped deduplicated sorted restart-required fields",
			err: fmt.Errorf("reload: %w", errors.Join(
				&RestartRequiredError{Fields: []string{"USE_SYSLOG", "SERVER_ROLE"}},
				&RestartRequiredError{Fields: []string{"SERVER_ROLE"}},
			)),
			wantFields: []string{"SERVER_ROLE", "USE_SYSLOG"},
			wantOnly:   true,
		},
		{
			name: "genuine failure",
			err:  genuineErr,
		},
		{
			name: "mixed restart-required and genuine failure",
			err: errors.Join(
				&RestartRequiredError{Fields: []string{"SERVER_ROLE"}},
				genuineErr,
			),
			wantFields: []string{"SERVER_ROLE"},
		},
		{
			name: "empty restart-required error is malformed",
			err:  &RestartRequiredError{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyReloadError(tt.err)
			if !slices.Equal(got.RestartRequiredFields, tt.wantFields) {
				t.Fatalf("RestartRequiredFields = %v, want %v", got.RestartRequiredFields, tt.wantFields)
			}
			if got.OnlyRestartRequired != tt.wantOnly {
				t.Fatalf("OnlyRestartRequired = %t, want %t", got.OnlyRestartRequired, tt.wantOnly)
			}
		})
	}
}
