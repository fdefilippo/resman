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
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// PublicFieldContract describes one public configuration key from the runtime
// default and lifecycle sources of truth.
type PublicFieldContract struct {
	Key                    string
	Default                string
	Lifecycle              FieldLifecycle
	EmptyOrDisabledMeaning string
}

var specialFieldMeanings = map[string]string{
	"AUTODETECT_PATTERNS":    "false disables workload-pattern classification and quota selection.",
	"BLACKOUT":               "Empty means no blackout; enforcement is always permitted by schedule.",
	"CPU_THRESHOLD_DURATION": "0 makes CPU threshold activation immediate after a valid sample.",
	"ENABLE_PROMETHEUS":      "false creates no Prometheus listener.",
	"IO_BOOST_DURATION":      "0 is rejected while I/O remediation is enabled.",
	"IO_DEVICE_FILTER":       "all selects every eligible whole block device.",
	"IO_LIMIT_ENABLED":       "false disables I/O enforcement while observation remains available.",
	"IO_READ_BPS":            "max disables the read-bandwidth decision and limit dimension.",
	"IO_READ_IOPS":           "0 disables the read-IOPS decision and limit dimension.",
	"IO_REMEDIATION_ENABLED": "false disables starvation remediation.",
	"IO_THRESHOLD_DURATION":  "0 makes I/O threshold activation immediate.",
	"IO_USER_EXCLUDE_LIST":   "Empty excludes nobody from I/O eligibility.",
	"IO_USER_INCLUDE_LIST":   "Empty includes every non-excluded user for I/O eligibility.",
	"IO_WRITE_BPS":           "max disables the write-bandwidth decision and limit dimension.",
	"IO_WRITE_IOPS":          "0 disables the write-IOPS decision and limit dimension.",
	"LIMIT_HOOK_ENABLED":     "false disables script and URL hook delivery.",
	"MCP_ALLOW_WRITE_OPS":    "false omits manual limit tools and rejects configuration writes.",
	"MCP_AUTH_TOKEN":         "Empty is valid only for stdio; HTTP transport requires a token.",
	"MCP_ENABLED":            "false creates no MCP server.",
	"MCP_TLS_CA_FILE":        "Empty disables client-certificate authentication; the bearer token is still required over HTTP.",
	"MCP_TRANSPORT":          "stdio is local and creates no network listener.",
	"METRICS_DB_ENABLED":     "false disables metrics persistence and database-backed MCP queries.",
	"PROCESS_EXCLUDE_LIST":   "Empty excludes no process from enforcement.",
	"PROMETHEUS_AUTH_TYPE":   "none disables Prometheus authentication.",
	"PROMETHEUS_TLS_CA_FILE": "Empty disables client-certificate authentication.",
	"PROMETHEUS_TLS_ENABLED": "false serves plain HTTP when the exporter is enabled; keep the default loopback bind unless transport security is configured.",
	"PSI_EVENT_DRIVEN":       "false uses the polling control loop instead of PSI-triggered cycles.",
	"RAM_HIGH_RATIO":         "0 disables memory.high while memory.max remains enforced.",
	"RAM_LIMIT_ENABLED":      "false disables RAM enforcement while observation remains available.",
	"RAM_USER_EXCLUDE_LIST":  "Empty excludes nobody from RAM eligibility.",
	"RAM_USER_INCLUDE_LIST":  "Empty includes every non-excluded user for RAM eligibility.",
	"SERVER_ROLE":            "Empty omits an operator-defined role value.",
	"USER_EXCLUDE_LIST":      "Empty excludes nobody from CPU eligibility.",
	"USER_INCLUDE_LIST":      "Empty makes no user eligible for CPU limiting; observation remains active. Use .* for every non-excluded user.",
}

// PublicFieldContracts returns the complete public key inventory in key order.
func PublicFieldContracts() []PublicFieldContract {
	defaults := DefaultConfig()
	typeOfConfig := reflect.TypeOf(defaults).Elem()
	valueOfConfig := reflect.ValueOf(defaults).Elem()
	contracts := make([]PublicFieldContract, 0, len(configFieldLifecycles))

	for index := 0; index < typeOfConfig.NumField(); index++ {
		field := typeOfConfig.Field(index)
		key := field.Tag.Get("config")
		if key == "" || key == "-" {
			continue
		}
		lifecycle, ok := LifecycleForField(key)
		if !ok {
			panic(fmt.Sprintf("configuration key %s has no lifecycle", key))
		}
		defaultValue := formatPublicDefault(valueOfConfig.Field(index))
		if key == "SYSTEM_UID_MAX" {
			defaultValue = "host /proc/sys/kernel/pid_max (fallback 60000)"
		}
		meaning := specialFieldMeanings[key]
		if meaning == "" {
			meaning = "—"
		}
		contracts = append(contracts, PublicFieldContract{
			Key:                    key,
			Default:                defaultValue,
			Lifecycle:              lifecycle,
			EmptyOrDisabledMeaning: meaning,
		})
	}
	sort.Slice(contracts, func(i, j int) bool { return contracts[i].Key < contracts[j].Key })
	return contracts
}

// RenderPublicConfigReference renders the generated configuration contract.
func RenderPublicConfigReference() string {
	var output strings.Builder
	output.WriteString("# ResMan configuration reference\n\n")
	output.WriteString("This file is generated from `config.Config`, `DefaultConfig`, and the authoritative lifecycle table. ")
	output.WriteString("Regenerate it with `go run ./scripts/generate-config-reference`; do not edit the table by hand.\n\n")
	output.WriteString("`dynamic` keys are applied by hot reload. `restart-required` keys keep their effective value and report a rejected reload until the daemon restarts. ")
	output.WriteString("An em dash means the key has no special empty or disabled contract beyond its literal value. ")
	output.WriteString("The copyable, commented configuration is [`config/resman.conf.example`](../config/resman.conf.example).\n\n")
	output.WriteString("| Key | Runtime default | Lifecycle | Empty, disabled, or special value |\n")
	output.WriteString("|---|---|---|---|\n")
	for _, contract := range PublicFieldContracts() {
		fmt.Fprintf(&output, "| `%s` | `%s` | `%s` | %s |\n",
			contract.Key,
			escapeMarkdownTable(contract.Default),
			contract.Lifecycle,
			escapeMarkdownTable(contract.EmptyOrDisabledMeaning),
		)
	}
	return output.String()
}

func formatPublicDefault(value reflect.Value) string {
	switch value.Kind() {
	case reflect.Bool:
		return strconv.FormatBool(value.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(value.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(value.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(value.Float(), 'f', -1, 64)
	case reflect.String:
		if value.String() == "" {
			return "(empty)"
		}
		return value.String()
	case reflect.Slice:
		if value.Len() == 0 {
			return "(empty)"
		}
		items := make([]string, value.Len())
		for index := range items {
			items[index] = fmt.Sprint(value.Index(index).Interface())
		}
		return strings.Join(items, ",")
	default:
		panic(fmt.Sprintf("unsupported public configuration default type %s", value.Type()))
	}
}

func escapeMarkdownTable(value string) string {
	return strings.ReplaceAll(value, "|", "\\|")
}
