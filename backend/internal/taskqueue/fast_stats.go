package taskqueue

import (
	"context"
	"github.com/redis/go-redis/v9"
	"strconv"
	"time"
)

type FastCount struct {
	Completed int64 `json:"completed"`
	Success   int64 `json:"success"`
	Failed    int64 `json:"failed"`
	Abnormal  int64 `json:"abnormal"`
}
type FastStats struct {
	Minute FastCount `json:"minute"`
	Hour   FastCount `json:"hour"`
}

func (q *Redis) fastKey(minute int64) string {
	return q.prefix + ":fast:minute:" + strconv.FormatInt(minute, 10)
}
func (q *Redis) RecordFast(ctx context.Context, success, abnormal bool) error {
	key := q.fastKey(time.Now().Unix() / 60)
	outcome := "failed"
	if abnormal {
		outcome = "abnormal"
	} else if success {
		outcome = "success"
	}
	_, err := q.client.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HIncrBy(ctx, key, "completed", 1)
		p.HIncrBy(ctx, key, outcome, 1)
		p.Expire(ctx, key, 2*time.Hour)
		return nil
	})
	return err
}

// Minute reports the last completed UTC minute; hour includes the current bucket.
// Only 120 small hashes are retained, independent of probe throughput.
func (q *Redis) FastCounts(ctx context.Context) (FastStats, error) {
	now := time.Now().Unix() / 60
	commands := make([]*redis.MapStringStringCmd, 60)
	_, err := q.client.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i := range commands {
			commands[i] = p.HGetAll(ctx, q.fastKey(now-int64(i)))
		}
		return nil
	})
	var stats FastStats
	if err != nil {
		return stats, err
	}
	for i, cmd := range commands {
		values := cmd.Val()
		read := func(key string) int64 { n, _ := strconv.ParseInt(values[key], 10, 64); return n }
		count := FastCount{read("completed"), read("success"), read("failed"), read("abnormal")}
		if i == 1 {
			stats.Minute = count
		}
		stats.Hour.Completed += count.Completed
		stats.Hour.Success += count.Success
		stats.Hour.Failed += count.Failed
		stats.Hour.Abnormal += count.Abnormal
	}
	return stats, nil
}
