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
