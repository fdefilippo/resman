package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRegexListRejectsCommaBearingPatternsAcrossAll(t *testing.T) {
	keys := []string{
		"USER_INCLUDE_LIST",
		"USER_EXCLUDE_LIST",
		"PROCESS_EXCLUDE_LIST",
		"RAM_USER_INCLUDE_LIST",
		"RAM_USER_EXCLUDE_LIST",
		"IO_USER_INCLUDE_LIST",
		"IO_USER_EXCLUDE_LIST",
	}
	values := []string{
		"^svc[0-9]{2,4}$",
		"^a{2,}$",
		"^(foo|bar){1,3}$",
	}

	for _, key := range keys {
		for _, value := range values {
			t.Run(key+"/"+value, func(t *testing.T) {
				cfg := DefaultConfig()
				if err := setConfigField(cfg, key, "^preserved$"); err != nil {
					t.Fatalf("set initial value: %v", err)
				}
				before := regexListFieldValue(t, cfg, key)

				err := setConfigField(cfg, key, value)
				if err == nil {
					t.Fatal("comma-bearing regex pattern was accepted")
				}
				for _, required := range []string{key, value, "comma is the list separator", "expanding the alternatives"} {
					if !strings.Contains(err.Error(), required) {
						t.Errorf("error %q omits %q", err, required)
					}
				}
				if got := regexListFieldValue(t, cfg, key); !reflect.DeepEqual(got, before) {
					t.Fatalf("rejected value changed %s from %v to %v", key, before, got)
				}
			})
		}
	}
}

func TestRegexListAcceptsUnambiguousMultiplePatterns(t *testing.T) {
	patterns, err := parseRegexList("USER_INCLUDE_LIST", `^alice$,^svc[0-9]{3}$`)
	if err != nil {
		t.Fatalf("parseRegexList() error: %v", err)
	}
	want := []string{"^alice$", "^svc[0-9]{3}$"}
	if !reflect.DeepEqual(patterns, want) {
		t.Fatalf("patterns = %v, want %v", patterns, want)
	}
}

func TestRegexListSeparatorDetectionTracksEscapesAndCharacterClasses(t *testing.T) {
	for _, value := range []string{`^a\{2$,^bob$`, `^[{]$,^bob$`} {
		if _, err := parseRegexList("USER_INCLUDE_LIST", value); err != nil {
			t.Errorf("parseRegexList(%q) error: %v", value, err)
		}
	}
	if _, err := parseRegexList("USER_INCLUDE_LIST", `^[a,b]$`); err == nil {
		t.Fatal("comma inside a character class was accepted as a list separator")
	}
}

func TestRegexListValidationCoversFileEnvironmentAndPublicEditor(t *testing.T) {
	value := "^svc[0-9]{2,4}$"
	directory := t.TempDir()
	configPath := filepath.Join(directory, "resman.conf")
	if err := os.WriteFile(configPath, []byte("USER_INCLUDE_LIST="+value+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	fileErr := loadFromFile(configPath, DefaultConfig())
	if fileErr == nil {
		t.Fatal("configuration file accepted a comma-bearing quantifier")
	}
	for _, required := range []string{configPath, "line 1", "USER_INCLUDE_LIST", value} {
		if !strings.Contains(fileErr.Error(), required) {
			t.Errorf("file error %q omits %q", fileErr, required)
		}
	}

	t.Setenv("USER_INCLUDE_LIST", value)
	environmentErr := loadFromEnvironment(DefaultConfig())
	if environmentErr == nil || !strings.Contains(environmentErr.Error(), `USER_INCLUDE_LIST="`+value+`"`) {
		t.Fatalf("environment error = %v, want key and original value", environmentErr)
	}

	editorErr := ValidatePublicFieldValue("USER_INCLUDE_LIST", value)
	if editorErr == nil || !strings.Contains(editorErr.Error(), value) {
		t.Fatalf("public editor error = %v, want original value", editorErr)
	}
}

func regexListFieldValue(t *testing.T, cfg *Config, key string) []string {
	t.Helper()
	configType := reflect.TypeOf(cfg).Elem()
	configValue := reflect.ValueOf(cfg).Elem()
	for index := 0; index < configType.NumField(); index++ {
		if configType.Field(index).Tag.Get("config") == key {
			return append([]string(nil), configValue.Field(index).Interface().([]string)...)
		}
	}
	t.Fatalf("configuration key %s has no field", key)
	return nil
}
