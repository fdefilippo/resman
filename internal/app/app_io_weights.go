package app

import (
	"time"

	"github.com/fdefilippo/resman/state"
)

var ioWeightRetryDelays = [...]time.Duration{
	time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
	30 * time.Second,
}

type ioWeightRetryTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type ioWeightRetryClock interface {
	NewTimer(time.Duration) ioWeightRetryTimer
}

type systemIOWeightRetryClock struct{}

func (systemIOWeightRetryClock) NewTimer(delay time.Duration) ioWeightRetryTimer {
	return systemIOWeightRetryTimer{timer: time.NewTimer(delay)}
}

type systemIOWeightRetryTimer struct{ timer *time.Timer }

func (t systemIOWeightRetryTimer) C() <-chan time.Time { return t.timer.C }
func (t systemIOWeightRetryTimer) Stop() bool          { return t.timer.Stop() }

type ioWeightRetryController struct {
	clock ioWeightRetryClock
	timer ioWeightRetryTimer
	c     <-chan time.Time
	step  int
	delay time.Duration
}

func newIOWeightRetryController() *ioWeightRetryController {
	return newIOWeightRetryControllerWithClock(systemIOWeightRetryClock{})
}

func newIOWeightRetryControllerWithClock(clock ioWeightRetryClock) *ioWeightRetryController {
	return &ioWeightRetryController{clock: clock}
}

func (c *ioWeightRetryController) C() <-chan time.Time { return c.c }

func (c *ioWeightRetryController) Reset() {
	c.Stop()
	c.step = 0
}

func (c *ioWeightRetryController) Schedule(result state.IODeviceWeightAttemptResult) time.Duration {
	c.Stop()
	if !result.Retry {
		return 0
	}
	delay := ioWeightRetryDelays[len(ioWeightRetryDelays)-1]
	if result.Status.State != state.IODeviceWeightFunctionallyAccepted {
		index := c.step
		if index >= len(ioWeightRetryDelays) {
			index = len(ioWeightRetryDelays) - 1
		}
		delay = ioWeightRetryDelays[index]
		if c.step < len(ioWeightRetryDelays)-1 {
			c.step++
		}
	} else {
		c.step = len(ioWeightRetryDelays) - 1
	}
	c.timer = c.clock.NewTimer(delay)
	c.c = c.timer.C()
	c.delay = delay
	return delay
}

func (c *ioWeightRetryController) Stop() {
	if c.timer != nil {
		if !c.timer.Stop() {
			select {
			case <-c.timer.C():
			default:
			}
		}
	}
	c.timer = nil
	c.c = nil
	c.delay = 0
}
