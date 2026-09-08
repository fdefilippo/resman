package cgroup

import (
	"fmt"
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
		{name: "variable-aliased interface", source: `package sample; var a = "cgroup"; var b = ".procs"; var path = a + b`},
		{name: "formatted interface", source: `package sample; import "fmt"; var path = fmt.Sprintf("%s.procs", "cgroup")`},
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
	definitions := stringDefinitions(file)
	var findings []string
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.Ident:
			if forbiddenPIDRelocationIdentifiers[typed.Name] {
				findings = append(findings, "retired PID-relocation identifier "+typed.Name)
			}
		case ast.Expr:
			for _, value := range resolvedStringValues(typed, definitions, map[string]bool{}, 1000) {
				if strings.Contains(value, "cgroup.procs") || strings.Contains(value, "migration_enabled") {
					findings = append(findings, "retired PID-relocation interface "+strconv.Quote(value))
				}
			}
		}
		return true
	})
	return findings
}

func stringDefinitions(file *ast.File) map[string][]ast.Expr {
	definitions := make(map[string][]ast.Expr)
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.ValueSpec:
			if len(typed.Names) != len(typed.Values) {
				return true
			}
			for index, name := range typed.Names {
				definitions[name.Name] = append(definitions[name.Name], typed.Values[index])
			}
		case *ast.AssignStmt:
			if len(typed.Lhs) != len(typed.Rhs) {
				return true
			}
			for index, left := range typed.Lhs {
				name, ok := left.(*ast.Ident)
				if ok {
					definitions[name.Name] = append(definitions[name.Name], typed.Rhs[index])
				}
			}
		}
		return true
	})
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
	case *ast.CallExpr:
		selector, ok := typed.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Sprintf" || len(typed.Args) == 0 {
			return nil
		}
		packageName, ok := selector.X.(*ast.Ident)
		if !ok || packageName.Name != "fmt" {
			return nil
		}
		formats := resolvedStringValues(typed.Args[0], definitions, visiting, budget-1)
		argumentValues := make([][]string, 0, len(typed.Args)-1)
		for _, argument := range typed.Args[1:] {
			values := resolvedStringValues(argument, definitions, visiting, budget-1)
			if len(values) == 0 {
				return nil
			}
			argumentValues = append(argumentValues, values)
		}
		var values []string
		for _, format := range formats {
			values = append(values, resolvedFormattedStrings(format, argumentValues, nil, budget-1)...)
		}
		return values
	default:
		return nil
	}
}

func resolvedFormattedStrings(format string, remaining [][]string, arguments []any, budget int) []string {
	if budget <= 0 {
		return nil
	}
	if len(remaining) == 0 {
		return []string{fmt.Sprintf(format, arguments...)}
	}
	var values []string
	for _, value := range remaining[0] {
		values = append(values, resolvedFormattedStrings(format, remaining[1:], append(arguments, value), budget-1)...)
	}
	return values
}
