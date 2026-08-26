package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigContractCheckerRejectsMissingRuntimeConsumer(t *testing.T) {
	tests := []struct {
		name       string
		consumer   string
		wantFailed bool
	}{
		{name: "consumer present", consumer: `package app; import "github.com/fdefilippo/resman/config"; func use(c *config.Config) { _ = c.GetValue() }`},
		{name: "consumer missing despite unrelated selector", consumer: `package app; type other struct{ Value string }; func use(o other) { _ = o.Value }`, wantFailed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := newCheckerFixture(t)
			writeFixture(t, root, "config/config.go", `package config
import "fmt"
type Config struct { Value string `+"`config:\"VALUE\"`"+` }
type handler func(*Config, string) error
var configFieldHandlers = map[string]handler{"VALUE": func(c *Config, value string) error { c.Value = value; return nil }}
func setConfigField(c *Config, key, value string) error { h, ok := configFieldHandlers[key]; if !ok { return fmt.Errorf("unknown configuration key %q", key) }; return h(c, value) }
func (c *Config) GetValue() string { return c.Value }
`)
			writeFixture(t, root, "app/app.go", tt.consumer)
			writeFixture(t, root, "scripts/verify-contracts/config-consumers.allowlist", "# empty\n")
			sources, parseFindings := loadGoFiles(root)
			if len(parseFindings) != 0 {
				t.Fatalf("parse findings: %v", parseFindings)
			}
			result := checkConfigContracts(root, sources)
			if got := len(result.findings) > 0; got != tt.wantFailed {
				t.Fatalf("failed = %v, want %v; findings=%v", got, tt.wantFailed, result.findings)
			}
		})
	}
}

func TestPrometheusCallSiteCheckerRejectsRegisteredButUnusedMetric(t *testing.T) {
	tests := []struct {
		name       string
		update     string
		wantFailed bool
	}{
		{name: "updated", update: `func (exp *Exporter) update() { exp.requests.Inc() }`},
		{name: "read but not updated", update: `func (exp *Exporter) inspect() { _ = exp.requests }`, wantFailed: true},
		{name: "not updated", wantFailed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := newCheckerFixture(t)
			writeFixture(t, root, "metrics/prometheus.go", `package metrics
type metric interface{ Inc() }
type Exporter struct{ requests metric }
type factory struct{}
func (factory) NewCounter(any) metric { return nil }
var prometheusFactory factory
func (exp *Exporter) registerMetrics() { exp.requests = prometheusFactory.NewCounter(struct{}{}) }
`+tt.update)
			sources, _ := loadGoFiles(root)
			result := checkPrometheusCallSites(sources)
			if got := len(result.findings) > 0; got != tt.wantFailed {
				t.Fatalf("failed = %v, want %v; findings=%v", got, tt.wantFailed, result.findings)
			}
		})
	}
}

func TestSleepCheckerRequiresExplicitNonStaleClassification(t *testing.T) {
	tests := []struct {
		name       string
		allowlist  string
		wantFailed bool
	}{
		{name: "allowed", allowlist: "app/app.go | retry | 1 | bounded retry | allowed | Backoff after a failed operation.\n"},
		{name: "unlisted", allowlist: "# empty\n", wantFailed: true},
		{name: "stale", allowlist: "app/app.go | other | 1 | bounded retry | allowed | Wrong function.\n", wantFailed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := newCheckerFixture(t)
			writeFixture(t, root, "app/app.go", `package app; import "time"; func retry(){ time.Sleep(time.Second) }`)
			writeFixture(t, root, "scripts/verify-contracts/time-sleep.allowlist", tt.allowlist)
			writeFixture(t, root, "scripts/verify-contracts/time-sleep.known", "# empty\n")
			sources, _ := loadGoFiles(root)
			result := checkProductionSleeps(root, sources)
			if got := len(result.findings) > 0; got != tt.wantFailed {
				t.Fatalf("failed = %v, want %v; findings=%v", got, tt.wantFailed, result.findings)
			}
		})
	}
}

