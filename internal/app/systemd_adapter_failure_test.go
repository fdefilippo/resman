package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/systemdunit"
)

// injectedSystemdAdapters answers every construction with a prepared outcome
// and counts how often enforcement was attempted.
type injectedSystemdAdapters struct {
	err     error
	refusal *systemdunit.CapabilityRefusal

	mu           sync.Mutex
	newCalls     int
	observeCalls int
}

func (f *injectedSystemdAdapters) New(context.Context, string, time.Duration, systemdunit.StartupRequirements) (*systemdunit.Adapter, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newCalls++
	return nil, f.err
}

func (f *injectedSystemdAdapters) NewOrObserve(context.Context, string, time.Duration, systemdunit.StartupRequirements) (*systemdunit.Adapter, *systemdunit.CapabilityRefusal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observeCalls++
	return nil, f.refusal, f.err
}

func (f *injectedSystemdAdapters) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.newCalls + f.observeCalls
}

// recordingLogger keeps every message with its key/value pairs.
type recordingLogger struct {
	mu      sync.Mutex
	entries []string
}

func (l *recordingLogger) record(level, message string, fields ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, fmt.Sprintf("%s %s %v", level, message, fields))
}

func (l *recordingLogger) Debug(message string, fields ...interface{}) {
	l.record("DEBUG", message, fields...)
}
func (l *recordingLogger) Info(message string, fields ...interface{}) {
	l.record("INFO", message, fields...)
}
func (l *recordingLogger) Warn(message string, fields ...interface{}) {
	l.record("WARN", message, fields...)
}
func (l *recordingLogger) Error(message string, fields ...interface{}) {
	l.record("ERROR", message, fields...)
}
func (l *recordingLogger) InfoChecked(message string, fields ...interface{}) error {
	l.record("INFO", message, fields...)
	return nil
}

func (l *recordingLogger) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.entries, "\n")
}

func systemdOwnedHostStatus() cgroup.EnforcementStatus {
	return cgroup.EnforcementStatus{
		Mode:   cgroup.EnforcementModeObservationOnly,
		Reason: cgroup.EnforcementReasonSystemdOwnsHostWorkloads,
	}
}

func applicationWithInjectedAdapters(t *testing.T, declared config.EnforcementPolicy, adapters *injectedSystemdAdapters) (*App, *recordingLogger) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.EnforcementMode = string(declared)
	logger := &recordingLogger{}
	return &App{cfg: cfg, ctx: context.Background(), logger: logger, systemdAdapters: adapters}, logger
}

func TestAdapterFailureNeverDegradesAConfiguredEnforcerToObservation(t *testing.T) {
	// resman-318.3: on vmgclalpr2111 the adapter could not reach systemd at
	// boot and the daemon observed for two days under the default
	// systemd_native, publishing systemd_owns_host_workloads. Every failure
	// that is not a definitive capability refusal must now stop startup and
	// let the service manager retry it, under both enforcing declarations.
	failures := []struct {
		name   string
		reason systemdunit.ErrorReason
	}{
		{name: "systemd connection unavailable", reason: systemdunit.ReasonBusUnavailable},
		{name: "call timeout", reason: systemdunit.ReasonTimeout},
		{name: "authorization denied", reason: systemdunit.ReasonAuthorizationDenied},
		{name: "lease journal unreadable", reason: systemdunit.ReasonLeaseStore},
	}
	declarations := []config.EnforcementPolicy{
		config.EnforcementPolicySystemdNative,
		config.EnforcementPolicyAuto,
	}

	for _, declared := range declarations {
		for _, failure := range failures {
			t.Run(string(declared)+"/"+failure.name, func(t *testing.T) {
				injected := &systemdunit.AdapterError{
					Reason: failure.reason, Operation: "construct",
					Err: errors.New("dial unix /run/systemd/private: connect: no such file or directory"),
				}
				adapters := &injectedSystemdAdapters{err: injected}
				application, logger := applicationWithInjectedAdapters(t, declared, adapters)

				adapter, _, err := application.initializeSystemdEnforcement(systemdOwnedHostStatus())

				if adapter != nil {
					t.Fatal("a failed construction returned an adapter")
				}
				if err == nil {
					t.Fatal("startup continued in observation after an adapter failure")
				}
				if IsPermanentStartupError(err) {
					t.Fatalf("a %s failure is permanent; the service manager must retry it: %v", failure.reason, err)
				}
				var adapterErr *systemdunit.AdapterError
				if !errors.As(err, &adapterErr) || adapterErr.Reason != failure.reason {
					t.Fatalf("startup error %v does not carry the %s failure", err, failure.reason)
				}
				if adapters.calls() != 1 {
					t.Fatalf("adapter constructions = %d, want exactly 1", adapters.calls())
				}
				logged := logger.joined()
				if !strings.Contains(logged, "failure_class "+string(failure.reason)) {
					t.Fatalf("the failure class %s was not logged:\n%s", failure.reason, logged)
				}
				if strings.Contains(logged, string(cgroup.EnforcementReasonSystemdOwnsHostWorkloads)) {
					t.Fatalf("an adapter failure was published as %s:\n%s",
						cgroup.EnforcementReasonSystemdOwnsHostWorkloads, logged)
				}
				if strings.Contains(logged, "remaining observation-only") {
					t.Fatalf("an adapter failure still degrades to observation:\n%s", logged)
				}
			})
		}
	}
}

