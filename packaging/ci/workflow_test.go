package ci

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkflowUsesOneSharedQualityDefinition(t *testing.T) {
	root := repositoryRoot(t)
	ciWorkflow := readFile(t, filepath.Join(root, ".github/workflows/ci.yml"))
	releaseWorkflow := readFile(t, filepath.Join(root, ".github/workflows/release.yml"))
	qualityWorkflow := readFile(t, filepath.Join(root, ".github/workflows/quality.yml"))
	makefile := readFile(t, filepath.Join(root, "Makefile"))

	assertContains(t, ciWorkflow, "pull_request:")
	assertContains(t, ciWorkflow, "branches:\n      - main")
	assertContains(t, ciWorkflow, "uses: ./.github/workflows/quality.yml")
	assertContains(t, releaseWorkflow, "uses: ./.github/workflows/quality.yml")
	assertNotContains(t, releaseWorkflow, "go test -race")
	assertNotContains(t, releaseWorkflow, "golangci/golangci-lint-action")

	assertContains(t, qualityWorkflow, "workflow_call:")
	assertContains(t, qualityWorkflow, `CGO_ENABLED: "1"`)
	assertContains(t, qualityWorkflow, "go-version-file: go.mod")
	assertContains(t, qualityWorkflow, "sudo apt-get install --yes prometheus shellcheck")
	assertContains(t, qualityWorkflow, "make lint-install")
	assertContains(t, qualityWorkflow, `REQUIRE_SHELLCHECK: "1"`)
	assertContains(t, qualityWorkflow, `run: make ci-quality GOLANGCI_LINT="$(go env GOPATH)/bin/golangci-lint"`)

	qualityTarget := makeTarget(t, makefile, "ci-quality")
	for _, required := range []string{
		"verify-modules verify-format verify-promtool verify-shellcheck",
		"$(GO) build ./...",
		"$(GO) vet ./...",
		"$(MAKE) verify-contracts",
		"$(MAKE) ci-test",
		"$(MAKE) lint-required",
	} {
		assertContains(t, qualityTarget, required)
	}
	moduleTarget := makeTarget(t, makefile, "verify-modules")
	assertContains(t, moduleTarget, "git diff --exit-code -- go.mod go.sum")
	lintTarget := makeTarget(t, makefile, "lint-required")
	assertNotContains(t, lintTarget, "version --short")
}

func TestFuzzWorkflowGeneratesInputsAndPreservesFailureEvidence(t *testing.T) {
	root := repositoryRoot(t)
	fuzzWorkflow := readFile(t, filepath.Join(root, ".github/workflows/fuzz.yml"))
	ciWorkflow := readFile(t, filepath.Join(root, ".github/workflows/ci.yml"))
	qualityWorkflow := readFile(t, filepath.Join(root, ".github/workflows/quality.yml"))

	for _, required := range []string{
		"schedule:",
		"cron:",
		"workflow_dispatch:",
		"timeout-minutes: 30",
		"FUZZTIME: 2m",
		`make fuzz FUZZTIME="$FUZZTIME"`,
		"set -o pipefail",
		"if: failure()",
		"uses: actions/upload-artifact@v4",
		"fuzz.log",
		"**/testdata/fuzz/**",
		"resman-go-build/fuzz/**",
	} {
		assertContains(t, fuzzWorkflow, required)
	}
	assertNotContains(t, ciWorkflow, "make fuzz")
	assertNotContains(t, qualityWorkflow, "make fuzz")
}

func TestWorkflowGateStepsCannotIgnoreFailures(t *testing.T) {
	root := repositoryRoot(t)
	workflowDir := filepath.Join(root, ".github/workflows")
	entries, err := os.ReadDir(workflowDir)
	if err != nil {
		t.Fatalf("read workflow directory: %v", err)
	}

	workflowCount := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yml" && ext != ".yaml" {
			continue
		}
		workflowCount++
		assertWorkflowHasNoContinueOnError(t, filepath.Join(workflowDir, entry.Name()))
	}
	if workflowCount == 0 {
		t.Fatal("no GitHub Actions workflows found")
	}
}

