package systemdunit

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestStartupCapabilitiesUseTransientSystemdProbesAndOnlyEnabledResources(t *testing.T) {
	transport := newFakeUnitTransport()
	verifier := &fakeKernelVerifier{}
	adapter := mustTestAdapter(t, transport, verifier)
	capabilities, err := startupCapabilities(StartupRequirements{Memory: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range capabilities {
		if err := adapter.probeStartupCapability(context.Background(), transport, capability); err != nil {
			t.Fatalf("probeStartupCapability(%s) error = %v", capability.interfaceName, err)
		}
	}
	want := []PropertyName{PropertyCPUQuotaPerSecUSec, PropertyMemoryHigh, PropertyMemoryMax}
	if len(verifier.preflightCalls) != len(want) || len(transport.probeStarts) != len(want) || len(transport.probeStops) != len(want) {
		t.Fatalf("calls: preflight=%d start=%d stop=%d, want %d each", len(verifier.preflightCalls), len(transport.probeStarts), len(transport.probeStops), len(want))
	}
	for index, property := range want {
		if got := verifier.preflightCalls[index][0].Name(); got != property {
			t.Fatalf("preflight call %d property = %s, want %s", index, got, property)
		}
		if transport.probeStarts[index].unit != transport.probeStops[index] {
			t.Fatalf("probe %d start=%s stop=%s", index, transport.probeStarts[index].unit, transport.probeStops[index])
		}
	}
}

func TestStartupCapabilitiesClassifyEnabledIOMissingInterfaceAndCleanProbe(t *testing.T) {
	transport := newFakeUnitTransport()
	verifier := &fakeKernelVerifier{preflightByName: map[PropertyName]error{
		PropertyIOReadBandwidthMax: errors.New("io.max is absent"),
	}}
	adapter := mustTestAdapter(t, transport, verifier)
	assignment, err := NewDevicePropertyAssignment(PropertyIOReadBandwidthMax, []DeviceLimit{{Path: "/dev/vda", Value: 1 << 30}})
	if err != nil {
		t.Fatal(err)
	}
	capability := startupCapability{feature: "I/O limiting", controller: "io", interfaceName: "io.max", assignment: assignment}
	err = adapter.probeStartupCapability(context.Background(), transport, capability)
	if !IsRequiredCapabilityError(err) {
		t.Fatalf("probeStartupCapability() error = %v, want required capability", err)
	}
	for _, part := range []string{"I/O limiting", `controller "io"`, `interface "io.max"`} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("probeStartupCapability() error = %v, want %q", err, part)
		}
	}
	if len(transport.probeStarts) != 1 || len(transport.probeStops) != 1 || transport.probeStarts[0].unit != transport.probeStops[0] {
		t.Fatalf("probe cleanup calls start=%v stop=%v", transport.probeStarts, transport.probeStops)
	}
}

func TestStartupCapabilitiesPropagateProbeCleanupFailure(t *testing.T) {
	transport := newFakeUnitTransport()
	transport.probeStopErr = errors.New("injected cleanup failure")
	adapter := mustTestAdapter(t, transport, &fakeKernelVerifier{})
	assignment, err := NewPropertyAssignment(PropertyCPUQuotaPerSecUSec, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	err = adapter.probeStartupCapability(context.Background(), transport, startupCapability{
		feature: "CPU limiting", controller: "cpu", interfaceName: "cpu.max", assignment: assignment,
	})
	if err == nil || !strings.Contains(err.Error(), "injected cleanup failure") {
		t.Fatalf("probeStartupCapability() error = %v, want cleanup failure", err)
	}
}
