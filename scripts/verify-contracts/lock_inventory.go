package main

import (
	"bufio"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const lockBoundaryInventoryPath = "docs/LOCK-BOUNDARY-INVENTORY.md"
const lockBoundaryAllowlistPath = "scripts/verify-contracts/lock-boundary-inventory.allowlist"

type lockBoundaryType struct {
	id     string
	path   string
	line   int
	fields []string
}

type lockInventoryEntry struct {
	id   string
	line int
}

func checkLockBoundaryInventory(root string, sources []goSource) checkResult {
	result := checkResult{name: "lock-boundary-inventory"}
	boundaries := productionLockBoundaryTypes(sources)
	inventory := loadLockBoundaryInventory(root, &result)
	allowlist := loadAllowlist(root, lockBoundaryAllowlistPath, 4, &result)

	byID := make(map[string][]lockInventoryEntry)
	for _, entry := range inventory {
		byID[entry.id] = append(byID[entry.id], entry)
	}
	for id, entries := range byID {
		if len(entries) < 2 {
			continue
		}
		for _, entry := range entries[1:] {
			result.fail(lockBoundaryInventoryPath, entry.line, "duplicate lock-boundary inventory entry for %s", id)
		}
	}

	boundaryByID := make(map[string]lockBoundaryType, len(boundaries))
	for _, boundary := range boundaries {
		boundaryByID[boundary.id] = boundary
		if len(byID[boundary.id]) > 0 {
			continue
		}
		if matchLockBoundaryException(boundary, allowlist, &result) {
			continue
		}
		result.fail(
			boundary.path,
			boundary.line,
			"synchronization-bearing type %s is missing from %s (fields: %s)",
			boundary.id,
			lockBoundaryInventoryPath,
			strings.Join(boundary.fields, ", "),
		)
	}

	for id, entries := range byID {
		if _, ok := boundaryByID[id]; ok {
			continue
		}
		for _, entry := range entries {
			result.fail(lockBoundaryInventoryPath, entry.line, "stale lock-boundary inventory entry for %s", id)
		}
	}
	requireUsedAllowlist(lockBoundaryAllowlistPath, allowlist, &result)
	return result
}

func productionLockBoundaryTypes(sources []goSource) []lockBoundaryType {
	var boundaries []lockBoundaryType
	for _, source := range productionGoFiles(sources) {
		// Generated files are excluded explicitly: their synchronization boundaries
		// belong to the generator's contract, not to hand-maintained production code.
		if ast.IsGenerated(source.file) {
			continue
		}
		syncNames, syncDot, gateNames, gateDot := synchronizationImports(source.file)
		for _, declaration := range source.file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok || typeSpec.Assign.IsValid() {
					continue
				}
				structure, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				var fields []string
				for _, field := range structure.Fields.List {
					kind, ok := synchronizationFieldKind(field.Type, syncNames, syncDot, gateNames, gateDot)
					if !ok {
						continue
					}
					if len(field.Names) == 0 {
						fields = append(fields, kind+":"+kind)
						continue
					}
					for _, name := range field.Names {
						fields = append(fields, name.Name+":"+kind)
					}
				}
				if len(fields) == 0 {
					continue
				}
				sort.Strings(fields)
				boundaries = append(boundaries, lockBoundaryType{
					id:     lockBoundaryTypeID(source, typeSpec.Name.Name),
					path:   source.path,
					line:   sourceLine(source, typeSpec.Pos()),
					fields: fields,
				})
			}
		}
	}
	sort.Slice(boundaries, func(i, j int) bool { return boundaries[i].id < boundaries[j].id })
	return boundaries
}

func synchronizationImports(file *ast.File) (map[string]bool, bool, map[string]bool, bool) {
	syncNames := make(map[string]bool)
	gateNames := make(map[string]bool)
	var syncDot, gateDot bool
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			continue
		}
		name := filepath.Base(path)
		if imported.Name != nil {
			name = imported.Name.Name
		}
		switch path {
		case "sync":
			if name == "." {
				syncDot = true
			} else if name != "_" {
				syncNames[name] = true
			}
		case "github.com/fdefilippo/resman/internal/operationgate":
			if name == "." {
				gateDot = true
			} else if name != "_" {
				gateNames[name] = true
			}
		}
	}
	return syncNames, syncDot, gateNames, gateDot
}

