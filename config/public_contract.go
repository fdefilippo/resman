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

	"github.com/fdefilippo/resman/internal/cpupoints"
)

// PublicFieldContract describes one public configuration key from the runtime
// default and lifecycle sources of truth.
type PublicFieldContract struct {
	Key                    string                `json:"key"`
	Kind                   PublicFieldKind       `json:"kind"`
	Default                string                `json:"default"`
	Lifecycle              FieldLifecycle        `json:"lifecycle"`
	Sensitive              bool                  `json:"sensitive"`
	Editable               bool                  `json:"editable"`
	Constraint             PublicFieldConstraint `json:"constraint"`
	EmptyOrDisabledMeaning string                `json:"empty_or_disabled_meaning"`
	Remedy                 string                `json:"remedy"`
}

// PublicFieldKind is the closed wire vocabulary for editor value types.
type PublicFieldKind string

const (
	PublicFieldBoolean    PublicFieldKind = "boolean"
	PublicFieldInteger    PublicFieldKind = "integer"
	PublicFieldNumber     PublicFieldKind = "number"
	PublicFieldString     PublicFieldKind = "string"
	PublicFieldStringList PublicFieldKind = "string_list"
)

// PublicFieldConstraint describes the scalar parser constraint exposed to
// independent editors. Cross-field validation remains authoritative in
// ValidateCandidate and is always run before persistence.
type PublicFieldConstraint struct {
	Minimum *float64 `json:"minimum,omitempty"`
	Maximum *float64 `json:"maximum,omitempty"`
	Enum    []string `json:"enum,omitempty"`
	Format  string   `json:"format,omitempty"`
}

// ConfigSource is one member of the authoritative source-precedence order.
type ConfigSource string

const (
	ConfigSourceDefault     ConfigSource = "default"
	ConfigSourceFile        ConfigSource = "file"
	ConfigSourceEnvironment ConfigSource = "environment"
)

const environmentShadowingRemedy = "Inspect the running service with `systemctl show resman --property=Environment --property=EnvironmentFiles --property=DropInPaths` and `systemctl cat resman`. Modify the authoritative drop-in with `systemctl edit resman`, or modify the referenced environment file or configuration-management source. Reload the systemd unit definition where required, then restart `resman`; `systemctl reload resman` alone cannot change the environment of the running process."

// PublicConfigSourcePrecedence returns the single ordered source contract.
func PublicConfigSourcePrecedence() []ConfigSource {
	return []ConfigSource{ConfigSourceDefault, ConfigSourceFile, ConfigSourceEnvironment}
}

// EnvironmentShadowingRemedy returns the operator procedure shared by the
// generated reference and MCP editor snapshots.
func EnvironmentShadowingRemedy() string { return environmentShadowingRemedy }

// ValidatePublicFieldValue parses one serialized editor value with the same
// handler used by file and environment loading, then runs complete daemon
// validation. Editors must treat the structured constraint as presentation
// metadata and this function as the acceptance authority.
func ValidatePublicFieldValue(key, value string) error {
	contract, ok := publicFieldContractByKey(key)
	if !ok {
		return fmt.Errorf("unknown public configuration key %q", key)
	}
	if !contract.Editable {
		return fmt.Errorf("configuration key %s is not editable through the public editor contract", key)
	}
	candidate := DefaultConfig()
	if err := setConfigField(candidate, key, value); err != nil {
		return err
	}
	return validateConfig(candidate)
}

func publicFieldContractByKey(key string) (PublicFieldContract, bool) {
	for _, contract := range PublicFieldContracts() {
		if contract.Key == key {
			return contract, true
		}
	}
	return PublicFieldContract{}, false
}

func validateStructuredFieldConstraint(cfg *Config, key string) error {
	contract, ok := publicFieldContractByKey(key)
	if !ok {
		return fmt.Errorf("configuration key %s has no public contract", key)
	}
	typeOfConfig := reflect.TypeOf(cfg).Elem()
	valueOfConfig := reflect.ValueOf(cfg).Elem()
	var value reflect.Value
	for index := 0; index < typeOfConfig.NumField(); index++ {
		if typeOfConfig.Field(index).Tag.Get("config") == key {
			value = valueOfConfig.Field(index)
			break
		}
	}
	if !value.IsValid() {
		return fmt.Errorf("configuration key %s has no backing field", key)
	}
	constraint := contract.Constraint
	if len(constraint.Enum) > 0 {
		serialized := formatPublicDefault(value)
		for _, allowed := range constraint.Enum {
			if serialized == allowed {
				return nil
			}
		}
		return fmt.Errorf("%s must be one of: %s", key, strings.Join(constraint.Enum, ", "))
	}
	var numeric float64
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		numeric = float64(value.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		numeric = float64(value.Uint())
	case reflect.Float32, reflect.Float64:
		numeric = value.Float()
	default:
		return nil
	}
	if constraint.Minimum != nil && numeric < *constraint.Minimum {
		return fmt.Errorf("%s must be at least %g", key, *constraint.Minimum)
	}
	if constraint.Maximum != nil && numeric > *constraint.Maximum {
		return fmt.Errorf("%s must be at most %g", key, *constraint.Maximum)
	}
	return nil
}

