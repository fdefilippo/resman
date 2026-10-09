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
	"reflect"
	"strings"
	"testing"
)

func TestEnforcementModeDefaultsToTheUnchangedBehaviour(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.EnforcementMode != string(EnforcementPolicySystemdNative) {
		t.Fatalf("default ENFORCEMENT_MODE = %q, want %q", cfg.EnforcementMode, EnforcementPolicySystemdNative)
	}
	if got := cfg.GetEnforcementMode(); got != EnforcementPolicySystemdNative {
		t.Fatalf("GetEnforcementMode() = %q, want %q", got, EnforcementPolicySystemdNative)
	}
}

func TestEnforcementModeAcceptsExactlyThreeDeclarations(t *testing.T) {
	want := []string{"systemd_native", "observation_only", "auto"}
	if !reflect.DeepEqual(EnforcementPolicyValues(), want) {
		t.Fatalf("EnforcementPolicyValues() = %v, want %v", EnforcementPolicyValues(), want)
	}
	for _, value := range want {
		policy, err := ParseEnforcementPolicy(value)
		if err != nil || string(policy) != value {
			t.Fatalf("ParseEnforcementPolicy(%q) = %q, %v", value, policy, err)
		}
	}
	for _, invalid := range []string{"", "observation", "systemd-native", "SYSTEMD_NATIVE", "none", "off", "true"} {
		if _, err := ParseEnforcementPolicy(invalid); err == nil {
			t.Fatalf("ParseEnforcementPolicy(%q) was accepted", invalid)
		}
	}
}

func TestEnforcementModeRejectsAnUnknownValueInsteadOfDefaulting(t *testing.T) {
	// A typo must never quietly disable enforcement, so validation refuses the
	// configuration instead of selecting a mode for the operator.
	cfg := DefaultConfig()
	cfg.EnforcementMode = "observaton_only"
	err := validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "ENFORCEMENT_MODE") {
		t.Fatalf("validateConfig() = %v, want a rejection naming ENFORCEMENT_MODE", err)
	}
}

func TestEnforcementModeIsParsedCaseInsensitivelyAndTrimmedToTheVocabulary(t *testing.T) {
	cfg := DefaultConfig()
	setter, ok := configFieldHandlers["ENFORCEMENT_MODE"]
	if !ok {
		t.Fatal("ENFORCEMENT_MODE has no configured setter")
	}
	if err := setter(cfg, "Observation_Only"); err != nil {
		t.Fatalf("setter error = %v", err)
	}
	if cfg.GetEnforcementMode() != EnforcementPolicyObservationOnly {
		t.Fatalf("GetEnforcementMode() = %q, want %q", cfg.GetEnforcementMode(), EnforcementPolicyObservationOnly)
	}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("validateConfig() rejected a normalized value: %v", err)
	}
}

func TestEnforcementModeRequiresARestart(t *testing.T) {
	// The declared boundary is decided at startup only: a host must never gain
	// or lose enforcement spontaneously while it is running.
	lifecycle, ok := LifecycleForField("ENFORCEMENT_MODE")
	if !ok || lifecycle != LifecycleRestartRequired {
		t.Fatalf("LifecycleForField(ENFORCEMENT_MODE) = %q, %v, want restart-required", lifecycle, ok)
	}
}

func TestEnforcementModePublishesItsBoundedVocabularyToEditors(t *testing.T) {
	for _, contract := range PublicFieldContracts() {
		if contract.Key != "ENFORCEMENT_MODE" {
			continue
		}
		if !reflect.DeepEqual(contract.Constraint.Enum, EnforcementPolicyValues()) {
			t.Fatalf("published enum = %v, want %v", contract.Constraint.Enum, EnforcementPolicyValues())
		}
		if contract.Lifecycle != LifecycleRestartRequired || contract.Default != string(EnforcementPolicySystemdNative) {
			t.Fatalf("published contract = %+v", contract)
		}
		return
	}
	t.Fatal("ENFORCEMENT_MODE is absent from the public configuration contract")
}

func TestUnknownEnforcementModeNeverSilentlyEnablesObservation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EnforcementMode = "auto; observation_only"
	if got := cfg.GetEnforcementMode(); got != EnforcementPolicySystemdNative {
		t.Fatalf("GetEnforcementMode() = %q, want the enforcing default", got)
	}
}
