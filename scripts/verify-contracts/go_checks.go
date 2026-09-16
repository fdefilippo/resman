package main

import (
	"bufio"
	"go/ast"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type configField struct {
	name string
	key  string
	path string
	line int
}

func checkConfigContracts(root string, sources []goSource) checkResult {
	result := checkResult{name: "config-contracts"}
	const allowlistPath = "scripts/verify-contracts/config-consumers.allowlist"
	entries := loadAllowlist(root, allowlistPath, 4, &result)
	fields := make(map[string]configField)
	handlers := make(map[string]token.Position)
	configMethods := make(map[string]map[string]bool)
	configMethodCalls := make(map[string]map[string]bool)
	externalSelectors := make(map[string]bool)
	hasUnknownRejection := false
	configFieldsByPackage := externalConfigFieldsByPackage(sources)

	for _, source := range productionGoFiles(sources) {
		if source.packageName != "config" {
			packageKey := filepath.Dir(source.path) + "|" + source.packageName
			for selector := range externalConfigSelectors(source, configFieldsByPackage[packageKey]) {
				externalSelectors[selector] = true
			}
			continue
		}

		for _, decl := range source.file.Decls {
			general, ok := decl.(*ast.GenDecl)
			if ok {
				for _, spec := range general.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if ok && typeSpec.Name.Name == "Config" {
						structure, ok := typeSpec.Type.(*ast.StructType)
						if !ok {
							result.fail(source.path, sourceLine(source, typeSpec.Pos()), "Config is not a struct")
							continue
						}
						for _, field := range structure.Fields.List {
							if field.Tag == nil || len(field.Names) != 1 {
								continue
							}
							tagText, err := strconv.Unquote(field.Tag.Value)
							if err != nil {
								result.fail(source.path, sourceLine(source, field.Tag.Pos()), "invalid Config tag: %v", err)
								continue
							}
							key := reflect.StructTag(tagText).Get("config")
							if key == "" || key == "-" {
								continue
							}
							fields[field.Names[0].Name] = configField{
								name: field.Names[0].Name,
								key:  key,
								path: source.path,
								line: sourceLine(source, field.Pos()),
							}
						}
					}

					valueSpec, ok := spec.(*ast.ValueSpec)
					if !ok || len(valueSpec.Names) != 1 || valueSpec.Names[0].Name != "configFieldHandlers" || len(valueSpec.Values) != 1 {
						continue
					}
					literal, ok := valueSpec.Values[0].(*ast.CompositeLit)
					if !ok {
						result.fail(source.path, sourceLine(source, valueSpec.Pos()), "configFieldHandlers is not a composite literal")
						continue
					}
					for _, element := range literal.Elts {
						pair, ok := element.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						keyLiteral, ok := pair.Key.(*ast.BasicLit)
						if !ok || keyLiteral.Kind != token.STRING {
							continue
						}
						key, err := strconv.Unquote(keyLiteral.Value)
						if err != nil {
							continue
						}
						handlers[key] = source.fset.Position(keyLiteral.Pos())
					}
				}
			}

			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			if function.Name.Name == "setConfigField" {
				hasHandlerLookup := false
				hasUnknownMessage := false
				ast.Inspect(function.Body, func(node ast.Node) bool {
					switch typed := node.(type) {
					case *ast.IndexExpr:
						if identifier, ok := typed.X.(*ast.Ident); ok && identifier.Name == "configFieldHandlers" {
							hasHandlerLookup = true
						}
					case *ast.BasicLit:
						if typed.Kind == token.STRING && strings.Contains(typed.Value, "unknown configuration key") {
							hasUnknownMessage = true
						}
					}
					return true
				})
				hasUnknownRejection = hasHandlerLookup && hasUnknownMessage
			}

			receiver, ok := receiverName(function)
			if !ok {
				continue
			}
			usedFields := make(map[string]bool)
			calledMethods := make(map[string]bool)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				identifier, ok := selector.X.(*ast.Ident)
				if ok && identifier.Name == receiver {
					if _, exists := fields[selector.Sel.Name]; exists {
						usedFields[selector.Sel.Name] = true
					} else {
						calledMethods[selector.Sel.Name] = true
					}
				}
				return true
			})
			if len(usedFields) > 0 {
				configMethods[function.Name.Name] = usedFields
			}
			if len(calledMethods) > 0 {
				configMethodCalls[function.Name.Name] = calledMethods
			}
		}
	}

	changed := true
	for changed {
		changed = false
		for method, callees := range configMethodCalls {
			if configMethods[method] == nil {
				configMethods[method] = make(map[string]bool)
			}
			for callee := range callees {
				for field := range configMethods[callee] {
					if !configMethods[method][field] {
						configMethods[method][field] = true
						changed = true
					}
				}
			}
		}
	}

	if len(fields) == 0 {
		result.fail("config/config.go", 1, "Config fields with config tags were not found")
	}
	if len(handlers) == 0 {
		result.fail("config/config.go", 1, "configFieldHandlers entries were not found")
	}
	if !hasUnknownRejection {
		result.fail("config/config.go", 1, "setConfigField must reject keys absent from configFieldHandlers")
	}

	keyToField := make(map[string]configField, len(fields))
	for _, field := range fields {
		keyToField[field.key] = field
		if _, ok := handlers[field.key]; !ok {
			result.fail(field.path, field.line, "configuration key %s has no validated configFieldHandlers entry", field.key)
		}
	}
	for key, position := range handlers {
		if _, ok := keyToField[key]; !ok {
			path, _ := filepath.Rel(".", position.Filename)
			result.fail(path, position.Line, "configFieldHandlers key %s has no Config struct tag", key)
		}
	}

	for _, field := range fields {
		consumed := externalSelectors[field.name]
		if !consumed {
			for method, methodFields := range configMethods {
				if methodFields[field.name] && externalSelectors[method] {
					consumed = true
					break
				}
			}
		}
		if !consumed {
			matched := false
			for _, entry := range entries {
				if entry.fields[0] == field.key && entry.fields[1] == field.name {
					entry.used = true
					matched = true
					if validateClassification(entry, allowlistPath, &result) {
						result.known(field.path, field.line, "%s", entry.fields[3])
					}
					break
				}
			}
			if !matched {
				result.fail(field.path, field.line, "configuration key %s (%s) has no production consumer outside config", field.key, field.name)
			}
		}
	}
	requireUsedAllowlist(allowlistPath, entries, &result)
	return result
}