func TestWorkflowContinueOnErrorGuardParsesYAMLKeys(t *testing.T) {
	tests := []struct {
		name      string
		workflow  string
		wantLines []int
	}{
		{
			name: "normally formatted step key",
			workflow: "jobs:\n  test:\n    steps:\n      - continue-on-error: true\n" +
				"        run: make test\n",
			wantLines: []int{4},
		},
		{
			name: "space before colon at job and step levels",
			workflow: "jobs:\n  test:\n    continue-on-error : true\n    steps:\n" +
				"      - continue-on-error : false\n        run: make test\n",
			wantLines: []int{3, 5},
		},
		{
			name:     "value and comment are not keys",
			workflow: "name: \"continue-on-error:\"\n# continue-on-error: true\njobs: {}\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines, err := workflowContinueOnErrorLines([]byte(tt.workflow))
			if err != nil {
				t.Fatalf("parse workflow: %v", err)
			}
			if !slices.Equal(lines, tt.wantLines) {
				t.Fatalf("continue-on-error lines = %v, want %v", lines, tt.wantLines)
			}
		})
	}
}

func TestWorkflowContinueOnErrorGuardRejectsMalformedYAML(t *testing.T) {
	if _, err := workflowContinueOnErrorLines([]byte("jobs: [unterminated\n")); err == nil {
		t.Fatal("workflow guard accepted malformed YAML")
	}
}

func TestReleaseRPMWorkflowUsesOneFreshAuthoritativeDirectory(t *testing.T) {
	root := repositoryRoot(t)
	releaseWorkflow := readFile(t, filepath.Join(root, ".github/workflows/release.yml"))

	for _, required := range []string{
		`RPMBUILD_DIR: /tmp/resman-rpmbuild-${{ github.run_id }}-${{ github.run_attempt }}`,
		`test ! -e "$RPMBUILD_DIR"`,
		`make RPMBUILD_DIR="$RPMBUILD_DIR" rpm`,
		`bash packaging/rpm/collect-release-artifacts.sh "$RPMBUILD_DIR" build/release`,
		"expected one DEB, one binary RPM, one source RPM and SHA256SUMS",
	} {
		assertContains(t, releaseWorkflow, required)
	}
	assertNotContains(t, releaseWorkflow, `$HOME/rpmbuild`)
}

