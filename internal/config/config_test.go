package config

import (
	"log/slog"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr bool
	}{
		{
			name: "defaults",
			env:  map[string]string{"CLOUTE_DATABASE_URL": "postgres://db"},
			want: Config{
				HTTPAddr:        ":8080",
				DatabaseURL:     "postgres://db",
				LogFormat:       LogFormatText,
				LogLevel:        slog.LevelInfo,
				ShutdownTimeout: 10 * time.Second,
			},
		},
		{
			name: "overrides",
			env: map[string]string{
				"CLOUTE_DATABASE_URL":     "postgres://db",
				"CLOUTE_COOKIE_SECURE":    "true",
				"CLOUTE_HTTP_ADDR":        "127.0.0.1:9090",
				"CLOUTE_LOG_FORMAT":       "json",
				"CLOUTE_LOG_LEVEL":        "debug",
				"CLOUTE_SHUTDOWN_TIMEOUT": "3s",
			},
			want: Config{
				HTTPAddr:        "127.0.0.1:9090",
				DatabaseURL:     "postgres://db",
				CookieSecure:    true,
				LogFormat:       LogFormatJSON,
				LogLevel:        slog.LevelDebug,
				ShutdownTimeout: 3 * time.Second,
			},
		},
		{
			name:    "missing database URL",
			env:     nil,
			wantErr: true,
		},
		{
			name:    "invalid cookie flag",
			env:     map[string]string{"CLOUTE_DATABASE_URL": "postgres://db", "CLOUTE_COOKIE_SECURE": "sometimes"},
			wantErr: true,
		},
		{
			name:    "invalid log format",
			env:     map[string]string{"CLOUTE_DATABASE_URL": "postgres://db", "CLOUTE_LOG_FORMAT": "xml"},
			wantErr: true,
		},
		{
			name:    "invalid log level",
			env:     map[string]string{"CLOUTE_DATABASE_URL": "postgres://db", "CLOUTE_LOG_LEVEL": "loud"},
			wantErr: true,
		},
		{
			name:    "invalid shutdown timeout",
			env:     map[string]string{"CLOUTE_DATABASE_URL": "postgres://db", "CLOUTE_SHUTDOWN_TIMEOUT": "ten seconds"},
			wantErr: true,
		},
		{
			name:    "non-positive shutdown timeout",
			env:     map[string]string{"CLOUTE_DATABASE_URL": "postgres://db", "CLOUTE_SHUTDOWN_TIMEOUT": "0s"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string { return tt.env[key] }

			got, err := Load(getenv)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() = %+v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Load() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
