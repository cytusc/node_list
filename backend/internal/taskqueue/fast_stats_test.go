package taskqueue

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestFastMinuteBucketsAreBoundedAndSeparate(t *testing.T) {
	address := os.Getenv("NODE_EXPLORER_TEST_REDIS")
	if address == "" {
		t.Skip("isolated Redis required")
	}
	ctx := context.Background()
	q, err := New(ctx, address, fmt.Sprintf("fast-test-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	key := q.fastKey(time.Now().Unix() / 60)
	defer q.client.Del(ctx, key)
	for _, v := range [][2]bool{{true, false}, {false, false}, {false, true}} {
		if err = q.RecordFast(ctx, v[0], v[1]); err != nil {
			t.Fatal(err)
		}
	}
	ttl := q.client.TTL(ctx, key).Val()
	if ttl <= 119*time.Minute || ttl > 2*time.Hour {
		t.Fatalf("unbounded bucket TTL %v", ttl)
	}
	stats, err := q.FastCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Hour != (FastCount{Completed: 3, Success: 1, Failed: 1, Abnormal: 1}) {
		t.Fatalf("outcomes mixed: %+v", stats)
	}
}