func TestRPMArtifactCollectionRejectsDivergentOrStaleTrees(t *testing.T) {
	root := repositoryRoot(t)
	script := filepath.Join(root, "packaging/rpm/collect-release-artifacts.sh")

	t.Run("collects one current binary and source package", func(t *testing.T) {
		buildDir := t.TempDir()
		binary := filepath.Join(buildDir, "RPMS/x86_64/resman-1.33.0-1.el8.x86_64.rpm")
		source := filepath.Join(buildDir, "SRPMS/resman-1.33.0-1.el8.src.rpm")
		writeFixtureFile(t, binary)
		writeFixtureFile(t, source)
		outputDir := filepath.Join(t.TempDir(), "release")

		output, err := runRPMArtifactCollector(script, buildDir, outputDir, "")
		if err != nil {
			t.Fatalf("collector rejected current RPM pair: %v\n%s", err, output)
		}
		for _, name := range []string{filepath.Base(binary), filepath.Base(source)} {
			if _, err := os.Stat(filepath.Join(outputDir, name)); err != nil {
				t.Errorf("collected artifact %s: %v", name, err)
			}
		}
	})

	t.Run("stale default tree cannot satisfy missing current build", func(t *testing.T) {
		home := t.TempDir()
		writeFixtureFile(t, filepath.Join(home, "rpmbuild/RPMS/x86_64/resman-1.24.0-1.el8.x86_64.rpm"))
		writeFixtureFile(t, filepath.Join(home, "rpmbuild/SRPMS/resman-1.24.0-1.el8.src.rpm"))
		outputDir := filepath.Join(t.TempDir(), "release")

		output, err := runRPMArtifactCollector(script, filepath.Join(t.TempDir(), "current"), outputDir, home)
		if err == nil {
			t.Fatalf("collector accepted stale default tree: %s", output)
		}
		if _, statErr := os.Stat(outputDir); !os.IsNotExist(statErr) {
			t.Fatalf("collector created output from stale tree: stat error=%v", statErr)
		}
	})

	t.Run("packages split across trees are rejected", func(t *testing.T) {
		buildDir := t.TempDir()
		writeFixtureFile(t, filepath.Join(buildDir, "RPMS/x86_64/resman-1.33.0-1.el8.x86_64.rpm"))
		if err := os.MkdirAll(filepath.Join(buildDir, "SRPMS"), 0700); err != nil {
			t.Fatalf("create empty current SRPM directory: %v", err)
		}
		home := t.TempDir()
		writeFixtureFile(t, filepath.Join(home, "rpmbuild/SRPMS/resman-1.33.0-1.el8.src.rpm"))
		outputDir := filepath.Join(t.TempDir(), "release")

		output, err := runRPMArtifactCollector(script, buildDir, outputDir, home)
		if err == nil {
			t.Fatalf("collector joined packages from divergent trees: %s", output)
		}
		if !strings.Contains(output, "found 1 binary and 0 source packages") {
			t.Fatalf("collector failure did not identify incomplete current tree: %s", output)
		}
	})

	t.Run("pre-existing release output is rejected", func(t *testing.T) {
		buildDir := t.TempDir()
		writeFixtureFile(t, filepath.Join(buildDir, "RPMS/x86_64/resman-1.33.0-1.el8.x86_64.rpm"))
		writeFixtureFile(t, filepath.Join(buildDir, "SRPMS/resman-1.33.0-1.el8.src.rpm"))
		outputDir := filepath.Join(t.TempDir(), "release")
		writeFixtureFile(t, filepath.Join(outputDir, "resman-1.24.0-1.el8.x86_64.rpm"))

		output, err := runRPMArtifactCollector(script, buildDir, outputDir, "")
		if err == nil {
			t.Fatalf("collector accepted pre-existing release output: %s", output)
		}
		if !strings.Contains(output, "release output directory is not empty") {
			t.Fatalf("collector failure did not identify stale output: %s", output)
		}
	})
}

func TestVerifyFormatRejectsAnUnformattedTrackedFile(t *testing.T) {
	root := repositoryRoot(t)
	fixture := t.TempDir()
	runCommand(t, fixture, nil, "git", "init", "--quiet")
	goFile := filepath.Join(fixture, "sample.go")
	if err := os.WriteFile(goFile, []byte("package sample\n\nfunc Unformatted( ){ }\n"), 0600); err != nil {
		t.Fatalf("write unformatted fixture: %v", err)
	}
	runCommand(t, fixture, nil, "git", "add", "sample.go")

	output, err := runMakeTarget(fixture, root, "verify-format")
	if err == nil {
		t.Fatalf("verify-format accepted an unformatted tracked file; output=%s", output)
	}
	if !strings.Contains(output, "sample.go") {
		t.Fatalf("verify-format did not name the unformatted file; output=%s", output)
	}
}

func TestVerifyFormatRejectsInvalidGoSyntax(t *testing.T) {
	root := repositoryRoot(t)
	fixture := t.TempDir()
	runCommand(t, fixture, nil, "git", "init", "--quiet")
	if err := os.WriteFile(filepath.Join(fixture, "invalid.go"), []byte("package sample\n\nfunc Invalid("), 0600); err != nil {
		t.Fatalf("write invalid fixture: %v", err)
	}
	runCommand(t, fixture, nil, "git", "add", "invalid.go")

	output, err := runMakeTarget(fixture, root, "verify-format")
	if err == nil {
		t.Fatalf("verify-format accepted invalid Go syntax; output=%s", output)
	}
	if !strings.Contains(output, "invalid.go") {
		t.Fatalf("verify-format did not preserve the syntax error evidence; output=%s", output)
	}
}