func externalConfigFieldsByPackage(sources []goSource) map[string]map[string]bool {
	result := make(map[string]map[string]bool)
	for _, source := range productionGoFiles(sources) {
		aliases := configImportAliases(source)
		if len(aliases) == 0 {
			continue
		}
		packageKey := filepath.Dir(source.path) + "|" + source.packageName
		if result[packageKey] == nil {
			result[packageKey] = make(map[string]bool)
		}
		ast.Inspect(source.file, func(node ast.Node) bool {
			structure, ok := node.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range structure.Fields.List {
				if !isConfigType(field.Type, aliases) {
					continue
				}
				for _, name := range field.Names {
					result[packageKey][name.Name] = true
				}
			}
			return true
		})
	}
	return result
}

func configImportAliases(source goSource) map[string]bool {
	aliases := make(map[string]bool)
	for _, imported := range source.file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil || path != "github.com/fdefilippo/resman/config" {
			continue
		}
		name := "config"
		if imported.Name != nil {
			name = imported.Name.Name
		}
		aliases[name] = true
	}
	return aliases
}

func externalConfigSelectors(source goSource, configFields map[string]bool) map[string]bool {
	result := make(map[string]bool)
	configAliases := configImportAliases(source)

	for _, decl := range source.file.Decls {
		function, ok := decl.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		typedIdentifiers := make(map[string]bool)
		collectTypedConfigNames(function.Type.Params, configAliases, typedIdentifiers)
		collectTypedConfigNames(function.Type.Results, configAliases, typedIdentifiers)
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.DeclStmt:
				general, ok := typed.Decl.(*ast.GenDecl)
				if !ok {
					return true
				}
				for _, spec := range general.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok || !isConfigType(value.Type, configAliases) {
						continue
					}
					for _, name := range value.Names {
						typedIdentifiers[name.Name] = true
					}
				}
			case *ast.AssignStmt:
				for index, rhs := range typed.Rhs {
					if !returnsConfigSyntactically(rhs, configAliases) || index >= len(typed.Lhs) {
						continue
					}
					if name, ok := typed.Lhs[index].(*ast.Ident); ok {
						typedIdentifiers[name.Name] = true
					}
				}
			}
			return true
		})

		ast.Inspect(function.Body, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch receiver := selector.X.(type) {
			case *ast.Ident:
				if typedIdentifiers[receiver.Name] {
					result[selector.Sel.Name] = true
				}
			case *ast.SelectorExpr:
				if configFields[receiver.Sel.Name] {
					result[selector.Sel.Name] = true
				}
			case *ast.CallExpr:
				if returnsConfigSyntactically(receiver, configAliases) {
					result[selector.Sel.Name] = true
				}
			}
			return true
		})
	}
	return result
}

