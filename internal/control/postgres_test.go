package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests run against a real Postgres when ENGRAM_TEST_POSTGRES_URL is
// set (make test-postgres starts one in Docker). Each test works in its own
// schema and drops it afterwards, so tests don't see each other's tables and
// the database can be shared.

func postgresURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("ENGRAM_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("ENGRAM_TEST_POSTGRES_URL isn't set; see make test-postgres")
	}
	return url
}

// testSchema creates an empty schema and returns a pool config whose
// connections use it.
func testSchema(t *testing.T, url string) *pgxpool.Config {
	t.Helper()
	ctx := context.Background()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	schema := "test_" + hex.EncodeToString(b[:])
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connecting to ENGRAM_TEST_POSTGRES_URL: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.WithoutCancel(ctx), `DROP SCHEMA `+schema+` CASCADE`)
		_ = admin.Close(context.WithoutCancel(ctx))
	})
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	return cfg
}

func TestPostgres(t *testing.T) {
	t.Parallel()
	url := postgresURL(t)
	testStore(t, func(t *testing.T) Store {
		p, err := openPostgres(context.Background(), testSchema(t, url))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	})
}

// TestPostgresMigrations: opening the same database again doesn't redo
// migrations, keeps its data, and several nodes starting at once on an
// empty database all succeed.
func TestPostgresMigrations(t *testing.T) {
	t.Parallel()
	url := postgresURL(t)
	ctx := context.Background()
	cfg := testSchema(t, url)

	var wg sync.WaitGroup
	stores := make([]*Postgres, 5)
	errs := make([]error, len(stores))
	for i := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stores[i], errs[i] = openPostgres(ctx, cfg.Copy())
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("node %d: %v", i, err)
		}
		t.Cleanup(stores[i].Close)
	}

	if _, err := stores[0].Register(ctx, Registration{Space: "at://a", At: t0}); err != nil {
		t.Fatal(err)
	}
	again, err := openPostgres(ctx, cfg.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(again.Close)
	if regs, err := again.Registrations(ctx); err != nil || len(regs) != 1 {
		t.Fatalf("data after reopening: %+v %v", regs, err)
	}

	// Migrating leaves nothing locked: a later node must never wait on a
	// lock a pooled connection forgot to release.
	var free bool
	if err := again.pool.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, migrationLock).Scan(&free); err != nil || !free {
		t.Fatalf("the migration lock is still held after opening: free=%v %v", free, err)
	}
	_, _ = again.pool.Exec(ctx, `SELECT pg_advisory_unlock($1)`, migrationLock)

	want, err := migrations()
	if err != nil || len(want) == 0 {
		t.Fatalf("migrations: %v %v", want, err)
	}
	var n int
	if err := again.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != len(want) {
		t.Fatalf("%d migrations recorded, want %d: %v", n, len(want), err)
	}
}

// TestPostgresRejectsBadDatabase: a database that can't be reached is an
// error at startup, not at the first request.
func TestPostgresRejectsBadDatabase(t *testing.T) {
	t.Parallel()
	if _, err := OpenPostgres(context.Background(), "not a url"); err == nil {
		t.Fatal("opened a malformed URL")
	}
	// Nothing listens on port 1.
	if _, err := OpenPostgres(context.Background(), "postgres://u:p@127.0.0.1:1/db?connect_timeout=1"); err == nil {
		t.Fatal("opened an unreachable database")
	}
	// The error is logged at startup, so it must not carry the password.
	for _, u := range []string{
		"postgres://u:hunter2@host:notaport/db",
		"postgres://u:hunter2@127.0.0.1:1/db?sslmode=bogus",
		"postgres://u:hunter2@127.0.0.1:1/db?connect_timeout=1",
	} {
		if _, err := OpenPostgres(context.Background(), u); err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("%s: error %v should exist and keep the password out", u, err)
		}
	}
}
