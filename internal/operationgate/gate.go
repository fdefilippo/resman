// Package operationgate provides explicit serialization for slow operations.
package operationgate

import (
	"context"
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
	g.initialize()
	<-g.token
	return g.releaseFunc()
}

// EnterContext waits for admission or context cancellation. A canceled caller
// receives no release function and never consumes the next operation's token.
func (g *Gate) EnterContext(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.initialize()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.token:
	}
	// If cancellation and admission became ready together, cancellation wins.
	if err := ctx.Err(); err != nil {
		g.token <- struct{}{}
		return nil, err
	}
	return g.releaseFunc(), nil
}

func (g *Gate) initialize() {
	g.once.Do(func() {
		g.token = make(chan struct{}, 1)
		g.token <- struct{}{}
	})
}

func (g *Gate) releaseFunc() func() {
	var released atomic.Bool
	return func() {
		if !released.CompareAndSwap(false, true) {
			panic(doubleReleasePanic)
		}
		g.token <- struct{}{}
	}
}
