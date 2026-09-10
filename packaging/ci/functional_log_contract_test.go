package ci

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

func TestFunctionalLogExpectationsUseProductionCompletionVocabulary(t *testing.T) {
	root := repositoryRoot(t)
	completionKeys := productionCompletionLogKeys(t, root)
	for _, required := range []string{
		"requested_policy_intent",
		"applied_enforcement_action",
		"enforcement_block_reason",
		"decision_reason",
	} {
		if !completionKeys[required] {
			t.Fatalf("production completion log does not emit %q", required)
		}
	}

	expectations := map[string][]string{
		"test/functional/smolvm/guest/non-systemd-observation.py": {
			"requested_policy_intent=activate",
		},
		"test/functional/smolvm/guest/run-functional.sh": {
			"requested_policy_intent=activate.*system_under_load=true.*ignore_system_load=false",
		},
		"test/functional/real-kernel/run.sh": {
			"requested_policy_intent=activate.*decision_reason=.*(read_iops|write_iops)",
			"requested_policy_intent=activate.*decision_reason=.*${decision_name}",
		},
		"test/functional/real-kernel/native_gate.py": {
			`require("requested_policy_intent=activate" not in text,`,
		},
	}
	for path, required := range expectations {
		content := readFile(t, root+"/"+path)
		for _, token := range required {
			assertContains(t, content, token)
		}
		assertNotContains(t, content, "decision=ACTIVATE_LIMITS")
	}
}

func productionCompletionLogKeys(t *testing.T, root string) map[string]bool {
	t.Helper()
	path := root + "/state/control_cycle.go"
	files, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	keys := make(map[string]bool)
	found := false
	ast.Inspect(files, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 3 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "InfoChecked" {
			return true
		}
		message, ok := call.Args[0].(*ast.BasicLit)
		if !ok || message.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(message.Value)
		if err != nil || value != "Control cycle completed" {
			return true
		}
		found = true
		for index := 1; index < len(call.Args); index += 2 {
			literal, ok := call.Args[index].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Fatalf("completion log key at argument %d is not a string literal", index)
			}
			key, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatalf("decode completion log key %s: %v", literal.Value, err)
			}
			keys[key] = true
		}
		return true
	})
	if !found {
		t.Fatal("production completion log call not found")
	}
	return keys
}