func collectTypedConfigNames(fields *ast.FieldList, aliases map[string]bool, target map[string]bool) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		if !isConfigType(field.Type, aliases) {
			continue
		}
		for _, name := range field.Names {
			target[name.Name] = true
		}
	}
}

func isConfigType(expression ast.Expr, aliases map[string]bool) bool {
	if expression == nil {
		return false
	}
	if pointer, ok := expression.(*ast.StarExpr); ok {
		expression = pointer.X
	}
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Config" {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && aliases[identifier.Name]
}

func returnsConfigSyntactically(expression ast.Expr, aliases map[string]bool) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if identifier, ok := selector.X.(*ast.Ident); ok && aliases[identifier.Name] {
		return selector.Sel.Name == "DefaultConfig" || selector.Sel.Name == "LoadAndValidate"
	}
	switch selector.Sel.Name {
	case "GetConfig", "getConfig", "currentConfig":
		return true
	default:
		return false
	}
}

func checkPrometheusCallSites(sources []goSource) checkResult {
	result := checkResult{name: "prometheus-call-sites"}
	registered := make(map[string]struct {
		path string
		line int
	})
	mutatedOutsideRegistration := make(map[string]bool)

	for _, source := range productionGoFiles(sources) {
		if source.packageName != "metrics" {
			continue
		}
		for _, decl := range source.file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			if function.Name.Name == "registerMetrics" {
				ast.Inspect(function.Body, func(node ast.Node) bool {
					assignment, ok := node.(*ast.AssignStmt)
					if !ok || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 || !containsPrometheusConstructor(assignment.Rhs[0]) {
						return true
					}
					selector, ok := assignment.Lhs[0].(*ast.SelectorExpr)
					if !ok {
						return true
					}
					registered[selector.Sel.Name] = struct {
						path string
						line int
					}{path: source.path, line: sourceLine(source, selector.Pos())}
					return true
				})
				continue
			}
			recordPrometheusMutations(function.Body, mutatedOutsideRegistration)
		}
	}
	if len(registered) == 0 {
		result.fail("metrics/prometheus.go", 1, "no registered Prometheus metric fields found")
		return result
	}
	for field, position := range registered {
		if !mutatedOutsideRegistration[field] {
			result.fail(position.path, position.line, "registered metric field %s has no production mutation call site", field)
		}
	}
	return result
}

func recordPrometheusMutations(body *ast.BlockStmt, mutated map[string]bool) {
	mutationMethods := map[string]bool{
		"Add":               true,
		"Dec":               true,
		"Delete":            true,
		"DeleteLabelValues": true,
		"Inc":               true,
		"Observe":           true,
		"Reset":             true,
		"Set":               true,
		"SetToCurrentTime":  true,
		"Sub":               true,
	}
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !mutationMethods[method.Sel.Name] {
			return true
		}
		ast.Inspect(method.X, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if ok {
				mutated[selector.Sel.Name] = true
			}
			return true
		})
		return true
	})
}

