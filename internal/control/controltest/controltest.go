// Package controltest gives tests a control-plane store.
package controltest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/haileyok/engram-garden/internal/control"
)

// New returns an empty store for a test. By default it is in memory. When
// ENGRAM_TEST_POSTGRES_URL is set (a postgres:// URL; make test-postgres
// sets it), it is a real Postgres: the test gets its own schema, dropped
// when the test ends, so tests don't see each other's data and the database
// can be shared.
//
// Many tests run at once, and Postgres allows only so many connections, so
// each store keeps at most two, and the connection that creates and drops
// the schema is open only while it does.
func New(t testing.TB) control.Store {
	t.Helper()
	raw := os.Getenv("ENGRAM_TEST_POSTGRES_URL")
	if raw == "" {
		return control.NewMemory()
	}
	ctx := context.Background()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	schema := "test_" + hex.EncodeToString(b[:])

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("ENGRAM_TEST_POSTGRES_URL must be a postgres:// URL: %v", err)
	}
	if err := exec(ctx, raw, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("connecting to ENGRAM_TEST_POSTGRES_URL: %v", err)
	}
	// Cleanups run last-registered first: the store closes before its
	// schema is dropped.
	t.Cleanup(func() {
		if err := exec(context.WithoutCancel(ctx), raw, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("dropping the test schema %s: %v", schema, err)
		}
	})

	q := u.Query()
	q.Set("search_path", schema)
	q.Set("pool_max_conns", "2")
	u.RawQuery = q.Encode()
	p, err := control.OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// exec runs one statement on a connection of its own.
func exec(ctx context.Context, url, sql string) error {
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer c.Close(context.WithoutCancel(ctx))
	_, err = c.Exec(ctx, sql)
	return err
}
