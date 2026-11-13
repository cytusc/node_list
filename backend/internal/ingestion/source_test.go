package ingestion

import (
	"errors"
	"fmt"
	"testing"
)

func TestSourceFailureClassification(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{ErrPartialBatch, false},
		{fmt.Errorf("wrapped: %w", ErrPartialBatch), false},
		{ErrNoAdmissibleNodes, true},
		{errors.New("subscription request failed"), true},
	} {
		if got := sourceFailure(tt.err); got != tt.want {
			t.Errorf("sourceFailure(%v)=%v want %v", tt.err, got, tt.want)
		}
	}
}
