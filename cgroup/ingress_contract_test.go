package cgroup

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestManagedCgroupIngressWritesHaveOneNamespaceGuardedBoundary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cgroup package: %v", err)
	}

	allowed := map[string]string{
		"moveProcessBatchExpected": "guarded ingress",
		"restoreProcessesExpected": "restore and recovery bypass ingress guard",
	}
	found := make(map[string]int)
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, entry.Name(), nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", entry.Name(), parseErr)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "writePIDToCgroup" {
					return true
				}
				found[function.Name.Name]++
				if _, ok := allowed[function.Name.Name]; !ok {
					position := fset.Position(call.Pos())
					t.Errorf("%s writes cgroup.procs outside the guarded ingress or restore boundaries", position)
				}
				return true
			})
		}
	}

	for function, reason := range allowed {
		if found[function] == 0 {
			t.Errorf("expected %s write boundary %s was not found", reason, function)
		}
	}
	if found["moveProcessBatchExpected"] != 1 {
		t.Errorf("guarded ingress has %d raw writes, want exactly one", found["moveProcessBatchExpected"])
	}

	var callers []string
	for function := range found {
		callers = append(callers, function)
	}
	sort.Strings(callers)
	if len(callers) != len(allowed) {
		t.Fatalf("raw cgroup.procs write boundaries = %v, want exactly guarded ingress and restore", callers)
	}
}
