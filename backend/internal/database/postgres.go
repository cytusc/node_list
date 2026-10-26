package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config is the PostgreSQL connection configuration.
type Config struct {
	Host     string
	Port     int
	Database string
	User     string
	Password string
	PoolMax  int
}

// ConfigFromEnv reads the standard PostgreSQL environment variables.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Host:     os.Getenv("PGHOST"),
		Database: os.Getenv("PGDATABASE"),
		User:     os.Getenv("PGUSER"),
		Password: os.Getenv("PGPASSWORD"),
		Port:     5432,
		PoolMax:  0,
	}
	if cfg.Host == "" {
		return cfg, errors.New("PGHOST is not set")
	}
	if cfg.Database == "" || cfg.User == "" {
		return cfg, errors.New("PostgreSQL requires PGDATABASE, PGUSER and PGPASSWORD")
	}
	if v := os.Getenv("PGPORT"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &cfg.Port); err != nil || cfg.Port <= 0 {
			return cfg, fmt.Errorf("invalid PGPORT %q", v)
		}
	}
	if v := os.Getenv("PGPOOL_MAX"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &cfg.PoolMax); err != nil || cfg.PoolMax <= 0 {
			return cfg, fmt.Errorf("invalid PGPOOL_MAX %q", v)
		}
	}
	return cfg, nil
}

func (c Config) connString() string {
	return fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s",
		quoteConnValue(c.Host), c.Port, quoteConnValue(c.Database), quoteConnValue(c.User), quoteConnValue(c.Password))
}

func quoteConnValue(value string) string {
	return "'" + strings.NewReplacer("\\", "\\\\", "'", "\\'").Replace(value) + "'"
}

// Connect opens a pool without running migrations, for lightweight health checks.
func Connect(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.connString())
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}
	maxConns := int32(cfg.PoolMax)
	if maxConns <= 0 {
		maxConns = 6
	}
	poolCfg.MaxConns = maxConns
	poolCfg.MinConns = 1
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute
	// A hung statement must never hold a connection forever.
	poolCfg.ConnConfig.RuntimeParams["statement_timeout"] = "30000"
	poolCfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "60000"

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	return pool, nil
}
