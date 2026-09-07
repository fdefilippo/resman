package main

import (
	"go/ast"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var removedCPUPointsSurface = regexp.MustCompile(`(?i)guaranteed_domain_|best_effort_domain_|guaranteed_priority|cpu_points_lending_state|"lending_state"|CPUPointsLending`)

func checkCPUPointsSurfaces(root string, sources []goSource) checkResult {
	result := checkResult{name: "cpu-points-flat-public-contract"}
	for _, source := range productionGoFiles(sources) {
		ast.Inspect(source.file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if ok && removedCPUPointsSurface.MatchString(literal.Value) {
				result.fail(source.path, sourceLine(source, literal.Pos()), "removed CPU Points domain/lending contract")
			}
			return true
		})
	}
	checkFile := func(path string) {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			result.fail(path, 1, "read public CPU Points surface: %v", err)
			return
		}
		for index, line := range strings.Split(string(data), "\n") {
			if removedCPUPointsSurface.MatchString(line) {
				result.fail(path, index+1, "removed CPU Points domain/lending contract")
			}
		}
	}
	checkFile("README.md")
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		checkFile(rel)
		return nil
	})
	if err != nil {
		result.fail("docs", 1, "inspect public CPU Points surfaces: %v", err)
	}
	return result
}