func containsPrometheusConstructor(expression ast.Expr) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok && (strings.HasPrefix(selector.Sel.Name, "NewGauge") ||
			strings.HasPrefix(selector.Sel.Name, "NewCounter") ||
			strings.HasPrefix(selector.Sel.Name, "NewHistogram") ||
			strings.HasPrefix(selector.Sel.Name, "NewSummary")) {
			found = true
			return false
		}
		return true
	})
	return found
}

func checkProductionSleeps(root string, sources []goSource) checkResult {
	result := checkResult{name: "production-sleeps"}
	const allowlistPath = "scripts/verify-contracts/time-sleep.allowlist"
	allowedEntries := loadAllowlist(root, allowlistPath, 6, &result)
	const knownPath = "scripts/verify-contracts/time-sleep.known"
	knownEntries := loadAllowlist(root, knownPath, 6, &result)
	entries := append(allowedEntries, knownEntries...)

	for _, source := range productionGoFiles(sources) {
		for _, decl := range source.file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ordinal := 0
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || !isTimeSleep(call.Fun) {
					return true
				}
				ordinal++
				matched := false
				for _, entry := range entries {
					wantedOrdinal, err := parsePositiveInt(entry.fields[2])
					if err != nil {
						result.fail(entrySource(entry, allowedEntries, allowlistPath, knownPath), entry.line, "%v", err)
						entry.used = true
						continue
					}
					if entry.fields[0] == source.path && entry.fields[1] == functionName(function) && wantedOrdinal == ordinal {
						entry.used = true
						matched = true
						entryPath := entrySource(entry, allowedEntries, allowlistPath, knownPath)
						if validateClassification(entry, entryPath, &result) {
							result.known(source.path, sourceLine(source, call.Pos()), "%s", entry.fields[5])
						}
						break
					}
				}
				if !matched {
					result.fail(source.path, sourceLine(source, call.Pos()), "time.Sleep in %s is not an allowlisted bounded backoff or linked finding", function.Name.Name)
				}
				return true
			})
		}
	}
	requireUsedAllowlist(allowlistPath, allowedEntries, &result)
	requireUsedAllowlist(knownPath, knownEntries, &result)
	return result
}

func isTimeSleep(expression ast.Expr) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Sleep" {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && identifier.Name == "time"
}

type mapKeyOccurrence struct {
	path string
	line int
	pkg  string
}

func checkCrossPackageMapKeys(root string, sources []goSource) checkResult {
	result := checkResult{name: "cross-package-map-keys"}
	const allowlistPath = "scripts/verify-contracts/cross-package-map-keys.allowlist"
	allowedEntries := loadAllowlist(root, allowlistPath, 4, &result)
	const knownPath = "scripts/verify-contracts/cross-package-map-keys.known"
	knownEntries := loadAllowlist(root, knownPath, 4, &result)
	entries := append(allowedEntries, knownEntries...)
	byKey := make(map[string][]mapKeyOccurrence)

	for _, source := range productionGoFiles(sources) {
		recordKey := func(literal *ast.BasicLit) {
			if literal.Kind != token.STRING {
				return
			}
			key, err := strconv.Unquote(literal.Value)
			if err != nil {
				return
			}
			byKey[key] = append(byKey[key], mapKeyOccurrence{
				path: source.path,
				line: sourceLine(source, literal.Pos()),
				pkg:  source.packageName,
			})
		}
		ast.Inspect(source.file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.IndexExpr:
				if literal, ok := typed.Index.(*ast.BasicLit); ok {
					recordKey(literal)
				}
			case *ast.CompositeLit:
				for _, element := range typed.Elts {
					pair, ok := element.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if literal, ok := pair.Key.(*ast.BasicLit); ok {
						recordKey(literal)
					}
				}
			}
			return true
		})
	}

	for key, occurrences := range byKey {
		packages := make(map[string]bool)
		for _, occurrence := range occurrences {
			packages[occurrence.pkg] = true
		}
		if len(packages) < 2 {
			continue
		}
		packageNames := make([]string, 0, len(packages))
		for name := range packages {
			packageNames = append(packageNames, name)
		}
		sort.Strings(packageNames)
		packageSet := strings.Join(packageNames, ",")
		matched := false
		for _, entry := range entries {
			if entry.fields[0] == key && entry.fields[1] == packageSet {
				entry.used = true
				matched = true
				entryPath := entrySource(entry, allowedEntries, allowlistPath, knownPath)
				if validateClassification(entry, entryPath, &result) {
					first := occurrences[0]
					result.known(first.path, first.line, "string map key %q crosses packages %s: %s", key, packageSet, entry.fields[3])
				}
				break
			}
		}
		if !matched {
			for _, occurrence := range occurrences {
				result.fail(occurrence.path, occurrence.line, "string map key %q is duplicated across packages %s; replace the map boundary with a typed contract or shared constants", key, packageSet)
			}
		}
	}
	requireUsedAllowlist(allowlistPath, allowedEntries, &result)
	requireUsedAllowlist(knownPath, knownEntries, &result)
	return result
}

