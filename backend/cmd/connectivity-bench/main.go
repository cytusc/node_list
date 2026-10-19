// connectivity-bench reads once, closes the database, then probes dedicated
// kernels. It cannot enqueue work or persist a production observation.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	bench "github.com/cytusc/node_list/backend/internal/connectivitybench"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func readJSON(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if json.Unmarshal(b, out) != nil {
		return errors.New("invalid input JSON")
	}
	return nil
}
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	// Publish only complete JSON. A hard link makes publication atomic without
	// replacing an existing result or following an existing symlink.
	f, err := os.CreateTemp(filepath.Dir(path), ".connectivity-bench-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Link(f.Name(), path)
}
func run() error {
	mode := flag.String("mode", "run", "sample or run")
	samplePath := flag.String("sample", "", "private fixed sample JSON")
	instancesPath := flag.String("instances", "", "dedicated instance JSON")
	output := flag.String("output", "", "new result JSON path")
	concurrency := flag.Int("concurrency", 16, "normal lane admission budget")
	batch := flag.Int("batch", 128, "immutable target wave size (1 for baseline)")
	due := flag.Bool("due", false, "sample all currently due recovery candidates")
	proc := flag.String("host-proc", "/hostproc", "read-only host /proc mount")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *output == "" {
		return errors.New("--output is required")
	}
	if _, err := os.Lstat(*output); err == nil {
		return errors.New("output already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if *mode == "sample" {
		all, err := bench.ReadCandidates(ctx)
		if err != nil {
			return err
		}
		sample := bench.MakeSample(all, *due)
		if err = writeJSON(*output, sample); err != nil {
			return err
		}
		fmt.Printf("sample saved: targets=%d references=%d residential=%d (private file)\n", len(sample.Targets), len(sample.References), len(sample.Residential))
		return nil
	}
	if *mode != "run" || *samplePath == "" || *instancesPath == "" {
		return errors.New("run requires --sample and --instances")
	}
	var sample bench.Sample
	var instances bench.Instances
	if err := readJSON(*samplePath, &sample); err != nil {
		return err
	}
	if err := readJSON(*instancesPath, &instances); err != nil {
		return err
	}
	if err := instances.Validate(); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ready := make(chan error, 1)
	host := make(chan bench.HostMetrics, 1)
	go func() { host <- bench.Monitor(runCtx, *proc, cancel, ready) }()
	if err := <-ready; err != nil {
		return err
	}
	report, runErr := bench.Run(runCtx, sample, instances, bench.Options{Concurrency: *concurrency, BatchSize: *batch, Progress: func(lane string, done, total int) {
		fmt.Fprintf(os.Stderr, "progress lane=%s completed=%d/%d\n", lane, done, total)
	}})
	cancel()
	report.Host = <-host
	if report.Host.StopReason != "" {
		report.Aborted = report.Host.StopReason
	} else if runErr != nil {
		report.Aborted = "cancelled or incomplete"
	}
	if err := writeJSON(*output, report); err != nil {
		return err
	}
	fmt.Printf("normal=%d elapsed=%.2fs reachable=%d dead=%d reloads=%d controller_errors=%d cpu=%.1f%% min_ram=%dMiB residential_p95_queue=%.1fms\n", report.Normal.Count, report.Normal.ElapsedSeconds, report.Normal.Reachable, report.Normal.ConfirmedDead, report.Normal.Reloads, report.Normal.ControllerErrors, report.Host.AverageCPU, report.Host.MinAvailableMiB, report.Residential.QueueP95MS)
	if report.Normal.Reservations > 0 && float64(report.Normal.ControllerErrors)/float64(report.Normal.Reservations) > .01 {
		return errors.New("controller error rate exceeds 1%; reject tier")
	}
	if runErr != nil {
		return runErr
	}
	return nil
}
