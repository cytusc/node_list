package service

import (
	"testing"
	"time"
)

func TestProbeServicesHaveIndependentConcurrency(t *testing.T) {
	t.Setenv("MIHOMO_MANAGER_URL", "http://manager:9080")
	t.Setenv("MIHOMO_MANAGER_TOKEN", "test")
	t.Setenv("RESIDENTIAL_CONCURRENCY", "2")
	t.Setenv("NORMAL_CONCURRENCY", "2")
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
	if err != nil || exit.HighConcurrency != 8 {
		t.Fatalf("managed default: %d %v", exit.HighConcurrency, err)
	}
}

func TestExitProbeTimeoutConfig(t *testing.T) {
	t.Setenv("MIHOMO_MANAGER_URL", "http://manager:9080")
	t.Setenv("MIHOMO_MANAGER_TOKEN", "test")
	cases := []struct {
		name, value string
		want        time.Duration
	}{
		{"default", "", 8 * time.Second},
		{"below range", "1", 2 * time.Second},
		{"above range", "60", 30 * time.Second},
		{"override", "12", 12 * time.Second},
		{"invalid", "abc", 8 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("EXIT_PROBE_TIMEOUT_SECONDS", c.value)
			cfg, err := ConfigFromEnv("exit-provision")
			if err != nil || cfg.ExitProbeTimeout != c.want {
				t.Fatalf("exit probe timeout: %s %v", cfg.ExitProbeTimeout, err)
			}
		})
	}
}

func TestManagedConcurrencyHonorsConfiguration(t *testing.T) {
	t.Setenv("MIHOMO_MANAGER_URL", "http://manager:9080")
	t.Setenv("MIHOMO_MANAGER_TOKEN", "test")
	t.Setenv("MIHOMO_MANAGER_URL", "http://manager:9080")
	t.Setenv("MIHOMO_MANAGER_TOKEN", "test")
	t.Setenv("MIHOMO_EXIT_CONCURRENCY", "12")
	t.Setenv("IP_SERVICE_HIGH_CONCURRENCY", "3")
	t.Setenv("IP_SERVICE_NORMAL_CONCURRENCY", "9")
	t.Setenv("IP_SERVICE_RISK_CONCURRENCY", "1")
	t.Setenv("NORMAL_RECOVERY_CONCURRENCY", "4")
	cfg, err := ConfigFromEnv("exit-provision")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HighConcurrency != 3 || cfg.NormalConcurrency != 9 || cfg.ExitConcurrency != 12 {
		t.Fatalf("managed exit overrides lost: %+v", cfg)
	}
	cfg, err = ConfigFromEnv("exit-attributes")
	if err != nil || cfg.LaneConcurrency[KindRisk] != 1 {
		t.Fatalf("risk override: %+v %v", cfg, err)
	}

}

func TestWorkersRequireManager(t *testing.T) {
	t.Setenv("MIHOMO_MANAGER_URL", "")
	for _, role := range []string{"exit-provision", "exit-attributes", "exit-provision"} {
		if _, err := ConfigFromEnv(role); err == nil {
			t.Fatalf("%s accepted missing manager", role)
		}
	}
}
