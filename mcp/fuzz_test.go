package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// FuzzMCPHTTPHandler drives the composed HTTP handler — auth, logging and the
// latest-only protocol middleware — with arbitrary credentials and bodies. The
// handler faces the network, so the property that matters is that no input
// reaches a successful response without the configured bearer token.
func FuzzMCPHTTPHandler(f *testing.F) {
	for _, seed := range []struct {
		token string
		body  string
	}{
		{protocolTestToken, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`},
		{protocolTestToken, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`},
		{"", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`},
		{"wrong-token", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`},
		{protocolTestToken + "x", `{}`},
		{protocolTestToken, ""},
		{protocolTestToken, "{"},
		{protocolTestToken, "\x00\x01\x02"},
		{protocolTestToken, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`},
		{protocolTestToken, strings.Repeat(`{"a":`, 512)},
	} {
		f.Add(seed.token, seed.body)
	}

	server := newProtocolTestServer(f)
	handler := server.newMCPHTTPHandler()

	f.Fuzz(func(t *testing.T, token, body string) {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set(mcpProtocolVersionHeader, mcpProtocolVersion)
		request.Header.Set(mcpMethodHeader, mcpMethodDiscover)

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if recorder.Code < 100 || recorder.Code > 599 {
			t.Fatalf("handler wrote status %d, want a valid HTTP status", recorder.Code)
		}
		if token != protocolTestToken && recorder.Code == http.StatusOK {
			t.Fatalf("handler answered %d for bearer token %q, want rejection", recorder.Code, token)
		}
	})
}
