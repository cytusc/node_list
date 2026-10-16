// Package taskqueue transports task references independently of business storage.
// PostgreSQL remains the delivery ledger; Redis is the ordered, disposable index.
package taskqueue

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type Queue interface {
	Push(context.Context, string, bool, string, time.Time) error
	Pop(context.Context, string, bool) (string, error)
	Close() error
}

type Delivery struct {
	Kind string
	High bool
	Ref  string
	Due  time.Time
}

// BatchQueue lets a transport send a publication wave in one round trip.
// Queue remains sufficient for alternate transports and small callers.
type BatchQueue interface {
	PushBatch(context.Context, []Delivery) error
}

type Redis struct {
	client *redis.Client
	prefix string
}

func New(ctx context.Context, address, prefix string) (*Redis, error) {
	if address == "" {
		return nil, fmt.Errorf("REDIS_URL is required")
	}
	o, err := redis.ParseURL(address)
	if err != nil {
		return nil, fmt.Errorf("invalid REDIS_URL")
	}
	c := redis.NewClient(o)
	if err := c.Ping(ctx).Err(); err != nil {
		c.Close()
		return nil, fmt.Errorf("Redis connection failed")
	}
	if prefix == "" {
		prefix = "node-explorer"
	}
	return &Redis{client: c, prefix: strings.TrimSuffix(prefix, ":")}, nil
}

func (q *Redis) key(kind string, high bool) string {
	lane := "normal"
	if high {
		lane = "residential"
	}
	return q.prefix + ":" + kind + ":" + lane
}

func (q *Redis) Push(ctx context.Context, kind string, high bool, id string, due time.Time) error {
	return q.client.ZAdd(ctx, q.key(kind, high), redis.Z{Score: deliveryScore(due), Member: id}).Err()
}

// Round up: Redis must not remove a reference a fraction of a millisecond
// before PostgreSQL permits its claim, especially with delayed repair scans.
func deliveryScore(due time.Time) float64 {
	score := due.UnixMilli()
	if due.Nanosecond()%int(time.Millisecond) != 0 {
		score++
	}
	return float64(score)
}

func (q *Redis) PushBatch(ctx context.Context, deliveries []Delivery) error {
	if len(deliveries) == 0 {
		return nil
	}
	_, err := q.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for _, d := range deliveries {
			pipe.ZAdd(ctx, q.key(d.Kind, d.High), redis.Z{Score: deliveryScore(d.Due), Member: d.Ref})
		}
		return nil
	})
	return err
}

var popDue = redis.NewScript(`
local item = redis.call('ZRANGE', KEYS[1], 0, 0, 'WITHSCORES')
if #item == 0 or tonumber(item[2]) > tonumber(ARGV[1]) then return '' end
redis.call('ZREM', KEYS[1], item[1])
return item[1]
`)

func (q *Redis) Pop(ctx context.Context, kind string, high bool) (string, error) {
	return popDue.Run(ctx, q.client, []string{q.key(kind, high)}, time.Now().UnixMilli()).Text()
}
func (q *Redis) Close() error { return q.client.Close() }
