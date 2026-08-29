package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/fdefilippo/resman/config"
)

const (
	markdownToolInventoryStart = "<!-- BEGIN MCP TOOL INVENTORY -->"
	markdownToolInventoryEnd   = "<!-- END MCP TOOL INVENTORY -->"
	manToolInventoryStart      = `.\" BEGIN MCP TOOL INVENTORY`
	manToolInventoryEnd        = `.\" END MCP TOOL INVENTORY`

	markdownFixedResourceInventoryStart = "<!-- BEGIN MCP FIXED RESOURCE INVENTORY -->"
	markdownFixedResourceInventoryEnd   = "<!-- END MCP FIXED RESOURCE INVENTORY -->"
	manFixedResourceInventoryStart      = `.\" BEGIN MCP FIXED RESOURCE INVENTORY`
	manFixedResourceInventoryEnd        = `.\" END MCP FIXED RESOURCE INVENTORY`

	markdownResourceTemplateInventoryStart = "<!-- BEGIN MCP RESOURCE TEMPLATE INVENTORY -->"
	markdownResourceTemplateInventoryEnd   = "<!-- END MCP RESOURCE TEMPLATE INVENTORY -->"
	manResourceTemplateInventoryStart      = `.\" BEGIN MCP RESOURCE TEMPLATE INVENTORY`
	manResourceTemplateInventoryEnd        = `.\" END MCP RESOURCE TEMPLATE INVENTORY`

	markdownPromptInventoryStart = "<!-- BEGIN MCP PROMPT INVENTORY -->"
	markdownPromptInventoryEnd   = "<!-- END MCP PROMPT INVENTORY -->"
	manPromptInventoryStart      = `.\" BEGIN MCP PROMPT INVENTORY`
	manPromptInventoryEnd        = `.\" END MCP PROMPT INVENTORY`
)

type documentedTool struct {
	name                  string
	registrationCondition string
	invocationRequirement string
}

func TestDocumentedToolInventoryMatchesProductionDiscovery(t *testing.T) {
	documented := readAuthoritativeToolInventory(t)
	allDocumentedNames := make([]string, 0, len(documented))
	defaultDocumentedNames := make([]string, 0, len(documented))
	for _, tool := range documented {
		allDocumentedNames = append(allDocumentedNames, tool.name)
		switch tool.registrationCondition {
		case "Always":
			defaultDocumentedNames = append(defaultDocumentedNames, tool.name)
		case "`MCP_ALLOW_WRITE_OPS=true`":
		default:
			t.Fatalf("tool %q has unsupported registration condition %q", tool.name, tool.registrationCondition)
		}
		switch tool.invocationRequirement {
		case "None", "`MCP_ALLOW_WRITE_OPS=true`", "`METRICS_DB_ENABLED=true`":
		default:
			t.Fatalf("tool %q has unsupported invocation requirement %q", tool.name, tool.invocationRequirement)
		}
	}
	slices.Sort(allDocumentedNames)
	slices.Sort(defaultDocumentedNames)

	tests := []struct {
		name          string
		allowWriteOps bool
		want          []string
	}{
		{name: "default registration", want: defaultDocumentedNames},
		{name: "write operations enabled", allowWriteOps: true, want: allDocumentedNames},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newToolInventoryTestServer(t, tt.allowWriteOps)
			got := listProductionToolNames(t, server)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("production tools/list = %v, documented inventory = %v", got, tt.want)
			}
		})
	}

	manNames := readManPageToolInventory(t)
	if !slices.Equal(manNames, allDocumentedNames) {
		t.Fatalf("man-page tool inventory = %v, authoritative inventory = %v", manNames, allDocumentedNames)
	}
}

func TestDocumentedResourceAndPromptInventoriesMatchProductionDiscovery(t *testing.T) {
	server := newToolInventoryTestServer(t, false)
	tests := []struct {
		name          string
		markdownStart string
		markdownEnd   string
		manStart      string
		manEnd        string
		production    func(*testing.T, *Server) []string
	}{
		{
			name:          "fixed resources",
			markdownStart: markdownFixedResourceInventoryStart,
			markdownEnd:   markdownFixedResourceInventoryEnd,
			manStart:      manFixedResourceInventoryStart,
			manEnd:        manFixedResourceInventoryEnd,
			production:    listProductionFixedResourceURIs,
		},
		{
			name:          "resource URI templates",
			markdownStart: markdownResourceTemplateInventoryStart,
			markdownEnd:   markdownResourceTemplateInventoryEnd,
			manStart:      manResourceTemplateInventoryStart,
			manEnd:        manResourceTemplateInventoryEnd,
			production:    listProductionResourceTemplates,
		},
		{
			name:          "prompts",
			markdownStart: markdownPromptInventoryStart,
			markdownEnd:   markdownPromptInventoryEnd,
			manStart:      manPromptInventoryStart,
			manEnd:        manPromptInventoryEnd,
			production:    listProductionPromptNames,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			documented := readAuthoritativeIdentifierInventory(t, tt.name, tt.markdownStart, tt.markdownEnd)
			production := tt.production(t, server)
			if !slices.Equal(production, documented) {
				t.Fatalf("production discovery = %v, documented inventory = %v", production, documented)
			}
			man := readManPageInventory(t, tt.name, tt.manStart, tt.manEnd)
			if !slices.Equal(man, documented) {
				t.Fatalf("man-page inventory = %v, authoritative inventory = %v", man, documented)
			}
		})
	}
}