var sensitivePublicFields = map[string]bool{
	"LIMIT_HOOK_URL":        true,
	"MCP_AUTH_TOKEN":        true,
	"MCP_EDITOR_AUTH_TOKEN": true,
}

var nonEditablePublicFields = map[string]string{
	"CPU_POINTS_FILE": "Change the policy-map path in the configuration file and restart resman; MCP map updates target the currently authoritative path only.",
}

func number(value float64) *float64 { return &value }

var publicFieldConstraints = map[string]PublicFieldConstraint{
	"CGROUP_BASE":                  {Format: "relative-cgroup-path"},
	"POLLING_INTERVAL":             {Minimum: number(5)},
	"METRICS_CACHE_TTL":            {Minimum: number(1)},
	"METRICS_REFRESH_INTERVAL":     {Minimum: number(5)},
	"PROCESS_MIN_AGE_SECONDS":      {Minimum: number(0)},
	"CPU_THRESHOLD":                {Minimum: number(1), Maximum: number(100)},
	"CPU_RELEASE_THRESHOLD":        {Minimum: number(1), Maximum: number(100)},
	"CPU_THRESHOLD_DURATION":       {Minimum: number(0)},
	"CPU_RESERVE_POINTS":           {Minimum: number(0), Maximum: number(990)},
	"CPU_ROOT_POINTS":              {Minimum: number(1), Maximum: number(1000)},
	"CPU_BEST_EFFORT_POINTS":       {Minimum: number(1), Maximum: number(1000)},
	"CPU_POINTS_FILE":              {Format: "absolute-clean-path"},
	"LIMIT_HOOK_SCRIPT":            {Format: "safe-executable-path"},
	"LIMIT_HOOK_URL":               {Format: "http-or-https-url"},
	"LIMIT_HOOK_TIMEOUT":           {Minimum: number(1)},
	"LIMIT_HOOK_MAX_CONCURRENCY":   {Minimum: number(1)},
	"LIMIT_HOOK_QUEUE_CAPACITY":    {Minimum: number(1)},
	"PROMETHEUS_METRICS_BIND_PORT": {Minimum: number(1), Maximum: number(65535)},
	"PROMETHEUS_TLS_MIN_VERSION":   {Enum: []string{"1.0", "1.1", "1.2", "1.3"}},
	"PROMETHEUS_AUTH_TYPE":         {Enum: []string{"none", "basic", "jwt", "both"}},
	"LOG_LEVEL":                    {Enum: []string{"DEBUG", "INFO", "WARN", "ERROR"}},
	"LOG_MAX_SIZE":                 {Minimum: number(1)},
	"SYSTEM_UID_MIN":               {Minimum: number(0)},
	"USER_INCLUDE_LIST":            {Format: "comma-separated-regex-list"},
	"USER_EXCLUDE_LIST":            {Format: "comma-separated-regex-list"},
	"PROCESS_EXCLUDE_LIST":         {Format: "comma-separated-regex-list"},
	"BLACKOUT":                     {Format: "blackout-timeframes"},
	"MCP_TRANSPORT":                {Enum: []string{"stdio", "http"}},
	"MCP_HTTP_PORT":                {Minimum: number(1), Maximum: number(65535)},
	"MCP_TLS_MIN_VERSION":          {Enum: []string{"1.0", "1.1", "1.2", "1.3"}},
	"MCP_LOG_LEVEL":                {Enum: []string{"DEBUG", "INFO", "WARN", "ERROR"}},
	"MCP_SHUTDOWN_TIMEOUT":         {Minimum: number(1)},
	"METRICS_DB_RETENTION_DAYS":    {Minimum: number(1)},
	"METRICS_DB_WRITE_INTERVAL":    {Minimum: number(5)},
	"USERNAME_CACHE_TTL":           {Minimum: number(1)},
	"CGROUP_OPERATION_TIMEOUT":     {Minimum: number(1)},
	"DAEMON_SHUTDOWN_TIMEOUT":      {Minimum: number(1)},
	"RAM_THRESHOLD":                {Minimum: number(1), Maximum: number(100)},
	"RAM_RELEASE_THRESHOLD":        {Minimum: number(1), Maximum: number(100)},
	"RAM_QUOTA_PER_USER":           {Format: "byte-quota"},
	"RAM_HIGH_RATIO":               {Minimum: number(0), Maximum: number(1)},
	"RAM_USER_INCLUDE_LIST":        {Format: "comma-separated-regex-list"},
	"RAM_USER_EXCLUDE_LIST":        {Format: "comma-separated-regex-list"},
	"IO_THRESHOLD":                 {Minimum: number(1), Maximum: number(100)},
	"IO_RELEASE_THRESHOLD":         {Minimum: number(1), Maximum: number(100)},
	"IO_READ_BPS":                  {Format: "byte-quota-or-max"},
	"IO_WRITE_BPS":                 {Format: "byte-quota-or-max"},
	"IO_READ_IOPS":                 {Minimum: number(0)},
	"IO_WRITE_IOPS":                {Minimum: number(0)},
	"IO_DEVICE_FILTER":             {Format: "all-or-device-number"},
	"IO_THRESHOLD_DURATION":        {Minimum: number(0)},
	"IO_USER_INCLUDE_LIST":         {Format: "comma-separated-regex-list"},
	"IO_USER_EXCLUDE_LIST":         {Format: "comma-separated-regex-list"},
	"IO_STARVATION_THRESHOLD":      {Minimum: number(1)},
	"IO_STARVATION_CHECK_INTERVAL": {Minimum: number(1)},
	"IO_BOOST_MULTIPLIER":          {Minimum: number(0)},
	"IO_BOOST_DURATION":            {Minimum: number(1)},
	"IO_BOOST_MAX_PER_HOUR":        {Minimum: number(0)},
	"IO_PSI_THRESHOLD":             {Minimum: number(0), Maximum: number(100)},
	"PATTERN_HISTORY_HOURS":        {Minimum: number(1)},
	"PATTERN_MIN_SAMPLES":          {Minimum: number(1)},
	"PATTERN_CONFIDENCE_THRESHOLD": {Minimum: number(0), Maximum: number(1)},
	"BATCH_NIGHT_RAM_QUOTA":        {Format: "byte-quota"},
	"INTERACTIVE_RAM_QUOTA":        {Format: "byte-quota"},
	"PSI_CPU_STALL_THRESHOLD":      {Minimum: number(0)},
	"PSI_IO_STALL_THRESHOLD":       {Minimum: number(0)},
	"PSI_WINDOW_US":                {Minimum: number(0)},
	"PSI_FALLBACK_INTERVAL":        {Minimum: number(0)},
}

