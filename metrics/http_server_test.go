package metrics

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestMetricsHTTPServerBoundsSlowHeaders(t *testing.T) {
	server := newMetricsHTTPServer("127.0.0.1:0", http.NewServeMux(), nil)
	if server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 || server.WriteTimeout <= 0 || server.IdleTimeout <= 0 {
		t.Fatal("metrics HTTP deadlines must all be bounded")
	}
	server.ReadHeaderTimeout = 20 * time.Millisecond
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { _ = server.Close(); <-done }()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close resource: %v", err)
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("GET /metrics HTTP/1.1\r\nHost: localhost\r\nX-Slow: ")); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return
	} // Go may close without a response.
	if err != nil {
		t.Fatalf("slow header connection not closed by server: %v", err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close resource: %v", err)
		}
	}()
	if response.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408", response.StatusCode)
	}
}
