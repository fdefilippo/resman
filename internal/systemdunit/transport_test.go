package systemdunit

import (
	"context"
	"testing"

	systemdbus "github.com/coreos/go-systemd/v22/dbus"
)

func TestDBusTransportLifecycleSurvivesCancelledRunContextUntilExplicitClose(t *testing.T) {
	runCtx, cancelRun := context.WithCancel(context.Background())
	ctx, cancel := newDBusTransportLifecycle(runCtx)
	cancelRun()
	select {
	case <-ctx.Done():
		t.Fatal("run-context cancellation closed the transport before shutdown restoration")
	default:
	}

	cancel()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("explicit transport close did not cancel its lifecycle")
	}
}

func TestTransportReachesPIDOneWithoutTheSystemBus(t *testing.T) {
	// resman-318.2: ResMan starts before sysinit.target, ahead of dbus.socket,
	// so the adapter must talk to PID 1 over its private socket.
	if systemdPrivateAddress != "unix:path=/run/systemd/private" {
		t.Fatalf("systemd transport address = %q, want the PID 1 private socket", systemdPrivateAddress)
	}
}

func TestCapabilityProbeUnitsCarryNoDefaultDependencies(t *testing.T) {
	// A probe with default dependencies is ordered after sysinit.target, which
	// waits for ResMan's readiness: the early-boot probe would deadlock until
	// its call timeout. Both the service and its slice must opt out.
	service, slice := capabilityProbeProperties("user-resmancapprobe0123456789abcdef.slice", nil)
	for name, properties := range map[string][]systemdbus.Property{"service": service, "slice": slice} {
		found := false
		for _, property := range properties {
			if property.Name != "DefaultDependencies" {
				continue
			}
			value, ok := property.Value.Value().(bool)
			if !ok || value {
				t.Fatalf("%s DefaultDependencies = %v, want false", name, property.Value)
			}
			found = true
		}
		if !found {
			t.Fatalf("%s properties do not set DefaultDependencies=false: %+v", name, properties)
		}
	}
}
