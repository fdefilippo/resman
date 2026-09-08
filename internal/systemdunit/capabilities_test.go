package systemdunit

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestStartupCapabilitiesCheckCPUAndOnlyEnabledOptionalResources(t *testing.T) {
	transport := newFakeUnitTransport()
	verifier := &fakeKernelVerifier{}
	adapter := mustTestAdapter(t, transport, verifier)

	if err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{Memory: true}); err != nil {
		t.Fatalf("requireStartupCapabilities() error = %v", err)
	}
	want := []PropertyName{PropertyCPUQuotaPerSecUSec, PropertyMemoryHigh, PropertyMemoryMax}
	if len(verifier.preflightCalls) != len(want) {
		t.Fatalf("preflight calls = %d, want %d", len(verifier.preflightCalls), len(want))
	}
	for index, property := range want {
		if got := verifier.preflightCalls[index][0].Name(); got != property {
			t.Fatalf("preflight call %d property = %s, want %s", index, got, property)
		}
	}
}

func TestStartupCapabilitiesClassifyEnabledIOMissingInterface(t *testing.T) {
	transport := newFakeUnitTransport()
	verifier := &fakeKernelVerifier{preflightByName: map[PropertyName]error{
		PropertyIOReadBandwidthMax: errors.New("io.max is absent"),
	}}
	adapter := mustTestAdapter(t, transport, verifier)

	err := adapter.requireStartupCapabilities(context.Background(), StartupRequirements{IO: true})
	if !IsRequiredCapabilityError(err) {
		t.Fatalf("requireStartupCapabilities() error = %v, want required capability", err)
	}
	for _, part := range []string{"I/O limiting", `controller "io"`, `interface "io.max"`} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("requireStartupCapabilities() error = %v, want %q", err, part)
		}
	}
}
