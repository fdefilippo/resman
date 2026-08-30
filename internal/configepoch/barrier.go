/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 */

// Package configepoch coordinates readers of one effective configuration
// epoch with configuration replacement.
package configepoch

import "sync"

// Barrier lets readers finish on the old epoch, prevents new readers from
// entering during an update, and publishes the new epoch atomically.
//
// BeginUpdate deliberately releases the internal mutex before returning so
// callers may perform component updates and I/O without holding a barrier lock.
type Barrier struct {
	mu       sync.Mutex
	cond     *sync.Cond
	readers  int
	waiters  int
	updating bool
}

func (b *Barrier) initLocked() {
	if b.cond == nil {
		b.cond = sync.NewCond(&b.mu)
	}
}

// Enter joins the current configuration epoch. The returned function must be
// called exactly once when the operation has stopped consuming configuration.
func (b *Barrier) Enter() func() {
	b.mu.Lock()
	b.initLocked()
	for b.updating {
		b.waiters++
		b.cond.Wait()
		b.waiters--
	}
	b.readers++
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.readers--
			if b.readers == 0 {
				b.cond.Broadcast()
			}
			b.mu.Unlock()
		})
	}
}

// BeginUpdate waits for readers of the old epoch to leave and prevents new
// readers from entering. The returned function publishes the new epoch.
func (b *Barrier) BeginUpdate() func() {
	b.mu.Lock()
	b.initLocked()
	for b.updating {
		b.cond.Wait()
	}
	b.updating = true
	for b.readers > 0 {
		b.cond.Wait()
	}
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.updating = false
			b.cond.Broadcast()
			b.mu.Unlock()
		})
	}
}
