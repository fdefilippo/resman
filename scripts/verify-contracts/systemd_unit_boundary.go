package main

import (
	"go/ast"
	"strconv"
	"strings"
)

const systemdUnitAdapterPath = "internal/systemdunit/transport.go"

func checkSystemdUnitMutationBoundary(sources []goSource) checkResult {
	result := checkResult{name: "systemd-unit-mutation-boundary"}
	for _, source := range productionGoFiles(sources) {
		ast.Inspect(source.file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.BasicLit:
				checkSystemdOwnedCgroupLiteral(source, typed, &result)
			case *ast.CallExpr:
				checkSystemdMutationCall(source, typed, &result)
			}
			return true
		})
	}
	return result
}

func checkSystemdOwnedCgroupLiteral(source goSource, literal *ast.BasicLit, result *checkResult) {
	if strings.HasPrefix(source.path, "internal/systemdunit/") {
		return
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return
	}
	if value == "user.slice" || strings.Contains(value, "/user.slice") {
		result.fail(source.path, sourceLine(source, literal.Pos()), "systemd-owned user.slice paths must come from the authoritative adapter, not a source literal")
	}
}

func checkSystemdMutationCall(source goSource, call *ast.CallExpr, result *checkResult) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	method := selector.Sel.Name
	switch method {
	case "SetUnitPropertiesContext":
		if source.path != systemdUnitAdapterPath {
			result.fail(source.path, sourceLine(source, call.Pos()), "SetUnitPropertiesContext is restricted to the authoritative systemd adapter")
		}
	case "RevertUnit", "RevertUnitContext", "StartUnit", "StartUnitContext", "StopUnit", "StopUnitContext", "RestartUnit", "RestartUnitContext", "KillUnit", "KillUnitContext":
		result.fail(source.path, sourceLine(source, call.Pos()), "%s is outside ResMan's systemd resource-property capability", method)
	}

	if !strings.HasPrefix(source.path, "internal/systemdunit/") {
		return
	}
	switch method {
	case "WriteFile", "OpenFile", "Create", "CreateTemp", "Mkdir", "MkdirAll", "Remove", "RemoveAll", "Rename", "Truncate", "Write", "WriteString":
		result.fail(source.path, sourceLine(source, call.Pos()), "%s is forbidden in the read-only cgroup verification package", method)
	}
}
