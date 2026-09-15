package main

import (
	"go/ast"
	"go/token"
	"path"
	"strconv"
	"strings"
)

const systemdUnitAdapterPath = "internal/systemdunit/transport.go"
const systemdUnitErrorsPath = "internal/systemdunit/errors.go"
const systemdUnitJournalPath = "internal/systemdunit/journal.go"
const systemdUnitFilesPath = "internal/systemdunit/unit_files.go"
const systemdUnitCoveragePath = "internal/systemdunit/coverage.go"
const systemdIODeviceWeightCleanupPath = "internal/systemdunit/io_device_weight_cleanup.go"
const systemdResourcePolicyPath = "state/systemd_resources.go"
const systemdIOWeightPolicyPath = "state/systemd_io_weights.go"

func checkSystemdUnitMutationBoundary(sources []goSource) checkResult {
	result := checkResult{name: "systemd-unit-mutation-boundary"}
	production := productionGoFiles(sources)
	packages := map[string]map[string][]ast.Expr{}
	for _, source := range production {
		key := path.Dir(source.path)
		if packages[key] == nil {
			packages[key] = map[string][]ast.Expr{}
		}
		for name, expressions := range systemdStringDefinitions(source.file) {
			packages[key][name] = append(packages[key][name], expressions...)
		}
	}
	for _, source := range production {
		ioRestore := systemdIORestoreException(source, &result)
		for node := range systemdIODeviceWeightPolicyExceptions(source) {
			ioRestore[node] = true
		}
		capabilityProbes := systemdCapabilityProbeExceptions(source)
		kernelResets := systemdIODeviceWeightKernelResetExceptions(source, &result)
		constants := packages[path.Dir(source.path)]
		ast.Inspect(source.file, func(node ast.Node) bool {
			checkSystemdIOWeightPolicy(source, node, ioRestore, constants, &result)
			switch typed := node.(type) {
			case *ast.BasicLit:
				checkSystemdOwnedCgroupLiteral(source, typed, &result)
			case *ast.CallExpr:
				checkIODeviceWeightClassifierReadOnly(source, typed, &result)
				checkSystemdMutationCall(source, typed, capabilityProbes, kernelResets, &result)
			case *ast.SelectorExpr:
				checkSystemdControlGroupCapability(source, typed, &result)
			}
			return true
		})
	}
	return result
}

// systemdIODeviceWeightKernelResetExceptions admits exactly one raw write in
// the adapter: file.Write(payload) inside writeIODeviceWeightReset, with the
// literal kernel removal token " default\n". The production function performs
// the remaining typed tuple, inode, mechanism and compare-before-restore checks.
func systemdIODeviceWeightKernelResetExceptions(source goSource, result *checkResult) map[ast.Node]bool {
	allowed := map[ast.Node]bool{}
	if source.path != systemdIODeviceWeightCleanupPath {
		return allowed
	}
	functions := 0
	for _, declaration := range source.file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "writeIODeviceWeightReset" || function.Body == nil {
			continue
		}
		functions++
		writes, tokens := 0, 0
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.BasicLit:
				if value, err := strconv.Unquote(typed.Value); err == nil && value == " default\n" {
					tokens++
				}
			case *ast.CallExpr:
				selector, ok := typed.Fun.(*ast.SelectorExpr)
				owner, ownerOK := selectorOwner(selector)
				if ok && ownerOK && owner == "file" && selector.Sel.Name == "Write" && len(typed.Args) == 1 && systemdNamedIdentifier(typed.Args[0], "payload") {
					writes++
					allowed[typed] = true
				}
			}
			return true
		})
		if writes != 1 || tokens != 1 {
			result.fail(source.path, sourceLine(source, function.Pos()), "IODeviceWeight kernel cleanup must contain one exact MAJ:MIN default write")
			for node := range allowed {
				delete(allowed, node)
			}
		}
	}
	if functions != 1 {
		result.fail(source.path, 1, "IODeviceWeight kernel cleanup must define one exact writeIODeviceWeightReset function")
	}
	return allowed
}