func TestCrossPackageMapKeyCheckerReportsLiteralBoundary(t *testing.T) {
	tests := []struct {
		name        string
		consumerKey string
		known       string
		wantFailed  bool
		wantKnown   bool
	}{
		{name: "typed or distinct", consumerKey: "other"},
		{name: "untracked duplicate", consumerKey: "path", wantFailed: true},
		{name: "tracked duplicate", consumerKey: "path", known: "path | cgroup,mcp | known | resman-test.1 owns the typed replacement.\n", wantKnown: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := newCheckerFixture(t)
			writeFixture(t, root, "cgroup/info.go", `package cgroup; func produce(m map[string]string){ _ = m["path"] }`)
			writeFixture(t, root, "mcp/info.go", `package mcp; func consume(m map[string]string){ _ = m["`+tt.consumerKey+`"] }`)
			writeFixture(t, root, "scripts/verify-contracts/cross-package-map-keys.allowlist", "# empty\n")
			writeFixture(t, root, "scripts/verify-contracts/cross-package-map-keys.known", "# known\n"+tt.known)
			sources, _ := loadGoFiles(root)
			result := checkCrossPackageMapKeys(root, sources)
			if got := len(result.findings) > 0; got != tt.wantFailed {
				t.Fatalf("failed = %v, want %v; findings=%v", got, tt.wantFailed, result.findings)
			}
			if got := len(result.notices) > 0; got != tt.wantKnown {
				t.Fatalf("known = %v, want %v; notices=%v", got, tt.wantKnown, result.notices)
			}
		})
	}
}

func TestCrossPackageMapKeyCheckerReportsCompositeLiteralBoundary(t *testing.T) {
	root := newCheckerFixture(t)
	writeFixture(t, root, "cgroup/info.go", `package cgroup; func produce() map[string]string { return map[string]string{"path": "/sys/fs/cgroup/user"} }`)
	writeFixture(t, root, "mcp/info.go", `package mcp; func consume() map[string]any { return map[string]any{"path": "visible"} }`)
	writeFixture(t, root, "scripts/verify-contracts/cross-package-map-keys.allowlist", "# empty\n")
	writeFixture(t, root, "scripts/verify-contracts/cross-package-map-keys.known", "# empty\n")
	sources, _ := loadGoFiles(root)
	result := checkCrossPackageMapKeys(root, sources)
	if len(result.findings) != 2 {
		t.Fatalf("findings = %d, want 2; findings=%v", len(result.findings), result.findings)
	}
	for _, item := range result.findings {
		if item.line != 1 || !strings.Contains(item.message, `string map key "path" is duplicated across packages cgroup,mcp`) {
			t.Fatalf("unexpected finding: %+v", item)
		}
	}
}

func TestMCPCheckerPinsSDKRevisionAndStatelessTransport(t *testing.T) {
	tests := []struct {
		name       string
		sdk        string
		revision   string
		stateless  string
		wantFailed bool
	}{
		{name: "current", sdk: "v1.7.0", revision: "2026-07-28", stateless: "true"},
		{name: "old sdk", sdk: "v1.6.9", revision: "2026-07-28", stateless: "true", wantFailed: true},
		{name: "prerelease sdk", sdk: "v1.7.0-rc.1", revision: "2026-07-28", stateless: "true", wantFailed: true},
		{name: "old revision", sdk: "v1.7.0", revision: "2025-11-25", stateless: "true", wantFailed: true},
		{name: "stateful", sdk: "v1.7.0", revision: "2026-07-28", stateless: "false", wantFailed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := newCheckerFixture(t)
			writeFixture(t, root, "go.mod", "module example.test/checker\n\ngo 1.25.7\n\nrequire github.com/modelcontextprotocol/go-sdk "+tt.sdk+"\n")
			writeFixture(t, root, "mcp/server.go", `package mcp
const protocolVersion = "`+tt.revision+`"
type StreamableHTTPOptions struct{ Stateless bool }
func serve(){ _ = sdk.StreamableHTTPOptions{Stateless: `+tt.stateless+`} }
var sdk struct{ StreamableHTTPOptions StreamableHTTPOptions }
`)
			writeFixture(t, root, "scripts/verify-contracts/mcp-literals.allowlist", "# empty\n")
			sources, parseFindings := loadGoFiles(root)
			if len(parseFindings) != 0 {
				t.Fatalf("parse findings: %v", parseFindings)
			}
			result := checkMCPContracts(root, sources)
			if got := len(result.findings) > 0; got != tt.wantFailed {
				t.Fatalf("failed = %v, want %v; findings=%v", got, tt.wantFailed, result.findings)
			}
		})
	}
}

