package source

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTriggerSerializesAndHonorsCancellation(t *testing.T) {
	var active, calls atomic.Int32
	w := NewWatcher(nil, WatcherConfig{Ingest: func(ctx context.Context) error {
		if active.Add(1) != 1 {
			t.Error("ingest callbacks overlapped")
		}
		defer active.Add(-1)
		calls.Add(1)
		time.Sleep(time.Millisecond)
		return nil
	}})
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { w.Trigger(context.Background()) })
	}
	wg.Wait()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	w.Trigger(cancelled)
	runs, _, _ := w.Stats()
	if runs != 12 || calls.Load() != 12 {
		t.Fatalf("runs=%d calls=%d", runs, calls.Load())
	}
}
