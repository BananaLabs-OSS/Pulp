package dependency

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestExecuteDependencyReadyAndBounded(t *testing.T) {
	plan, _ := Build([]Item{{ID: "a"}, {ID: "b"}, {ID: "c", DependsOn: []string{"a"}}})
	releaseB := make(chan struct{})
	cStarted := make(chan struct{})
	result := make(chan struct {
		ready []string
		err   error
	}, 1)
	go func() {
		ready, err := Execute(context.Background(), plan, 2, func(_ context.Context, id string) error {
			if id == "b" {
				<-releaseB
			}
			if id == "c" {
				close(cStarted)
			}
			return nil
		})
		result <- struct {
			ready []string
			err   error
		}{ready, err}
	}()
	select {
	case <-cStarted: // c starts as soon as a completes; it does not wait for level peer b.
	case <-time.After(time.Second):
		t.Fatal("dependent did not start while unrelated root was still running")
	}
	close(releaseB)
	got := <-result
	if got.err != nil {
		t.Fatal(got.err)
	}
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(got.ready, want) {
		t.Fatalf("ready = %#v, want %#v", got.ready, want)
	}
}

func TestExecuteHonorsConcurrencyLimit(t *testing.T) {
	plan, _ := Build([]Item{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	entered := make(chan string, 3)
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := Execute(context.Background(), plan, 2, func(_ context.Context, id string) error {
			entered <- id
			<-release
			return nil
		})
		result <- err
	}()
	<-entered
	<-entered
	select {
	case id := <-entered:
		t.Fatalf("third node %q exceeded concurrency limit", id)
	default:
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestExecuteFailureCancelsInflightAndSuppressesDependents(t *testing.T) {
	plan, _ := Build([]Item{{ID: "fail"}, {ID: "inflight"}, {ID: "blocked", DependsOn: []string{"fail"}}})
	var mu sync.Mutex
	started := []string{}
	ready, err := Execute(context.Background(), plan, 2, func(ctx context.Context, id string) error {
		mu.Lock()
		started = append(started, id)
		mu.Unlock()
		if id == "fail" {
			return errors.New("boom")
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("ready = %#v", ready)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range started {
		if id == "blocked" {
			t.Fatal("dependent started after failure")
		}
	}
}