func checkMCPContracts(root string, sources []goSource) checkResult {
	result := checkResult{name: "mcp-latest-stateless"}
	checkGoSDKFloor(root, &result)
	checkMCPGoDebug(root, &result)

	const allowlistPath = "scripts/verify-contracts/mcp-literals.allowlist"
	entries := loadAllowlist(root, allowlistPath, 5, &result)
	currentRevisionFound := false
	streamableOptionsFound := false
	datePattern := regexp.MustCompile(`^20[0-9]{2}-[0-9]{2}-[0-9]{2}$`)

	for _, source := range productionGoFiles(sources) {
		if source.packageName != "mcp" {
			continue
		}
		for _, decl := range source.file.Decls {
			contextName := "package"
			literalContexts := make(map[token.Pos]string)
			switch typed := decl.(type) {
			case *ast.FuncDecl:
				contextName = typed.Name.Name
			case *ast.GenDecl:
				for _, spec := range typed.Specs {
					if valueSpec, ok := spec.(*ast.ValueSpec); ok && len(valueSpec.Names) == 1 {
						name := valueSpec.Names[0].Name
						for _, value := range valueSpec.Values {
							ast.Inspect(value, func(node ast.Node) bool {
								if literal, ok := node.(*ast.BasicLit); ok {
									literalContexts[literal.Pos()] = name
								}
								return true
							})
						}
					}
				}
			}
			ast.Inspect(decl, func(node ast.Node) bool {
				switch typed := node.(type) {
				case *ast.BasicLit:
					if typed.Kind != token.STRING {
						return true
					}
					value, err := strconv.Unquote(typed.Value)
					if err != nil {
						return true
					}
					if value == "2026-07-28" {
						currentRevisionFound = true
					}
					if datePattern.MatchString(value) && value != "2026-07-28" {
						result.fail(source.path, sourceLine(source, typed.Pos()), "legacy MCP protocol revision %s in production", value)
					}
					if strings.Contains(value, "MCPGODEBUG") || strings.Contains(value, "Mcp-Session-Id") {
						literalContext := contextName
						if namedContext, ok := literalContexts[typed.Pos()]; ok {
							literalContext = namedContext
						}
						matched := false
						for _, entry := range entries {
							if entry.fields[0] == source.path && entry.fields[1] == literalContext && entry.fields[2] == value {
								entry.used = true
								matched = true
								if validateClassification(entry, allowlistPath, &result) {
									result.known(source.path, sourceLine(source, typed.Pos()), "%s", entry.fields[4])
								}
								break
							}
						}
						if !matched {
							result.fail(source.path, sourceLine(source, typed.Pos()), "forbidden MCP compatibility construct %q", value)
						}
					}
				case *ast.CompositeLit:
					selector, ok := typed.Type.(*ast.SelectorExpr)
					if !ok || selector.Sel.Name != "StreamableHTTPOptions" {
						return true
					}
					streamableOptionsFound = true
					stateless := false
					for _, element := range typed.Elts {
						pair, ok := element.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, keyOK := pair.Key.(*ast.Ident)
						value, valueOK := pair.Value.(*ast.Ident)
						if keyOK && valueOK && key.Name == "Stateless" && value.Name == "true" {
							stateless = true
						}
					}
					if !stateless {
						result.fail(source.path, sourceLine(source, typed.Pos()), "StreamableHTTPOptions must set Stateless: true explicitly")
					}
				}
				return true
			})
		}
	}
	if !currentRevisionFound {
		result.fail("mcp/server.go", 1, "production MCP package does not declare revision 2026-07-28")
	}
	if !streamableOptionsFound {
		result.fail("mcp/server.go", 1, "StreamableHTTPOptions construction was not found")
	}
	requireUsedAllowlist(allowlistPath, entries, &result)
	return result
}

