package layout

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/cpupoints"
)

func nativeUpgradePolicy(t *testing.T, body string, reserve, root, bestEffort uint64) (cpupoints.PolicySnapshot, error) {
	t.Helper()
	r, err := cpupoints.NewReservePoints(reserve)
	if err != nil {
		t.Fatal(err)
	}
	z, err := cpupoints.NewRootPoints(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := cpupoints.NewBestEffortPoints(bestEffort)
	if err != nil {
		t.Fatal(err)
	}
	p, err := cpupoints.NewPolicyMapPath(config.DefaultCPUPointsMapPath)
	if err != nil {
		t.Fatal(err)
	}
	return cpupoints.NewPolicyLoader().LoadContent(cpupoints.PolicyInputs{
		Reserve: r, Root: z, BestEffort: b, MapPath: p,
	}, []byte(body), exactCPUPointsResolver{"alice": 1001, "bob": 1002})
}

func TestNativeUpgradeExamplesUseTheProductionPolicyValidator(t *testing.T) {
	guide := readTextFile(t, filepath.Join(repositoryRoot(t), "docs/UPGRADING.md"))
	defaults := config.DefaultConfig()
	reserve, root, bestEffort := uint64(defaults.CPUReservePoints), uint64(defaults.CPURootPoints), uint64(defaults.CPUBestEffortPoints)
	mapBlock := regexp.MustCompile("(?s)```ini\\n(\\[resman-cpu-points-map-v1\\]\\n.*?)\\n```").FindStringSubmatch(guide)
	if len(mapBlock) != 2 {
		t.Fatal("upgrade guide has no executable old-policy map example")
	}
	_, err := nativeUpgradePolicy(t, mapBlock[1], reserve, root, bestEffort)
	var overcommit *cpupoints.PolicyOvercommitError
	if !errors.As(err, &overcommit) {
		t.Fatalf("old map must fail with typed overcommit, got %v", err)
	}
	if overcommit.Guarantees != 750 || overcommit.Guarantees+bestEffort > overcommit.Pool {
		t.Fatalf("example does not represent a previously valid 750-point map: %+v", overcommit)
	}
	if !strings.Contains(guide, err.Error()) {
		t.Fatalf("guide diagnostic differs from production: %s", err)
	}
	// The old saturated boundary is distinct from the representative 750-point case.
	_, err = nativeUpgradePolicy(t, cpupoints.PolicyMapMarker+"\nalice=400\nbob=400\n", reserve, root, bestEffort)
	if !errors.As(err, &overcommit) || overcommit.Guarantees != 800 {
		t.Fatalf("old saturated budget accepted: %v", err)
	}

	rows := regexp.MustCompile(`(?m)^\| (Reduce [^|]+) \| (\d+) \| (\d+) \| (\d+) \| (\d+) \| (\d+) \|$`).FindAllStringSubmatch(guide, -1)
	if len(rows) != 4 {
		t.Fatalf("want four explicit operator alternatives, got %d", len(rows))
	}
	expectedChoices := map[string]int{
		"Reduce mapped guarantees (alice=400, bob=300)": 0,
		"Reduce reserve":               1,
		"Reduce root entitlement":      2,
		"Reduce aggregate best effort": 3,
	}
	for _, row := range rows {
		t.Run(row[1], func(t *testing.T) {
			expectedTerm, ok := expectedChoices[row[1]]
			if !ok {
				t.Fatalf("unknown or duplicate operator choice %q", row[1])
			}
			delete(expectedChoices, row[1])
			values := make([]uint64, 5)
			for i := range values {
				v, parseErr := strconv.ParseUint(row[i+2], 10, 64)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				values[i] = v
			}
			mapped, r, z, b, pool := values[0], values[1], values[2], values[3], values[4]
			if mapped < 400 {
				t.Fatal("example cannot retain alice=400")
			}
			body := fmt.Sprintf("%s\nalice=400\nbob=%d\n", cpupoints.PolicyMapMarker, mapped-400)
			policy, loadErr := nativeUpgradePolicy(t, body, r, z, b)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if policy.Pool().Value() != pool {
				t.Fatalf("documented pool %d differs from runtime %d", pool, policy.Pool().Value())
			}
			before, after := []uint64{750, reserve, root, bestEffort}, []uint64{mapped, r, z, b}
			changed := 0
			for i := range before {
				if before[i] != after[i] {
					changed++
					if i != expectedTerm {
						t.Fatal("alternative reduces a different protection than its label")
					}
					if after[i] >= before[i] {
						t.Fatal("a reduction increased a budget term")
					}
				}
			}
			if changed != 1 {
				t.Fatalf("alternative must change exactly one protection, changed %d", changed)
			}
		})
	}
}

func TestNativeCardinalityExamplesMatchTheExactPlanner(t *testing.T) {
	for _, test := range []struct {
		name, body string
		bound      int
	}{
		{"empty default map", cpupoints.PolicyMapMarker + "\n", 10000},
		{"700-point guarantee", cpupoints.PolicyMapMarker + "\nalice=700\n", 1400},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy, err := nativeUpgradePolicy(t, test.body, 100, 100, 100)
			if err != nil {
				t.Fatal(err)
			}
			cpus, err := cpupoints.NewOnlineCPUCount(4)
			if err != nil {
				t.Fatal(err)
			}
			participants := make([]cpupoints.ActiveUserSlice, test.bound)
			for i := range participants {
				participants[i] = cpupoints.ActiveUserSlice{UID: 2000 + i}
			}
			plan, err := cpupoints.PlanFlatTopology(policy, cpus, participants)
			if err != nil {
				t.Fatal(err)
			}
			if int(plan.BestEffortAggregateWeight().Value()) != test.bound {
				t.Fatalf("bound differs from aggregate weight: %d", plan.BestEffortAggregateWeight().Value())
			}
			_, err = cpupoints.PlanFlatTopology(policy, cpus, append(participants, cpupoints.ActiveUserSlice{UID: 2000 + test.bound}))
			var rejected *cpupoints.FlatPlanError
			if !errors.As(err, &rejected) || rejected.Reason != cpupoints.FlatPlanBestEffortCardinality {
				t.Fatalf("bound+1 must fail before mutation: %v", err)
			}
		})
	}
}

