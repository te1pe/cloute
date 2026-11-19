package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

const (
	LogFormatText = "text"
	LogFormatJSON = "json"
)

type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	CookieSecure    bool
	LogFormat       string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
}

func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		HTTPAddr:        ":8080",
		LogFormat:       LogFormatText,
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: 10 * time.Second,
	}

	if v := getenv("CLOUTE_HTTP_ADDR"); v != "" {
		cfg.HTTPAddr = v
	}

	cfg.DatabaseURL = getenv("CLOUTE_DATABASE_URL")
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("CLOUTE_DATABASE_URL is required, e.g. postgres://user:password@localhost:5432/cloute")
	}

	if v := getenv("CLOUTE_COOKIE_SECURE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("CLOUTE_COOKIE_SECURE: %w", err)
		}
		cfg.CookieSecure = b
	}

	if v := getenv("CLOUTE_LOG_FORMAT"); v != "" {
		if v != LogFormatText && v != LogFormatJSON {
			return Config{}, fmt.Errorf("CLOUTE_LOG_FORMAT: want %q or %q, got %q", LogFormatText, LogFormatJSON, v)
		}
		cfg.LogFormat = v
	}

	if v := getenv("CLOUTE_LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return Config{}, fmt.Errorf("CLOUTE_LOG_LEVEL: %w", err)
		}
	}

	if v := getenv("CLOUTE_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("CLOUTE_SHUTDOWN_TIMEOUT: %w", err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("CLOUTE_SHUTDOWN_TIMEOUT: must be positive, got %s", d)
		}
		cfg.ShutdownTimeout = d
	}

	return cfg, nil
}
