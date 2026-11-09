package operationgate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestGateEnterContextRejectsCanceledContextWithoutLosingToken(t *testing.T) {
	var gate Gate
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range 100 {
		leave, err := gate.EnterContext(ctx)
		if leave != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled admission: release=%v err=%v", leave != nil, err)
		}
	}
	assertContextGateAvailable(t, &gate)
}

func TestGateEnterContextCancellationAndDeadlineWhileHeld(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancellation", true: "deadline"}[deadline], func(t *testing.T) {
			var gate Gate
			leaveOwner := gate.Enter()
			defer leaveOwner()
			ctx, cancel := context.WithCancel(t.Context())
			wantErr := context.Canceled
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 10*time.Millisecond)
				wantErr = context.DeadlineExceeded
			}
			defer cancel()
			started := make(chan struct{})
			result := make(chan error, 1)
			go func() {
				close(started)
				leave, err := gate.EnterContext(ctx)
				if leave != nil {
					leave()
					result <- errors.New("entered an occupied gate")
					return
				}
				result <- err
			}()
			<-started
			if !deadline {
				cancel()
			}
			select {
			case err := <-result:
				if !errors.Is(err, wantErr) {
					t.Fatalf("gate admission error=%v, want %v", err, wantErr)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled admission waited for the owner to release")
			}
		})
	}
}

func TestGateCanceledWaitersDoNotChangeOwnership(t *testing.T) {
	const waiters = 32
	var gate Gate
	leaveOwner := gate.Enter()
	ownerReleased := false
	defer func() {
		if !ownerReleased {
			leaveOwner()
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var ready sync.WaitGroup
	ready.Add(waiters)
	results := make(chan error, waiters)
	for range waiters {
		go func() {
			ready.Done()
			leave, err := gate.EnterContext(ctx)
			if leave != nil {
				leave()
				results <- errors.New("waiter consumed the owner's token")
				return
			}
			results <- err
		}()
	}
	ready.Wait()
	cancel()
	for range waiters {
		select {
		case err := <-results:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("waiter error=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("canceled waiter did not return")
		}
	}
	probe, stopProbe := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer stopProbe()
	if leave, err := gate.EnterContext(probe); leave != nil || !errors.Is(err, context.DeadlineExceeded) {
		if leave != nil {
			leave()
		}
		t.Fatalf("canceled waiters changed ownership: release=%v err=%v", leave != nil, err)
	}
	leaveOwner()
	ownerReleased = true
	assertContextGateAvailable(t, &gate)
}

func TestGateEnterContextReleaseRemainsSingleUse(t *testing.T) {
	var gate Gate
	leave, err := gate.EnterContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	leave()
	defer func() {
		if recovered := recover(); recovered != doubleReleasePanic {
			t.Fatalf("second release panic=%v", recovered)
		}
	}()
	leave()
}

func assertContextGateAvailable(t *testing.T, gate *Gate) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	leave, err := gate.EnterContext(ctx)
	if err != nil {
		t.Fatalf("gate token lost: %v", err)
	}
	leave()
}
