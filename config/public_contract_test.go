package config

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPublicConfigReferenceMatchesRuntimeContract(t *testing.T) {
	contracts := PublicFieldContracts()
	if len(contracts) != len(configFieldLifecycles) {
		t.Fatalf("public contracts = %d, lifecycle fields = %d", len(contracts), len(configFieldLifecycles))
	}
	seen := make(map[string]bool, len(contracts))
	for _, contract := range contracts {
		if seen[contract.Key] {
			t.Errorf("duplicate public contract for %s", contract.Key)
		}
		seen[contract.Key] = true
		if want, ok := LifecycleForField(contract.Key); !ok || contract.Lifecycle != want {
			t.Errorf("%s lifecycle = %q, want %q", contract.Key, contract.Lifecycle, want)
		}
		if contract.Kind == "" {
			t.Errorf("%s has no public value kind", contract.Key)
		}
		if contract.Remedy == "" {
			t.Errorf("%s has no operator remedy", contract.Key)
		}
		if contract.Sensitive && contract.Default != "(empty)" {
			t.Errorf("%s sensitive metadata retains non-empty default", contract.Key)
		}
	}

	referencePath := filepath.Join("..", "docs", "CONFIGURATION.md")
	tracked, err := os.ReadFile(referencePath)
	if err != nil {
		t.Fatalf("read %s: %v", referencePath, err)
	}
	if got, want := string(tracked), RenderPublicConfigReference(); got != want {
		t.Fatalf("%s is stale; run go run ./scripts/generate-config-reference", referencePath)
	}
}

func TestPublicSourcePrecedenceAndEnvironmentRemedyAreAuthoritative(t *testing.T) {
	want := []ConfigSource{ConfigSourceDefault, ConfigSourceFile, ConfigSourceEnvironment}
	if got := PublicConfigSourcePrecedence(); !reflect.DeepEqual(got, want) {
		t.Fatalf("source precedence = %v, want %v", got, want)
	}
	reference := RenderPublicConfigReference()
	ordered := "`default < file < environment`"
	if !strings.Contains(reference, ordered) {
		t.Fatalf("generated reference omits ordered precedence %s", ordered)
	}
	for _, required := range []string{
		"systemctl show resman --property=Environment --property=EnvironmentFiles --property=DropInPaths",
		"systemctl cat resman",
		"systemctl edit resman",
		"restart `resman`",
		"`systemctl reload resman` alone",
	} {
		if !strings.Contains(EnvironmentShadowingRemedy(), required) || !strings.Contains(reference, required) {
			t.Errorf("environment remedy omits %q", required)
		}
	}
}

func TestStructuredConstraintsUseProductionParsingAndValidation(t *testing.T) {
	tests := []struct {
		key     string
		valid   string
		invalid string
	}{
		{key: "CPU_RESERVE_POINTS", valid: "100", invalid: "991"},
		{key: "CPU_ROOT_POINTS", valid: "100", invalid: "0"},
		{key: "CPU_BEST_EFFORT_POINTS", valid: "100", invalid: "0"},
		{key: "CPU_THRESHOLD", valid: "75", invalid: "101"},
		{key: "MCP_TRANSPORT", valid: "stdio", invalid: "websocket"},
		{key: "PROMETHEUS_METRICS_BIND_PORT", valid: "1974", invalid: "65536"},
		{key: "LOG_LEVEL", valid: "warn", invalid: "verbose"},
		{key: "USER_INCLUDE_LIST", valid: "^alice$", invalid: "["},
	}
	contracts := make(map[string]PublicFieldContract)
	for _, contract := range PublicFieldContracts() {
		contracts[contract.Key] = contract
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if constraint := contracts[tt.key].Constraint; constraint.Minimum == nil && constraint.Maximum == nil && len(constraint.Enum) == 0 && constraint.Format == "" {
				t.Fatal("representative validated field has no structured constraint")
			}
			if err := ValidatePublicFieldValue(tt.key, tt.valid); err != nil {
				t.Fatalf("valid production value rejected: %v", err)
			}
			if err := ValidatePublicFieldValue(tt.key, tt.invalid); err == nil {
				t.Fatal("invalid production value accepted")
			}
		})
	}
}

