package layout

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareGoWorktreeRejectsRepositoryLocalCaches(t *testing.T) {
	repository := repositoryRoot(t)
	script := filepath.Join(repository, "scripts", "prepare-go-worktree.sh")
	project := t.TempDir()
	writeTestFile(t, filepath.Join(project, "go.mod"), "module example.test/resman\n\ngo 1.27.1\n")

	fakeGo := filepath.Join(t.TempDir(), "go")
	writeTestExecutable(t, fakeGo, `#!/bin/sh
test "$1" = env
case "$2" in
GOCACHE) printf '%s\n' "$TEST_GOCACHE" ;;
GOMODCACHE) printf '%s\n' "$TEST_GOMODCACHE" ;;
*) exit 2 ;;
esac
`)

	externalCache := t.TempDir()
	for _, tt := range []struct {
		name        string
		goCache     string
		moduleCache string
		want        string
	}{
		{name: "external caches", goCache: filepath.Join(externalCache, "build"), moduleCache: filepath.Join(externalCache, "modules")},
		{name: "build cache inside repository", goCache: filepath.Join(project, "build", "gocache"), moduleCache: filepath.Join(externalCache, "modules"), want: "GOCACHE resolves inside"},
		{name: "module cache inside repository", goCache: filepath.Join(externalCache, "build"), moduleCache: filepath.Join(project, "build", "gomodcache"), want: "GOMODCACHE resolves inside"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(script)
			cmd.Dir = project
			cmd.Env = append(os.Environ(),
				"GO="+fakeGo,
				"PROJECT_ROOT="+project,
				"TEST_GOCACHE="+tt.goCache,
				"TEST_GOMODCACHE="+tt.moduleCache,
			)
			output, err := cmd.CombinedOutput()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("external caches rejected: %v\n%s", err, output)
				}
				boundary := readTextFile(t, filepath.Join(project, "build", "go.mod"))
				for _, required := range []string{"module github.com/fdefilippo/resman/build-artifacts", "go 1.27.1"} {
					if !strings.Contains(boundary, required) {
						t.Errorf("generated boundary is missing %q", required)
					}
				}
				return
			}
			if err == nil {
				t.Fatalf("repository-local cache was accepted")
			}
			message := string(output)
			if !strings.Contains(message, tt.want) || !strings.Contains(message, "writable directories outside") {
				t.Errorf("cache rejection lacks path and remedy:\n%s", message)
			}
		})
	}

	inside := filepath.Join(project, "build", "symlinked-cache")
	if err := os.MkdirAll(inside, 0755); err != nil {
		t.Fatal(err)
	}
	cacheLink := filepath.Join(t.TempDir(), "apparently-external-cache")
	if err := os.Symlink(inside, cacheLink); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	cmd.Dir = project
	cmd.Env = append(os.Environ(),
		"GO="+fakeGo,
		"PROJECT_ROOT="+project,
		"TEST_GOCACHE="+cacheLink,
		"TEST_GOMODCACHE="+filepath.Join(externalCache, "modules"),
	)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "GOCACHE resolves inside") {
		t.Fatalf("symlinked repository-local cache was not rejected: %v\n%s", err, output)
	}
}

func TestGeneratedBoundaryExcludesArbitraryBuildContentFromPackageWalk(t *testing.T) {
	repository := repositoryRoot(t)
	project := t.TempDir()
	writeTestFile(t, filepath.Join(project, "go.mod"), "module example.test/resman\n\ngo 1.27.1\n")
	writeTestFile(t, filepath.Join(project, "main.go"), "package main\n\nfunc main() {}\n")
	writeTestFile(t, filepath.Join(project, "build", "gomodcache", "example.com", "module@v1.0.0", "pkg", "pkg.go"), "package pkg\n")

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("locate go: %v", err)
	}
	externalCache := t.TempDir()
	prepare := exec.Command(filepath.Join(repository, "scripts", "prepare-go-worktree.sh"))
	prepare.Dir = project
	prepare.Env = append(os.Environ(),
		"GO="+goBin,
		"PROJECT_ROOT="+project,
		"GOCACHE="+filepath.Join(externalCache, "build"),
		"GOMODCACHE="+filepath.Join(externalCache, "modules"),
	)
	if output, err := prepare.CombinedOutput(); err != nil {
		t.Fatalf("prepare worktree: %v\n%s", err, output)
	}

	list := exec.Command(goBin, "list", "./...")
	list.Dir = project
	list.Env = append(os.Environ(),
		"GOCACHE="+filepath.Join(externalCache, "build"),
		"GOMODCACHE="+filepath.Join(externalCache, "modules"),
		"GOFLAGS=-buildvcs=false",
	)
	output, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("go list crossed the generated boundary: %v\n%s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != "example.test/resman" {
		t.Fatalf("package walk included build artifacts: %q", got)
	}
}

func TestGoCacheGuardCoversMakeAndRPMBuildEntrypoints(t *testing.T) {
	root := repositoryRoot(t)
	makefile := readTextFile(t, filepath.Join(root, "Makefile"))
	spec := readTextFile(t, filepath.Join(root, "packaging", "rpm", "resman.spec"))

	for _, target := range []string{"deps", "ci-test", "test-sendmail", "verify-contracts", "lint-install", "fmt", "deps-check", "deps-check-json", "deps-verify", "deps-vuln", "deps-vuln-install", "deps-report", "deps-update", "deps-update-core"} {
		if !strings.Contains(makefile, "\n"+target+": prepare-go-worktree") {
			t.Errorf("Makefile target %s bypasses prepare-go-worktree", target)
		}
	}
	if block := makeTargetBlock(t, makefile, "prepare-go-worktree"); !strings.Contains(block, "\t@GO=") {
		t.Errorf("prepare-go-worktree must stay silent so deps-check-json remains machine-readable:\n%s", block)
	}
	if !strings.Contains(makefile, "scripts/sendmail.sh scripts/resman-sendmail-hook.sh scripts/prepare-go-worktree.sh") {
		t.Error("RPM source archive does not include the Go worktree guard")
	}
	if !strings.Contains(spec, `GO=go PROJECT_ROOT="$(pwd)" ./scripts/prepare-go-worktree.sh`) {
		t.Error("RPM build does not run the Go worktree guard")
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func writeTestExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
}
