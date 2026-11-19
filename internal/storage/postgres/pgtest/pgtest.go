package pgtest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"cloute/internal/storage/postgres"
)

const (
	DefaultAdminURL = "postgres://cloute:cloute@localhost:5434/cloute?sslmode=disable"
	testDatabase    = "cloute_test"
)

var (
	setupOnce sync.Once
	testURL   string
	setupErr  error
)

func URL(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("needs PostgreSQL (make db-up); skipped with -short")
	}
	setupOnce.Do(func() { testURL, setupErr = setup() })
	if setupErr != nil {
		t.Fatalf("test database: %v\nStart PostgreSQL with `make db-up`, set CLOUTE_TEST_DATABASE_URL, or run with -short.", setupErr)
	}
	return testURL
}

func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool, err := postgres.Connect(t.Context(), URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func DocID() string {
	return "test-" + rand.Text()
}

func Document(t testing.TB, pool *pgxpool.Pool) (docID string, owner int64) {
	t.Helper()
	store := postgres.NewAuthStore(pool)
	u, err := store.CreateUser(t.Context(), "owner-"+strings.ToLower(rand.Text())+"@example.test", "Owner", []byte("not a real hash"))
	if err != nil {
		t.Fatal(err)
	}
	docID = DocID()
	if err := store.CreateDocument(t.Context(), docID, "test", u.ID); err != nil {
		t.Fatal(err)
	}
	return docID, u.ID
}

func setup() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminURL := os.Getenv("CLOUTE_TEST_DATABASE_URL")
	if adminURL == "" {
		adminURL = DefaultAdminURL
	}
	cfg, err := pgx.ParseConfig(adminURL)
	if err != nil {
		return "", err
	}
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return "", err
	}
	defer func() { _ = admin.Close(ctx) }()

	// гонка тут не страшна
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+testDatabase); err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42P04" {
			return "", err
		}
	}

	dbURL, err := withDatabase(adminURL, testDatabase)
	if err != nil {
		return "", err
	}
	pool, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		return "", err
	}
	defer pool.Close()
	if _, err := postgres.Migrate(ctx, pool); err != nil {
		return "", err
	}
	return dbURL, nil
}

func withDatabase(rawURL, database string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("CLOUTE_TEST_DATABASE_URL must be a postgres:// URL, got scheme %q", u.Scheme)
	}
	u.Path = "/" + database
	return u.String(), nil
}
