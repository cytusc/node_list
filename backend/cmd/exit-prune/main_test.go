package main

import (
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name      string
		olderThan time.Duration
		limit     int
		wantErr   bool
	}{
		{name: "default window", olderThan: defaultOlderThan, limit: 0},
		{name: "fractional day", olderThan: 36 * time.Hour, limit: 0},
		{name: "minutes", olderThan: 90 * time.Minute, limit: 0},
		{name: "bounded batch", olderThan: time.Hour, limit: 5000},
		{name: "zero interval", olderThan: 0, limit: 0, wantErr: true},
		{name: "negative interval", olderThan: -time.Hour, limit: 0, wantErr: true},
		{name: "negative limit", olderThan: defaultOlderThan, limit: -1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate(tt.olderThan, tt.limit)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate(%v, %d) error = %v, wantErr %t", tt.olderThan, tt.limit, err, tt.wantErr)
			}
		})
	}
}

// The predicate must never match a row that carries an exit IP in either the
// payload or the canonical column.
func TestStaleEmptyExitRequiresEmptyIP(t *testing.T) {
	for _, guard := range []string{"checked_at < now() - $1::interval", "coalesce(e.payload->>'ip','')=''", "coalesce(e.ip,'')=''"} {
		if !strings.Contains(staleEmptyExit, guard) {
			t.Fatalf("staleEmptyExit lost %q:\n%s", guard, staleEmptyExit)
		}
	}
}
