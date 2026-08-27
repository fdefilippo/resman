package operationgate

import (
	"testing"
	"time"
)

func TestGateSerializesOperations(t *testing.T) {
	var gate Gate
	leaveFirst := gate.Enter()
	enteredSecond := make(chan struct{})
	secondDone := make(chan struct{})
	go func() {
		leaveSecond := gate.Enter()
		close(enteredSecond)
		leaveSecond()
		close(secondDone)
	}()
	select {
	case <-enteredSecond:
		t.Fatal("second operation entered before the first left")
	case <-time.After(25 * time.Millisecond):
	}
	leaveFirst()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second operation did not enter after the first left")
	}
}
