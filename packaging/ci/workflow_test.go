package ci

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	assertContains(t, qualityWorkflow, "sudo apt-get install --yes prometheus")
	assertContains(t, qualityWorkflow, "make lint-install")
	assertContains(t, qualityWorkflow, "run: make ci-quality")

	qualityTarget := makeTarget(t, makefile, "ci-quality")
	for _, required := range []string{
		"verify-modules verify-format",
		"$(GO) build ./...",
		"$(GO) vet ./...",
		"$(MAKE) verify-contracts",
		"$(MAKE) ci-test",
		"$(MAKE) lint",
	} {
		assertContains(t, qualityTarget, required)
	}
	moduleTarget := makeTarget(t, makefile, "verify-modules")
	assertContains(t, moduleTarget, "git diff --exit-code -- go.mod go.sum")
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

func runMakeTarget(dir, root, target string) (string, error) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("locate go binary: %w", err)
	}
	command := exec.Command("make", "--no-print-directory", "-f", filepath.Join(root, "Makefile"), target, "GO="+goBinary)
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
