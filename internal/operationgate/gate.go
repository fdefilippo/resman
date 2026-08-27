// Package operationgate provides explicit serialization for slow operations.
package operationgate

import "sync"

// Gate serializes operations without using a shared-state mutex. Its zero value
// is ready for use. Holding a Gate across I/O is intentional: callers that only
// need component state do not wait for the operation to finish.
type Gate struct {
	once  sync.Once
	token chan struct{}
}

// Enter waits for the preceding operation and returns a release function.
func (g *Gate) Enter() func() {
	g.once.Do(func() {
		g.token = make(chan struct{}, 1)
		g.token <- struct{}{}
	})
	<-g.token
	return func() {
		g.token <- struct{}{}
	}
}