func checkIODeviceWeightClassifierReadOnly(source goSource, call *ast.CallExpr, result *checkResult) {
	if source.path != "internal/systemdunit/io_device_weight_classifier.go" && source.path != "internal/systemdunit/io_device_weight_devices.go" {
		return
	}
	for owner, members := range map[string]map[string]bool{
		"os": {
			"Chmod": true, "Chown": true, "Chtimes": true, "Create": true, "CreateTemp": true,
			"Lchown": true, "Link": true, "Mkdir": true, "MkdirAll": true, "OpenFile": true,
			"Remove": true, "RemoveAll": true, "Rename": true, "Symlink": true, "Truncate": true, "WriteFile": true,
		},
		"syscall": {"Mount": true, "Setxattr": true, "Unmount": true, "Write": true},
		"unix":    {"Mount": true, "Setxattr": true, "Unmount": true, "Write": true},
	} {
		selector, ok := call.Fun.(*ast.SelectorExpr)
		identifier, ownerOK := selectorOwner(selector)
		if ok && ownerOK && identifier == owner && members[selector.Sel.Name] {
			result.fail(source.path, sourceLine(source, call.Pos()), "weighted-I/O capability classification must remain read-only")
			return
		}
	}
	if systemdNamedSelector(call.Fun, "exec", "Command") || systemdNamedSelector(call.Fun, "exec", "CommandContext") {
		result.fail(source.path, sourceLine(source, call.Pos()), "weighted-I/O capability classification must not execute commands")
	}
}

func selectorOwner(selector *ast.SelectorExpr) (string, bool) {
	if selector == nil {
		return "", false
	}
	identifier, ok := selector.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return identifier.Name, true
}

// systemdCapabilityProbeExceptions permits exactly one start and stop call in
// the transport methods that create and remove an empty startup probe slice.
// All other unit-lifecycle calls remain outside ResMan's capability boundary.
func systemdCapabilityProbeExceptions(source goSource) map[ast.Node]bool {
	allowed := map[ast.Node]bool{}
	if source.path != systemdUnitAdapterPath {
		return allowed
	}
	want := map[string]string{
		"startCapabilityProbe": "StartTransientUnitAux",
		"stopCapabilityProbe":  "StopUnitContext",
	}
	for _, declaration := range source.file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || !systemdDBusTransportMethod(function) {
			continue
		}
		method, ok := want[function.Name.Name]
		if !ok || function.Body == nil {
			continue
		}
		var matches []ast.Node
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if ok && selector.Sel.Name == method {
				matches = append(matches, call)
			}
			return true
		})
		if len(matches) == 1 {
			allowed[matches[0]] = true
		}
	}
	return allowed
}

func systemdDBusTransportMethod(function *ast.FuncDecl) bool {
	if function.Recv == nil || len(function.Recv.List) != 1 {
		return false
	}
	receiver := function.Recv.List[0].Type
	if pointer, ok := receiver.(*ast.StarExpr); ok {
		receiver = pointer.X
	}
	identifier, ok := receiver.(*ast.Ident)
	return ok && identifier.Name == "dbusTransport"
}

func checkSystemdIOWeightPolicy(source goSource, node ast.Node, allowed map[ast.Node]bool, constants map[string][]ast.Expr, result *checkResult) {
	if strings.HasPrefix(source.path, "internal/systemdunit/") || allowed[node] {
		return
	}
	forbidden := false
	switch typed := node.(type) {
	case *ast.Ident:
		forbidden = typed.Name == "PropertyIOWeight" || typed.Name == "PropertyIODeviceWeight" || typed.Name == "IOWeight" || typed.Name == "NewIODeviceWeightAssignment" || (typed.Name == "ProbeIODeviceWeights" && source.path != systemdIOWeightPolicyPath) || typed.Name == "ioSystemdProperties"
	case *ast.BasicLit:
		value, err := strconv.Unquote(typed.Value)
		forbidden = err == nil && (value == "IOWeight" || value == "IODeviceWeight")
	case *ast.BinaryExpr:
		expansion := systemdStringExpansion{remaining: 1000, visiting: map[string]bool{}, memo: map[ast.Expr][]string{}}
		for _, value := range systemdStringValues(typed, constants, &expansion) {
			forbidden = forbidden || value == "IOWeight" || value == "IODeviceWeight"
		}
		if expansion.exhausted {
			result.fail(source.path, sourceLine(source, node.Pos()), "IOWeight constant inspection exceeded its bounded expansion; simplify the ambiguous source expression")
		}
	}
	if forbidden {
		result.fail(source.path, sourceLine(source, node.Pos()), "I/O weight mutation is restricted to the adapter, the exact restoration inventory and the dedicated typed policy")
	}
}

