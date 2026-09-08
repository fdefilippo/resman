package layout

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/fdefilippo/resman/internal/cpupoints"
)

type exactCPUPointsResolver map[string]int

func TestCPUPointsDeliveryDocumentsRequirePlacementQualification(t *testing.T) {
	root := repositoryRoot(t)
	for _, path := range []string{"docs/UPGRADING.md", "docs/CPU-POINTS-OBSERVABILITY.md", "docs/resman.8", "config/resman.conf.example"} {
		content := readTextFile(t, filepath.Join(root, path))
		for _, required := range []string{"realized share depends on thread placement", "scheduling entitlements", "CPU-POINTS-OBSERVABILITY.md"} {
			if path == "docs/CPU-POINTS-OBSERVABILITY.md" && required == "CPU-POINTS-OBSERVABILITY.md" {
				continue
			}
			if !strings.Contains(content, required) {
				t.Errorf("%s lacks delivery qualification %q", path, required)
			}
		}
		normalized := strings.Join(strings.Fields(strings.ReplaceAll(content, "#", "")), " ")
		for _, forbidden := range []string{"minimum proportional entitlements to effective parent bandwidth", "Actual delivery is proportional to usable parent bandwidth", "CPU Points are minimum proportional entitlements"} {
			if strings.Contains(normalized, forbidden) {
				t.Errorf("%s restores an unqualified delivered-minimum claim: %s", path, forbidden)
			}
		}
	}
	observation := readTextFile(t, filepath.Join(root, "docs/CPU-POINTS-OBSERVABILITY.md"))
	for _, fragment := range []string{"complete contention alone is insufficient", "17.2815 percentage points", "not operator thresholds", "resman_user_cpu_points_leaf_usage_microseconds_delta", "resman_cpu_points_parent_usage_microseconds_delta"} {
		if !strings.Contains(observation, fragment) {
			t.Errorf("observation procedure lacks %q", fragment)
		}
	}
}

func (r exactCPUPointsResolver) ResolveExactUsername(username string) ([]cpupoints.ResolvedUserIdentity, error) {
	uid, ok := r[username]
	if !ok {
		return nil, nil
	}
	return []cpupoints.ResolvedUserIdentity{{Username: username, UID: uid}}, nil
}

func TestShippedCPUPointsMapPassesTheProductionParserAndCapacityInvariant(t *testing.T) {
	root := repositoryRoot(t)
	example := readTextFile(t, filepath.Join(root, "config/cpu-points.map.example"))

	loadShippedCPUPointsMap(t, example, exactCPUPointsResolver{})
	activeExample := strings.ReplaceAll(example, "# alice=300", "alice=300")
	activeExample = strings.ReplaceAll(activeExample, "# john.smith=200", "john.smith=200")
	loadShippedCPUPointsMap(t, activeExample, exactCPUPointsResolver{
		"alice":      1001,
		"john.smith": 1002,
	})

	t.Run("marker drift", func(t *testing.T) {
		mutated := strings.Replace(example, cpupoints.PolicyMapMarker, "[cpu-points-v1]", 1)
		if _, err := loadCPUPointsMap(t, mutated, exactCPUPointsResolver{}); err == nil {
			t.Fatal("production loader accepted a drifted map marker")
		}
	})
	t.Run("guarantee overcommit", func(t *testing.T) {
		mutated := cpupoints.PolicyMapMarker + "\nalice=800\nbob=1\n"
		if _, err := loadCPUPointsMap(t, mutated, exactCPUPointsResolver{"alice": 1001, "bob": 1002}); err == nil {
			t.Fatal("production loader accepted guarantees plus root and best effort above the parent pool")
		}
	})
}

