package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type finding struct {
	check   string
	path    string
	line    int
	message string
}

type notice struct {
	kind    string
	check   string
	path    string
	line    int
	message string
}

type checkResult struct {
	name     string
	findings []finding
	notices  []notice
}

func (r *checkResult) fail(path string, line int, format string, args ...any) {
	r.findings = append(r.findings, finding{
		check:   r.name,
		path:    filepath.ToSlash(path),
		line:    line,
		message: fmt.Sprintf(format, args...),
	})
}

func (r *checkResult) known(path string, line int, format string, args ...any) {
	r.notices = append(r.notices, notice{
		kind:    "KNOWN",
		check:   r.name,
		path:    filepath.ToSlash(path),
		line:    line,
		message: fmt.Sprintf(format, args...),
	})
}

func (r *checkResult) warn(path string, line int, format string, args ...any) {
	r.notices = append(r.notices, notice{
		kind:    "WARN",
		check:   r.name,
		path:    filepath.ToSlash(path),
		line:    line,
		message: fmt.Sprintf(format, args...),
	})
}

func main() {
	root, err := repositoryRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify-contracts: %v\n", err)
		os.Exit(1)
	}

	goFiles, parseFindings := loadGoFiles(root)
	results := []checkResult{
		{name: "parse", findings: parseFindings},
		checkConfigContracts(root, goFiles),
		checkPrometheusCallSites(goFiles),
		checkProductionSleeps(root, goFiles),
		checkCrossPackageMapKeys(root, goFiles),
		checkMCPContracts(root, goFiles),
		checkShippedAssets(root, goFiles),
		checkEnglishLanguage(root, goFiles),
		checkPrometheusAssets(root),
	}

	failed := false
	for _, result := range results {
		sort.Slice(result.findings, func(i, j int) bool {
			if result.findings[i].path != result.findings[j].path {
				return result.findings[i].path < result.findings[j].path
			}
			return result.findings[i].line < result.findings[j].line
		})
		sort.Slice(result.notices, func(i, j int) bool {
			if result.notices[i].path != result.notices[j].path {
				return result.notices[i].path < result.notices[j].path
			}
			return result.notices[i].line < result.notices[j].line
		})

		for _, item := range result.notices {
			fmt.Printf("%s [%s] %s:%d: %s\n", item.kind, item.check, item.path, item.line, item.message)
		}
		for _, item := range result.findings {
			failed = true
			fmt.Fprintf(os.Stderr, "FAIL [%s] %s:%d: %s\n", item.check, item.path, item.line, item.message)
		}
		if len(result.findings) == 0 {
			fmt.Printf("PASS [%s]\n", result.name)
		}
	}

	if failed {
		os.Exit(1)
	}
}

func repositoryRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}
