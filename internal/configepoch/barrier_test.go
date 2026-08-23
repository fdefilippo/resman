/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 */

package configepoch

import (
	"runtime"
	"testing"
)

func TestBarrierPreventsMixedEpochReaders(t *testing.T) {
	var barrier Barrier
	leaveOldEpoch := barrier.Enter()

	updateStarted := make(chan struct{})
	updateAcquired := make(chan func())
	go func() {
		close(updateStarted)
		updateAcquired <- barrier.BeginUpdate()
	}()
	<-updateStarted
	for i := 0; ; i++ {
		barrier.mu.Lock()
		updating := barrier.updating
		barrier.mu.Unlock()
		if updating {
			break
		}
		if i == 10000 {
			t.Fatal("update did not reach the old-epoch drain point")
		}
		runtime.Gosched()
	}
	leaveOldEpoch()
	finishUpdate := <-updateAcquired

	readerEntered := make(chan func())
	go func() {
		readerEntered <- barrier.Enter()
	}()
	for i := 0; ; i++ {
		barrier.mu.Lock()
		waiters := barrier.waiters
		barrier.mu.Unlock()
		if waiters == 1 {
			break
		}
		if i == 10000 {
			t.Fatal("reader did not reach the active-update wait point")
		}
		runtime.Gosched()
	}
	select {
	case <-readerEntered:
		t.Fatal("reader entered while configuration update was active")
	default:
	}

	finishUpdate()
	leaveNewEpoch := <-readerEntered
	leaveNewEpoch()
}
