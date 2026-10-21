package service

import (
	"testing"
	"time"
)

func TestProbeServicesHaveIndependentConcurrency(t *testing.T) {
	t.Setenv("RESIDENTIAL_CONCURRENCY", "2")
	t.Setenv("NORMAL_CONCURRENCY", "2")
	t.Setenv("IP_SERVICE_HIGH_CONTROLLER", "http://high-1:9090,http://high-2:9090,http://high-3:9090,http://high-4:9090")
	t.Setenv("IP_SERVICE_HIGH_PROXY_URL", "http://high-1:7890,http://high-2:7890,http://high-3:7890,http://high-4:7890")
	t.Setenv("IP_SERVICE_NORMAL_CONTROLLER", "http://normal-1:9090,http://normal-2:9090,http://normal-3:9090,http://normal-4:9090")
	t.Setenv("IP_SERVICE_NORMAL_PROXY_URL", "http://normal-1:7890,http://normal-2:7890,http://normal-3:7890,http://normal-4:7890")
	t.Setenv("IP_SERVICE_HIGH_CONCURRENCY", "3")
	t.Setenv("IP_SERVICE_NORMAL_CONCURRENCY", "4")
	exit, err := ConfigFromEnv("exit-provision")
	if err != nil {
		t.Fatal(err)
	}
	if exit.HighConcurrency != 3 || exit.NormalConcurrency != 4 {
		t.Fatalf("role overrides lost: exit %d/%d", exit.HighConcurrency, exit.NormalConcurrency)
	}
	t.Setenv("IP_SERVICE_HIGH_CONCURRENCY", "")
	exit, err = ConfigFromEnv("exit-provision")
	if err != nil || exit.HighConcurrency != 2 {
		t.Fatalf("legacy fallback: %d %v", exit.HighConcurrency, err)
	}
}

func TestRecoveryRoleDefaultsAndIntervalOverride(t *testing.T) {
	t.Setenv("NORMAL_RECOVERY_INTERVAL_SECONDS", "")
	t.Setenv("NORMAL_RECOVERY_CONCURRENCY", "")
	cfg, err := ConfigFromEnv("recovery")
	if err != nil || cfg.NormalRecoveryInterval != 20*time.Minute {
		t.Fatalf("normal default: %+v %v", cfg, err)
	}
	r := Runner{Config: cfg}
	want := []string{KindRecovery, KindNormalRecovery, KindFast, KindLive, KindLifecycle}
	if kinds := r.kinds(); len(kinds) != len(want) || kinds[0] != KindRecovery || kinds[1] != KindNormalRecovery {
		t.Fatalf("wrong queue: %v", kinds)
	}
	if r.laneCount(KindNormalRecovery, false) != 2 || ServiceName(cfg.Role) != "recovery" {
		t.Fatal("wrong concurrency or heartbeat")
	}
	t.Setenv("NORMAL_RECOVERY_INTERVAL_SECONDS", "43200")
	cfg, err = ConfigFromEnv("recovery")
	if err != nil || cfg.NormalRecoveryInterval != 12*time.Hour {
		t.Fatalf("interval override: %+v %v", cfg, err)
	}
}

func TestNormalRecoveryConcurrencyOverride(t *testing.T) {
	for _, n := range []string{"1", "2", "4", "8"} {
		t.Run(n, func(t *testing.T) {
			t.Setenv("NORMAL_RECOVERY_CONCURRENCY", n)
			cfg, err := ConfigFromEnv("recovery")
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
