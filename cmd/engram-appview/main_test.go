package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/haileyok/engram-garden/internal/control"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestOpenControl: a bucket needs a database; a directory (development)
// doesn't, and then the state is only in memory.
func TestOpenControl(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	if _, _, err := openControl(ctx, quiet(), "s3", ""); err == nil || !strings.Contains(err.Error(), "ENGRAM_DATABASE_URL is required") {
		t.Fatalf("s3 without a database: %v", err)
	}

	db, closeDB, err := openControl(ctx, quiet(), "dir", "")
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	if _, ok := db.(*control.Memory); !ok {
		t.Fatalf("directory storage without a database gave %T", db)
	}

	// A database that's configured but can't be used is an error, never a
	// quiet fall back to memory, and it doesn't echo the password.
	_, _, err = openControl(ctx, quiet(), "dir", "postgres://u:hunter2@127.0.0.1:1/db?connect_timeout=1")
	if err == nil {
		t.Fatal("opened an unreachable database")
	}
	if strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "ENGRAM_DATABASE_URL") {
		t.Fatalf("error %q should name the setting and keep the password out", err)
	}
}