// systemdIODeviceWeightPolicyExceptions admits only the typed mutation entry
// points used by the dedicated production policy. IODeviceWeight data types are
// intentionally not restricted: status, persistence and public adapters must be
// able to carry typed values without gaining authority to mutate a unit.
func systemdIODeviceWeightPolicyExceptions(source goSource) map[ast.Node]bool {
	allowed := map[ast.Node]bool{}
	if source.path != systemdIOWeightPolicyPath {
		return allowed
	}
	ast.Inspect(source.file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if systemdNamedSelector(call.Fun, "systemdunit", "NewIODeviceWeightAssignment") {
			allowed[call.Fun.(*ast.SelectorExpr).Sel] = true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "ProbeIODeviceWeights" {
			return true
		}
		allowed[selector.Sel] = true
		return true
	})
	ast.Inspect(source.file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 3 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "RestoreProperties" {
			return true
		}
		properties, ok := call.Args[2].(*ast.CompositeLit)
		if !ok || !systemdPropertySlice(properties.Type) || len(properties.Elts) != 1 {
			return true
		}
		property, ok := properties.Elts[0].(*ast.SelectorExpr)
		if ok && systemdNamedSelector(property, "systemdunit", "PropertyIODeviceWeight") {
			allowed[property.Sel] = true
		}
		return true
	})
	return allowed
}

// systemdStringDefinitions intentionally retains shadowed candidates. The gate
// rejects an ambiguous source form conservatively instead of pretending to have
// Go type information or accepting a shadowed constant as a policy escape.
func systemdStringDefinitions(file *ast.File) map[string][]ast.Expr {
	definitions := map[string][]ast.Expr{}
	ast.Inspect(file, func(node ast.Node) bool {
		declaration, ok := node.(*ast.GenDecl)
		if !ok || declaration.Tok != token.CONST {
			return true
		}
		for _, specification := range declaration.Specs {
			value, ok := specification.(*ast.ValueSpec)
			if !ok || len(value.Names) != len(value.Values) {
				continue
			}
			for index, name := range value.Names {
				definitions[name.Name] = append(definitions[name.Name], value.Values[index])
			}
		}
		return true
	})
	return definitions
}

type systemdStringExpansion struct {
	remaining int
	exhausted bool
	visiting  map[string]bool
	memo      map[ast.Expr][]string
}

func systemdStringValues(expression ast.Expr, definitions map[string][]ast.Expr, expansion *systemdStringExpansion) (result []string) {
	if cached, ok := expansion.memo[expression]; ok {
		return cached
	}
	expansion.remaining--
	if expansion.remaining < 0 {
		expansion.exhausted = true
		return nil
	}
	defer func() { expansion.memo[expression] = result }()
	unique := map[string]bool{}
	add := func(value string) {
		// Any component of a concatenation yielding an approved weight property
		// must itself be a substring. This keeps the expansion bounded.
		if (strings.Contains("IOWeight", value) || strings.Contains("IODeviceWeight", value)) && !unique[value] {
			unique[value] = true
			result = append(result, value)
		}
	}
	switch value := expression.(type) {
	case *ast.BasicLit:
		if value.Kind == token.STRING {
			if text, err := strconv.Unquote(value.Value); err == nil {
				add(text)
			}
		}
	case *ast.ParenExpr:
		return systemdStringValues(value.X, definitions, expansion)
	case *ast.BinaryExpr:
		if value.Op == token.ADD {
			leftValues := systemdStringValues(value.X, definitions, expansion)
			rightValues := systemdStringValues(value.Y, definitions, expansion)
			for _, left := range leftValues {
				for _, right := range rightValues {
					add(left + right)
				}
			}
		}
	case *ast.Ident:
		if expansion.visiting[value.Name] {
			expansion.exhausted = true
		} else {
			expansion.visiting[value.Name] = true
			defer delete(expansion.visiting, value.Name)
			for _, definition := range definitions[value.Name] {
				for _, candidate := range systemdStringValues(definition, definitions, expansion) {
					add(candidate)
				}
			}
		}
	}
	return result
}

