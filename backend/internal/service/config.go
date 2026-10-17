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
	Timeout, RetryDelay                 time.Duration
	ScheduleInterval                    time.Duration
	HighConcurrency, NormalConcurrency  int
	// LaneConcurrency overrides the consumer count per work kind. Slow lanes
	// (risk, attributes, lifecycle, recovery) default to one so raising a
	// count stays an explicit operational choice.
	LaneConcurrency          map[string]int
	APIOrigin                string
	HighRouter, NormalRouter *transport.Router
	DryRun                   bool
}

// laneConcurrency reads per-kind consumer counts. The role prefix wins over
// the generic name, mirroring the lane concurrency convention; both default
// to one consumer so probe-heavy lanes are unaffected unless configured.
func laneConcurrency(role string) map[string]int {
	prefix := "CONNECTIVITY"
	switch role {
	case "ip":
		prefix = "IP_SERVICE"
	case "residential":
		prefix = "RESIDENTIAL_RECOVERY"
	case "lifecycle":
		prefix = "LIFECYCLE"
	}
	out := map[string]int{}
	for _, kind := range []string{KindRisk, KindAttributes, KindLifecycle, KindRecovery} {
		name := strings.ToUpper(kind)
		out[kind] = Number(prefix+"_"+name+"_CONCURRENCY", Number(name+"_CONCURRENCY", 1))
	}
	return out
}

func ConfigFromEnv(role string) (Config, error) {
	c := Config{Role: role, ResidentialInterval: Seconds("RESIDENTIAL_INTERVAL_SECONDS", 300), NormalInterval: Seconds("NORMAL_INTERVAL_SECONDS", 1800),
		Timeout: Seconds("PROBE_TIMEOUT_SECONDS", 12), RetryDelay: Seconds("PROBE_RETRY_SECONDS", 10),
		ScheduleInterval: Seconds("SCHEDULE_INTERVAL_SECONDS", 5),
		HighConcurrency:  Number("RESIDENTIAL_CONCURRENCY", 2), NormalConcurrency: Number("NORMAL_CONCURRENCY", 2),
		LaneConcurrency: laneConcurrency(role),
		APIOrigin:       Env("IPPURE_API_ORIGIN", "https://api.123169.xyz"), DryRun: Env("LIFECYCLE_DRY_RUN", "false") == "true"}
	if role != "connectivity" && role != "ip" {
		return c, nil
	}
	prefix := "CONNECTIVITY"
	if role == "ip" {
		prefix = "IP_SERVICE"
	}
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
