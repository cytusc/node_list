package runtimeenv

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
