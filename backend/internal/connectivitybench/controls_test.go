package connectivitybench

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestControlsCoalesceOnlyWhileInFlight(t *testing.T) {
	g := newControlFlights()
	entered := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.do(context.Background(), "host", func() (bool, error) { requests.Add(1); close(entered); <-release; return true, nil })
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if ok, err := g.do(ctx, "host", func() (bool, error) { t.Error("duplicate request"); return true, nil }); err == nil || ok {
		t.Fatal("cancelled follower got proof")
	}
	other, err := g.do(context.Background(), "reference:other", func() (bool, error) { return true, nil })
	if err != nil || !other {
		t.Fatal("unrelated control blocked")
	}
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := g.do(context.Background(), "host", func() (bool, error) { requests.Add(1); return true, nil })
			if err != nil || !ok {
				t.Error("valid shared proof missing")
			}
		}()
	}
	deadline := time.Now().Add(time.Second)
	for {
		_, joined := g.stats()
		if joined == 11 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("followers never joined")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	if requests.Load() != 1 {
		t.Fatal("overlapping checks were duplicated")
	}
	g.do(context.Background(), "host", func() (bool, error) { requests.Add(1); return true, nil })
	if requests.Load() != 2 {
		t.Fatal("completed proof was cached")
	}
}
