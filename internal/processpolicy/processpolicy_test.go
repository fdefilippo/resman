package processpolicy

import (
	"testing"

	"github.com/fdefilippo/resman/config"
)

func TestEvaluateUsesOneCanonicalIdentityForAnchoredExclusions(t *testing.T) {
	tests := []struct {
		name       string
		executable string
		comm       string
		wantName   string
	}{
		{name: "executable basename", executable: "/usr/lib/systemd/systemd", comm: "systemd", wantName: "systemd"},
		{name: "deleted executable suffix", executable: "/usr/lib/systemd/systemd (deleted)", comm: "systemd", wantName: "systemd"},
	}

	cfg := config.DefaultConfig()
	cfg.ProcessExcludeList = []string{"^systemd$", "^dbus-broker$"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selection := Evaluate(cfg, tt.executable, tt.comm)
			if selection.Name != tt.wantName || selection.Enforceable || !selection.IdentityTrusted {
				t.Fatalf("Evaluate() = %+v, want trusted name %q and enforceable=false", selection, tt.wantName)
			}
		})
	}
}

func TestEvaluateTreatsCommFallbackAsEnforceableAndUntrusted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ProcessExcludeList = []string{"^systemd$"}

	selection := Evaluate(cfg, "", "systemd")
	if selection.Name != "systemd" || !selection.Enforceable || selection.IdentityTrusted {
		t.Fatalf("Evaluate() = %+v, want display name systemd, enforceable=true, trusted=false", selection)
	}
}

func TestEvaluateDoesNotTrustSpoofableProcessNameWhenExecutableIsAvailable(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ProcessExcludeList = []string{"^systemd$"}
	selection := Evaluate(cfg, "/usr/bin/stress", "systemd")
	if selection.Name != "stress" || !selection.Enforceable || !selection.IdentityTrusted {
		t.Fatalf("Evaluate() = %+v, want executable identity stress and enforceable=true", selection)
	}
}

func TestEvaluateIncludesUnmatchedProcess(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ProcessExcludeList = []string{"^systemd$"}
	selection := Evaluate(cfg, "/usr/bin/stress", "stress")
	if selection.Name != "stress" || !selection.Enforceable || !selection.IdentityTrusted {
		t.Fatalf("Evaluate() = %+v, want stress and enforceable=true", selection)
	}
}
