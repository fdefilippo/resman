// Package operationgate provides explicit serialization for slow operations.
package operationgate

import (
	"sync"
	"sync/atomic"
)

const doubleReleasePanic = "operationgate: release called more than once"

// Gate serializes operations without using a shared-state mutex. Its zero value
// is ready for use. Holding a Gate across I/O is intentional: callers that only
// need component state do not wait for the operation to finish.
type Gate struct {
	once  sync.Once
	token chan struct{}
}

// Enter waits for the preceding operation and returns a single-use release
// function. Calling the returned function more than once panics immediately.
func (g *Gate) Enter() func() {
	g.once.Do(func() {
		g.token = make(chan struct{}, 1)
		g.token <- struct{}{}
	})
	<-g.token
	var released atomic.Bool
	return func() {
		if !released.CompareAndSwap(false, true) {
			panic(doubleReleasePanic)
		}
		g.token <- struct{}{}
	}
}
