package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fdefilippo/resman/config"
)

func main() {
	check := flag.Bool("check", false, "fail if the generated reference differs from the tracked file")
	flag.Parse()

	root, err := repositoryRoot()
	if err != nil {
		fatal(err)
	}
	path := filepath.Join(root, "docs", "CONFIGURATION.md")
	want := []byte(config.RenderPublicConfigReference())
	if *check {
		got, err := os.ReadFile(path)
		if err != nil {
			fatal(fmt.Errorf("read %s: %w", path, err))
		}
		if string(got) != string(want) {
			fatal(fmt.Errorf("%s is stale; run go run ./scripts/generate-config-reference", path))
		}
		return
	}
	if err := os.WriteFile(path, want, 0644); err != nil {
		fatal(fmt.Errorf("write %s: %w", path, err))
	}
}

func repositoryRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("go.mod not found above %s", directory)
		}
		directory = parent
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