func checkGoSDKFloor(root string, result *checkResult) {
	file, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		result.fail("go.mod", 1, "open module file: %v", err)
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			result.fail("go.mod", 1, "close module file: %v", err)
		}
	}()

	found := false
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 3 && fields[0] == "require" {
			fields = fields[1:]
		}
		if len(fields) < 2 || fields[0] != "github.com/modelcontextprotocol/go-sdk" {
			continue
		}
		found = true
		if compareGoVersion(fields[1], "v1.7.0") < 0 {
			result.fail("go.mod", line, "github.com/modelcontextprotocol/go-sdk %s is below required v1.7.0", fields[1])
		}
	}
	if err := scanner.Err(); err != nil {
		result.fail("go.mod", 1, "read module file: %v", err)
	}
	if !found {
		result.fail("go.mod", 1, "github.com/modelcontextprotocol/go-sdk dependency is missing")
	}
}

func compareGoVersion(left, right string) int {
	leftPrerelease := strings.Contains(strings.TrimPrefix(left, "v"), "-")
	rightPrerelease := strings.Contains(strings.TrimPrefix(right, "v"), "-")
	parse := func(value string) [3]int {
		value = strings.TrimPrefix(value, "v")
		value = strings.SplitN(value, "-", 2)[0]
		parts := strings.Split(value, ".")
		var result [3]int
		for index := range result {
			if index < len(parts) {
				result[index], _ = strconv.Atoi(parts[index])
			}
		}
		return result
	}
	a, b := parse(left), parse(right)
	for index := range a {
		if a[index] < b[index] {
			return -1
		}
		if a[index] > b[index] {
			return 1
		}
	}
	if leftPrerelease && !rightPrerelease {
		return -1
	}
	if !leftPrerelease && rightPrerelease {
		return 1
	}
	return 0
}

func checkMCPGoDebug(root string, result *checkResult) {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			rel, _ := filepath.Rel(root, path)
			result.fail(rel, 1, "scan for MCPGODEBUG: %v", walkErr)
			return nil
		}
		if entry.IsDir() {
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if rel == ".git" || rel == ".beads" || rel == "build" || rel == "dist" || rel == "vendor" || rel == "scripts/verify-contracts" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, "_test.go") || rel == "docs/DEVELOPMENT.md" {
			return nil
		}
		if !mcpTextCandidate(rel) {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			result.fail(rel, 1, "scan for MCPGODEBUG: %v", err)
			return nil
		}
		defer func() {
			if err := file.Close(); err != nil {
				result.fail(rel, 1, "close after MCPGODEBUG scan: %v", err)
			}
		}()
		reader := bufio.NewReader(file)
		for line := 1; ; line++ {
			text, readErr := reader.ReadString('\n')
			if strings.Contains(text, "MCPGODEBUG") {
				result.fail(rel, line, "MCPGODEBUG compatibility flags are forbidden")
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				result.fail(rel, line, "scan for MCPGODEBUG: %v", readErr)
				break
			}
		}
		return nil
	})
}

func mcpTextCandidate(path string) bool {
	base := filepath.Base(path)
	if base == "Makefile" || base == "go.mod" || base == "go.sum" {
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".sh", ".md", ".yml", ".yaml", ".toml", ".conf", ".service", ".spec", ".json":
		return true
	default:
		return false
	}
}
