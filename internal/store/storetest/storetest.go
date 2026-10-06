// Package storetest opens isolated stores for tests.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/haileyok/engram-garden/internal/store"
)

// EnvVar names the Postgres URL tests run against.
const EnvVar = "ENGRAM_TEST_DATABASE_URL"

// New opens a store in a fresh schema of $ENGRAM_TEST_DATABASE_URL, dropped
// when the test ends. It skips the test when the variable is unset.
func New(t testing.TB, dims int) *store.Store {
	t.Helper()
	dsn := os.Getenv(EnvVar)
	if dsn == "" {
		t.Skip(EnvVar + " not set")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Create the extension in the default schema, before any test schema
	// is first on the search path.
	if err := store.EnsureExtension(ctx, conn.Config()); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = conn.Close(context.Background())
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	s, err := store.OpenConfig(ctx, cfg, dims)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
