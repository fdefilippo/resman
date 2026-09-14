package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

const ioRestoreFixture = `package state
import systemdunit "github.com/fdefilippo/resman/internal/systemdunit"
var ioSystemdProperties = []systemdunit.PropertyName{
 systemdunit.PropertyIOWeight,
 systemdunit.PropertyIODeviceWeight,
 systemdunit.PropertyIOReadBandwidthMax,
 systemdunit.PropertyIOWriteBandwidthMax,
 systemdunit.PropertyIOReadIOPSMax,
 systemdunit.PropertyIOWriteIOPSMax,
}
func (m *Manager) restoreSystemdResource(ctx context.Context, identity systemdunit.UnitIdentity, resource systemdunit.ResourceKind) {
 properties := memorySystemdProperties
 if resource == systemdunit.ResourceIO { properties = ioSystemdProperties }
 m.systemdUnits.RestoreProperties(ctx, identity, properties)
}
`

func TestSystemdIOWeightCannotBecomeProductionPolicyThroughAliases(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"direct property", `func apply(){ systemdunit.NewPropertyAssignment(systemdunit.PropertyIOWeight, 100) }`},
		{"raw property literal", `func apply(){ systemdunit.NewPropertyAssignment("IOWeight", 100) }`},
		{"typed property literal", `func apply(){ systemdunit.NewPropertyAssignment(systemdunit.PropertyName("IOWeight"), 100) }`},
		{"import alias", `func apply(){ su.NewPropertyAssignment(su.PropertyIOWeight, 100) }`},
		{"constant alias", `const property = systemdunit.PropertyIOWeight; func apply(){ systemdunit.NewPropertyAssignment(property, 100) }`},
		{"constructor alias", `var build = systemdunit.NewPropertyAssignment; func apply(){ build(systemdunit.PropertyIOWeight, 100) }`},
		{"property table", `var names = []systemdunit.PropertyName{systemdunit.PropertyIOWeight}; func apply(){ systemdunit.NewPropertyAssignment(names[0], 100) }`},
		{"dot imported property", `func apply(){ NewPropertyAssignment(PropertyIOWeight, 100) }`},
		{"constant concatenation", `func apply(){ systemdunit.NewPropertyAssignment(systemdunit.PropertyName("IO" + "Weight"), 100) }`},
		{"concatenated aliases", `const prefix = "IO"; const suffix = "Weight"; const property = prefix + suffix; func apply(){ systemdunit.NewPropertyAssignment(property, 100) }`},
		{"inventory selected indirectly", `func apply(){ name := ioSystemdProperties[0]; systemdunit.NewPropertyAssignment(name, 100) }`},
		{"device-weight constructor", `func apply(){ systemdunit.NewIODeviceWeightAssignment(nil) }`},
		{"device-weight request", `var request = systemdunit.IODeviceWeightRequest{}`},
		{"startup requirement field", `func apply(requirements *systemdunit.StartupRequirements){ requirements.IODeviceWeights = nil }`},
		{"device-weight property", `func apply(){ systemdunit.NewDevicePropertyAssignment(systemdunit.PropertyIODeviceWeight, nil) }`},
		{"raw device-weight property", `func apply(){ systemdunit.NewDevicePropertyAssignment("IODeviceWeight", nil) }`},
		{"concatenated device-weight property", `const middle = "Device"; func apply(){ systemdunit.NewDevicePropertyAssignment(systemdunit.PropertyName("IO" + middle + "Weight"), nil) }`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := newCheckerFixture(t)
			writeFixture(t, root, systemdResourcePolicyPath, ioRestoreFixture)
			writeFixture(t, root, "state/new_policy.go", "package state\nimport su \"github.com/fdefilippo/resman/internal/systemdunit\"\n"+test.content)
			result := inspectSystemdBoundaryFixture(t, root)
			if len(result.findings) == 0 {
				t.Fatal("weighted-I/O production policy escaped the mechanical boundary")
			}
			for _, finding := range result.findings {
				if finding.path != "state/new_policy.go" || finding.line < 3 {
					t.Fatalf("finding did not identify the new policy: %+v", finding)
				}
			}
		})
	}
}

