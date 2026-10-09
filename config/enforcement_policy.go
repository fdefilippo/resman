/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 */

package config

import "fmt"

// EnforcementPolicy is the bounded operator declaration of how this host
// treats enforcement when a mandatory capability is unavailable. It governs
// the decision only at startup and on explicit reload, because the key is
// restart-required: a host never changes enforcement spontaneously.
type EnforcementPolicy string

const (
	// EnforcementPolicySystemdNative is the default. A mandatory capability
	// the host cannot provide remains a permanent startup error, so an
	// operator who configured enforcement is never left silently unprotected.
	EnforcementPolicySystemdNative EnforcementPolicy = "systemd_native"
	// EnforcementPolicyObservationOnly declares that this host observes and
	// never enforces. No capability probe runs, no transient unit is created
	// and no property is ever written.
	EnforcementPolicyObservationOnly EnforcementPolicy = "observation_only"
	// EnforcementPolicyAuto declares that this host enforces when it can and
	// observes when the kernel structurally refuses a mandatory capability. A
	// transient failure still refuses to start, so a bus fault or a timeout
	// can never remove enforcement.
	EnforcementPolicyAuto EnforcementPolicy = "auto"
)

// EnforcementPolicyValues lists the accepted values in the order published to
// operators and to independent editors.
func EnforcementPolicyValues() []string {
	return []string{
		string(EnforcementPolicySystemdNative),
		string(EnforcementPolicyObservationOnly),
		string(EnforcementPolicyAuto),
	}
}

// ParseEnforcementPolicy converts one configured value into the bounded
// vocabulary. An unknown value is an error rather than a default, so a typo
// can never quietly disable enforcement.
func ParseEnforcementPolicy(value string) (EnforcementPolicy, error) {
	switch EnforcementPolicy(value) {
	case EnforcementPolicySystemdNative:
		return EnforcementPolicySystemdNative, nil
	case EnforcementPolicyObservationOnly:
		return EnforcementPolicyObservationOnly, nil
	case EnforcementPolicyAuto:
		return EnforcementPolicyAuto, nil
	default:
		return "", fmt.Errorf("ENFORCEMENT_MODE must be systemd_native, observation_only or auto, got %q", value)
	}
}

// GetEnforcementMode returns the operator declaration as a bounded value.
// Validation accepts nothing else, and an empty value keeps the default, so
// the caller never has to interpret an unknown string.
func (c *Config) GetEnforcementMode() EnforcementPolicy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.EnforcementMode == "" {
		return EnforcementPolicySystemdNative
	}
	policy, err := ParseEnforcementPolicy(c.EnforcementMode)
	if err != nil {
		return EnforcementPolicySystemdNative
	}
	return policy
}
