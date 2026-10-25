package service

import (
	"testing"
	"time"
)

func TestSweepStartsSoonAfterRestart(t *testing.T) {
	// A restart must rebuild the availability view promptly: waiting a full
	// interval leaves sweep-dependent consumers blind for that whole window.
	r := newSweepRunner(30 * time.Minute)
	wait := time.Until(r.status().NextRunAt)
	if wait > time.Minute {
		t.Fatalf("first round scheduled %s after start", wait.Round(time.Second))
	}
	// Long intervals keep their cadence; short ones are not stretched.
	if r := newSweepRunner(5 * time.Second); time.Until(r.status().NextRunAt) > 6*time.Second {
		t.Fatalf("short interval stretched")
	}
}