func TestNativeUpgradeSurfacesCarryEveryCompatibilityBreak(t *testing.T) {
	root := repositoryRoot(t)
	guide := readTextFile(t, filepath.Join(root, "docs/UPGRADING.md"))
	for _, row := range []string{"Enforcement mode", "Flat lending", "Root sessions", "Excluded users", "Rootless inheritance", "Split authority", "History reset", "Telemetry replacement", "Measurement", "Reload", "Durable ownership"} {
		if !strings.Contains(guide, "| "+row+" |") {
			t.Errorf("missing compatibility-break row %s", row)
		}
	}
	for _, required := range []string{"701–800 with the defaults", "CPU_ROOT_POINTS=100", "user-0.slice", "system.slice", "schema version is 7", "systemd-property-leases.json", "750-point map"} {
		if !strings.Contains(guide, required) {
			t.Errorf("missing native upgrade contract %q", required)
		}
	}
	for _, path := range []string{"README.md", "docs/ARCHITECTURE.md", "docs/TECHNICAL-SPECIFICATION.md", "docs/resman.8", "docs/CONFIGURATION.md"} {
		t.Run(path, func(t *testing.T) {
			body := readTextFile(t, filepath.Join(root, path))
			for _, required := range []string{"CPU_ROOT_POINTS", "system.slice", "user.slice", "CPU-POINTS-OBSERVABILITY.md", "observation_only"} {
				if !strings.Contains(body, required) {
					t.Errorf("missing %q", required)
				}
			}
		})
	}
	for _, path := range []string{"docs/CONFIGURATION.md", "docs/resman.8", "docs/UPGRADING.md"} {
		body := readTextFile(t, filepath.Join(root, path))
		for _, required := range []string{"floor(10000 / M)", "10000", "1400", "best_effort_cardinality"} {
			if !strings.Contains(body, required) {
				t.Errorf("%s lacks cardinality contract %q", path, required)
			}
		}
	}
	defaults := config.DefaultConfig()
	for _, path := range []string{"docs/UPGRADING.md", "docs/resman.8", "docs/CONFIGURATION.md", "config/resman.conf.example", "packaging/deb/control.in", "packaging/rpm/resman.spec"} {
		body := readTextFile(t, filepath.Join(root, path))
		matches := regexp.MustCompile(`CPU_ROOT_POINTS=(\d+)`).FindAllStringSubmatch(body, -1)
		if len(matches) == 0 {
			t.Errorf("%s has no root default", path)
		}
		for _, match := range matches {
			if match[1] != strconv.Itoa(defaults.CPURootPoints) {
				t.Errorf("%s has divergent root default %s", path, match[0])
			}
		}
	}
}

