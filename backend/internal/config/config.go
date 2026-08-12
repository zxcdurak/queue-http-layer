// Package config читает настройки из переменных окружения.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config — настройки сервиса.
type Config struct {
	DatabaseURL string
	ListenAddr  string

	GrantTTL        time.Duration
	ExpirerInterval time.Duration

	CORSOrigins []string

	RateLimitRPS   float64
	RateLimitBurst int

	ShutdownTimeout time.Duration
	LogLevel        slog.Level
}

// Load собирает конфигурацию из окружения и проверяет её.
func Load() (Config, error) {
	logLevel, err := parseLogLevel(env("LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}

	grantTTL, err := envSeconds("GRANT_TTL_SECONDS", 120)
	if err != nil {
		return Config{}, err
	}

	expirerInterval, err := envSeconds("EXPIRER_INTERVAL_SECONDS", 1)
	if err != nil {
		return Config{}, err
	}

	shutdownTimeout, err := envSeconds("SHUTDOWN_TIMEOUT_SECONDS", 10)
	if err != nil {
		return Config{}, err
	}

	rateLimitRPS, err := envFloat("RATE_LIMIT_RPS", 5)
	if err != nil {
		return Config{}, err
	}

	rateLimitBurst, err := envInt("RATE_LIMIT_BURST", 10)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		DatabaseURL:     env("DATABASE_URL", "postgres://app:app@localhost:55432/queue?sslmode=disable"),
		ListenAddr:      env("LISTEN_ADDR", ":8080"),
		GrantTTL:        grantTTL,
		ExpirerInterval: expirerInterval,
		CORSOrigins:     splitOrigins(env("CORS_ORIGINS", "http://localhost:5173")),
		RateLimitRPS:    rateLimitRPS,
		RateLimitBurst:  rateLimitBurst,
		ShutdownTimeout: shutdownTimeout,
		LogLevel:        logLevel,
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	switch {
	case c.DatabaseURL == "":
		return fmt.Errorf("DATABASE_URL не задан")
	case c.ListenAddr == "":
		return fmt.Errorf("LISTEN_ADDR не задан")
	case c.GrantTTL <= 0:
		return fmt.Errorf("GRANT_TTL_SECONDS должен быть больше нуля, получено %s", c.GrantTTL)
	case c.ExpirerInterval <= 0:
		return fmt.Errorf("EXPIRER_INTERVAL_SECONDS должен быть больше нуля, получено %s", c.ExpirerInterval)
	case c.RateLimitRPS <= 0:
		return fmt.Errorf("RATE_LIMIT_RPS должен быть больше нуля, получено %v", c.RateLimitRPS)
	case c.RateLimitBurst <= 0:
		return fmt.Errorf("RATE_LIMIT_BURST должен быть больше нуля, получено %d", c.RateLimitBurst)
	}
	return nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: ожидалось целое число, получено %q", key, raw)
	}
	return v, nil
}

func envFloat(key string, fallback float64) (float64, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: ожидалось число, получено %q", key, raw)
	}
	return v, nil
}

func envSeconds(key string, fallback int) (time.Duration, error) {
	v, err := envInt(key, fallback)
	if err != nil {
		return 0, err
	}
	return time.Duration(v) * time.Second, nil
}

func splitOrigins(raw string) []string {
	parts := strings.Split(raw, ",")
	origins := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			origins = append(origins, trimmed)
		}
	}
	return origins
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("LOG_LEVEL: ожидалось debug|info|warn|error, получено %q", raw)
	}
}
