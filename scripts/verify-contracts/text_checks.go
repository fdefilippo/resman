package main

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var staleAssetTokens = []string{"9100", "9101", "cpu_manager", "cpu-manager", "CPU Manager"}

func checkShippedAssets(root string) checkResult {
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
			checkShippedAssetFile(root, path, allowlistPath, knownPath, allowedEntries, entries, &result)
			continue
		}
		_ = filepath.WalkDir(path, func(candidate string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				rel, _ := filepath.Rel(root, candidate)
				result.fail(rel, 1, "scan shipped asset: %v", walkErr)
				return nil
			}
			if entry.IsDir() {
				if filepath.ToSlash(candidate) == filepath.ToSlash(filepath.Join(root, "scripts/verify-contracts")) {
					return filepath.SkipDir
				}
				return nil
			}
			checkShippedAssetFile(root, candidate, allowlistPath, knownPath, allowedEntries, entries, &result)
			return nil
		})
	}
	requireUsedAllowlist(allowlistPath, allowedEntries, &result)
	requireUsedAllowlist(knownPath, knownEntries, &result)
	return result
}

func checkShippedAssetFile(root, path, allowlistPath, knownPath string, allowedEntries, entries []*allowEntry, result *checkResult) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return
	}
	rel = filepath.ToSlash(rel)
	lowerPath := strings.ToLower(rel)
	if strings.Contains(lowerPath, "changelog") || strings.HasSuffix(lowerPath, ".gz") || strings.HasSuffix(lowerPath, ".png") || strings.HasSuffix(lowerPath, ".jpg") {
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
		for _, token := range staleAssetTokens {
			if !strings.Contains(text, token) {
				continue
			}
			matched := false
			for _, entry := range entries {
				if entry.fields[0] == rel && entry.fields[1] == token && strings.Contains(text, entry.fields[2]) {
					entry.used = true
					matched = true
					entryPath := entrySource(entry, allowedEntries, allowlistPath, knownPath)
					if validateClassification(entry, entryPath, result) {
						result.known(rel, line, "%s", entry.fields[5])
					}
					break
				}
			}
			if !matched {
				result.fail(rel, line, "stale shipped asset token %q", token)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		result.fail(rel, 1, "read shipped asset: %v", err)
	}
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
