package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

type startupCaptureLogger struct {
	errors []string
}

func (*startupCaptureLogger) Debug(string, ...interface{}) {}
func (*startupCaptureLogger) Info(string, ...interface{})  {}
func (*startupCaptureLogger) Warn(string, ...interface{})  {}
func (l *startupCaptureLogger) Error(message string, fields ...interface{}) {
	l.errors = append(l.errors, fmt.Sprint(append([]interface{}{message, " "}, fields...)...))
}
func (*startupCaptureLogger) InfoChecked(string, ...interface{}) error { return nil }

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

func TestCPUPointsPolicyRejectionIsPermanentAndOperatorVisible(t *testing.T) {
	policyDir := t.TempDir()
	if err := os.Chmod(policyDir, 0700); err != nil {
		t.Fatalf("secure CPU Points policy directory: %v", err)
	}
	policyPath := filepath.Join(policyDir, "cpu-points.map")
	if err := os.WriteFile(policyPath, []byte("[resman-cpu-points-map-v1]\nroot=801\n"), 0600); err != nil {
		t.Fatalf("write CPU Points policy: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.CPUReservePoints = 100
	cfg.CPUBestEffortPoints = 100
	cfg.CPUPointsFile = policyPath
	logger := &startupCaptureLogger{}
	application := &App{cfg: cfg, logger: logger}

	err := application.WithStateManager().Run()
	if !IsPermanentStartupError(err) {
		t.Fatalf("Run() error = %v, want permanent CPU Points rejection", err)
	}
	if !strings.Contains(err.Error(), "configured guarantees 801 plus best effort 100") {
		t.Fatalf("Run() error = %v, want overcommit diagnostic", err)
	}
	if len(logger.errors) != 1 || !strings.Contains(logger.errors[0], "configured guarantees 801 plus best effort 100") {
		t.Fatalf("operator error records = %q, want one overcommit diagnostic", logger.errors)
	}
}
