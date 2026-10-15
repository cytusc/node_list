package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cytusc/node_list/backend/internal/patrol"
)

func TestStatusHeartbeatUsesDetachedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	writer := &statusWriter{path: path}
	current := status{RecoveryState: map[string]patrol.RecoveryState{}}
	writer.update(current)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			writer.heartbeat()
		}
	})
	for i := range 200 {
		current.Running = i%2 == 0
		current.RecoveryState["one"] = patrol.RecoveryState{Successes: i}
		writer.update(current)
	}
	wg.Wait()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved status
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.UpdatedAt == "" || saved.RecoveryState["one"].Successes != 199 {
		t.Fatalf("invalid saved state: %+v", saved)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v %v", entries, err)
	}
}