func TestShippedMCPDocumentsPointToTheAuthoritativeDiscoveryInventories(t *testing.T) {
	tests := []struct {
		path string
		link string
	}{
		{path: "../README.md", link: "docs/MCP-README.md#discovery-inventories"},
		{path: "../docs/MCP-BLUEPRINT.md", link: "MCP-README.md#discovery-inventories"},
		{path: "../docs/TECHNICAL-SPECIFICATION.md", link: "MCP-README.md#discovery-inventories"},
	}
	for _, tt := range tests {
		t.Run(filepath.Base(tt.path), func(t *testing.T) {
			content, err := os.ReadFile(tt.path)
			if err != nil {
				t.Fatalf("read %s: %v", tt.path, err)
			}
			if count := strings.Count(string(content), tt.link); count != 1 {
				t.Fatalf("%s authoritative inventory link count = %d, want 1", tt.path, count)
			}
		})
	}
}

func TestCurrentShippedMCPDescriptionsDoNotMaintainNumericDiscoveryCounts(t *testing.T) {
	pattern := regexp.MustCompile(`(?i)\b(?:resources|prompts)\s*\([0-9]+|\b[0-9]+\s+(?:MCP\s+resources|pre-built\s+prompts)\b`)
	paths := []string{
		"../README.md",
		"../docs/MCP-README.md",
		"../docs/MCP-BLUEPRINT.md",
		"../docs/TECHNICAL-SPECIFICATION.md",
		"../packaging/rpm/resman.spec",
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if strings.HasSuffix(path, ".spec") {
				current, _, found := strings.Cut(string(content), "%prep")
				if !found {
					t.Fatalf("%s has no %%prep boundary before the historical changelog", path)
				}
				content = []byte(current)
			}
			if match := pattern.Find(content); match != nil {
				t.Fatalf("%s contains independent MCP discovery count %q", path, match)
			}
		})
	}
}

func readAuthoritativeToolInventory(t *testing.T) []documentedTool {
	t.Helper()
	content, err := os.ReadFile("../docs/MCP-README.md")
	if err != nil {
		t.Fatalf("read MCP-README.md: %v", err)
	}
	section := inventorySection(t, string(content), markdownToolInventoryStart, markdownToolInventoryEnd)
	seen := make(map[string]struct{})
	tools := make([]documentedTool, 0)
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		fields := strings.Split(strings.Trim(line, "|"), "|")
		if len(fields) != 4 {
			t.Fatalf("invalid authoritative inventory row %q", line)
		}
		name := strings.Trim(strings.TrimSpace(fields[0]), "`")
		if _, duplicate := seen[name]; duplicate {
			t.Fatalf("tool %q appears more than once in the authoritative inventory", name)
		}
		seen[name] = struct{}{}
		tools = append(tools, documentedTool{
			name:                  name,
			registrationCondition: strings.TrimSpace(fields[2]),
			invocationRequirement: strings.TrimSpace(fields[3]),
		})
	}
	if len(tools) == 0 {
		t.Fatal("authoritative MCP tool inventory is empty")
	}
	return tools
}

func readManPageToolInventory(t *testing.T) []string {
	return readManPageInventory(t, "tools", manToolInventoryStart, manToolInventoryEnd)
}

func readAuthoritativeIdentifierInventory(t *testing.T, kind, startMarker, endMarker string) []string {
	t.Helper()
	content, err := os.ReadFile("../docs/MCP-README.md")
	if err != nil {
		t.Fatalf("read MCP-README.md: %v", err)
	}
	section := inventorySection(t, string(content), startMarker, endMarker)
	seen := make(map[string]struct{})
	identifiers := make([]string, 0)
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		fields := strings.Split(strings.Trim(line, "|"), "|")
		if len(fields) != 2 {
			t.Fatalf("invalid authoritative %s inventory row %q", kind, line)
		}
		identifier := strings.Trim(strings.TrimSpace(fields[0]), "`")
		if _, duplicate := seen[identifier]; duplicate {
			t.Fatalf("%s identifier %q appears more than once in the authoritative inventory", kind, identifier)
		}
		seen[identifier] = struct{}{}
		identifiers = append(identifiers, identifier)
	}
	if len(identifiers) == 0 {
		t.Fatalf("authoritative MCP %s inventory is empty", kind)
	}
	slices.Sort(identifiers)
	return identifiers
}

