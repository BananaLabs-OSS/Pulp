package host

import (
	"context"
	"testing"
	"time"
)

func TestCellCallStackTracksNestedCallers(t *testing.T) {
	first := &Cell{}
	second := &Cell{}

	ctx := withCellOnCallStack(context.Background(), first)
	if !callStackContainsCell(ctx, first) {
		t.Fatal("first caller is missing from stack")
	}
	if callStackContainsCell(ctx, second) {
		t.Fatal("independent target unexpectedly appears in stack")
	}

	nested := withCellOnCallStack(ctx, second)
	if !callStackContainsCell(nested, first) || !callStackContainsCell(nested, second) {
		t.Fatal("nested call stack did not preserve both callers")
	}
}

func TestCellLockForCallWaitsForAnnotatedIndependentCaller(t *testing.T) {
	target := &Cell{}
	target.mu.Lock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan bool, 1)
	go func() {
		result <- target.lockForCall(withCellOnCallStack(ctx, &Cell{}))
	}()

	select {
	case acquired := <-result:
		target.mu.Unlock()
		t.Fatalf("independent annotated caller returned before grace elapsed: acquired=%v", acquired)
	case <-time.After(reentrantCallGrace + 50*time.Millisecond):
	}

	target.mu.Unlock()
	if acquired := <-result; !acquired {
		t.Fatal("independent annotated caller did not acquire after target released")
	}
	target.mu.Unlock()
}

func TestCellLockForCallRejectsStackLoopback(t *testing.T) {
	target := &Cell{}
	if target.lockForCall(withCellOnCallStack(context.Background(), target)) {
		target.mu.Unlock()
		t.Fatal("loopback unexpectedly acquired target")
	}
}