func TestDefinitiveCapabilityRefusalKeepsItsDeclaredOutcome(t *testing.T) {
	capability := systemdunit.MissingCapability{
		Feature: "CPU limiting", Controller: "cpu", InterfaceName: "cpu.max", Property: "CPUQuotaPerSecUSec",
	}
	missing := &systemdunit.AdapterError{
		Reason: systemdunit.ReasonRequiredCapability, Operation: "startup_capabilities",
		Err: errors.New("cpu.max unavailable"),
	}

	t.Run("systemd_native refuses permanently", func(t *testing.T) {
		adapters := &injectedSystemdAdapters{err: missing}
		application, _ := applicationWithInjectedAdapters(t, config.EnforcementPolicySystemdNative, adapters)
		_, _, err := application.initializeSystemdEnforcement(systemdOwnedHostStatus())
		if err == nil || !IsPermanentStartupError(err) {
			t.Fatalf("missing capability under systemd_native = %v, want a permanent startup error", err)
		}
	})

	t.Run("auto observes with the refusal reason", func(t *testing.T) {
		adapters := &injectedSystemdAdapters{refusal: &systemdunit.CapabilityRefusal{Capability: capability, Err: missing}}
		application, _ := applicationWithInjectedAdapters(t, config.EnforcementPolicyAuto, adapters)
		adapter, status, err := application.initializeSystemdEnforcement(systemdOwnedHostStatus())
		if err != nil || adapter != nil {
			t.Fatalf("definitive refusal under auto = (%v, %v), want observation without error", adapter, err)
		}
		want := cgroup.EnforcementStatus{
			Mode:   cgroup.EnforcementModeObservationOnly,
			Reason: cgroup.EnforcementReasonMandatoryCapabilityUnavailable,
		}
		if status != want {
			t.Fatalf("status = %+v, want %+v", status, want)
		}
	})
}

func TestDeclaredObservationNeverConstructsTheAdapterUnderInjectedFailure(t *testing.T) {
	adapters := &injectedSystemdAdapters{err: &systemdunit.AdapterError{Reason: systemdunit.ReasonBusUnavailable}}
	application, _ := applicationWithInjectedAdapters(t, config.EnforcementPolicyObservationOnly, adapters)
	// The returned error depends on whether this host's lease directory is
	// inspectable; the contract under test is that no construction happens.
	_, _, _ = application.initializeSystemdEnforcement(systemdOwnedHostStatus())
	if adapters.calls() != 0 {
		t.Fatalf("observation_only constructed the adapter %d times", adapters.calls())
	}
}

func TestUnreachableSystemdRuntimeObservesWithoutOpeningTheAdapter(t *testing.T) {
	// The documented container deployment sees systemd as PID 1 through the
	// host PID namespace but not its runtime. It must keep observing under its
	// own reason, whatever the declaration, instead of failing to start on an
	// adapter that can never be reached from that mount namespace.
	unreachable := cgroup.EnforcementStatus{
		Mode:   cgroup.EnforcementModeObservationOnly,
		Reason: cgroup.EnforcementReasonSystemdRuntimeUnreachable,
	}
	for _, declared := range []config.EnforcementPolicy{config.EnforcementPolicySystemdNative, config.EnforcementPolicyAuto} {
		t.Run(string(declared), func(t *testing.T) {
			adapters := &injectedSystemdAdapters{err: &systemdunit.AdapterError{Reason: systemdunit.ReasonBusUnavailable}}
			application, _ := applicationWithInjectedAdapters(t, declared, adapters)
			adapter, status, err := application.initializeSystemdEnforcement(unreachable)
			if err != nil || adapter != nil {
				t.Fatalf("unreachable runtime = (%v, %v), want observation without error", adapter, err)
			}
			if status != unreachable {
				t.Fatalf("status = %+v, want %+v", status, unreachable)
			}
			if adapters.calls() != 0 {
				t.Fatalf("the adapter was constructed %d times without a reachable runtime", adapters.calls())
			}
		})
	}
}