func TestMCPCheckerRejectsMCPGoDebugOutsideMCPPackage(t *testing.T) {
	root := newCheckerFixture(t)
	writeFixture(t, root, "go.mod", "module example.test/checker\n\ngo 1.25.7\n\nrequire github.com/modelcontextprotocol/go-sdk v1.7.0\n")
	writeFixture(t, root, "mcp/server.go", `package mcp
const protocolVersion = "2026-07-28"
type StreamableHTTPOptions struct{ Stateless bool }
func serve(){ _ = sdk.StreamableHTTPOptions{Stateless: true} }
var sdk struct{ StreamableHTTPOptions StreamableHTTPOptions }
`)
	writeFixture(t, root, "internal/app/debug.go", `package app; const compatibilityEnvironment = "MCPGODEBUG=legacy"`)
	writeFixture(t, root, "scripts/verify-contracts/mcp-literals.allowlist", "# empty\n")
	sources, _ := loadGoFiles(root)
	result := checkMCPContracts(root, sources)
	if len(result.findings) == 0 {
		t.Fatal("MCPGODEBUG outside package mcp was not rejected")
	}
}

func TestShippedAssetCheckerRejectsTokensAndStaleAllowlist(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		content    string
		allowlist  string
		wantFailed bool
	}{
		{name: "clean", content: "ResMan listens on 1974.\n"},
		{name: "stale token", content: "Copy CPU Manager configuration.\n", wantFailed: true},
		{name: "mixed-case product name", content: "Copy cPu MaNaGeR configuration.\n", wantFailed: true},
		{name: "uppercase configuration namespace", content: "CPU_MANAGER_BLACKOUT was the historical key.\n", wantFailed: true},
		{name: "classified uppercase configuration namespace", content: "CPU_MANAGER_BLACKOUT was the historical key.\n", allowlist: "docs/example.md | CPU_MANAGER | CPU_MANAGER_BLACKOUT | historical configuration key | allowed | This exact historical key is part of the version record.\n"},
		{name: "changelog-like path is scanned", path: "docs/changelog-notes.md", content: "Copy CPU Manager configuration.\n", wantFailed: true},
		{name: "RPM changelog is historical", path: "packaging/example.spec", content: "%changelog\n- CPU Manager was the former product name.\n"},
		{name: "allowed provenance", content: "The old port was 9101.\n", allowlist: "docs/example.md | 9101 | old port was 9101 | provenance | allowed | Historical audit provenance.\n"},
		{name: "stale allowlist", content: "ResMan listens on 1974.\n", allowlist: "docs/example.md | 9101 | old port was 9101 | provenance | allowed | Historical audit provenance.\n", wantFailed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := newCheckerFixture(t)
			path := tt.path
			if path == "" {
				path = "docs/example.md"
			}
			writeFixture(t, root, path, tt.content)
			writeFixture(t, root, "docs/empty", "")
			writeFixture(t, root, "packaging/empty", "")
			writeFixture(t, root, "scripts/verify-contracts/shipped-assets.allowlist", "# allowed\n"+tt.allowlist)
			writeFixture(t, root, "scripts/verify-contracts/shipped-assets.known", "# known\n")
			writeFixture(t, root, "README.md", "ResMan\n")
			writeFixture(t, root, "CONTRIBUTING.md", "ResMan\n")
			sources, parseFindings := loadGoFiles(root)
			if len(parseFindings) != 0 {
				t.Fatalf("parse findings: %v", parseFindings)
			}
			result := checkShippedAssets(root, sources)
			if got := len(result.findings) > 0; got != tt.wantFailed {
				t.Fatalf("failed = %v, want %v; findings=%v", got, tt.wantFailed, result.findings)
			}
		})
	}
}

