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

type ioWeightRetryController struct {
	timer *time.Timer
	c     <-chan time.Time
	step  int
	delay time.Duration
}

func newIOWeightRetryController() *ioWeightRetryController {
	return &ioWeightRetryController{}
}

func (c *ioWeightRetryController) C() <-chan time.Time { return c.c }

func (c *ioWeightRetryController) Reset() {
	c.Stop()
	c.step = 0
}

func (c *ioWeightRetryController) Schedule(result state.IODeviceWeightAttemptResult) {
	c.Stop()
	if !result.Retry {
		return
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
	c.timer = time.NewTimer(delay)
	c.c = c.timer.C
	c.delay = delay
}

func (c *ioWeightRetryController) Stop() {
	if c.timer != nil {
		if !c.timer.Stop() {
			select {
			case <-c.timer.C:
			default:
			}
		}
	}
	c.timer = nil
	c.c = nil
	c.delay = 0
}
