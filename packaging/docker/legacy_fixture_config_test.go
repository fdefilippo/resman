package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/cpupoints"
)

func TestLegacyGuestGeneratedConfigPassesProductionValidation(t *testing.T) {
	work := t.TempDir()
	// The guest's map is empty, so this fixture requires no host NSS accounts.
	if err := os.WriteFile(filepath.Join(work, "cpu-points.map"), []byte(cpupoints.PolicyMapMarker+"\n"), 0600); err != nil {
		t.Fatalf("write empty guest policy map: %v", err)
	}
	// Import the actual guest renderer: a copied configuration fixture could
	// remain valid while the guest's generated candidate silently drifts.
	command := exec.Command("python3", "-c", `
import importlib.util
from pathlib import Path
import sys
spec = importlib.util.spec_from_file_location("legacy_fixture", sys.argv[1])
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)
print(fixture.render_config(Path(sys.argv[2]).read_text(), Path(sys.argv[3]),
                            Path("/sys/fs/cgroup/resman-legacy-rtest")), end="")
`, "../../test/functional/smolvm/guest/non-systemd-migration.py", "../../config/resman.conf.example", work)
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("render actual legacy guest config: %v\n%s", err, output)
	}
	for _, tc := range []struct {
		name string
		old  string
		new  string
	}{
		{name: "complete generated candidate"},
		{name: "polling below minimum", old: "POLLING_INTERVAL=5", new: "POLLING_INTERVAL=2"},
		{name: "refresh below minimum", old: "METRICS_REFRESH_INTERVAL=5", new: "METRICS_REFRESH_INTERVAL=2"},
		{name: "inconsistent CPU thresholds", old: "CPU_RELEASE_THRESHOLD=1\n", new: "CPU_RELEASE_THRESHOLD=2\n"},
		{name: "unknown generated key", old: "POLLING_INTERVAL=5", new: "UNKNOWN_FIXTURE_KEY=5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := string(output)
			if tc.old != "" {
				if strings.Count(candidate, tc.old) != 1 {
					t.Fatalf("mutation must replace exactly one key: %q", tc.old)
				}
				candidate = strings.Replace(candidate, tc.old, tc.new, 1)
			}
			path := filepath.Join(work, "resman.conf")
			if err := os.WriteFile(path, []byte(candidate), 0600); err != nil {
				t.Fatalf("write generated candidate: %v", err)
			}
			cfg, err := config.LoadAndValidate(path)
			if tc.old != "" {
				if err == nil {
					t.Fatal("production validation accepted the invalid generated candidate")
				}
				return
			}
			if err != nil {
				t.Fatalf("production validation rejected the guest's complete generated candidate: %v", err)
			}
			if cfg.GetPollingInterval() != 5 || cfg.GetMetricsRefreshInterval() != 5 {
				t.Fatal("validated guest cadence differs from the intended five-second fixture")
			}
		})
	}
}
