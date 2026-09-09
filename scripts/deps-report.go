package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type dependencyModule struct {
	Path       string
	Version    string
	Main       bool
	Indirect   bool
	Deprecated string
	Update     *struct {
		Version string
	}
	Retracted any
}

func main() {
	if len(os.Args) != 2 && len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: deps-report.go <go-list-json> [active-modules]")
		os.Exit(2)
	}
	activeModules, err := readActiveModules(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read active modules: %v\n", err)
		os.Exit(1)
	}
	input, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
	defer func() { _ = input.Close() }()

	decoder := json.NewDecoder(input)
	var directUpdates []dependencyModule
	var deprecated []dependencyModule
	var inactiveDeprecated []dependencyModule
	var retracted []dependencyModule
	indirectGroups := map[string]int{}
	totalIndirectUpdates := 0

	for {
		var module dependencyModule
		err := decoder.Decode(&module)
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "decode module JSON: %v\n", err)
			os.Exit(1)
		}
		if module.Main {
			continue
		}
		if module.Deprecated != "" {
			if len(activeModules) == 0 || activeModules[module.Path] {
				deprecated = append(deprecated, module)
			} else {
				inactiveDeprecated = append(inactiveDeprecated, module)
			}
		}
		if module.Retracted != nil {
			retracted = append(retracted, module)
		}
		if module.Update == nil {
			continue
		}
		if module.Indirect {
			totalIndirectUpdates++
			indirectGroups[moduleGroup(module.Path)]++
			continue
		}
		directUpdates = append(directUpdates, module)
	}

	sort.Slice(directUpdates, func(i, j int) bool { return directUpdates[i].Path < directUpdates[j].Path })
	sort.Slice(deprecated, func(i, j int) bool { return deprecated[i].Path < deprecated[j].Path })
	sort.Slice(inactiveDeprecated, func(i, j int) bool { return inactiveDeprecated[i].Path < inactiveDeprecated[j].Path })
	sort.Slice(retracted, func(i, j int) bool { return retracted[i].Path < retracted[j].Path })

	printModuleUpdates(directUpdates, indirectGroups, totalIndirectUpdates)
	printDeprecatedModules(deprecated, inactiveDeprecated)
	printRetractedModules(retracted)
}

func printModuleUpdates(direct []dependencyModule, indirectGroups map[string]int, totalIndirect int) {
	fmt.Println("## Direct module updates")
	if len(direct) == 0 {
		fmt.Println("- none")
	} else {
		for _, module := range direct {
			fmt.Printf("- %s %s -> %s\n", module.Path, module.Version, module.Update.Version)
		}
	}
	fmt.Println()

	fmt.Println("## Indirect module updates")
	fmt.Printf("- total: %d\n", totalIndirect)
	groups := make([]string, 0, len(indirectGroups))
	for group := range indirectGroups {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	for _, group := range groups {
		fmt.Printf("- %s: %d\n", group, indirectGroups[group])
	}
	fmt.Println()
}

func printDeprecatedModules(active, inactive []dependencyModule) {
	fmt.Println("## Deprecated modules used by resman")
	if len(active) == 0 {
		fmt.Println("- none")
	} else {
		for _, module := range active {
			fmt.Printf("- %s %s: %s\n", module.Path, module.Version, oneLine(module.Deprecated))
		}
	}
	fmt.Println()

	fmt.Println("## Deprecated modules outside the resman package graph")
	if len(inactive) == 0 {
		fmt.Println("- none")
	} else {
		fmt.Println("- these modules occur in upstream metadata but are not imported by the configured package graph")
		for _, module := range inactive {
			fmt.Printf("- %s %s: %s\n", module.Path, module.Version, oneLine(module.Deprecated))
		}
	}
	fmt.Println()
}

func printRetractedModules(retracted []dependencyModule) {
	fmt.Println("## Retracted modules")
	if len(retracted) == 0 {
		fmt.Println("- none")
		return
	}
	for _, module := range retracted {
		fmt.Printf("- %s %s\n", module.Path, module.Version)
	}
}

func readActiveModules(args []string) (map[string]bool, error) {
	if len(args) == 0 {
		return nil, nil
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			active[line] = true
		}
	}
	return active, nil
}

func moduleGroup(path string) string {
	switch {
	case strings.HasPrefix(path, "cloud.google.com/go"):
		return "cloud.google.com/go"
	case strings.HasPrefix(path, "go.opentelemetry.io/"):
		return "go.opentelemetry.io"
	case strings.HasPrefix(path, "golang.org/x/"):
		return "golang.org/x"
	case strings.HasPrefix(path, "google.golang.org/"):
		return "google.golang.org"
	default:
		parts := strings.Split(path, "/")
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
		return path
	}
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
