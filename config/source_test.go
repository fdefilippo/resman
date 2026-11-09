package config

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadConfigSourceBindsDescriptorAndRefusesReplacement(t *testing.T) {
	for _, scenario := range []string{"regular", "missing", "symlink", "directory", "fifo", "symlink before open", "replace after open"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "resman.conf")
			content := []byte("SERVER_ROLE=original\n")
			if err := os.WriteFile(path, content, 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "other.conf")
			if err := os.WriteFile(target, []byte("SERVER_ROLE=other\n"), 0640); err != nil {
				t.Fatal(err)
			}
			replaceWithSymlink := func() {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				replaceWithSymlink()
			case "directory", "fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if scenario == "directory" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			open := func(name string, flags int, mode os.FileMode) (*os.File, error) {
				if scenario == "symlink before open" {
					replaceWithSymlink()
				}
				file, err := os.OpenFile(name, flags, mode)
				if scenario == "replace after open" && err == nil {
					if err := os.Rename(target, path); err != nil {
						t.Fatal(err)
					}
				}
				return file, err
			}
			info, got, exists, err := readConfigSource(path, open)
			switch scenario {
			case "regular":
				if err != nil || !exists || string(got) != string(content) || info.Mode().Perm() != 0600 {
					t.Fatalf("read = %v %q %v %v", info, got, exists, err)
				}
			case "missing":
				if err != nil || exists {
					t.Fatalf("missing = %v, %v", exists, err)
				}
			default:
				if err == nil || got != nil {
					t.Fatalf("unsafe source returned content %q, error %v", got, err)
				}
				if scenario == "symlink before open" && !errors.Is(err, syscall.ELOOP) {
					t.Fatalf("open followed symlink: %v", err)
				}
			}
		})
	}
}

func TestConfigValuesPreserveInteriorQuotesAndQuotedWhitespace(t *testing.T) {
	for _, tt := range []struct {
		name, raw, want string
		invalid         bool
	}{
		{"unquoted", " plain # comment", "plain", false},
		{"single", "'plain'", "plain", false},
		{"double", `"plain"`, "plain", false},
		{"inner double", `'"plain"'`, `"plain"`, false},
		{"inner single", `"'plain'"`, `'plain'`, false},
		{"repeated", `""plain""`, `"plain"`, false},
		{"whitespace", `" plain " # comment`, " plain ", false},
		{"quoted hash", `'plain # data' # comment`, "plain # data", false},
		{"empty", `''`, "", false},
		{"literal trailing quote", `plain'`, `plain'`, false},
		{"unclosed", `'plain`, "", true},
		{"mismatched", `'plain"`, "", true},
		{"single quote", `'`, "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte("SERVER_ROLE=" + tt.raw + "\n")
			cfg := DefaultConfig()
			err := loadFromData("quotes.conf", data, cfg)
			public, editorErr := parseAuthoredPublicValues(data)
			if tt.invalid {
				if err == nil || editorErr == nil {
					t.Fatalf("invalid value accepted: loader=%v editor=%v", err, editorErr)
				}
				return
			}
			if err != nil || editorErr != nil || cfg.ServerRole != tt.want || public["SERVER_ROLE"] != tt.want {
				t.Fatalf("value=%q public=%q want=%q errors=%v, %v", cfg.ServerRole, public["SERVER_ROLE"], tt.want, err, editorErr)
			}
		})
	}
}
