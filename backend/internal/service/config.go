package service

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cytusc/node_list/backend/internal/transport"
)

func Env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func Seconds(key string, def int) time.Duration {
	v, e := strconv.Atoi(os.Getenv(key))
	if e != nil || v <= 0 {
		v = def
	}
	return time.Duration(v) * time.Second
}

// EnvInt reads an integer with an explicit range, and unlike Number it can
// express 0. Number caps at 64 and rejects 0 by falling back to the default,
// which silently breaks any "0 disables this" switch and any fan-out wider than
// 64. Out-of-range values fall back to def rather than failing the role.
func EnvInt(key string, def, low, high int) int {
	v, e := strconv.Atoi(os.Getenv(key))
	if e != nil || v < low || v > high {
		return def
	}
	return v
}

func Number(key string, def int) int {
	v, e := strconv.Atoi(os.Getenv(key))
	if e != nil || v < 1 || v > 64 {
		return def
	}
	return v
}

// BoundedSeconds clamps an out-of-range value instead of failing, so a bad
// deployment value degrades probe timing rather than stopping the role.
func BoundedSeconds(key string, def, low, high int) time.Duration {
	v, e := strconv.Atoi(os.Getenv(key))
	if e != nil || v <= 0 {
		v = def
	}
	return time.Duration(min(max(v, low), high)) * time.Second
}

const (
	defaultExitProbeTimeout = 8
	exitProbeTimeoutMin     = 2
	exitProbeTimeoutMax     = 30
)

// LoadEnv only fills missing environment variables; Compose always wins.
func LoadEnv(root string) {
	f, err := os.Open(filepath.Join(root, ".env"))
	if err != nil {
		return
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), "\"'")
		if _, set := os.LookupEnv(k); !set {
			_ = os.Setenv(k, v)
		}
	}
}

func Root() string {
	wd, _ := os.Getwd()
	for _, p := range []string{wd, filepath.Dir(wd), "/app"} {
		if _, e := os.Stat(filepath.Join(p, "dist")); e == nil {
			return p
		}
		if _, e := os.Stat(filepath.Join(p, "package.json")); e == nil {
			return p
		}
	}
	return wd
}

type Config struct {
	Role                                string
	ResidentialInterval, NormalInterval time.Duration
	// ExitProbeTimeout bounds one echo batch. Slow exits routinely need more
	// than the old fixed five seconds, so it stays tunable per deployment.
	ExitProbeTimeout time.Duration
	// RiskRefreshInterval is the steady-state IPPure re-query window. Ranges
	// with a high bot share refresh faster; see riskRefreshInterval.
	RiskRefreshInterval                time.Duration
	ScheduleInterval                   time.Duration
	HighConcurrency, NormalConcurrency int
	ExitConcurrency                    int
	// LaneConcurrency overrides the consumer count per work kind. Slow lanes
	// (risk, attributes) default to one so raising a
	// count stays an explicit operational choice.
	LaneConcurrency          map[string]int
	APIOrigin                string
	HighRouter, NormalRouter *transport.Router
	Manager                  *transport.ManagerClient
}

// laneConcurrency reads per-kind consumer counts. The role prefix wins over
// the generic name, mirroring the lane concurrency convention; both default
// to one consumer so probe-heavy lanes are unaffected unless configured.
//
// Attributes are the exception: one consumer per lane caps the whole
// classification lane at roughly the latency of a single lookup, which cannot
// work through a backlog that a quota outage produced. Three still leaves the
// database pool room, because a consumer holds a connection only while it
// queries, not while it waits on an upstream.
func laneConcurrency(role string) map[string]int {
	prefix := "CONNECTIVITY"
	switch role {
	case "exit-provision", "exit-attributes":
		prefix = "IP_SERVICE"
	}
	out := map[string]int{}
	for _, kind := range []string{KindRisk, KindAttributes} {
		name := strings.ToUpper(strings.ReplaceAll(kind, "-", "_"))
		fallback := 1
		if kind == KindAttributes {
			fallback = 3
		}
		out[kind] = Number(prefix+"_"+name+"_CONCURRENCY", Number(name+"_CONCURRENCY", fallback))
	}
	return out
}

func ConfigFromEnv(role string) (Config, error) {
	c := Config{Role: role, ResidentialInterval: Seconds("RESIDENTIAL_INTERVAL_SECONDS", 300), NormalInterval: Seconds("NORMAL_INTERVAL_SECONDS", 1800),
		ExitProbeTimeout:    BoundedSeconds("EXIT_PROBE_TIMEOUT_SECONDS", defaultExitProbeTimeout, exitProbeTimeoutMin, exitProbeTimeoutMax),
		RiskRefreshInterval: Seconds("RISK_REFRESH_INTERVAL_SECONDS", 21600),
		ScheduleInterval:    Seconds("SCHEDULE_INTERVAL_SECONDS", 5),
		HighConcurrency:     Number("RESIDENTIAL_CONCURRENCY", 2), NormalConcurrency: Number("NORMAL_CONCURRENCY", 2),
		LaneConcurrency: laneConcurrency(role),
		APIOrigin:       Env("IPPURE_API_ORIGIN", "https://api.123169.xyz")}
	if address := os.Getenv("MIHOMO_MANAGER_URL"); address != "" {
		if os.Getenv("MIHOMO_MANAGER_TOKEN") == "" {
			return c, fmt.Errorf("MIHOMO_MANAGER_TOKEN is required")
		}
		c.Manager = &transport.ManagerClient{URL: address, Token: os.Getenv("MIHOMO_MANAGER_TOKEN")}
		kind := "risk"
		if role == "exit-provision" {
			kind = "exit"
		}
		c.HighRouter = transport.NewRouter(&transport.ManagedEngine{Manager: c.Manager, Kind: kind, High: true})
		c.NormalRouter = transport.NewRouter(&transport.ManagedEngine{Manager: c.Manager, Kind: kind})
		if role == "exit-provision" {
			c.ExitConcurrency = Number("MIHOMO_EXIT_CONCURRENCY", 8)
			c.HighConcurrency = Number("IP_SERVICE_HIGH_CONCURRENCY", 8)
			c.NormalConcurrency = Number("IP_SERVICE_NORMAL_CONCURRENCY", 8)
		}
		if role == "exit-attributes" {
			c.LaneConcurrency[KindRisk] = Number("IP_SERVICE_RISK_CONCURRENCY", Number("RISK_CONCURRENCY", 2))
		}
		return c, nil
	}
	return c, fmt.Errorf("MIHOMO_MANAGER_URL is required")
}
