package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

func TestPermanentStartupErrorClassification(t *testing.T) {
	sentinel := errors.New("sentinel")
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", want: false},
		{name: "ordinary error", err: sentinel, want: false},
		{name: "permanent startup error", err: NewPermanentStartupError(sentinel), want: true},
		{name: "wrapped permanent startup error", err: errors.Join(errors.New("context"), NewPermanentStartupError(sentinel)), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsPermanentStartupError(tt.err); got != tt.want {
				t.Fatalf("IsPermanentStartupError() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestCgroupCapabilityRejectionIsPermanent(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CgroupRoot = t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	application := NewApp(cfg, "", ctx, cancel, nil, logging.GetLogger()).WithCgroupManager()

	err := application.Run()
	if !IsPermanentStartupError(err) {
		t.Fatalf("Run() error = %v, want permanent startup rejection", err)
	}
	if !strings.Contains(err.Error(), "cgroup.controllers") {
		t.Fatalf("Run() error = %v, want missing capability diagnostic", err)
	}
}

func TestCgroupSetupFailureRemainsRestartable(t *testing.T) {
	injected := errors.New("transient subtree_control contention")
	err := classifyCgroupStartupError(injected)
	if !errors.Is(err, injected) {
		t.Fatalf("classifyCgroupStartupError() error = %v, want injected setup failure", err)
	}
	if IsPermanentStartupError(err) {
		t.Fatalf("classifyCgroupStartupError() marked transient setup failure permanent: %v", err)
	}
}

func TestMCPMissingTLSCredentialsIsPermanent(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MCPEnabled = true
	cfg.MCPTransport = "http"
	cfg.MCPAuthToken = "test-token"
	cfg.MCPTLSEnabled = true
	cfg.MCPTLSCertFile = filepath.Join(t.TempDir(), "missing.crt")
	cfg.MCPTLSKeyFile = filepath.Join(t.TempDir(), "missing.key")
	cfg.MCPTLSMinVersion = "1.3"

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	application := NewApp(cfg, "", ctx, cancel, nil, logging.GetLogger()).WithMCPServer()

	err := application.Run()
	if !IsPermanentStartupError(err) {
		t.Fatalf("Run() error = %v, want permanent startup rejection", err)
	}
	if !strings.Contains(err.Error(), "loading MCP TLS configuration") {
		t.Fatalf("Run() error = %v, want TLS diagnostic", err)
	}
}
