package main

import (
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fdefilippo/resman/internal/app"
)

func TestApplicationExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "transient runtime failure", err: errors.New("listener temporarily unavailable"), want: exitStatusFailure},
		{name: "configuration or capability rejection", err: app.NewPermanentStartupError(errors.New("operator action required")), want: exitStatusConfiguration},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := applicationExitCode(tt.err); got != tt.want {
				t.Fatalf("applicationExitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestInvalidConfigurationExitsWithPermanentStartupStatus(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		required []string
	}{
		{name: "invalid threshold", content: "CPU_THRESHOLD=0\n"},
		{
			name:    "comma-bearing regex quantifier",
			content: "USER_INCLUDE_LIST=^svc[0-9]{2,4}$\n",
			required: []string{
				"USER_INCLUDE_LIST", "^svc[0-9]{2,4}$", "comma is the list separator", "expanding the alternatives",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "invalid.conf")
			if err := os.WriteFile(configPath, []byte(tt.content), 0600); err != nil {
				t.Fatalf("write invalid configuration: %v", err)
			}

			cmd := exec.Command(os.Args[0], "-test.run=TestMainInvalidConfigurationHelper")
			cmd.Env = append(os.Environ(),
				"RESMAN_MAIN_HELPER=invalid-configuration",
				"RESMAN_MAIN_HELPER_CONFIG="+configPath,
			)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("main subprocess error = %v, want exit status %d; output=%s", err, exitStatusConfiguration, output)
			}
			if got := exitErr.ExitCode(); got != exitStatusConfiguration {
				t.Fatalf("main subprocess exit status = %d, want %d; output=%s", got, exitStatusConfiguration, output)
			}
			if !strings.Contains(string(output), "Failed to load configuration") {
				t.Fatalf("main subprocess output lacks configuration diagnostic: %s", output)
			}
			for _, required := range tt.required {
				if !strings.Contains(string(output), required) {
					t.Errorf("main subprocess output %q omits %q", output, required)
				}
			}
		})
	}
}

func TestMainInvalidConfigurationHelper(t *testing.T) {
	if os.Getenv("RESMAN_MAIN_HELPER") != "invalid-configuration" {
		return
	}

	flag.CommandLine = flag.NewFlagSet("resman", flag.ContinueOnError)
	os.Args = []string{"resman", "--config", os.Getenv("RESMAN_MAIN_HELPER_CONFIG")}
	main()
	t.Fatal("main returned after invalid configuration")
}
