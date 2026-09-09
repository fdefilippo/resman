package layout

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestDependencyMaintenanceTargetsKeepInspectionAndMutationSeparate(t *testing.T) {
	root := repositoryRoot(t)
	makefile := readTextFile(t, root+"/Makefile")
	documentation := readTextFile(t, root+"/docs/DEPENDENCY-MANAGEMENT.md")

	targets := []string{
		"deps-check",
		"deps-check-json",
		"deps-verify",
		"deps-vuln",
		"deps-vuln-install",
		"deps-audit",
		"deps-weekly",
		"deps-report",
		"deps-test",
		"deps-update",
		"deps-update-core",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			if !strings.Contains(makefile, "\n"+target+":") {
				t.Fatalf("Makefile does not define %s", target)
			}
			if !strings.Contains(documentation, "`"+target+"`") && !strings.Contains(documentation, "make "+target) {
				t.Errorf("dependency documentation does not describe %s", target)
			}
		})
	}

	for _, target := range []string{"deps-check", "deps-check-json", "deps-verify", "deps-vuln", "deps-report"} {
		recipe := makeTargetBlock(t, makefile, target)
		for _, mutating := range []string{" mod tidy", " get -u"} {
			if strings.Contains(recipe, mutating) {
				t.Errorf("non-mutating target %s contains %q", target, strings.TrimSpace(mutating))
			}
		}
	}

	if block := makeTargetBlock(t, makefile, "deps-vuln-install"); !strings.Contains(block, "@$(GOVULNCHECK_VERSION)") || strings.Contains(block, "@latest") {
		t.Errorf("deps-vuln-install must install the pinned scanner version:\n%s", block)
	}
	if block := makeTargetBlock(t, makefile, "deps-check-json"); !strings.Contains(block, "\t@$(GO) list") {
		t.Errorf("deps-check-json must suppress Make command echo so stdout remains valid JSON:\n%s", block)
	}
	if block := makeTargetBlock(t, makefile, "deps-vuln"); !strings.Contains(block, `scanner_version" != "$(GOVULNCHECK_VERSION)`) {
		t.Errorf("deps-vuln must reject a scanner version other than the pinned version:\n%s", block)
	}
	for _, target := range []string{"deps-update", "deps-update-core"} {
		block := makeTargetBlock(t, makefile, target)
		for _, required := range []string{"git status --porcelain -- go.mod go.sum", "$(GO) get -u", "$(GO) mod tidy", "$(GO) mod verify"} {
			if !strings.Contains(block, required) {
				t.Errorf("%s is missing %q", target, required)
			}
		}
	}
	if block := makeTargetBlock(t, makefile, "deps-test"); !strings.Contains(block, "ci-quality") || !strings.Contains(block, "fuzz") {
		t.Errorf("deps-test must run the quality and fuzz gates:\n%s", block)
	}
}

func TestDependencyVulnerabilityTargetRefusesAnUnpinnedScanner(t *testing.T) {
	root := repositoryRoot(t)
	fakeScanner := filepath.Join(t.TempDir(), "govulncheck")
	if err := os.WriteFile(fakeScanner, []byte(`#!/bin/sh
if [ "$1" = "-version" ]; then
	echo "Scanner: govulncheck@v0.0.1"
	exit 0
fi
echo "scanner was invoked" >&2
exit 91
`), 0700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("make", "--no-print-directory", "deps-vuln", "GOVULNCHECK="+fakeScanner, "GOVULNCHECK_VERSION=v0.0.2")
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("deps-vuln accepted an unpinned scanner:\n%s", output)
	}
	text := string(output)
	if !strings.Contains(text, "version v0.0.2 is required") {
		t.Errorf("deps-vuln did not explain the required version:\n%s", text)
	}
	if strings.Contains(text, "scanner was invoked") {
		t.Errorf("deps-vuln scanned with the rejected version:\n%s", text)
	}
}

func TestDependencyCoreUpdateSetMatchesDirectModules(t *testing.T) {
	root := repositoryRoot(t)
	makefile := readTextFile(t, root+"/Makefile")
	goMod := readTextFile(t, root+"/go.mod")

	got := continuedMakeWords(t, makefile, "DEPS_CORE_MODULES")
	want := directGoModules(goMod)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DEPS_CORE_MODULES must match direct go.mod requirements\ngot:  %v\nwant: %v", got, want)
	}
}

func makeTargetBlock(t *testing.T, makefile, target string) string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	prefix := target + ":"
	for index, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		block := []string{line}
		for _, following := range lines[index+1:] {
			if strings.HasPrefix(following, "\t") {
				block = append(block, following)
				continue
			}
			if following == "" {
				break
			}
			break
		}
		return strings.Join(block, "\n")
	}
	t.Fatalf("Makefile target %s was not found", target)
	return ""
}

func continuedMakeWords(t *testing.T, makefile, name string) []string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	prefix := name + " ="
	var words []string
	collecting := false
	for _, line := range lines {
		if !collecting {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			collecting = true
			line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
		} else {
			line = strings.TrimSpace(line)
		}
		continued := strings.HasSuffix(line, "\\")
		line = strings.TrimSpace(strings.TrimSuffix(line, "\\"))
		words = append(words, strings.Fields(line)...)
		if !continued {
			return words
		}
	}
	t.Fatalf("Makefile variable %s was not found or was unterminated", name)
	return nil
}

func directGoModules(goMod string) []string {
	var modules []string
	inRequireBlock := false
	for _, line := range strings.Split(goMod, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "require (":
			inRequireBlock = true
		case inRequireBlock && trimmed == ")":
			inRequireBlock = false
		case inRequireBlock && trimmed != "" && !strings.Contains(trimmed, "// indirect"):
			fields := strings.Fields(trimmed)
			if len(fields) >= 2 {
				modules = append(modules, fields[0])
			}
		}
	}
	return modules
}