// systemdIORestoreException pins both the superset inventory and its only data
// flow. Permitting the declaration alone would let an applier select entry zero
// indirectly, without mentioning PropertyIOWeight at the assignment boundary.
func systemdIORestoreException(source goSource, result *checkResult) map[ast.Node]bool {
	allowed := map[ast.Node]bool{}
	if source.path != systemdResourcePolicyPath {
		return allowed
	}
	want := []string{"PropertyIOWeight", "PropertyIOReadBandwidthMax", "PropertyIOWriteBandwidthMax", "PropertyIOReadIOPSMax", "PropertyIOWriteIOPSMax"}
	declarations, consumers := 0, 0
	for _, declaration := range source.file.Decls {
		switch typed := declaration.(type) {
		case *ast.GenDecl:
			for _, specification := range typed.Specs {
				value, ok := specification.(*ast.ValueSpec)
				if !ok || typed.Tok != token.VAR || len(value.Names) != 1 || value.Names[0].Name != "ioSystemdProperties" || len(value.Values) != 1 {
					continue
				}
				literal, ok := value.Values[0].(*ast.CompositeLit)
				if !ok || !systemdPropertySlice(literal.Type) || len(literal.Elts) != len(want) {
					continue
				}
				valid := true
				for index, element := range literal.Elts {
					valid = valid && systemdNamedSelector(element, "systemdunit", want[index])
				}
				if valid {
					declarations++
					allowed[value.Names[0]] = true
					allowed[literal.Elts[0].(*ast.SelectorExpr).Sel] = true
				}
			}
		case *ast.FuncDecl:
			if typed.Name.Name == "restoreSystemdResource" && typed.Body != nil {
				consumers += systemdIORestoreConsumer(source, typed, allowed, result)
			}
		}
	}
	if declarations != 1 || consumers != 1 {
		result.fail(source.path, 1, "IOWeight restoration exception is missing, obsolete or changed: need one exact inventory and one restricted restore consumer")
	}
	return allowed
}

func systemdPropertySlice(expression ast.Expr) bool {
	array, ok := expression.(*ast.ArrayType)
	return ok && array.Len == nil && systemdNamedSelector(array.Elt, "systemdunit", "PropertyName")
}

func systemdNamedSelector(expression ast.Expr, owner, member string) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != member {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && identifier.Name == owner
}

func systemdNamedIdentifier(expression ast.Expr, name string) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Name == name
}

