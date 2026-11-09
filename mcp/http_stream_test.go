package mcp

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMCPIdleDeadlinePreservesActiveStreamingResponse(t *testing.T) {
	release := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, "first")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = fmt.Fprintln(w, "last")
	})
	policy := newMCPHTTPServer("127.0.0.1:0", handler, nil)
	if policy.WriteTimeout != 0 || policy.IdleTimeout <= 0 {
		t.Fatal("invalid streaming timeout policy")
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config = policy
	server.Config.IdleTimeout = 10 * time.Millisecond
	server.Start()
	defer server.Close()
	defer close(release)
	client := server.Client()
	client.Timeout = 2 * time.Second
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close resource: %v", err)
		}
	}()
	scanner := bufio.NewScanner(response.Body)
	if !scanner.Scan() || scanner.Text() != "first" {
		t.Fatalf("first chunk unavailable: %v", scanner.Err())
	}
	// Keep this response active for longer than the connection's idle deadline.
	timer := time.NewTimer(30 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	release <- struct{}{}
	if !scanner.Scan() || scanner.Text() != "last" {
		t.Fatalf("stream expired at idle deadline: %v", scanner.Err())
	}
}