func TestRemovedAndNonEditableFieldsCannotBeValidatedAsEditorValues(t *testing.T) {
	if err := ValidatePublicFieldValue("MIN_SYSTEM_"+"CORES", "1"); err == nil {
		t.Fatal("removed key remained renderable through editor validation")
	}
	if err := ValidatePublicFieldValue("CPU_POINTS_FILE", "/tmp/points"); err == nil {
		t.Fatal("non-editable policy-map path accepted through editor validation")
	}
}

func TestExampleConfigMatchesRuntimeDefaults(t *testing.T) {
	path := "resman.conf.example"
	activeKeys := activeExampleKeys(t, path)
	for _, contract := range PublicFieldContracts() {
		if contract.Key == "SYSTEM_UID_MAX" {
			continue
		}
		if activeKeys[contract.Key] != 1 {
			t.Errorf("%s active assignments = %d, want exactly 1", contract.Key, activeKeys[contract.Key])
		}
		delete(activeKeys, contract.Key)
	}
	if len(activeKeys) != 0 {
		t.Errorf("example contains assignments outside the public contract: %v", activeKeys)
	}

	want := DefaultConfig()
	got := DefaultConfig()
	if err := loadFromFile(path, got); err != nil {
		t.Fatalf("loadFromFile(%s) error = %v", path, err)
	}
	typeOfConfig := reflect.TypeOf(want).Elem()
	wantValue := reflect.ValueOf(want).Elem()
	gotValue := reflect.ValueOf(got).Elem()
	for index := 0; index < typeOfConfig.NumField(); index++ {
		field := typeOfConfig.Field(index)
		key := field.Tag.Get("config")
		if key == "" || key == "-" {
			continue
		}
		if gotDefault, wantDefault := formatPublicDefault(gotValue.Field(index)), formatPublicDefault(wantValue.Field(index)); gotDefault != wantDefault {
			t.Errorf("%s example value = %q, runtime default = %q", key, gotDefault, wantDefault)
		}
	}
}

func TestEmptyIncludeListMeaningsMatchEligibility(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UserIncludeList = nil
	cfg.UserExcludeList = nil
	cfg.RAMUserIncludeList = nil
	cfg.RAMUserExcludeList = nil
	cfg.IOUserIncludeList = nil
	cfg.IOUserExcludeList = nil

	tests := []struct {
		key      string
		eligible bool
		meaning  string
	}{
		{key: "USER_INCLUDE_LIST", eligible: cfg.IsUserWhitelisted("alice"), meaning: "Empty makes no user eligible for CPU Points enforcement"},
		{key: "RAM_USER_INCLUDE_LIST", eligible: cfg.IsUserWhitelistedForRAM("alice"), meaning: "Empty includes every non-excluded user for RAM eligibility"},
		{key: "IO_USER_INCLUDE_LIST", eligible: cfg.IsUserWhitelistedForIO("alice"), meaning: "Empty includes every non-excluded user for I/O eligibility"},
	}
	contracts := make(map[string]PublicFieldContract)
	for _, contract := range PublicFieldContracts() {
		contracts[contract.Key] = contract
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if tt.eligible != (tt.key != "USER_INCLUDE_LIST") {
				t.Errorf("runtime eligibility = %t", tt.eligible)
			}
			if !strings.HasPrefix(contracts[tt.key].EmptyOrDisabledMeaning, tt.meaning) {
				t.Errorf("documented meaning = %q, want prefix %q", contracts[tt.key].EmptyOrDisabledMeaning, tt.meaning)
			}
		})
	}
}

