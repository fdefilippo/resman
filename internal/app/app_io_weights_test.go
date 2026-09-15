package app

import (
	"testing"

	"github.com/fdefilippo/resman/state"
)

func TestIOWeightRetryControllerUsesApprovedCappedBackoff(t *testing.T) {
	controller := newIOWeightRetryController()
	defer controller.Stop()
	want := ioWeightRetryDelays[:]
	for index, expected := range want {
		controller.Schedule(state.IODeviceWeightAttemptResult{Retry: true, Status: state.IODeviceWeightStatus{State: state.IODeviceWeightRequestedPending}})
		if controller.timer == nil {
			t.Fatalf("step %d did not create a timer", index)
		}
		if controller.delay != expected {
			t.Fatalf("step %d delay = %s, want %s", index, controller.delay, expected)
		}
	}
	if controller.step != len(ioWeightRetryDelays)-1 {
		t.Fatalf("retry step = %d, want capped %d", controller.step, len(ioWeightRetryDelays)-1)
	}
	controller.Schedule(state.IODeviceWeightAttemptResult{Retry: true, Status: state.IODeviceWeightStatus{State: state.IODeviceWeightFunctionallyAccepted}})
	if controller.step != len(ioWeightRetryDelays)-1 {
		t.Fatalf("accepted step = %d, want capped", controller.step)
	}
	controller.Reset()
	if controller.step != 0 || controller.C() != nil {
		t.Fatalf("Reset() left step=%d channel=%v", controller.step, controller.C())
	}
}
