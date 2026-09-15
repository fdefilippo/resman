package app

import (
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
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

func TestIOWeightRetryCadenceIsIndependentFromPSIControlMode(t *testing.T) {
	tests := []struct {
		name                string
		psiConfigured       bool
		psiActive           bool
		wantControlInterval int
	}{
		{name: "polling", wantControlInterval: 17},
		{name: "PSI event driven", psiConfigured: true, psiActive: true, wantControlInterval: 300},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.PSIEventDriven = test.psiConfigured
			cfg.PollingInterval = 17
			cfg.PSIFallbackInterval = 300
			manager, err := state.NewManager(cfg, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			application := &App{cfg: cfg, stateManager: manager, psiEventDriven: test.psiActive}
			if got := application.controlCycleInterval(); got != test.wantControlInterval {
				t.Fatalf("controlCycleInterval() = %d, want %d", got, test.wantControlInterval)
			}

			clock := &fakeIOWeightRetryClock{}
			controller := newIOWeightRetryControllerWithClock(clock)
			defer controller.Stop()
			for range len(ioWeightRetryDelays) + 2 {
				controller.Schedule(state.IODeviceWeightAttemptResult{Retry: true, Status: state.IODeviceWeightStatus{State: state.IODeviceWeightRequestedPending}})
			}
			want := append(append([]time.Duration(nil), ioWeightRetryDelays[:]...), 30*time.Second, 30*time.Second)
			if len(clock.delays) != len(want) {
				t.Fatalf("retry delays = %v, want %v", clock.delays, want)
			}
			for index := range want {
				if clock.delays[index] != want[index] {
					t.Fatalf("retry delay %d = %s, want %s", index, clock.delays[index], want[index])
				}
			}
		})
	}
}