func TestSystemdIOWeightExceptionIsExactAndRestoreOnly(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"inventory removed", strings.Replace(ioRestoreFixture, "systemdunit.PropertyIOWeight,", "", 1)},
		{"device inventory removed", strings.Replace(ioRestoreFixture, "systemdunit.PropertyIODeviceWeight,", "", 1)},
		{"inventory duplicated", ioRestoreFixture + "\nvar ioSystemdProperties = []systemdunit.PropertyName{systemdunit.PropertyIOWeight}\n"},
		{"inventory renamed", strings.ReplaceAll(ioRestoreFixture, "ioSystemdProperties", "newInventory")},
		{"exception consumed by apply", strings.Replace(ioRestoreFixture, "RestoreProperties", "Apply", 1)},
		{"exception copied outside restore", ioRestoreFixture + "\nvar copied = ioSystemdProperties\n"},
		{"inventory indexed inside restore", strings.Replace(ioRestoreFixture, "m.systemdUnits.RestoreProperties(ctx, identity, properties)", "m.systemdUnits.RestoreProperties(ctx, identity, properties); systemdunit.NewPropertyAssignment(properties[0], 100)", 1)},
		{"local alias inside restore", strings.Replace(ioRestoreFixture, "m.systemdUnits.RestoreProperties(ctx, identity, properties)", "m.systemdUnits.RestoreProperties(ctx, identity, properties); names := properties; systemdunit.NewPropertyAssignment(names[0], 100)", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := newCheckerFixture(t)
			writeFixture(t, root, systemdResourcePolicyPath, test.content)
			if result := inspectSystemdBoundaryFixture(t, root); len(result.findings) == 0 {
				t.Fatal("changed restoration exception was not rejected")
			}
		})
	}
}

func TestSystemdIOWeightAllowsAdapterTestsAndExactRestoreInventory(t *testing.T) {
	root := newCheckerFixture(t)
	writeFixture(t, root, systemdResourcePolicyPath, ioRestoreFixture)
	writeFixture(t, root, "internal/systemdunit/io.go", `package systemdunit; const PropertyIOWeight = "IOWeight"; func assign(){ NewPropertyAssignment(PropertyIOWeight, 100) }`)
	writeFixture(t, root, "state/policy_test.go", `package state; func fixture(){ systemdunit.NewPropertyAssignment(systemdunit.PropertyIOWeight, 100) }`)
	writeFixture(t, root, "state/other_resource.go", `package state; func apply(){ systemdunit.NewPropertyAssignment(systemdunit.PropertyMemoryHigh, 4096) }`)
	if result := inspectSystemdBoundaryFixture(t, root); len(result.findings) != 0 {
		t.Fatalf("legitimate adapter, restoration or other-resource path refused: %v", result.findings)
	}
}

func TestSystemdIOWeightRejectsConcatenationThroughSeparateSourceConstants(t *testing.T) {
	root := newCheckerFixture(t)
	writeFixture(t, root, "state/constants.go", `package state; const prefix = "IO"; const suffix = "Weight"`)
	writeFixture(t, root, "state/apply.go", `package state; func apply(){ systemdunit.NewPropertyAssignment(systemdunit.PropertyName(prefix + suffix), 100) }`)
	result := inspectSystemdBoundaryFixture(t, root)
	if len(result.findings) != 1 || result.findings[0].path != "state/apply.go" {
		t.Fatalf("cross-file materialization escaped or was not located: %v", result.findings)
	}
}

func TestSystemdIOWeightConstantExpansionDeduplicatesAndMemoizesEmptyAliases(t *testing.T) {
	definitions := map[string][]ast.Expr{}
	for index := 0; index < 6; index++ {
		definitions["empty"] = append(definitions["empty"], &ast.BasicLit{Kind: token.STRING, Value: `""`})
	}
	expression, err := parser.ParseExpr(strings.Repeat("empty + ", 7) + `"IO" + "Weight"`)
	if err != nil {
		t.Fatal(err)
	}
	expansion := systemdStringExpansion{remaining: 1000, visiting: map[string]bool{}, memo: map[ast.Expr][]string{}}
	values := systemdStringValues(expression, definitions, &expansion)
	if len(values) != 1 || values[0] != "IOWeight" || expansion.exhausted || expansion.remaining < 970 {
		t.Fatalf("duplicate aliases expanded instead of sharing bounded results: values=%d budget=%d exhausted=%v", len(values), expansion.remaining, expansion.exhausted)
	}
}

func TestSystemdIOWeightConstantInspectionFailsClosedAtItsBudget(t *testing.T) {
	root := newCheckerFixture(t)
	writeFixture(t, root, "state/apply.go", "package state; var property = "+strings.Repeat(`"" + `, 1100)+`"Weight"`)
	result := inspectSystemdBoundaryFixture(t, root)
	found := false
	for _, finding := range result.findings {
		found = found || strings.Contains(finding.message, "bounded expansion")
	}
	if !found {
		t.Fatal("exhausted constant inspection was silently treated as safe")
	}
}

func inspectSystemdBoundaryFixture(t *testing.T, root string) checkResult {
	t.Helper()
	sources, findings := loadGoFiles(root)
	if len(findings) != 0 {
		t.Fatalf("fixture did not parse: %v", findings)
	}
	return checkSystemdUnitMutationBoundary(sources)
}
