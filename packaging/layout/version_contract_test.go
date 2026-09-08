package layout

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentReleaseVersionSurfacesAgree(t *testing.T) {
	root := repositoryRoot(t)
	makefile := readTextFile(t, filepath.Join(root, "Makefile"))
	version := makeVariable(t, makefile, "VERSION")
	release := makeVariable(t, makefile, "RELEASE")
	packageVersion := version + "-" + release

	tests := []struct {
		path     string
		required string
	}{
		{path: "main.go", required: fmt.Sprintf(`var version = %q`, version)},
		{path: "packaging/rpm/resman.spec", required: "Version: " + version},
		{path: "packaging/rpm/resman.spec", required: "- " + packageVersion + "\n"},
		{path: "packaging/deb/changelog", required: "resman (" + packageVersion + ")"},
		{path: "docs/resman.8", required: `"v` + version + `" "System Administration"`},
		{path: "README.md", required: "resman_" + packageVersion + "_<architecture>.deb"},
		{path: "docs/CONTAINER.md", required: "resman:" + version},
		{path: "docs/TECHNICAL-SPECIFICATION.md", required: "main.version=" + packageVersion},
		{path: "docs/UPGRADING.md", required: "to ResMan " + version},
		{path: "CONTRIBUTING.md", required: "git tag -a v" + version + ` -m "Release ` + version + `"`},
	}

	for _, tt := range tests {
		t.Run(tt.path+"/"+tt.required, func(t *testing.T) {
			content := readTextFile(t, filepath.Join(root, tt.path))
			if !strings.Contains(content, tt.required) {
				t.Errorf("%s does not expose current release %s via %q", tt.path, packageVersion, tt.required)
			}
		})
	}

	debianChangelog := readTextFile(t, filepath.Join(root, "packaging/deb/changelog"))
	if !strings.HasPrefix(debianChangelog, "resman ("+packageVersion+")") {
		t.Errorf("Debian changelog does not start with current release %s", packageVersion)
	}

	rpmSpec := readTextFile(t, filepath.Join(root, "packaging/rpm/resman.spec"))
	_, changelog, found := strings.Cut(rpmSpec, "%changelog\n")
	if !found || !strings.HasSuffix(strings.SplitN(changelog, "\n", 2)[0], " - "+packageVersion) {
		t.Errorf("RPM changelog does not start with current release %s", packageVersion)
	}
}

func TestVersionProgressionContractIsIdenticalAcrossAvailableNormativeGuides(t *testing.T) {
	const (
		begin = "<!-- BEGIN VERSION PROGRESSION v:1 -->"
		end   = "<!-- END VERSION PROGRESSION v:1 -->"
	)

	root := repositoryRoot(t)
	paths := []string{"CONTRIBUTING.md", "docs/DEVELOPMENT.md"}
	agentsPath := filepath.Join(root, "AGENTS.md")
	if _, err := os.Stat(agentsPath); err == nil {
		paths = append([]string{"AGENTS.md"}, paths...)
	} else if !os.IsNotExist(err) {
		t.Fatalf("inspect optional AGENTS.md: %v", err)
	}
	contracts := make(map[string]string, len(paths))
	for _, path := range paths {
		content := readTextFile(t, filepath.Join(root, path))
		if strings.Count(content, begin) != 1 || strings.Count(content, end) != 1 {
			t.Fatalf("%s must contain exactly one version-progression contract", path)
		}
		_, after, _ := strings.Cut(content, begin)
		contract, _, found := strings.Cut(after, end)
		if !found {
			t.Fatalf("%s has an unterminated version-progression contract", path)
		}
		contracts[path] = strings.TrimSpace(contract)
	}

	want := contracts[paths[0]]
	for _, path := range paths[1:] {
		if contracts[path] != want {
			t.Errorf("%s version progression differs from %s", path, paths[0])
		}
	}
	normalized := strings.Join(strings.Fields(want), " ")
	for _, required := range []string{
		"`1.34.0-1` becomes `1.35.0-1`",
		"`1.34.0-1` becomes `1.34.1-1`",
		"`1.34.0-1` becomes `1.34.0-2`",
		"A branch name alone never determines the version.",
	} {
		if !strings.Contains(normalized, required) {
			t.Errorf("version-progression contract is missing %q", required)
		}
	}
}

func TestCPUPointsCutoverDoesNotReuseLegacyCPUModelVersion(t *testing.T) {
	const lastLegacyCPUModelVersion = "1.30.8"
	const cpuPointsCutoverVersion = "1.31.1"

	root := repositoryRoot(t)
	if cpuPointsCutoverVersion == lastLegacyCPUModelVersion {
		t.Fatalf("CPU Points cutover reuses released legacy CPU model version %s", cpuPointsCutoverVersion)
	}

	guide := readTextFile(t, filepath.Join(root, "docs/UPGRADING.md"))
	required := "through " + lastLegacyCPUModelVersion + " to ResMan " + cpuPointsCutoverVersion
	if !strings.Contains(guide, required) {
		t.Errorf("upgrade guide does not connect the last legacy CPU model to the CPU Points release via %q", required)
	}
}

func makeVariable(t *testing.T, makefile, name string) string {
	t.Helper()
	prefix := name + " = "
	for _, line := range strings.Split(makefile, "\n") {
		if strings.HasPrefix(line, prefix) {
			value := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			if value == "" {
				t.Fatalf("Makefile variable %s is empty", name)
			}
			return value
		}
	}
	t.Fatalf("Makefile variable %s was not found", name)
	return ""
}