func systemdIORestoreConsumer(source goSource, function *ast.FuncDecl, allowed map[ast.Node]bool, result *checkResult) int {
	local := map[ast.Node]bool{}
	initializers, selections, restores := 0, 0, 0
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.AssignStmt:
			if len(typed.Lhs) == 1 && len(typed.Rhs) == 1 && systemdNamedIdentifier(typed.Lhs[0], "properties") {
				if typed.Tok == token.DEFINE && systemdNamedIdentifier(typed.Rhs[0], "memorySystemdProperties") {
					initializers++
					local[typed.Lhs[0]] = true
				}
				if typed.Tok == token.ASSIGN && systemdNamedIdentifier(typed.Rhs[0], "ioSystemdProperties") {
					selections++
					local[typed.Lhs[0]], local[typed.Rhs[0]] = true, true
				}
			}
		case *ast.CallExpr:
			selector, ok := typed.Fun.(*ast.SelectorExpr)
			if ok && selector.Sel.Name == "RestoreProperties" && systemdNamedSelector(selector.X, "m", "systemdUnits") &&
				len(typed.Args) == 3 && systemdNamedIdentifier(typed.Args[0], "ctx") && systemdNamedIdentifier(typed.Args[1], "identity") &&
				systemdNamedIdentifier(typed.Args[2], "properties") {
				restores++
				local[typed.Args[2]] = true
			}
		}
		return true
	})
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if identifier, ok := node.(*ast.Ident); ok && identifier.Name == "properties" && !local[node] {
			result.fail(source.path, sourceLine(source, node.Pos()), "the IOWeight-capable restoration inventory cannot flow to production policy or any other consumer")
		}
		return true
	})
	if initializers != 1 || selections != 1 || restores != 1 {
		return 0
	}
	for node := range local {
		allowed[node] = true
	}
	return 1
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
	if selector.Sel.Name == "ControlGroup" && source.path != "internal/systemdunit/kernel.go" && source.path != systemdUnitCoveragePath && source.path != systemdIODeviceWeightCleanupPath {
		result.fail(source.path, sourceLine(source, selector.Pos()), "the authoritative systemd control-group path is restricted to kernel verification, exact IODeviceWeight keyed reset and workload-coverage inspection")
	}
}

func checkSystemdMutationCall(source goSource, call *ast.CallExpr, capabilityProbes, kernelResets map[ast.Node]bool, result *checkResult) {
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
	case "StartTransientUnitAux", "StopUnitContext":
		if !capabilityProbes[call] {
			result.fail(source.path, sourceLine(source, call.Pos()), "%s is restricted to the exact empty startup capability probe", method)
		}
	case "RevertUnit", "RevertUnitContext", "RevertUnitFiles", "RevertUnitFilesContext",
		"StartUnit", "StartUnitContext", "StopUnit", "RestartUnit", "RestartUnitContext",
		"TryRestartUnit", "TryRestartUnitContext", "ReloadUnit", "ReloadUnitContext",
		"ReloadOrRestartUnit", "ReloadOrRestartUnitContext", "ReloadOrTryRestartUnit", "ReloadOrTryRestartUnitContext",
		"StartTransientUnit", "StartTransientUnitContext",
		"KillUnit", "KillUnitContext", "ResetFailedUnit", "ResetFailedUnitContext",
		"FreezeUnit", "ThawUnit", "AttachProcessesToUnit", "AttachProcessesToUnitContext", "EnqueueUnitJob", "EnqueueUnitJobContext",
		"LinkUnitFiles", "EnableUnitFiles", "DisableUnitFiles", "MaskUnitFiles", "UnmaskUnitFiles", "PresetUnitFiles", "PresetUnitFilesWithMode":
		result.fail(source.path, sourceLine(source, call.Pos()), "%s is outside ResMan's systemd resource-property capability", method)
	}

	if !strings.HasPrefix(source.path, "internal/systemdunit/") {
		return
	}
	if source.path == systemdIODeviceWeightCleanupPath && !kernelResets[call] &&
		(strings.HasPrefix(method, "Write") || strings.HasPrefix(method, "Fprint") || method == "Copy" || method == "CopyN") {
		result.fail(source.path, sourceLine(source, call.Pos()), "%s is outside the one exact IODeviceWeight keyed-reset write", method)
		return
	}
	switch method {
	case "OpenFile":
		if source.path != systemdUnitJournalPath && source.path != systemdUnitFilesPath {
			result.fail(source.path, sourceLine(source, call.Pos()), "%s is restricted to read-only footprint inspection and the durable lease journal", method)
		}
	case "WriteFile", "Create", "CreateTemp", "Mkdir", "MkdirAll", "Remove", "RemoveAll", "Rename", "Truncate", "Write", "WriteString":
		if source.path != systemdUnitJournalPath && !kernelResets[call] {
			result.fail(source.path, sourceLine(source, call.Pos()), "%s is restricted to the durable lease journal or the exact IODeviceWeight keyed reset inside the systemd adapter", method)
		}
	}
}
