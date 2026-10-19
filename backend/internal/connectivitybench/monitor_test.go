package connectivitybench

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResourceGate(t *testing.T) {
	at := time.Now()
	g := resourceGate{}
	if g.check(at, 90, 4096) != "" || g.check(at.Add(29*time.Second), 90, 4096) != "" {
		t.Fatal("premature CPU abort")
	}
	if g.check(at.Add(30*time.Second), 90, 4096) == "" {
		t.Fatal("sustained overload accepted")
	}
	g = resourceGate{}
	g.check(at, 90, 4096)
	g.check(at.Add(25*time.Second), 80, 4096)
	if g.check(at.Add(35*time.Second), 90, 4096) != "" {
		t.Fatal("CPU recovery did not reset timer")
	}
	if g.check(at, 0, 2047) == "" {
		t.Fatal("low memory accepted")
	}
}
func TestMonitorFailsClosedAndCancels(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan error, 1)
	result := Monitor(ctx, root, cancel, ready)
	if <-ready == nil || result.Samples != 0 {
		t.Fatal("missing metrics accepted")
	}
	os.WriteFile(filepath.Join(root, "stat"), []byte("cpu 1 2 3 4 5 6 7 8\n"), 0600)
	os.WriteFile(filepath.Join(root, "meminfo"), []byte("MemAvailable: 1024000 kB\n"), 0600)
	result = Monitor(ctx, root, cancel, ready)
	if <-ready == nil || result.StopReason == "" || ctx.Err() == nil {
		t.Fatal("low memory did not cancel")
	}
}