func TestCPUPointsOperatorSurfacesCarryTheCompleteContract(t *testing.T) {
	root := repositoryRoot(t)
	requiredByPath := map[string][]string{
		"README.md": {
			"john.smith=200",
			"without moving processes",
			"CPU-POINTS-OBSERVABILITY.md",
			"60-second observation window",
			"memory.high < memory.max",
		},
		"config/resman.conf.example": {
			"Even reserve zero programs finite cpu.max",
			"cannot exempt one process",
			"Active class changes",
			"memory.high < memory.max",
		},
		"docs/ARCHITECTURE.md": {
			"CPU-POINTS-OBSERVABILITY.md",
			"60-second window",
			"including excluded users",
			"john.smith",
			"No resource transition changes process",
		},
		"docs/TECHNICAL-SPECIFICATION.md": {
			"floor(online_cpus * period * (1000 - reserve) / 1000)",
			"1000 microseconds",
			"CPU-POINTS-OBSERVABILITY.md",
			"60-second window",
			"authoritative weight",
		},
		"docs/resman.8": {
			"john.smith=200",
			"60-second measurement procedure",
			"never creates a standalone cgroup",
			"existing slice charges",
			"high-equals-max control",
		},
		"docs/UPGRADING.md": {
			"ceil(1000*m/N)",
			"When `m >= N`, no exact CPU Points equivalent exists",
			"CPU_DEFAULT_" + "POINTS",
			"No PID-relocation state is migrated",
			"newly shipped map file",
			"CPU-POINTS-OBSERVABILITY.md",
			"startup attempt reports every distinct removed key",
			"remove the complete reported set",
		},
	}

	for path, required := range requiredByPath {
		t.Run(path, func(t *testing.T) {
			content := readTextFile(t, filepath.Join(root, path))
			for _, fragment := range required {
				if !strings.Contains(content, fragment) {
					t.Errorf("%s is missing CPU Points contract %q", path, fragment)
				}
			}
		})
	}
}

func TestCPUPointsContractsRejectYAMLPrefixAndAbsoluteGuaranteeDrift(t *testing.T) {
	root := repositoryRoot(t)
	paths := []string{
		"README.md",
		"config/resman.conf.example",
		"docs/ARCHITECTURE.md",
		"docs/TECHNICAL-SPECIFICATION.md",
		"docs/resman.8",
	}
	forbidden := []*regexp.Regexp{
		regexp.MustCompile(`(?i)cpu-points\.(?:yaml|yml)`),
		regexp.MustCompile(`USER_POINTS\.`),
		regexp.MustCompile(`(?i)absolute CPU guarantee`),
		regexp.MustCompile(`(?i)guaranteed CPU cores?`),
		regexp.MustCompile(`(?i)CPU Points (?:provide|are|give) (?:an )?absolute`),
	}

	for _, path := range paths {
		content := readTextFile(t, filepath.Join(root, path))
		for _, pattern := range forbidden {
			if match := pattern.FindString(content); match != "" {
				t.Errorf("%s contains forbidden CPU Points contract %q", path, match)
			}
		}
	}

	entries, err := os.ReadDir(filepath.Join(root, "config"))
	if err != nil {
		t.Fatalf("read config directory: %v", err)
	}
	yamlMapName := regexp.MustCompile(`(?i)^cpu-points\.(?:yaml|yml)$`)
	for _, entry := range entries {
		if yamlMapName.MatchString(entry.Name()) {
			t.Errorf("config contains forbidden YAML policy map %s", entry.Name())
		}
	}
}

func loadShippedCPUPointsMap(t *testing.T, content string, resolver exactCPUPointsResolver) {
	t.Helper()
	if _, err := loadCPUPointsMap(t, content, resolver); err != nil {
		t.Fatalf("load shipped CPU Points example: %v", err)
	}
}

func loadCPUPointsMap(t *testing.T, content string, resolver exactCPUPointsResolver) (cpupoints.PolicySnapshot, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatalf("secure CPU Points test directory: %v", err)
	}
	path := filepath.Join(dir, "cpu-points.map")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write CPU Points test map: %v", err)
	}
	mapPath, err := cpupoints.NewPolicyMapPath(path)
	if err != nil {
		t.Fatalf("construct CPU Points map path: %v", err)
	}
	reserve, err := cpupoints.NewReservePoints(100)
	if err != nil {
		t.Fatalf("construct reserve: %v", err)
	}
	root, err := cpupoints.NewRootPoints(100)
	if err != nil {
		t.Fatalf("construct root points: %v", err)
	}
	bestEffort, err := cpupoints.NewBestEffortPoints(100)
	if err != nil {
		t.Fatalf("construct best effort: %v", err)
	}
	return cpupoints.NewPolicyLoader().Load(cpupoints.PolicyInputs{
		Reserve:    reserve,
		Root:       root,
		BestEffort: bestEffort,
		MapPath:    mapPath,
	}, resolver)
}
