package main

import (
	"bufio"
	"bytes"
	"go/ast"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var obsoleteProductPattern = regexp.MustCompile(`(?i)\bcpu[ _-]+manager`)

type obsoleteTokenMatch struct {
	token  string
	offset int
}

func checkShippedAssets(root string, sources []goSource) checkResult {
	return checkShippedAssetsWithTrackedPaths(root, sources, trackedRepositoryPaths(root))
}

func checkShippedAssetsWithTrackedPaths(root string, sources []goSource, trackedPaths map[string]bool) checkResult {
	result := checkResult{name: "shipped-assets"}
	const allowlistPath = "scripts/verify-contracts/shipped-assets.allowlist"
	allowedEntries := loadAllowlist(root, allowlistPath, 6, &result)
	const knownPath = "scripts/verify-contracts/shipped-assets.known"
	knownEntries := loadAllowlist(root, knownPath, 6, &result)
	entries := append(allowedEntries, knownEntries...)
	paths := []string{"docs", "packaging", "scripts", "README.md", "CONTRIBUTING.md"}

	for _, relative := range paths {
		path := filepath.Join(root, relative)
		info, err := os.Stat(path)
		if err != nil {
			result.fail(relative, 1, "inspect shipped asset: %v", err)
			continue
		}
		if !info.IsDir() {
			checkShippedAssetFile(root, path, trackedPaths, allowlistPath, knownPath, allowedEntries, entries, &result)
			continue
		}
		_ = filepath.WalkDir(path, func(candidate string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				rel, _ := filepath.Rel(root, candidate)
				result.fail(rel, 1, "scan shipped asset: %v", walkErr)
				return nil
			}
			if entry.IsDir() {
				// The checker and its policy files must name every forbidden token, so
				// scanning their own implementation would create a recursive exception.
				if filepath.ToSlash(candidate) == filepath.ToSlash(filepath.Join(root, "scripts/verify-contracts")) {
					return filepath.SkipDir
				}
				return nil
			}
			checkShippedAssetFile(root, candidate, trackedPaths, allowlistPath, knownPath, allowedEntries, entries, &result)
			return nil
		})
	}
	checkProductionGoTerminology(sources, trackedPaths, allowlistPath, knownPath, allowedEntries, entries, &result)
	requireUsedAllowlist(allowlistPath, allowedEntries, &result)
	requireUsedAllowlist(knownPath, knownEntries, &result)
	return result
}

func checkShippedAssetFile(root, path string, trackedPaths map[string]bool, allowlistPath, knownPath string, allowedEntries, entries []*allowEntry, result *checkResult) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return
	}
	rel = filepath.ToSlash(rel)
	if trackedPaths != nil && !trackedPaths[rel] {
		return
	}
	lowerPath := strings.ToLower(rel)
	if strings.HasSuffix(lowerPath, ".gz") || strings.HasSuffix(lowerPath, ".png") || strings.HasSuffix(lowerPath, ".jpg") {
		return
	}
	file, err := os.Open(path)
	if err != nil {
		result.fail(rel, 1, "open shipped asset: %v", err)
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			result.fail(rel, 1, "close shipped asset: %v", err)
		}
	}()

	inRPMChangelog := false
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if strings.HasSuffix(lowerPath, ".spec") && strings.TrimSpace(text) == "%changelog" {
			inRPMChangelog = true
			continue
		}
		if inRPMChangelog {
			continue
		}
		for _, match := range staleShippedAssetTokens(text) {
			classifyObsoleteToken(rel, line, text, match.token, allowlistPath, knownPath, allowedEntries, entries, result)
		}
	}
	if err := scanner.Err(); err != nil {
		result.fail(rel, 1, "read shipped asset: %v", err)
	}
}

