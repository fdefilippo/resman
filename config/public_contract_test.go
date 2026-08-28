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
		{key: "USER_INCLUDE_LIST", eligible: cfg.IsUserWhitelisted("alice"), meaning: "Empty makes no user eligible for CPU limiting"},
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
