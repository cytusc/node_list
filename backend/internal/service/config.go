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
	NormalRecoveryInterval              time.Duration
	ResidentialInterval, NormalInterval time.Duration
	FastInterval                        time.Duration
	Timeout, RetryDelay                 time.Duration
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
	// (risk, attributes, lifecycle, recovery) default to one so raising a
	// count stays an explicit operational choice.
	LaneConcurrency          map[string]int
	APIOrigin                string
	HighRouter, NormalRouter *transport.Router
	Manager                  *transport.ManagerClient
	DryRun                   bool
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
	case "recovery":
		prefix = "RECOVERY"
	}
	out := map[string]int{}
	for _, kind := range []string{KindRisk, KindAttributes, KindLifecycle, KindRecovery} {
		name := strings.ToUpper(strings.ReplaceAll(kind, "-", "_"))
		fallback := 1
		if kind == KindAttributes {
			fallback = 3
		}
		out[kind] = Number(prefix+"_"+name+"_CONCURRENCY", Number(name+"_CONCURRENCY", fallback))
	}
	out[KindNormalRecovery] = Number("NORMAL_RECOVERY_CONCURRENCY", 2)
	// The connectivity lane carries the sweeper's retirement evidence, the
	// recovery assessments and the cleanup role's second-verification probe. It
	// defaulted to the normal-lane count; naming it lets an operator raise the
	// verification rate without touching assessment admission.
	out[KindLive] = Number(prefix+"_LIVE_CONCURRENCY", Number("LIVE_CONCURRENCY", 2))
	out[KindCleanup] = Number("NODE_CLEANUP_CONCURRENCY", 2)
	return out
}

func ConfigFromEnv(role string) (Config, error) {
	c := Config{Role: role, NormalRecoveryInterval: Seconds("NORMAL_RECOVERY_INTERVAL_SECONDS", 1200), ResidentialInterval: Seconds("RESIDENTIAL_INTERVAL_SECONDS", 300), NormalInterval: Seconds("NORMAL_INTERVAL_SECONDS", 1800),
		Timeout: Seconds("PROBE_TIMEOUT_SECONDS", 12), RetryDelay: Seconds("PROBE_RETRY_SECONDS", 10),
		ExitProbeTimeout:    BoundedSeconds("EXIT_PROBE_TIMEOUT_SECONDS", defaultExitProbeTimeout, exitProbeTimeoutMin, exitProbeTimeoutMax),
		RiskRefreshInterval: Seconds("RISK_REFRESH_INTERVAL_SECONDS", 21600),
		ScheduleInterval:    Seconds("SCHEDULE_INTERVAL_SECONDS", 5),
		FastInterval:        Seconds("FAST_INTERVAL_SECONDS", 1200),
		HighConcurrency:     Number("RESIDENTIAL_CONCURRENCY", 2), NormalConcurrency: Number("NORMAL_CONCURRENCY", 2),
		LaneConcurrency: laneConcurrency(role),
		APIOrigin:       Env("IPPURE_API_ORIGIN", "https://api.123169.xyz"), DryRun: Env("LIFECYCLE_DRY_RUN", "false") == "true"}
	if address := os.Getenv("MIHOMO_MANAGER_URL"); address != "" {
		if os.Getenv("MIHOMO_MANAGER_TOKEN") == "" {
			return c, fmt.Errorf("MIHOMO_MANAGER_TOKEN is required")
		}
		c.Manager = &transport.ManagerClient{URL: address, Token: os.Getenv("MIHOMO_MANAGER_TOKEN")}
		kind := "strict"
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
	// Only exit-provision keeps a native mihomo fleet; every other manager-less
	// role waits for MIHOMO_MANAGER_URL (compose always provides it).
	if role != "exit-provision" {
		return c, nil
	}
	prefix := "IP_SERVICE"
	c.HighConcurrency = Number(prefix+"_HIGH_CONCURRENCY", c.HighConcurrency)
	c.NormalConcurrency = Number(prefix+"_NORMAL_CONCURRENCY", c.NormalConcurrency)
	seen := map[string]bool{}
	for _, lane := range []string{"HIGH", "NORMAL"} {
		base := prefix + "_" + lane
		controllers := strings.Split(os.Getenv(base+"_CONTROLLER"), ",")
		proxies := strings.Split(os.Getenv(base+"_PROXY_URL"), ",")
		if len(controllers) != len(proxies) {
			return c, fmt.Errorf("%s controller/proxy counts differ", base)
		}
		instances := []transport.MihomoConfig{}
		for i, controller := range controllers {
			controller = strings.TrimRight(strings.TrimSpace(controller), "/")
			proxy := strings.TrimSpace(proxies[i])
			if controller == "" || proxy == "" {
				return c, fmt.Errorf("%s_CONTROLLER and %s_PROXY_URL are required", base, base)
			}
			if seen[controller] {
				return c, fmt.Errorf("residential and normal lanes require different Mihomo controllers")
			}
			seen[controller] = true
			instances = append(instances, transport.MihomoConfig{Controller: controller, ProxyURL: proxy, Secret: os.Getenv(base + "_CONTROLLER_SECRET")})
		}
		engine := transport.NewMihomoEngine(instances, 15*time.Second)
		router := transport.NewRouter(transport.NewNativeEngine()).WithFallback(engine)
		if lane == "HIGH" {
			c.HighRouter = router
		} else {
			c.NormalRouter = router
		}
	}
	return c, nil
}
