package service

import (
	"testing"
	"time"
)

func TestProbeServicesHaveIndependentConcurrency(t *testing.T) {
	t.Setenv("RESIDENTIAL_CONCURRENCY", "2")
	t.Setenv("NORMAL_CONCURRENCY", "2")
	for _, prefix := range []string{"CONNECTIVITY", "IP_SERVICE"} {
		t.Setenv(prefix+"_HIGH_CONTROLLER", "http://high-1:9090,http://high-2:9090,http://high-3:9090,http://high-4:9090")
		t.Setenv(prefix+"_HIGH_PROXY_URL", "http://high-1:7890,http://high-2:7890,http://high-3:7890,http://high-4:7890")
		t.Setenv(prefix+"_NORMAL_CONTROLLER", "http://normal-1:9090,http://normal-2:9090,http://normal-3:9090,http://normal-4:9090")
		t.Setenv(prefix+"_NORMAL_PROXY_URL", "http://normal-1:7890,http://normal-2:7890,http://normal-3:7890,http://normal-4:7890")
	}
	t.Setenv("CONNECTIVITY_HIGH_CONCURRENCY", "4")
	t.Setenv("CONNECTIVITY_NORMAL_CONCURRENCY", "4")
	t.Setenv("IP_SERVICE_HIGH_CONCURRENCY", "3")
	t.Setenv("IP_SERVICE_NORMAL_CONCURRENCY", "4")
	live, err := ConfigFromEnv("connectivity")
	if err != nil {
		t.Fatal(err)
	}
	ip, err := ConfigFromEnv("ip")
	if err != nil {
		t.Fatal(err)
	}
	if live.HighConcurrency != 4 || live.NormalConcurrency != 4 || ip.HighConcurrency != 3 || ip.NormalConcurrency != 4 {
		t.Fatalf("role overrides lost: live %d/%d ip %d/%d", live.HighConcurrency, live.NormalConcurrency, ip.HighConcurrency, ip.NormalConcurrency)
	}
	t.Setenv("IP_SERVICE_HIGH_CONCURRENCY", "")
	ip, err = ConfigFromEnv("ip")
	if err != nil || ip.HighConcurrency != 2 {
		t.Fatalf("legacy fallback: %d %v", ip.HighConcurrency, err)
	}
}

func TestNormalRecoveryDefaultsAndIntervalOverride(t *testing.T) {
	t.Setenv("NORMAL_RECOVERY_INTERVAL_SECONDS", "")
	t.Setenv("NORMAL_RECOVERY_CONCURRENCY", "")
	cfg, err := ConfigFromEnv("normal-recovery")
	if err != nil || cfg.NormalRecoveryInterval != 20*time.Minute {
		t.Fatalf("normal default: %+v %v", cfg, err)
	}
	r := Runner{Config: cfg}
	if kinds := r.kinds(); len(kinds) != 1 || kinds[0] != KindNormalRecovery {
		t.Fatalf("wrong queue: %v", kinds)
	}
	if r.laneCount(KindNormalRecovery, false) != 2 || ServiceName(cfg.Role) != "normal-recovery" {
		t.Fatal("wrong concurrency or heartbeat")
	}
	t.Setenv("NORMAL_RECOVERY_INTERVAL_SECONDS", "43200")
	cfg, err = ConfigFromEnv("normal-recovery")
	if err != nil || cfg.NormalRecoveryInterval != 12*time.Hour {
		t.Fatalf("interval override: %+v %v", cfg, err)
	}
}

func TestNormalRecoveryConcurrencyOverride(t *testing.T) {
	for _, n := range []string{"1", "2", "4", "8"} {
		t.Run(n, func(t *testing.T) {
			t.Setenv("NORMAL_RECOVERY_CONCURRENCY", n)
			cfg, err := ConfigFromEnv("normal-recovery")
			if err != nil {
				t.Fatal(err)
			}
			r := Runner{Config: cfg}
			repo := Repository{NormalRecoveryConcurrency: r.laneCount(KindNormalRecovery, false)}
			if repo.normalRecoveryLimit() != Number("NORMAL_RECOVERY_CONCURRENCY", 2) {
				t.Fatal("consumer and repository limits differ")
			}
		})
	}
}
