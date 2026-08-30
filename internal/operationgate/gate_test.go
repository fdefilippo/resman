package operationgate

import (
	"fmt"
	"testing"
	"time"
)

func TestGateReleaseContract(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "single release unblocks the next operation", run: testSingleRelease},
		{name: "double release panics without blocking", run: testDoubleRelease},
		{name: "concurrent release misuse returns exactly one token", run: testConcurrentRelease},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func testSingleRelease(t *testing.T) {
	var gate Gate
	leaveFirst := gate.Enter()
	enteredSecond := make(chan func(), 1)
	go func() {
		enteredSecond <- gate.Enter()
	}()
	assertNoEntry(t, enteredSecond)
	leaveFirst()
	leaveSecond := awaitEntry(t, enteredSecond)
	leaveSecond()
}

func testDoubleRelease(t *testing.T) {
	var gate Gate
	release := gate.Enter()
	release()

	result := make(chan any, 1)
	go func() {
		defer func() {
			result <- recover()
		}()
		release()
	}()

	select {
	case recovered := <-result:
		if recovered == nil {
			t.Fatal("second release returned without panicking")
		}
		if message := fmt.Sprint(recovered); message != doubleReleasePanic {
			t.Fatalf("panic = %q, want %q", message, doubleReleasePanic)
		}
	case <-time.After(time.Second):
		t.Fatal("second release blocked instead of failing immediately")
	}
}

func testConcurrentRelease(t *testing.T) {
	const callers = 32
	var gate Gate
	release := gate.Enter()
	start := make(chan struct{})
	results := make(chan any, callers)
	for range callers {
		go func() {
			<-start
			defer func() {
				results <- recover()
			}()
			release()
		}()
	}
	close(start)

	successes := 0
	panics := 0
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for range callers {
		select {
		case recovered := <-results:
			if recovered == nil {
				successes++
				continue
			}
			if message := fmt.Sprint(recovered); message != doubleReleasePanic {
				t.Fatalf("panic = %q, want %q", message, doubleReleasePanic)
			}
			panics++
		case <-timer.C:
			t.Fatal("concurrent release call blocked")
		}
	}
	if successes != 1 || panics != callers-1 {
		t.Fatalf("successful releases = %d, panics = %d; want 1 and %d", successes, panics, callers-1)
	}

	leaveNext := gate.Enter()
	enteredFollowing := make(chan func(), 1)
	go func() {
		enteredFollowing <- gate.Enter()
	}()
	assertNoEntry(t, enteredFollowing)
	leaveNext()
	leaveFollowing := awaitEntry(t, enteredFollowing)
	leaveFollowing()
}

func assertNoEntry(t *testing.T, entered <-chan func()) {
	t.Helper()
	select {
	case release := <-entered:
		release()
		t.Fatal("operation entered while the gate was held")
	case <-time.After(25 * time.Millisecond):
	}
}

func awaitEntry(t *testing.T, entered <-chan func()) func() {
	t.Helper()
	select {
	case release := <-entered:
		return release
	case <-time.After(time.Second):
		t.Fatal("operation did not enter after the gate was released")
		return nil
	}
}
