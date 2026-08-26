package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
)

type goSource struct {
	path        string
	packageName string
	file        *ast.File
	fset        *token.FileSet
	test        bool
}

func loadGoFiles(root string) ([]goSource, []finding) {
	var sources []goSource
	var findings []finding
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			rel, _ := filepath.Rel(root, path)
			findings = append(findings, finding{check: "parse", path: filepath.ToSlash(rel), line: 1, message: walkErr.Error()})
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".beads", "build", "dist", "vendor":
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			findings = append(findings, finding{check: "parse", path: filepath.ToSlash(rel), line: 1, message: err.Error()})
			return nil
		}
		sources = append(sources, goSource{
			path:        filepath.ToSlash(rel),
			packageName: parsed.Name.Name,
			file:        parsed,
			fset:        fset,
			test:        strings.HasSuffix(entry.Name(), "_test.go"),
		})
		return nil
	})
	if err != nil {
		findings = append(findings, finding{check: "parse", path: ".", line: 1, message: err.Error()})
	}
	return sources, findings
}

func sourceLine(source goSource, pos token.Pos) int {
	line := source.fset.Position(pos).Line
	if line < 1 {
		return 1
	}
	return line
}

func productionGoFiles(sources []goSource) []goSource {
	result := make([]goSource, 0, len(sources))
	for _, source := range sources {
		if !source.test && !strings.HasPrefix(source.path, "scripts/verify-contracts/") {
			result = append(result, source)
		}
	}
	return result
}

func receiverName(decl *ast.FuncDecl) (string, bool) {
	if decl.Recv == nil || len(decl.Recv.List) != 1 || len(decl.Recv.List[0].Names) != 1 {
		return "", false
	}
	return decl.Recv.List[0].Names[0].Name, true
}

func functionName(decl *ast.FuncDecl) string {
	if decl.Recv == nil {
		return decl.Name.Name
	}
	return decl.Name.Name
}
