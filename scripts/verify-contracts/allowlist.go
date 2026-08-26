package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type allowEntry struct {
	fields []string
	line   int
	used   bool
}

func loadAllowlist(root, relativePath string, fieldCount int, result *checkResult) []*allowEntry {
	path := filepath.Join(root, relativePath)
	file, err := os.Open(path)
	if err != nil {
		result.fail(relativePath, 1, "open allowlist: %v", err)
		return nil
	}
	defer func() {
		if err := file.Close(); err != nil {
			result.fail(relativePath, 1, "close allowlist: %v", err)
		}
	}()

	var entries []*allowEntry
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Split(text, "|")
		if len(fields) != fieldCount {
			result.fail(relativePath, line, "allowlist entry has %d fields, want %d", len(fields), fieldCount)
			continue
		}
		for index := range fields {
			fields[index] = strings.TrimSpace(fields[index])
			if fields[index] == "" {
				result.fail(relativePath, line, "allowlist field %d is empty", index+1)
			}
		}
		entries = append(entries, &allowEntry{fields: fields, line: line})
	}
	if err := scanner.Err(); err != nil {
		result.fail(relativePath, 1, "read allowlist: %v", err)
	}
	return entries
}

func requireUsedAllowlist(relativePath string, entries []*allowEntry, result *checkResult) {
	for _, entry := range entries {
		if !entry.used {
			result.fail(relativePath, entry.line, "stale allowlist entry: %s", strings.Join(entry.fields, " | "))
		}
	}
}

func validateClassification(entry *allowEntry, relativePath string, result *checkResult) bool {
	classification := entry.fields[len(entry.fields)-2]
	reason := entry.fields[len(entry.fields)-1]
	switch classification {
	case "allowed":
		return false
	case "known":
		if !strings.Contains(reason, "resman-") {
			result.fail(relativePath, entry.line, "known finding must name its open resman issue")
		}
		return true
	default:
		result.fail(relativePath, entry.line, "classification %q must be allowed or known", classification)
		return false
	}
}

func entrySource(entry *allowEntry, first []*allowEntry, firstPath, secondPath string) string {
	for _, candidate := range first {
		if candidate == entry {
			return firstPath
		}
	}
	return secondPath
}

func parsePositiveInt(value string) (int, error) {
	var parsed int
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil || parsed < 1 {
		return 0, fmt.Errorf("%q is not a positive integer", value)
	}
	return parsed, nil
}
