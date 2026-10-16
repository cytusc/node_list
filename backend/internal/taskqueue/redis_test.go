package taskqueue

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeliveryScoreNeverPrecedesDatabaseDueTime(t *testing.T) {
	base := time.Unix(1700000000, 0)
	for _, offset := range []time.Duration{0, time.Microsecond, time.Millisecond - time.Microsecond, time.Millisecond} {
		due := base.Add(offset)
		score := int64(deliveryScore(due))
		if time.UnixMilli(score).Before(due) || time.UnixMilli(score).Sub(due) >= time.Millisecond {
			t.Fatalf("due %v became Redis score %d", due, score)
		}
	}
}

func TestRedisAtomicDeliveryDueTimeAndLaneIsolation(t *testing.T) {
	address := os.Getenv("NODE_EXPLORER_TEST_REDIS")
	if address == "" {
		t.Skip("set NODE_EXPLORER_TEST_REDIS to an isolated Redis")
	}
	ctx := context.Background()
	q, e := New(ctx, address, fmt.Sprintf("test-%d", time.Now().UnixNano()))
	if e != nil {
		t.Fatal(e)
	}
	defer q.Close()
	defer q.client.Del(ctx, q.key("exit", true), q.key("exit", false))
	for range 2 {
		if e = q.PushBatch(ctx, []Delivery{{Kind: "exit", High: true, Ref: "one", Due: time.Now().Add(-time.Second)}}); e != nil {
			t.Fatal(e)
		}
	}
	if e = q.Push(ctx, "exit", false, "later", time.Now().Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	var received atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			v, e := q.Pop(ctx, "exit", true)
			if e != nil {
				t.Error(e)
			}
			if v != "" {
				received.Add(1)
			}
		})
	}
	wg.Wait()
	if received.Load() != 1 {
		t.Fatalf("duplicate delivery: %d", received.Load())
	}
	if v, e := q.Pop(ctx, "exit", false); e != nil || v != "" {
		t.Fatal("future work popped early")
	}
	if e = q.Push(ctx, "exit", false, "normal", time.Now().Add(-time.Second)); e != nil {
		t.Fatal(e)
	}
	if v, e := q.Pop(ctx, "exit", true); e != nil || v != "" {
		t.Fatal("normal work occupied reserved residential lane")
	}
	if v, e := q.Pop(ctx, "exit", false); e != nil || v != "normal" {
		t.Fatal("normal lane was not independent")
	}
}
