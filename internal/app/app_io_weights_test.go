package app

import (
	"testing"
	"time"

	"github.com/fdefilippo/resman/state"
)

type fakeIOWeightRetryTimer struct {
	channel chan time.Time
	stopped bool
}

func (t *fakeIOWeightRetryTimer) C() <-chan time.Time { return t.channel }
func (t *fakeIOWeightRetryTimer) Stop() bool {
	wasActive := !t.stopped
	t.stopped = true
	return wasActive
}

type fakeIOWeightRetryClock struct {
	delays []time.Duration
	timers []*fakeIOWeightRetryTimer
}

func (c *fakeIOWeightRetryClock) NewTimer(delay time.Duration) ioWeightRetryTimer {
	timer := &fakeIOWeightRetryTimer{channel: make(chan time.Time, 1)}
	c.delays = append(c.delays, delay)
	c.timers = append(c.timers, timer)
	return timer
}

func TestIOWeightRetryControllerUsesApprovedCappedBackoff(t *testing.T) {
	clock := &fakeIOWeightRetryClock{}
	controller := newIOWeightRetryControllerWithClock(clock)
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
		if clock.delays[index] != expected {
			t.Fatalf("fake clock step %d = %s, want %s", index, clock.delays[index], expected)
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
