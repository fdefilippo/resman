package main

import (
	"go/ast"
	"strconv"
	"strings"
)

const systemdUnitAdapterPath = "internal/systemdunit/transport.go"
const systemdUnitErrorsPath = "internal/systemdunit/errors.go"

func checkSystemdUnitMutationBoundary(sources []goSource) checkResult {
	result := checkResult{name: "systemd-unit-mutation-boundary"}
	for _, source := range productionGoFiles(sources) {
		ast.Inspect(source.file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.BasicLit:
				checkSystemdOwnedCgroupLiteral(source, typed, &result)
			case *ast.CallExpr:
				checkSystemdMutationCall(source, typed, &result)
			case *ast.SelectorExpr:
				checkSystemdControlGroupCapability(source, typed, &result)
			}
			return true
		})
	}
	return result
}

func checkSystemdOwnedCgroupLiteral(source goSource, literal *ast.BasicLit, result *checkResult) {
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return
	}
	if !strings.HasPrefix(source.path, "internal/systemdunit/") && (value == "user.slice" || strings.Contains(value, "/user.slice")) {
		result.fail(source.path, sourceLine(source, literal.Pos()), "systemd-owned user.slice paths must come from the authoritative adapter, not a source literal")
	}
	if strings.HasPrefix(value, "org.freedesktop.systemd1.Manager.") &&
		(source.path != systemdUnitAdapterPath || value != "org.freedesktop.systemd1.Manager.RevertUnitFiles") {
		result.fail(source.path, sourceLine(source, literal.Pos()), "raw systemd manager D-Bus members are restricted to the authoritative adapter's guarded cleanup")
	}
	if value == "github.com/coreos/go-systemd/v22/dbus" && source.path != systemdUnitAdapterPath {
		result.fail(source.path, sourceLine(source, literal.Pos()), "the go-systemd client is restricted to the authoritative systemd transport")
	}
	if value == "github.com/godbus/dbus/v5" && source.path != systemdUnitAdapterPath && source.path != systemdUnitErrorsPath {
		result.fail(source.path, sourceLine(source, literal.Pos()), "the raw D-Bus client is restricted to the authoritative systemd transport and typed error classifier")
	}
}

func checkSystemdControlGroupCapability(source goSource, selector *ast.SelectorExpr, result *checkResult) {
	if selector.Sel.Name == "ControlGroup" && source.path != "internal/systemdunit/kernel.go" {
		result.fail(source.path, sourceLine(source, selector.Pos()), "the authoritative systemd control-group path is restricted to read-only kernel verification")
	}
}

func checkSystemdMutationCall(source goSource, call *ast.CallExpr, result *checkResult) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	method := selector.Sel.Name
	switch method {
	case "SetUnitProperties", "SetUnitPropertiesContext":
		if source.path != systemdUnitAdapterPath {
			result.fail(source.path, sourceLine(source, call.Pos()), "%s is restricted to the authoritative systemd adapter", method)
		}
	case "RevertUnit", "RevertUnitContext", "RevertUnitFiles", "RevertUnitFilesContext",
		"StartUnit", "StartUnitContext", "StopUnit", "StopUnitContext", "RestartUnit", "RestartUnitContext",
		"TryRestartUnit", "TryRestartUnitContext", "ReloadUnit", "ReloadUnitContext",
		"ReloadOrRestartUnit", "ReloadOrRestartUnitContext", "ReloadOrTryRestartUnit", "ReloadOrTryRestartUnitContext",
		"StartTransientUnit", "StartTransientUnitContext", "StartTransientUnitAux",
		"KillUnit", "KillUnitContext", "ResetFailedUnit", "ResetFailedUnitContext",
		"FreezeUnit", "ThawUnit", "AttachProcessesToUnit", "AttachProcessesToUnitContext", "EnqueueUnitJob", "EnqueueUnitJobContext",
		"LinkUnitFiles", "EnableUnitFiles", "DisableUnitFiles", "MaskUnitFiles", "UnmaskUnitFiles", "PresetUnitFiles", "PresetUnitFilesWithMode":
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