func TestNativeUpgradeRejectsStaleCurrentContracts(t *testing.T) {
	root := repositoryRoot(t)
	for _, path := range []string{"README.md", "docs/UPGRADING.md", "docs/TECHNICAL-SPECIFICATION.md", "docs/ARCHITECTURE.md", "docs/METRICS-DATABASE.md", "docs/resman.8"} {
		body := strings.Join(strings.Fields(readTextFile(t, filepath.Join(root, path))), " ")
		for _, stale := range []string{"schema version is 5", "schema version 5 can be created", "restart to create version 5", "ResMan currently runs in `observation_only_systemd`", "architecture is explicitly observation-only", "systemd-native replacement is specified"} {
			if strings.Contains(body, stale) {
				t.Errorf("%s contains stale current contract %q", path, stale)
			}
		}
	}
	makefile := readTextFile(t, filepath.Join(root, "Makefile"))
	if makeVariable(t, makefile, "VERSION") == "1.32.0" {
		t.Fatal("native cutover reuses the released observation-only version")
	}
}

func TestCurrentMetricsSchemaReferencesMatchTheWriter(t *testing.T) {
	root := repositoryRoot(t)
	source, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, "database/manager.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	version := ""
	ast.Inspect(source, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if ok && len(spec.Names) == 1 && spec.Names[0].Name == "metricsSchemaVersion" && len(spec.Values) == 1 {
			if literal, ok := spec.Values[0].(*ast.BasicLit); ok && literal.Kind == token.INT {
				version = literal.Value
			}
		}
		return true
	})
	if version == "" {
		t.Fatal("production schema version was not inspected")
	}
	for _, path := range []string{"README.md", "docs/UPGRADING.md", "docs/ARCHITECTURE.md", "docs/TECHNICAL-SPECIFICATION.md", "docs/METRICS-DATABASE.md", "docs/CPU-POINTS-OBSERVABILITY.md", "docs/resman.8"} {
		body := readTextFile(t, filepath.Join(root, path))
		matches := regexp.MustCompile(`Current metrics schema: ([0-9]+)\.`).FindAllStringSubmatch(body, -1)
		if len(matches) != 1 || matches[0][1] != version {
			t.Errorf("%s disagrees with production schema %s: %v", path, version, matches)
		}
	}
}

func TestNativeReferencesAreShippedAndOperatorFilesPreserved(t *testing.T) {
	root := repositoryRoot(t)
	for _, path := range []string{"Makefile", "packaging/deb/prepare-package.sh", "packaging/rpm/resman.spec", "packaging/docker/Dockerfile", "packaging/layout/verify-package-layout.sh"} {
		body := readTextFile(t, filepath.Join(root, path))
		for _, doc := range []string{"CPU-POINTS-OBSERVABILITY.md", "SYSTEMD-PROPERTY-LEASES.md", "UPGRADING.md", "CONFIGURATION.md"} {
			if !strings.Contains(body, doc) {
				t.Errorf("%s omits %s", path, doc)
			}
		}
	}
	makefile := readTextFile(t, filepath.Join(root, "Makefile"))
	for _, name := range []string{"resman.conf", "cpu-points.map"} {
		guard := "sudo test -e $(CONF_DIR)/" + name + " || sudo test -L $(CONF_DIR)/" + name
		if !strings.Contains(makefile, guard) {
			t.Errorf("make install can overwrite existing %s", name)
		}
	}
}