var specialFieldMeanings = map[string]string{
	"AUTODETECT_PATTERNS":        "false disables workload-pattern classification and RAM policy selection.",
	"BLACKOUT":                   "Empty means no blackout; enforcement is always permitted by schedule.",
	"CPU_THRESHOLD_DURATION":     "0 makes CPU threshold activation immediate after a valid sample.",
	"CPU_RESERVE_POINTS":         "Nominal headroom outside user.slice, including system.slice, not an unbounded root shell or physical isolation. 0 still programs a finite parent quota.",
	"CPU_ROOT_POINTS":            "Lendable minimum scheduling entitlement for an active user-0.slice; it is not a CPU ceiling.",
	"CPU_BEST_EFFORT_POINTS":     "One aggregate entitlement shared by active non-root user slices without an eligible mapped guarantee.",
	"CPU_POINTS_FILE":            "Absolute restart-required path to the strict direct username guarantee map.",
	"ENABLE_PROMETHEUS":          "false creates no Prometheus listener.",
	"IO_BOOST_DURATION":          "0 is rejected while I/O remediation is enabled.",
	"IO_DEVICE_FILTER":           "all selects every eligible whole block device.",
	"IO_LIMIT_ENABLED":           "false disables I/O enforcement while observation remains available.",
	"IO_READ_BPS":                "max disables the read-bandwidth decision and limit dimension.",
	"IO_READ_IOPS":               "0 disables the read-IOPS decision and limit dimension.",
	"IO_REMEDIATION_ENABLED":     "false disables starvation remediation.",
	"IO_THRESHOLD_DURATION":      "0 makes I/O threshold activation immediate.",
	"IO_USER_EXCLUDE_LIST":       "Empty excludes nobody from I/O eligibility.",
	"IO_USER_INCLUDE_LIST":       "Empty includes every non-excluded user for I/O eligibility.",
	"IO_WRITE_BPS":               "max disables the write-bandwidth decision and limit dimension.",
	"IO_WRITE_IOPS":              "0 disables the write-IOPS decision and limit dimension.",
	"LIMIT_HOOK_ENABLED":         "false disables script and URL hook delivery.",
	"LIMIT_HOOK_SCRIPT":          "Empty disables script delivery and requires script user and group to be empty.",
	"LIMIT_HOOK_SCRIPT_USER":     "Required non-root NSS username whenever LIMIT_HOOK_SCRIPT is set.",
	"LIMIT_HOOK_SCRIPT_GROUP":    "Required non-root NSS group whenever LIMIT_HOOK_SCRIPT is set.",
	"LIMIT_HOOK_MAX_CONCURRENCY": "Fixed number of delivery workers; changing it requires restart.",
	"LIMIT_HOOK_QUEUE_CAPACITY":  "Fixed pending-delivery capacity; saturation is reported and never blocks enforcement.",
	"LIMIT_HOOK_TIMEOUT":         "Full timeout in seconds applied independently to every script or HTTP delivery.",
	"LIMIT_HOOK_URL":             "Empty disables HTTP delivery; configured requests use a dedicated no-retry client.",
	"MCP_ALLOW_WRITE_OPS":        "false omits manual limit tools and rejects configuration writes.",
	"MCP_AUTH_TOKEN":             "Empty is valid only for stdio; HTTP transport requires a token.",
	"MCP_EDITOR_AUTH_TOKEN":      "Dedicated least-privilege HTTP credential for independent configuration editors; required when write operations are enabled.",
	"MCP_ENABLED":                "false creates no MCP server.",
	"MCP_TLS_CA_FILE":            "Empty disables client-certificate authentication; the bearer token is still required over HTTP.",
	"MCP_TRANSPORT":              "stdio is local and creates no network listener.",
	"METRICS_CACHE_TTL":          "Controls observation value reuse only; control-cycle host CPU decisions use an independent uncached /proc/stat stream.",
	"METRICS_DB_ENABLED":         "false disables metrics persistence and database-backed MCP queries.",
	"PROCESS_EXCLUDE_LIST":       "Empty excludes no process from enforcement.",
	"PROMETHEUS_AUTH_TYPE":       "none disables Prometheus authentication.",
	"PROMETHEUS_TLS_CA_FILE":     "Empty disables client-certificate authentication.",
	"PROMETHEUS_TLS_ENABLED":     "false serves plain HTTP when the exporter is enabled; keep the default loopback bind unless transport security is configured.",
	"PSI_EVENT_DRIVEN":           "false uses the polling control loop instead of PSI-triggered cycles.",
	"RAM_HIGH_RATIO":             "0 disables memory.high while memory.max remains enforced.",
	"RAM_LIMIT_ENABLED":          "false disables RAM enforcement while observation remains available.",
	"RAM_USER_EXCLUDE_LIST":      "Empty excludes nobody from RAM eligibility.",
	"RAM_USER_INCLUDE_LIST":      "Empty includes every non-excluded user for RAM eligibility.",
	"SERVER_ROLE":                "Empty omits an operator-defined role value.",
	"USER_EXCLUDE_LIST":          "Empty excludes nobody from CPU eligibility. Excluded native user slices remain inside the parent quota and share aggregate best effort.",
	"USER_INCLUDE_LIST":          "Empty makes no user eligible for CPU Points enforcement; observation remains active. Use .* for every non-excluded user.",
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
		remedy := "Correct the authored value and submit the complete candidate for validation."
		if lifecycle == LifecycleRestartRequired {
			remedy = "Persist the value and restart resman for it to become effective."
		}
		if specific := nonEditablePublicFields[key]; specific != "" {
			remedy = specific
		}
		contracts = append(contracts, PublicFieldContract{
			Key:                    key,
			Kind:                   publicFieldKind(field.Type),
			Default:                defaultValue,
			Lifecycle:              lifecycle,
			Sensitive:              sensitivePublicFields[key],
			Editable:               nonEditablePublicFields[key] == "",
			Constraint:             publicFieldConstraints[key],
			EmptyOrDisabledMeaning: meaning,
			Remedy:                 remedy,
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
	output.WriteString("## Source precedence\n\n")
	output.WriteString("The authoritative order is `default < file < environment`: runtime defaults are loaded first, the authored configuration file overrides them, and environment variables override both. Editing a file value while an environment override exists does not change the effective value.\n\n")
	output.WriteString("Environment-shadowing remedy: " + environmentShadowingRemedy + "\n\n")
	output.WriteString("| Key | Kind | Runtime default | Lifecycle | Sensitive | Editable | Constraint | Empty, disabled, or special value | Remedy |\n")
	output.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, contract := range PublicFieldContracts() {
		defaultValue := contract.Default
		if contract.Sensitive {
			defaultValue = "(redacted)"
		}
		fmt.Fprintf(&output, "| `%s` | `%s` | `%s` | `%s` | `%t` | `%t` | %s | %s | %s |\n",
			contract.Key,
			contract.Kind,
			escapeMarkdownTable(defaultValue),
			contract.Lifecycle,
			contract.Sensitive,
			contract.Editable,
			escapeMarkdownTable(formatPublicConstraint(contract.Constraint)),
			escapeMarkdownTable(contract.EmptyOrDisabledMeaning),
			escapeMarkdownTable(contract.Remedy),
		)
	}
	output.WriteString("\n## Native CPU Points planning\n\n")
	defaults := DefaultConfig()
	fmt.Fprintf(&output, "Defaults: CPU_RESERVE_POINTS=%d, CPU_ROOT_POINTS=%d, CPU_BEST_EFFORT_POINTS=%d; at most %d mapped points. Root is a lendable minimum, not a ceiling; ResMan never writes CPUQuota on user-0.slice.\n\n",
		defaults.CPUReservePoints, defaults.CPURootPoints, defaults.CPUBestEffortPoints,
		1000-defaults.CPUReservePoints-defaults.CPURootPoints-defaults.CPUBestEffortPoints)
	output.WriteString("Admission requires `sum(mapped guarantees) + CPU_ROOT_POINTS + CPU_BEST_EFFORT_POINTS <= 1000 - CPU_RESERVE_POINTS`. All mapped entries count, including inactive or currently ineligible accounts. UID 0 is forbidden in the map.\n\n")
	fmt.Fprintf(&output, "Let `M` be the largest configured entitlement among root, aggregate best effort and all mapped guarantees. The exact scale is `floor(%d / M)`. The active best-effort slice count must not exceed `CPU_BEST_EFFORT_POINTS * floor(%d / M)`. Best-effort weights differ by at most one and their sum is exact. With an empty default map the bound is 10000 slices; with a 700-point guarantee and best effort 100 it is 1400. An impossible plan reports `best_effort_cardinality` before mutation, including active count, aggregate weight and maximum scale.\n\n", cpupoints.MaximumKernelCPUWeight, cpupoints.MaximumKernelCPUWeight)
	output.WriteString("Under systemd_native, active class changes reconcile weights in place; the non-systemd migration backend still rejects active cross-class moves. Map contents and the three point settings reload as one verified epoch; CPU_POINTS_FILE requires restart. See [UPGRADING.md](UPGRADING.md) for the 701–800 default-budget break and the 750-point rebalance example, and [CPU-POINTS-OBSERVABILITY.md](CPU-POINTS-OBSERVABILITY.md) for synchronized measurement.\n")
	return output.String()
}

func publicFieldKind(fieldType reflect.Type) PublicFieldKind {
	switch fieldType.Kind() {
	case reflect.Bool:
		return PublicFieldBoolean
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return PublicFieldInteger
	case reflect.Float32, reflect.Float64:
		return PublicFieldNumber
	case reflect.String:
		return PublicFieldString
	case reflect.Slice:
		if fieldType.Elem().Kind() == reflect.String {
			return PublicFieldStringList
		}
	}
	panic(fmt.Sprintf("unsupported public configuration field type %s", fieldType))
}

func formatPublicConstraint(constraint PublicFieldConstraint) string {
	parts := make([]string, 0, 3)
	if constraint.Minimum != nil {
		parts = append(parts, fmt.Sprintf("min %g", *constraint.Minimum))
	}
	if constraint.Maximum != nil {
		parts = append(parts, fmt.Sprintf("max %g", *constraint.Maximum))
	}
	if len(constraint.Enum) > 0 {
		parts = append(parts, "one of "+strings.Join(constraint.Enum, ", "))
	}
	if constraint.Format != "" {
		parts = append(parts, "format "+constraint.Format)
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "; ")
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