func TestShippedAssetCheckerRejectsObsoleteProductNamesInProductionGo(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		content    string
		allowlist  string
		wantFailed bool
		wantLine   int
	}{
		{
			name:       "string literal",
			content:    "package app\n\nconst description = \"Get CPU Manager configuration\"\n",
			wantFailed: true,
			wantLine:   3,
		},
		{
			name:       "comment",
			content:    "package app\n\n// CPU-Manager applies limits.\nfunc apply() {}\n",
			wantFailed: true,
			wantLine:   3,
		},
		{
			name:       "mixed-case comment",
			content:    "package app\n\n// cPu_mAnAgEr was the old namespace.\nfunc apply() {}\n",
			wantFailed: true,
			wantLine:   3,
		},
		{
			name:      "classified uppercase configuration identifier",
			content:   "package app\n\nconst historicalKey = \"CPU_MANAGER_BLACKOUT\"\n",
			allowlist: "app/app.go | CPU_MANAGER | CPU_MANAGER_BLACKOUT | historical configuration key | allowed | This exact historical key is retained as explicit provenance.\n",
		},
		{
			name:    "test source is excluded",
			path:    "app/app_test.go",
			content: "package app\n\nconst obsoleteFixture = \"CPU Manager\"\n",
		},
		{
			name:    "checker source is self-excluded",
			path:    "scripts/verify-contracts/self.go",
			content: "package main\n\nconst forbiddenPattern = \"CPU Manager\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := newCheckerFixture(t)
			path := tt.path
			if path == "" {
				path = "app/app.go"
			}
			writeFixture(t, root, path, tt.content)
			writeFixture(t, root, "docs/example.md", "ResMan\n")
			writeFixture(t, root, "packaging/empty", "")
			writeFixture(t, root, "scripts/verify-contracts/shipped-assets.allowlist", "# allowed\n"+tt.allowlist)
			writeFixture(t, root, "scripts/verify-contracts/shipped-assets.known", "# known\n")
			writeFixture(t, root, "README.md", "ResMan\n")
			writeFixture(t, root, "CONTRIBUTING.md", "ResMan\n")

			sources, parseFindings := loadGoFiles(root)
			if len(parseFindings) != 0 {
				t.Fatalf("parse findings: %v", parseFindings)
			}
			result := checkShippedAssets(root, sources)
			if got := len(result.findings) > 0; got != tt.wantFailed {
				t.Fatalf("failed = %v, want %v; findings=%v", got, tt.wantFailed, result.findings)
			}
			if tt.wantFailed && (len(result.findings) != 1 || result.findings[0].path != path || result.findings[0].line != tt.wantLine) {
				t.Fatalf("unexpected finding location: %+v", result.findings)
			}
		})
	}
}

func TestShippedAssetCheckerIgnoresUntrackedWorkspaceFiles(t *testing.T) {
	root := newCheckerFixture(t)
	writeFixture(t, root, "docs/example.md", "ResMan\n")
	writeFixture(t, root, "docs/analysis/local.md", "CPU Manager was a local analysis note.\n")
	writeFixture(t, root, "app/local.go", "package app\n\nconst localNote = \"CPU_MANAGER_LOCAL\"\n")
	writeFixture(t, root, "packaging/empty", "")
	writeFixture(t, root, "scripts/verify-contracts/shipped-assets.allowlist", "# allowed\n")
	writeFixture(t, root, "scripts/verify-contracts/shipped-assets.known", "# known\n")
	writeFixture(t, root, "README.md", "ResMan\n")
	writeFixture(t, root, "CONTRIBUTING.md", "ResMan\n")

	sources, parseFindings := loadGoFiles(root)
	if len(parseFindings) != 0 {
		t.Fatalf("parse findings: %v", parseFindings)
	}
	tracked := map[string]bool{
		"docs/example.md": true,
		"packaging/empty": true,
		"README.md":       true,
		"CONTRIBUTING.md": true,
	}
	result := checkShippedAssetsWithTrackedPaths(root, sources, tracked)
	if len(result.findings) != 0 {
		t.Fatalf("untracked workspace files produced findings: %v", result.findings)
	}
}

func newCheckerFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, root, "go.mod", "module example.test/checker\n\ngo 1.25.7\n")
	return root
}

func writeFixture(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(content)+"\n"), 0600); err != nil {
		t.Fatalf("write fixture %s: %v", relative, err)
	}
}