func synchronizationFieldKind(expression ast.Expr, syncNames map[string]bool, syncDot bool, gateNames map[string]bool, gateDot bool) (string, bool) {
	if pointer, ok := expression.(*ast.StarExpr); ok {
		return synchronizationFieldKind(pointer.X, syncNames, syncDot, gateNames, gateDot)
	}
	if selector, ok := expression.(*ast.SelectorExpr); ok {
		qualifier, ok := selector.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		if syncNames[qualifier.Name] && (selector.Sel.Name == "Mutex" || selector.Sel.Name == "RWMutex") {
			return "sync." + selector.Sel.Name, true
		}
		if gateNames[qualifier.Name] && selector.Sel.Name == "Gate" {
			return "operationgate.Gate", true
		}
		return "", false
	}
	identifier, ok := expression.(*ast.Ident)
	if !ok {
		return "", false
	}
	if syncDot && (identifier.Name == "Mutex" || identifier.Name == "RWMutex") {
		return "sync." + identifier.Name, true
	}
	if gateDot && identifier.Name == "Gate" {
		return "operationgate.Gate", true
	}
	return "", false
}

func lockBoundaryTypeID(source goSource, typeName string) string {
	directory := filepath.ToSlash(filepath.Dir(source.path))
	if directory == "." {
		directory = source.packageName
	}
	return directory + "." + typeName
}

func loadLockBoundaryInventory(root string, result *checkResult) []lockInventoryEntry {
	file, err := os.Open(filepath.Join(root, lockBoundaryInventoryPath))
	if err != nil {
		result.fail(lockBoundaryInventoryPath, 1, "open lock-boundary inventory: %v", err)
		return nil
	}
	defer func() {
		if err := file.Close(); err != nil {
			result.fail(lockBoundaryInventoryPath, 1, "close lock-boundary inventory: %v", err)
		}
	}()

	var entries []lockInventoryEntry
	headerFound := false
	insideTable := false
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(text, "|") || !strings.HasSuffix(text, "|") {
			if insideTable && text != "" {
				insideTable = false
			}
			continue
		}
		cells := markdownTableCells(text)
		if len(cells) < 2 {
			continue
		}
		if cells[0] == "Component" && cells[1] == "Synchronization-bearing types" {
			if headerFound {
				result.fail(lockBoundaryInventoryPath, line, "duplicate lock-boundary inventory table header")
			}
			headerFound = true
			insideTable = true
			continue
		}
		if !insideTable || markdownSeparator(cells[1]) {
			continue
		}
		if cells[1] == "" {
			result.fail(lockBoundaryInventoryPath, line, "inventory row %q has no synchronization-bearing type identifier", cells[0])
			continue
		}
		for _, value := range strings.Split(cells[1], "<br>") {
			value = strings.TrimSpace(value)
			if len(value) < 3 || value[0] != '`' || value[len(value)-1] != '`' {
				result.fail(lockBoundaryInventoryPath, line, "inventory type identifier %q must be enclosed in backticks", value)
				continue
			}
			id := value[1 : len(value)-1]
			if !validLockBoundaryTypeID(id) {
				result.fail(lockBoundaryInventoryPath, line, "invalid lock-boundary type identifier %q", id)
				continue
			}
			entries = append(entries, lockInventoryEntry{id: id, line: line})
		}
	}
	if err := scanner.Err(); err != nil {
		result.fail(lockBoundaryInventoryPath, 1, "read lock-boundary inventory: %v", err)
	}
	if !headerFound {
		result.fail(lockBoundaryInventoryPath, 1, "lock-boundary inventory table with machine-readable type identifiers was not found")
	}
	return entries
}

func markdownTableCells(line string) []string {
	parts := strings.Split(line, "|")
	if len(parts) < 3 {
		return nil
	}
	cells := make([]string, 0, len(parts)-2)
	for _, part := range parts[1 : len(parts)-1] {
		cells = append(cells, strings.TrimSpace(part))
	}
	return cells
}

func markdownSeparator(value string) bool {
	value = strings.Trim(value, ":")
	return len(value) >= 3 && strings.Trim(value, "-") == ""
}

func validLockBoundaryTypeID(value string) bool {
	separator := strings.LastIndexByte(value, '.')
	if separator < 1 || separator == len(value)-1 || !token.IsIdentifier(value[separator+1:]) {
		return false
	}
	for _, part := range strings.Split(value[:separator], "/") {
		if !token.IsIdentifier(part) {
			return false
		}
	}
	return true
}

func matchLockBoundaryException(boundary lockBoundaryType, entries []*allowEntry, result *checkResult) bool {
	fieldSet := strings.Join(boundary.fields, ",")
	for _, entry := range entries {
		if entry.fields[0] != boundary.id || entry.fields[1] != fieldSet {
			continue
		}
		entry.used = true
		if validateClassification(entry, lockBoundaryAllowlistPath, result) {
			result.known(boundary.path, boundary.line, "lock-boundary inventory exception for %s: %s", boundary.id, entry.fields[3])
		}
		return true
	}
	return false
}
