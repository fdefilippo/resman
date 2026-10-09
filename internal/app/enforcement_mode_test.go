package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// enforcementInitialization returns the declaration that resolves the
// enforcement boundary at startup.
func enforcementInitialization(t *testing.T) *ast.FuncDecl {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "app_bootstrap.go", nil, 0)
	if err != nil {
		t.Fatalf("parse app_bootstrap.go: %v", err)
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == "initializeSystemdEnforcement" {
			return function
		}
	}
	t.Fatal("initializeSystemdEnforcement declaration not found")
	return nil
}

// callsInside names every package-qualified call reachable from one node.
func callsInside(node ast.Node) []string {
	var calls []string
	ast.Inspect(node, func(current ast.Node) bool {
		call, ok := current.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
			if receiver, ok := selector.X.(*ast.Ident); ok {
				calls = append(calls, receiver.Name+"."+selector.Sel.Name)
			}
		}
		return true
	})
	return calls
}

// declaredObservationBranch returns the branch taken when the operator declared
// observation, identified by its guard rather than by its position.
func declaredObservationBranch(t *testing.T, function *ast.FuncDecl) ast.Node {
	t.Helper()
	var branch ast.Node
	ast.Inspect(function.Body, func(node ast.Node) bool {
		statement, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		binary, ok := statement.Cond.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		if !strings.Contains(exprText(binary.Y), "EnforcementPolicyObservationOnly") {
			return true
		}
		branch = statement.Body
		return false
	})
	if branch == nil {
		t.Fatal("initializeSystemdEnforcement has no branch for a declared observation-only host")
	}
	return branch
}

func exprText(expression ast.Expr) string {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok {
		identifier, ok := expression.(*ast.Ident)
		if !ok {
			return ""
		}
		return identifier.Name
	}
	return exprText(selector.X) + "." + selector.Sel.Name
}

func TestDeclaredObservationNeverOpensTheEnforcementAdapter(t *testing.T) {
	// A host declared observation-only must run no capability probe, create no
	// transient unit and install no mutating adapter, so the declared branch
	// never constructs one.
	branch := declaredObservationBranch(t, enforcementInitialization(t))
	for _, call := range callsInside(branch) {
		switch call {
		case "systemdunit.New", "systemdunit.NewOrObserve", "state.WithSystemdCPUEnforcement",
			"systemdunit.ProbeIODeviceWeights":
			t.Fatalf("the declared observation-only branch calls %s", call)
		}
	}
}

func TestDeclaredObservationReleasesPropertiesOwnedByAnEarlierRun(t *testing.T) {
	// Limits applied before the declaration must not stay applied with no
	// owner, so the declared branch gives up ownership before observing.
	branch := declaredObservationBranch(t, enforcementInitialization(t))
	found := false
	ast.Inspect(branch, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "releaseBeforeDeclaredObservation" {
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Fatal("the declared observation-only branch does not release owned properties")
	}
}

func TestOnlyTheAutoDeclarationMayGiveUpEnforcement(t *testing.T) {
	// NewOrObserve is the single constructor that can answer with an
	// observation refusal instead of an error. It must be reachable only when
	// the operator declared auto, so a default host keeps failing closed.
	function := enforcementInitialization(t)
	var guarded, total int
	ast.Inspect(function.Body, func(node ast.Node) bool {
		statement, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		binary, ok := statement.Cond.(*ast.BinaryExpr)
		if !ok || !strings.Contains(exprText(binary.Y), "EnforcementPolicyAuto") {
			return true
		}
		for _, call := range callsInside(statement.Body) {
			if call == "systemdunit.NewOrObserve" {
				guarded++
			}
		}
		return true
	})
	for _, call := range callsInside(function.Body) {
		if call == "systemdunit.NewOrObserve" {
			total++
		}
	}
	if total != 1 || guarded != 1 {
		t.Fatalf("NewOrObserve calls: total=%d guarded by the auto declaration=%d, want 1 and 1", total, guarded)
	}
}

func TestADefinitiveRefusalIsPublishedWithItsOwnReason(t *testing.T) {
	function := enforcementInitialization(t)
	body := exprSource(t, function)
	for _, required := range []string{
		"EnforcementReasonMandatoryCapabilityUnavailable",
		"EnforcementReasonOperatorObservationOnly",
		"refusal.Capability.Feature",
		"refusal.Capability.Controller",
		"refusal.Capability.InterfaceName",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("initializeSystemdEnforcement does not publish %s", required)
		}
	}
}

func exprSource(t *testing.T, function *ast.FuncDecl) string {
	t.Helper()
	var builder strings.Builder
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.SelectorExpr:
			builder.WriteString(exprText(typed) + "\n")
		case *ast.Ident:
			builder.WriteString(typed.Name + "\n")
		}
		return true
	})
	return builder.String()
}