func readManPageInventory(t *testing.T, kind, startMarker, endMarker string) []string {
	t.Helper()
	content, err := os.ReadFile("../docs/resman.8")
	if err != nil {
		t.Fatalf("read resman.8: %v", err)
	}
	section := inventorySection(t, string(content), startMarker, endMarker)
	lines := strings.Split(section, "\n")
	names := make([]string, 0)
	seen := make(map[string]struct{})
	for index, line := range lines {
		if strings.TrimSpace(line) != ".B" && !strings.HasPrefix(strings.TrimSpace(line), ".B ") {
			continue
		}
		trimmed := strings.TrimSpace(line)
		var name string
		if trimmed == ".B" {
			if index+1 >= len(lines) {
				t.Fatalf("man-page inventory ends after .B")
			}
			name = strings.TrimSpace(lines[index+1])
		} else {
			name = strings.TrimSpace(strings.TrimPrefix(trimmed, ".B "))
		}
		if _, duplicate := seen[name]; duplicate {
			t.Fatalf("%s identifier %q appears more than once in the man-page inventory", kind, name)
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func inventorySection(t *testing.T, content, startMarker, endMarker string) string {
	t.Helper()
	if count := strings.Count(content, startMarker); count != 1 {
		t.Fatalf("inventory start marker %q count = %d, want 1", startMarker, count)
	}
	if count := strings.Count(content, endMarker); count != 1 {
		t.Fatalf("inventory end marker %q count = %d, want 1", endMarker, count)
	}
	start := strings.Index(content, startMarker)
	start += len(startMarker)
	endRelative := strings.Index(content[start:], endMarker)
	return content[start : start+endRelative]
}

func newToolInventoryTestServer(t *testing.T, allowWriteOps bool) *Server {
	t.Helper()
	cfg := config.DefaultConfig()
	configureMCPTestTLS(t, cfg)
	cfg.MCPEnabled = true
	cfg.MCPTransport = "http"
	cfg.MCPHTTPHost = "127.0.0.1"
	cfg.MCPAuthToken = protocolTestToken
	cfg.MCPAllowWriteOps = allowWriteOps
	server, err := NewServer(cfg, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return server
}

func listProductionToolNames(t *testing.T, server *Server) []string {
	t.Helper()
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	callProductionDiscovery(t, server, "tools/list", &result)
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func listProductionFixedResourceURIs(t *testing.T, server *Server) []string {
	t.Helper()
	var result struct {
		Resources []struct {
			URI string `json:"uri"`
		} `json:"resources"`
	}
	callProductionDiscovery(t, server, "resources/list", &result)
	identifiers := make([]string, 0, len(result.Resources))
	for _, resource := range result.Resources {
		identifiers = append(identifiers, resource.URI)
	}
	slices.Sort(identifiers)
	return identifiers
}

func listProductionResourceTemplates(t *testing.T, server *Server) []string {
	t.Helper()
	var result struct {
		ResourceTemplates []struct {
			URITemplate string `json:"uriTemplate"`
		} `json:"resourceTemplates"`
	}
	callProductionDiscovery(t, server, "resources/templates/list", &result)
	identifiers := make([]string, 0, len(result.ResourceTemplates))
	for _, resource := range result.ResourceTemplates {
		identifiers = append(identifiers, resource.URITemplate)
	}
	slices.Sort(identifiers)
	return identifiers
}

func listProductionPromptNames(t *testing.T, server *Server) []string {
	t.Helper()
	var result struct {
		Prompts []struct {
			Name string `json:"name"`
		} `json:"prompts"`
	}
	callProductionDiscovery(t, server, "prompts/list", &result)
	identifiers := make([]string, 0, len(result.Prompts))
	for _, prompt := range result.Prompts {
		identifiers = append(identifiers, prompt.Name)
	}
	slices.Sort(identifiers)
	return identifiers
}

func callProductionDiscovery(t *testing.T, server *Server, method string, result any) {
	t.Helper()
	request := newProtocolHTTPRequest(t, method, latestProtocolParams(nil))
	recorder := httptest.NewRecorder()
	server.newMCPHTTPHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s HTTP status = %d, want 200; body: %s", method, recorder.Code, recorder.Body.String())
	}
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode %s response: %v", method, err)
	}
	if len(response.Error) != 0 && string(response.Error) != "null" {
		t.Fatalf("%s returned protocol error: %s", method, response.Error)
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		t.Fatalf("decode %s result: %v", method, err)
	}
}
