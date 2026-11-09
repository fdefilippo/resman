package preflight

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fdefilippo/resman/config"
)

func TestCheckSharesValidationWithoutCreatingRuntimeArtifacts(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(dir, "cpu-points.map")
	if err := os.WriteFile(mapPath, []byte("[resman-cpu-points-map-v1]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, extra string
		invalid     bool
	}{
		{"valid", "", false},
		{"bad threshold", "CPU_THRESHOLD=0\n", true},
		{"missing map", "CPU_POINTS_FILE=" + filepath.Join(dir, "missing.map") + "\n", true},
		{"bad regex", "USER_INCLUDE_LIST=[\n", true},
		{"bad environment", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ENFORCEMENT_MODE", "observation_only")
			if tt.name == "bad environment" {
				t.Setenv("CPU_THRESHOLD", "0")
			}
			path := filepath.Join(dir, "resman.conf")
			content := "CPU_POINTS_FILE=" + mapPath + "\nLOG_FILE=" + filepath.Join(dir, "never.log") + "\nMETRICS_DB_ENABLED=true\nMETRICS_DB_PATH=" + filepath.Join(dir, "absent", "metrics.db") + "\nUSER_INCLUDE_LIST=.*\n" + tt.extra
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			report := Check(t.Context(), path)
			if report.Valid() == tt.invalid {
				t.Fatalf("report=%+v", report)
			}
			var b bytes.Buffer
			if err := report.Print(&b); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"never.log", "absent", "metrics.db-wal", "metrics.db-shm"} {
				if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("artifact %s exists or unexpected error: %v", name, err)
				}
			}
			if !tt.invalid && (!strings.Contains(b.String(), "unverified runtime_permissions") || !strings.Contains(b.String(), "classification only")) {
				t.Fatalf("missing limits in report: %s", b.String())
			}
		})
	}
}

func TestCheckRequiresFileAndUsesLifecycleAuthority(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing")
	report := Check(t.Context(), path)
	if report.Valid() || len(report.Findings) != 1 || report.Findings[0].Name != "configuration" {
		t.Fatalf("report=%+v", report)
	}
	for _, field := range config.RestartRequiredFields() {
		lifecycle, ok := config.LifecycleForField(field)
		if !ok || lifecycle != config.LifecycleRestartRequired {
			t.Fatalf("field=%s", field)
		}
	}
}
