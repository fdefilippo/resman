package app

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

func TestNotifySystemdReady(t *testing.T) {
	tests := []struct {
		name      string
		socket    string
		wantError string
	}{
		{name: "outside systemd"},
		{name: "invalid relative socket", socket: "notify.sock", wantError: "absolute or abstract"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NOTIFY_SOCKET", tt.socket)
			err := notifySystemdReady()
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("notifySystemdReady() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("notifySystemdReady() error = %v, want text %q", err, tt.wantError)
			}
		})
	}
}

func TestNotifySystemdReadyWritesReadyState(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen on notification socket: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close notification listener: %v", err)
		}
	})
	t.Setenv("NOTIFY_SOCKET", socket)

	if err := notifySystemdReady(); err != nil {
		t.Fatalf("notifySystemdReady() error = %v", err)
	}
	if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set notification deadline: %v", err)
	}
	buffer := make([]byte, 256)
	count, _, err := listener.ReadFromUnix(buffer)
	if err != nil {
		t.Fatalf("read readiness notification: %v", err)
	}
	if got := string(buffer[:count]); got != systemdReadyState {
		t.Fatalf("readiness notification = %q, want %q", got, systemdReadyState)
	}
}

func TestRunDoesNotNotifyReadinessAfterBootstrapFailure(t *testing.T) {
	sentinel := errors.New("bootstrap rejected")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	application := NewApp(config.DefaultConfig(), "", ctx, cancel, nil, logging.GetLogger())
	application.err = sentinel
	notified := false
	application.notifyReady = func() error {
		notified = true
		return nil
	}

	err := application.Run()
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run() error = %v, want bootstrap rejection", err)
	}
	if notified {
		t.Fatal("Run() announced readiness after bootstrap had failed")
	}
}

func TestRunPropagatesReadinessFailure(t *testing.T) {
	sentinel := errors.New("notification unavailable")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	application := NewApp(config.DefaultConfig(), "", ctx, cancel, nil, logging.GetLogger())
	application.notifyReady = func() error { return sentinel }

	err := application.Run()
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run() error = %v, want readiness failure", err)
	}
}
