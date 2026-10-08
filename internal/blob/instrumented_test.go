package blob

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestInstrumentedCountsResults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := Instrumented(Dir{Root: t.TempDir()})
	count := func(op, result string) float64 { return testutil.ToFloat64(blobRequests.WithLabelValues(op, result)) }

	puts, missing, exists := count("put", "ok"), count("get_range", "not_found"), count("put_if_absent", "exists")
	if err := PutBytes(ctx, s, "instrumented-test/a", []byte("hello"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRange(ctx, "instrumented-test/missing", 0, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing object: %v", err)
	}
	if err := PutBytes(ctx, s, "instrumented-test/a", []byte("x"), true); !errors.Is(err, ErrExists) {
		t.Fatalf("existing object: %v", err)
	}
	if count("put", "ok")-puts < 1 || count("get_range", "not_found")-missing < 1 || count("put_if_absent", "exists")-exists < 1 {
		t.Fatal("calls not counted by result")
	}
}