func TestVerifyFormatIgnoresTrackedFilesDeletedByTheChange(t *testing.T) {
	root := repositoryRoot(t)
	fixture := t.TempDir()
	runCommand(t, fixture, nil, "git", "init", "--quiet")
	goFile := filepath.Join(fixture, "retired.go")
	if err := os.WriteFile(goFile, []byte("package retired\n"), 0600); err != nil {
		t.Fatalf("write retired fixture: %v", err)
	}
	runCommand(t, fixture, nil, "git", "add", "retired.go")
	if err := os.Remove(goFile); err != nil {
		t.Fatalf("remove retired fixture: %v", err)
	}

	if output, err := runMakeTarget(fixture, root, "verify-format"); err != nil {
		t.Fatalf("verify-format inspected a tracked file deleted by the change: %v; output=%s", err, output)
	}
}

func TestVerifyPromtoolRejectsMissingBinary(t *testing.T) {
	root := repositoryRoot(t)
	makeBinary, err := exec.LookPath("make")
	if err != nil {
		t.Fatalf("locate make: %v", err)
	}
	command := exec.Command(makeBinary, "--no-print-directory", "-f", filepath.Join(root, "Makefile"), "verify-promtool")
	command.Dir = t.TempDir()
	command.Env = []string{"PATH=" + t.TempDir()}
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("verify-promtool accepted a missing binary; output=%s", output)
	}
	if !strings.Contains(string(output), "promtool is required by ci-quality") {
		t.Fatalf("verify-promtool failure was not diagnostic; output=%s", output)
	}
}

func TestVerifyShellcheckRejectsADeliberatelyDefectiveTrackedScript(t *testing.T) {
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("ShellCheck is not installed; CI installs it before running this test")
	}
	root := repositoryRoot(t)
	fixture := t.TempDir()
	runCommand(t, fixture, nil, "git", "init", "--quiet")
	defective := filepath.Join(fixture, "deliberate-defect")
	if err := os.WriteFile(defective, []byte("#!/bin/sh\nprintf '%s\\n' $1\n"), 0700); err != nil {
		t.Fatalf("write defective shell fixture: %v", err)
	}
	runCommand(t, fixture, nil, "git", "add", "deliberate-defect")

	output, err := runMakeTarget(fixture, root, "verify-shellcheck", "REQUIRE_SHELLCHECK=1")
	if err == nil {
		t.Fatalf("verify-shellcheck accepted a deliberately defective tracked script; output=%s", output)
	}
	if !strings.Contains(output, "deliberate-defect") || !strings.Contains(output, "SC2086") {
		t.Fatalf("verify-shellcheck did not preserve the ShellCheck evidence; output=%s", output)
	}
}

func TestVerifyShellcheckFailsClosedWhenRequiredBinaryIsMissing(t *testing.T) {
	root := repositoryRoot(t)
	fixture := t.TempDir()
	runCommand(t, fixture, nil, "git", "init", "--quiet")
	output, err := runMakeTarget(
		fixture,
		root,
		"verify-shellcheck",
		"REQUIRE_SHELLCHECK=1",
		"SHELLCHECK="+filepath.Join(fixture, "missing-shellcheck"),
	)
	if err == nil {
		t.Fatalf("verify-shellcheck accepted a missing required binary; output=%s", output)
	}
	if !strings.Contains(output, "ShellCheck is required by ci-quality in CI") {
		t.Fatalf("verify-shellcheck failure was not diagnostic; output=%s", output)
	}
}

func TestLintRequiredAcceptsAnAvailableLinterWithoutVersionGate(t *testing.T) {
	root := repositoryRoot(t)
	fixture := t.TempDir()
	if err := os.WriteFile(filepath.Join(fixture, "go.mod"), []byte("module example.test/lint-version\n\ngo 1.25.7\n"), 0600); err != nil {
		t.Fatalf("write fixture go.mod: %v", err)
	}
	fakeLint := filepath.Join(fixture, "golangci-lint")
	marker := filepath.Join(fixture, "lint-ran")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"" + marker + "\"\nexit 0\n"
	if err := os.WriteFile(fakeLint, []byte(script), 0700); err != nil {
		t.Fatalf("write fake linter: %v", err)
	}

	output, err := runMakeTarget(fixture, root, "lint-required", "GOLANGCI_LINT="+fakeLint)
	if err != nil {
		t.Fatalf("lint-required rejected an available linter because of its version: %v\n%s", err, output)
	}
	invocation, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read fake linter invocation: %v", err)
	}
	if !strings.Contains(string(invocation), "run --max-same-issues=0 --max-issues-per-linter=0 ./...") {
		t.Fatalf("lint-required did not execute the full lint gate; invocation=%s", invocation)
	}
}

