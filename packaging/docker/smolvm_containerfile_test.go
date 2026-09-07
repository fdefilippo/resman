package docker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSmolVMContainerCopySourcesExistInRepositoryContext(t *testing.T) {
	content, err := os.ReadFile("../../test/functional/smolvm/Containerfile")
	if err != nil {
		t.Fatalf("read SmolVM Containerfile: %v", err)
	}
	for _, tc := range []struct {
		name string
		old  string
		new  string
	}{
		{name: "current sources"},
		{name: "stale example path", old: "COPY config/resman.conf.example ", new: "COPY resman.conf.example "},
		{name: "missing continued source", old: "test/functional/smolvm/guest/evidence-metadata.py", new: "test/functional/smolvm/guest/missing-metadata.py"},
		{name: "missing builder input", old: "COPY state ./state", new: "COPY missing-state ./state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := string(content)
			if tc.old != "" {
				if strings.Count(candidate, tc.old) != 1 {
					t.Fatalf("mutation must replace exactly one source: %q", tc.old)
				}
				candidate = strings.Replace(candidate, tc.old, tc.new, 1)
			}
			err := validateSmolVMCopySources("../..", candidate)
			if tc.old == "" && err != nil {
				t.Fatalf("SmolVM build context: %v", err)
			}
			if tc.old != "" && err == nil {
				t.Fatal("missing COPY source was accepted")
			}
		})
	}
}

// validateSmolVMCopySources checks the fixture's plain, literal COPY syntax.
// Stage copies are not repository paths; unsupported syntax requires an explicit
// checker update rather than silently escaping the source-existence contract.
func validateSmolVMCopySources(root, content string) error {
	content = strings.ReplaceAll(content, "\\\n", " ")
	count := 0
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.EqualFold(fields[0], "COPY") {
			continue
		}
		if len(fields) >= 2 && strings.HasPrefix(fields[1], "--from=") {
			continue
		}
		if len(fields) < 3 {
			return fmt.Errorf("incomplete COPY instruction: %q", line)
		}
		for _, source := range fields[1 : len(fields)-1] {
			if strings.HasPrefix(source, "--") || strings.ContainsAny(source, "\"'[]*$?") || !filepath.IsLocal(source) {
				return fmt.Errorf("unsupported COPY source syntax: %q", source)
			}
			if _, err := os.Stat(filepath.Join(root, source)); err != nil {
				return fmt.Errorf("COPY source %q: %w", source, err)
			}
			count++
		}
	}
	if count == 0 {
		return fmt.Errorf("no repository COPY sources checked")
	}
	return nil
}
