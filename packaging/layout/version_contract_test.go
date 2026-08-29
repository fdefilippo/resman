package layout

import (
	"fmt"
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