func TestCITestRejectsADeliberatelyFailingTest(t *testing.T) {
	root := repositoryRoot(t)
	fixture := t.TempDir()
	if err := os.WriteFile(filepath.Join(fixture, "go.mod"), []byte("module example.test/ci-failure\n\ngo 1.25.7\n"), 0600); err != nil {
		t.Fatalf("write fixture go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "failure_test.go"), []byte("package failure\n\nimport \"testing\"\n\nfunc TestDeliberateFailure(t *testing.T) { t.Fatal(\"deliberate CI mutation\") }\n"), 0600); err != nil {
		t.Fatalf("write failing fixture: %v", err)
	}

	output, err := runMakeTarget(fixture, root, "ci-test")
	if err == nil {
		t.Fatalf("ci-test accepted a deliberately failing test; output=%s", output)
	}
	if !strings.Contains(output, "deliberate CI mutation") || !strings.Contains(output, "FAIL") {
		t.Fatalf("ci-test failure did not preserve the test evidence; output=%s", output)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve workflow test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

func assertContains(t *testing.T, content, expected string) {
	t.Helper()
	if !strings.Contains(content, expected) {
		t.Fatalf("content does not contain %q", expected)
	}
}

func assertNotContains(t *testing.T, content, forbidden string) {
	t.Helper()
	if strings.Contains(content, forbidden) {
		t.Fatalf("content unexpectedly contains %q", forbidden)
	}
}

func assertWorkflowHasNoContinueOnError(t *testing.T, path string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines, err := workflowContinueOnErrorLines(content)
	if err != nil {
		t.Fatalf("parse workflow %s: %v", path, err)
	}
	if len(lines) > 0 {
		t.Fatalf("workflow %s contains continue-on-error at lines %v", path, lines)
	}
}

func workflowContinueOnErrorLines(content []byte) ([]int, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		return nil, fmt.Errorf("decode YAML: %w", err)
	}

	var lines []int
	var visit func(*yaml.Node)
	visit = func(node *yaml.Node) {
		if node.Kind == yaml.MappingNode {
			for index := 0; index+1 < len(node.Content); index += 2 {
				key := node.Content[index]
				value := node.Content[index+1]
				if key.Kind == yaml.ScalarNode && key.Value == "continue-on-error" {
					lines = append(lines, key.Line)
				}
				visit(value)
			}
			return
		}
		for _, child := range node.Content {
			visit(child)
		}
	}
	visit(&document)
	return lines, nil
}

func makeTarget(t *testing.T, makefile, target string) string {
	t.Helper()
	marker := target + ":"
	start := strings.Index(makefile, marker)
	if start < 0 {
		t.Fatalf("Makefile target %s is missing", target)
	}
	rest := makefile[start:]
	if end := strings.Index(rest, "\n\n"); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

func runMakeTarget(dir, root, target string, variables ...string) (string, error) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("locate go binary: %w", err)
	}
	args := []string{"--no-print-directory", "-f", filepath.Join(root, "Makefile"), target, "GO=" + goBinary}
	args = append(args, variables...)
	command := exec.Command("make", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "PATH="+filepath.Dir(goBinary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, runErr := command.CombinedOutput()
	return string(output), runErr
}

func runCommand(t *testing.T, dir string, env []string, name string, args ...string) {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), env...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run %s: %v\n%s", name, err, output)
	}
}

func writeFixtureFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("fixture\n"), 0600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

func runRPMArtifactCollector(script, buildDir, outputDir, home string) (string, error) {
	command := exec.Command("bash", script, buildDir, outputDir)
	command.Env = make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if home == "" || !strings.HasPrefix(entry, "HOME=") {
			command.Env = append(command.Env, entry)
		}
	}
	if home != "" {
		command.Env = append(command.Env, "HOME="+home)
	}
	output, err := command.CombinedOutput()
	return string(output), err
}
