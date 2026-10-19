package connectivitybench

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type HostMetrics struct {
	Samples         int     `json:"samples"`
	AverageCPU      float64 `json:"average_cpu_percent"`
	PeakCPU         float64 `json:"peak_cpu_percent"`
	MinAvailableMiB uint64  `json:"min_available_mib"`
	PeakTCP         int     `json:"peak_tcp_sockets"`
	StopReason      string  `json:"stop_reason,omitempty"`
}
type cpuReading struct{ idle, total uint64 }

type resourceGate struct{ highSince time.Time }

func (g *resourceGate) check(now time.Time, cpu float64, memory uint64) string {
	if memory < 2048 {
		return "available memory below 2 GiB"
	}
	if cpu > 85 {
		if g.highSince.IsZero() {
			g.highSince = now
		}
	} else {
		g.highSince = time.Time{}
	}
	if !g.highSince.IsZero() && now.Sub(g.highSince) >= 30*time.Second {
		return "host CPU above 85% for 30 seconds"
	}
	return ""
}

func readCPU(root string) (cpuReading, error) {
	b, e := os.ReadFile(filepath.Join(root, "stat"))
	if e != nil {
		return cpuReading{}, e
	}
	fields := strings.Fields(strings.SplitN(string(b), "\n", 2)[0])
	if len(fields) < 9 {
		return cpuReading{}, fmt.Errorf("invalid host stat")
	}
	var c cpuReading
	for i, s := range fields[1:9] {
		v, e := strconv.ParseUint(s, 10, 64)
		if e != nil {
			return c, e
		}
		c.total += v
		if i == 3 || i == 4 {
			c.idle += v
		}
	}
	return c, nil
}
func readMem(root string) (uint64, error) {
	f, e := os.Open(filepath.Join(root, "meminfo"))
	if e != nil {
		return 0, e
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if strings.HasPrefix(s.Text(), "MemAvailable:") {
			var kb uint64
			_, e = fmt.Sscanf(s.Text(), "MemAvailable: %d kB", &kb)
			return kb / 1024, e
		}
	}
	return 0, fmt.Errorf("MemAvailable missing")
}
func sockets(root string) int {
	n := 0
	for _, name := range []string{"tcp", "tcp6"} {
		b, e := os.ReadFile(filepath.Join(root, "net", name))
		if e == nil {
			n += max(0, strings.Count(string(b), "\n")-1)
		}
	}
	return n
}

// Monitor must be fed the host proc mount, not container CPU quotas. Missing
// telemetry fails closed before starting a tier. A 30-second CPU violation or
// memory below 2 GiB cancels the experiment, never the production workers.
func Monitor(ctx context.Context, root string, cancel context.CancelFunc, ready chan<- error) HostMetrics {
	m := HostMetrics{}
	last, e := readCPU(root)
	if e != nil {
		ready <- e
		return m
	}
	mem, e := readMem(root)
	if e != nil {
		ready <- e
		return m
	}
	m.MinAvailableMiB = mem
	if mem < 2048 {
		m.StopReason = "available memory below 2 GiB"
		ready <- fmt.Errorf("%s", m.StopReason)
		cancel()
		return m
	}
	ready <- nil
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	gate := resourceGate{}
	for {
		select {
		case <-ctx.Done():
			return m
		case now := <-ticker.C:
			current, e := readCPU(root)
			memory, me := readMem(root)
			if e != nil || me != nil {
				m.StopReason = "host telemetry failed"
				cancel()
				return m
			}
			delta := current.total - last.total
			cpu := 0.0
			if delta > 0 {
				cpu = 100 * (1 - float64(current.idle-last.idle)/float64(delta))
			}
			last = current
			m.Samples++
			m.AverageCPU += (cpu - m.AverageCPU) / float64(m.Samples)
			m.PeakCPU = max(m.PeakCPU, cpu)
			m.MinAvailableMiB = min(m.MinAvailableMiB, memory)
			m.PeakTCP = max(m.PeakTCP, sockets(root))
			m.StopReason = gate.check(now, cpu, memory)
			if m.StopReason != "" {
				cancel()
				return m
			}
		}
	}
}
