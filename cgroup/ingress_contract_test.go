package cgroup

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var forbiddenPIDRelocationIdentifiers = map[string]bool{
	"CPUPointsHierarchy":                 true,
	"CreateSharedCgroup":                 true,
	"CreateUserCgroup":                   true,
	"EnsureCPUPointsUserPlacement":       true,
	"MoveAllUserProcesses":               true,
	"MoveAllUserProcessesToSharedCgroup": true,
	"MoveProcessToCgroup":                true,
	"ProcessMembershipResult":            true,
	"ProcessMoveResult":                  true,
	"processOrigin":                      true,
	"processOrigins":                     true,
	"restoreProcessesExpectedResult":     true,
	"writePIDToCgroup":                   true,
}

func TestProductionSourceCannotNamePIDPlacementInterface(t *testing.T) {
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "build" || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, finding := range retiredPIDRelocationFindings(file) {
			t.Errorf("%s: %s", path, finding)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRetiredPIDRelocationGateRejectsDirectAndIndirectForms(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{name: "direct interface", source: `package sample; const path = "cgroup.procs"`},
		{name: "concatenated interface", source: `package sample; const path = "cgroup" + ".procs"`},
		{name: "aliased interface", source: `package sample; const a = "cgroup"; const b = ".procs"; const path = a + b`},
		{name: "retired mode", source: `package sample; const mode = "migration_" + "enabled"`},
		{name: "retired identifier", source: `package sample; func MoveProcessToCgroup() {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), test.name+".go", test.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if findings := retiredPIDRelocationFindings(file); len(findings) == 0 {
				t.Fatal("retired PID-relocation source was accepted")
			}
		})
	}
}

func retiredPIDRelocationFindings(file *ast.File) []string {
	constants := stringConstantDefinitions(file)
	var findings []string
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.Ident:
			if forbiddenPIDRelocationIdentifiers[typed.Name] {
				findings = append(findings, "retired PID-relocation identifier "+typed.Name)
			}
		case ast.Expr:
			for _, value := range resolvedStringValues(typed, constants, map[string]bool{}, 1000) {
				if strings.Contains(value, "cgroup.procs") || strings.Contains(value, "migration_enabled") {
					findings = append(findings, "retired PID-relocation interface "+strconv.Quote(value))
				}
			}
		}
		return true
	})
	return findings
}

func stringConstantDefinitions(file *ast.File) map[string][]ast.Expr {
	definitions := make(map[string][]ast.Expr)
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, specification := range general.Specs {
			value, ok := specification.(*ast.ValueSpec)
			if !ok || len(value.Names) != len(value.Values) {
				continue
			}
			for index, name := range value.Names {
				definitions[name.Name] = append(definitions[name.Name], value.Values[index])
			}
		}
	}
	return definitions
}

func resolvedStringValues(expression ast.Expr, definitions map[string][]ast.Expr, visiting map[string]bool, budget int) []string {
	if budget <= 0 {
		return nil
	}
	switch typed := expression.(type) {
	case *ast.BasicLit:
		if typed.Kind != token.STRING {
			return nil
		}
		value, err := strconv.Unquote(typed.Value)
		if err != nil {
			return nil
		}
		return []string{value}
	case *ast.BinaryExpr:
		if typed.Op != token.ADD {
			return nil
		}
		left := resolvedStringValues(typed.X, definitions, visiting, budget-1)
		right := resolvedStringValues(typed.Y, definitions, visiting, budget-1)
		var values []string
		for _, prefix := range left {
			for _, suffix := range right {
				values = append(values, prefix+suffix)
			}
		}
		return values
	case *ast.Ident:
		if visiting[typed.Name] {
			return nil
		}
		visiting[typed.Name] = true
		defer delete(visiting, typed.Name)
		var values []string
		for _, value := range definitions[typed.Name] {
			values = append(values, resolvedStringValues(value, definitions, visiting, budget-1)...)
		}
		return values
	default:
		return nil
	}
}
