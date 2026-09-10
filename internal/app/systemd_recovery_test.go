package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"testing"

	"github.com/fdefilippo/resman/internal/systemdunit"
)

type recoveryCaptureLogger struct {
	messages []string
	fields   []interface{}
}

func TestWithStateManagerLogsRecoveredSystemdLeasesAtBootstrap(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "app_bootstrap.go", nil, 0)
	if err != nil {
		t.Fatalf("parse app_bootstrap.go: %v", err)
	}
	var withStateManager *ast.FuncDecl
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == "WithStateManager" {
			withStateManager = function
			break
		}
	}
	if withStateManager == nil {
		t.Fatal("WithStateManager declaration not found")
	}
	found := false
	ast.Inspect(withStateManager.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		function, ok := call.Fun.(*ast.Ident)
		if !ok || function.Name != "logSystemdLeaseRecovery" {
			return true
		}
		if len(call.Args) != 2 || !isSelector(call.Args[0], "a", "logger") {
			return true
		}
		report, ok := call.Args[1].(*ast.CallExpr)
		if !ok || len(report.Args) != 0 || !isSelector(report.Fun, "systemdAdapter", "RecoveryReport") {
			return true
		}
		found = true
		return false
	})
	if !found {
		t.Fatal("WithStateManager does not log systemdAdapter.RecoveryReport() at bootstrap")
	}
}

func isSelector(expression ast.Expr, receiver, method string) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != method {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && identifier.Name == receiver
}

func (*recoveryCaptureLogger) Debug(string, ...interface{}) {}
func (l *recoveryCaptureLogger) Info(message string, fields ...interface{}) {
	l.messages = append(l.messages, message)
	l.fields = append([]interface{}(nil), fields...)
}
func (*recoveryCaptureLogger) Warn(string, ...interface{})              {}
func (*recoveryCaptureLogger) Error(string, ...interface{})             {}
func (*recoveryCaptureLogger) InfoChecked(string, ...interface{}) error { return nil }

func TestSystemdLeaseRecoveryIsLoggedOnceAsBoundedCounts(t *testing.T) {
	logger := &recoveryCaptureLogger{}
	report := []systemdunit.LeaseRecoveryOutcome{
		{Unit: "user.slice", State: systemdunit.LeaseRecoveryReclaimed},
		{Unit: "user-1000.slice", State: systemdunit.LeaseRecoveryReclaimed},
		{Unit: "user-1001.slice", State: systemdunit.LeaseRecoveryConflict},
	}
	logSystemdLeaseRecovery(logger, report)
	if !reflect.DeepEqual(logger.messages, []string{"Systemd property lease recovery completed"}) {
		t.Fatalf("recovery messages = %v", logger.messages)
	}
	want := []interface{}{
		"total_units", 3,
		"reclaimed_units", 2,
		"orphaned_units", 0,
		"inactive_cleaned_units", 0,
		"pending_units", 0,
		"conflicted_units", 1,
	}
	if !reflect.DeepEqual(logger.fields, want) {
		t.Fatalf("recovery fields = %#v, want %#v", logger.fields, want)
	}
	for _, field := range logger.fields {
		if field == "user.slice" || field == "user-1000.slice" || field == "user-1001.slice" {
			t.Fatalf("recovery log leaked an unbounded unit identity: %#v", logger.fields)
		}
	}
}

func TestEmptySystemdLeaseRecoveryDoesNotEmitNoise(t *testing.T) {
	logger := &recoveryCaptureLogger{}
	logSystemdLeaseRecovery(logger, nil)
	if len(logger.messages) != 0 {
		t.Fatalf("empty recovery emitted messages: %v", logger.messages)
	}
}
