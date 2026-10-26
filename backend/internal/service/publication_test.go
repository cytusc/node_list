package service

import (
	"context"

	"github.com/cytusc/node_list/backend/internal/taskqueue"
)

type publicationQueue struct {
	recordingQueue
	waves     [][]taskqueue.Delivery
	err       error
	afterPush func()
}

func (q *publicationQueue) PushBatch(_ context.Context, deliveries []taskqueue.Delivery) error {
	q.waves = append(q.waves, append([]taskqueue.Delivery(nil), deliveries...))
	if q.afterPush != nil {
		q.afterPush()
	}
	return q.err
}