func checkProductionGoTerminology(sources []goSource, trackedPaths map[string]bool, allowlistPath, knownPath string, allowedEntries, entries []*allowEntry, result *checkResult) {
	for _, source := range productionGoFiles(sources) {
		if trackedPaths != nil && !trackedPaths[source.path] {
			continue
		}
		ast.Inspect(source.file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			classifyGoText(source, literal.Pos(), literal.Value, allowlistPath, knownPath, allowedEntries, entries, result)
			return true
		})
		for _, group := range source.file.Comments {
			for _, comment := range group.List {
				classifyGoText(source, comment.Pos(), comment.Text, allowlistPath, knownPath, allowedEntries, entries, result)
			}
		}
	}
}

func trackedRepositoryPaths(root string) map[string]bool {
	command := exec.Command("git", "ls-files", "-z")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		// Source archives and unit-test fixtures have no Git metadata. In that
		// environment every file under the declared shipped paths is in scope.
		return nil
	}
	tracked := make(map[string]bool)
	for _, path := range bytes.Split(output, []byte{0}) {
		if len(path) > 0 {
			tracked[filepath.ToSlash(string(path))] = true
		}
	}
	return tracked
}

func classifyGoText(source goSource, pos token.Pos, text, allowlistPath, knownPath string, allowedEntries, entries []*allowEntry, result *checkResult) {
	startLine := sourceLine(source, pos)
	for _, match := range findObsoleteProductTokens(text) {
		line := startLine + strings.Count(text[:match.offset], "\n")
		classifyObsoleteToken(source.path, line, text, match.token, allowlistPath, knownPath, allowedEntries, entries, result)
	}
}

func classifyObsoleteToken(path string, line int, text, token, allowlistPath, knownPath string, allowedEntries, entries []*allowEntry, result *checkResult) {
	for _, entry := range entries {
		if entry.fields[0] != path || entry.fields[1] != token || !strings.Contains(text, entry.fields[2]) {
			continue
		}
		entry.used = true
		entryPath := entrySource(entry, allowedEntries, allowlistPath, knownPath)
		if validateClassification(entry, entryPath, result) {
			result.known(path, line, "%s", entry.fields[5])
		}
		return
	}
	result.fail(path, line, "obsolete product or namespace token %q", token)
}

func staleShippedAssetTokens(text string) []obsoleteTokenMatch {
	matches := findObsoleteProductTokens(text)
	for _, token := range []string{"9100", "9101"} {
		if offset := strings.Index(text, token); offset >= 0 {
			matches = append(matches, obsoleteTokenMatch{token: token, offset: offset})
		}
	}
	return matches
}

func findObsoleteProductTokens(text string) []obsoleteTokenMatch {
	indexes := obsoleteProductPattern.FindAllStringIndex(text, -1)
	matches := make([]obsoleteTokenMatch, 0, len(indexes))
	for _, index := range indexes {
		if index[1] < len(text) && isASCIIAlphaNumeric(text[index[1]]) {
			continue
		}
		matches = append(matches, obsoleteTokenMatch{
			token:  text[index[0]:index[1]],
			offset: index[0],
		})
	}
	return matches
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func checkPrometheusAssets(root string) checkResult {
	result := checkResult{name: "prometheus-assets"}
	promtool, err := exec.LookPath("promtool")
	if err != nil {
		result.warn("docs/alerting-rules.yml", 1, "promtool is unavailable; YAML validation was skipped")
		return result
	}

	commands := []struct {
		path string
		args []string
	}{
		{path: "docs/alerting-rules.yml", args: []string{"check", "rules", "docs/alerting-rules.yml"}},
		{path: "docs/prometheus.yml", args: []string{"check", "config", "docs/prometheus.yml"}},
	}
	for _, command := range commands {
		cmd := exec.Command(promtool, command.args...)
		cmd.Dir = root
		output, err := cmd.CombinedOutput()
		if err != nil {
			message := strings.TrimSpace(string(bytes.TrimSpace(output)))
			if message == "" {
				message = err.Error()
			}
			result.fail(command.path, 1, "promtool %s failed: %s", strings.Join(command.args, " "), message)
		}
	}
	return result
}