func TestSecondaryConfigurationReferencesStayFocusedAndSecure(t *testing.T) {
	references := map[string]string{
		filepath.Join("..", "README.md"):                          "](docs/CONFIGURATION.md)",
		filepath.Join("..", "docs", "TECHNICAL-SPECIFICATION.md"): "](CONFIGURATION.md)",
		filepath.Join("..", "docs", "resman.8"):                   "CONFIGURATION.md",
	}
	for path, marker := range references {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if count := strings.Count(string(content), marker); count != 1 {
			t.Errorf("%s reference count for %q = %d, want 1", path, marker, count)
		}
		if strings.Contains(string(content), "All Configuration Options") {
			t.Errorf("%s retains a duplicate complete configuration inventory", path)
		}
	}

	root := filepath.Join("..")
	marker := "non-default remote bind: requires tls, authentication, and firewall restrictions"
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel != "README.md" && rel != "config/resman.conf.example" && rel != "docs/resman.8" && !strings.HasPrefix(filepath.ToSlash(rel), "docs/") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Split(string(content), "\n")
		for line, text := range lines {
			if !strings.Contains(text, "PROMETHEUS_METRICS_BIND_HOST") || !strings.Contains(text, "0.0.0.0") {
				continue
			}
			start := max(0, line-3)
			context := strings.ToLower(strings.Join(lines[start:line+1], " "))
			if !strings.Contains(context, marker) {
				t.Errorf("%s:%d remote bind lacks the required security classification", rel, line+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan secondary configuration references: %v", err)
	}
}

func TestCPUPointsPublicKeysLandTogetherAtTheEnforcementCutover(t *testing.T) {
	want := map[string]bool{
		"CPU_RESERVE_POINTS":     false,
		"CPU_ROOT_POINTS":        false,
		"CPU_BEST_EFFORT_POINTS": false,
		"CPU_POINTS_FILE":        false,
	}
	for _, contract := range PublicFieldContracts() {
		if _, exists := want[contract.Key]; exists {
			want[contract.Key] = true
		}
	}
	for key, present := range want {
		if !present {
			t.Errorf("%s is absent from the atomic CPU Points cutover", key)
		}
	}
}

func TestRemovedCPUAllocationContractsStayOutOfTheLiveTree(t *testing.T) {
	root := filepath.Join("..")
	removedCPUKeys := []string{
		"MIN_SYSTEM_" + "CORES",
		"CPU_QUOTA_" + "NORMAL",
		"CPU_QUOTA_" + "LIMITED",
		"CPU_DEFAULT_" + "POINTS",
		"BATCH_NIGHT_CPU_" + "QUOTA",
		"INTERACTIVE_CPU_" + "QUOTA",
		"PSI_BOOST_" + "WEIGHT",
		"PSI_BOOST_" + "DURATION",
	}
	allowedHistory := map[string]bool{
		"docs/UPGRADING.md":         true,
		"packaging/rpm/resman.spec": true,
	}
	allowedReferences := map[string]map[string]string{
		"docs/DEVELOPMENT.md": {
			"CPU_QUOTA_" + "LIMITED": "historical defect provenance for the normative no-inert-knob rules",
		},
		"packaging/layout/upgrade_notes_test.go": {
			"CPU_QUOTA_" + "LIMITED": "assertion that the shipped breaking-change record retains the removed key",
		},
	}
	observedTombstones := make(map[string]int, len(removedCPUKeys))
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel == ".git" || rel == ".beads" || rel == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.IndexByte(string(content), 0) >= 0 {
			return nil
		}
		for _, key := range removedCPUKeys {
			count := strings.Count(string(content), key)
			if count == 0 {
				continue
			}
			switch {
			case rel == "config/config.go":
				observedTombstones[key] += count
			case allowedHistory[rel]:
				// Historical upgrade records may name removed keys verbatim.
			case strings.TrimSpace(allowedReferences[rel][key]) != "":
				// Normative provenance and tests may name a removed key only
				// through an exact, motivated entry above.
			default:
				t.Errorf("%s contains removed CPU allocation contract %s", rel, key)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan live tree for removed CPU allocation contracts: %v", err)
	}
	for _, key := range removedCPUKeys {
		if observedTombstones[key] != 1 {
			t.Errorf("%s tombstone occurrences in config/config.go = %d, want exactly 1", key, observedTombstones[key])
		}
	}
}

func activeExampleKeys(t *testing.T, path string) map[string]int {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close %s: %v", path, err)
		}
	}()

	result := make(map[string]int)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if ok {
			result[strings.TrimSpace(key)]++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return result
}
