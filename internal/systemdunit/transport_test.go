package systemdunit

import (
	"context"
	"testing"
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
