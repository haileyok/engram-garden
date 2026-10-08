package blob

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/haileyok/engram-garden/internal/metrics"
)

var (
	blobRequests = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_blob_requests_total",
		Help: "Object storage calls, by operation and result (ok, not_found, exists, error).",
	}, []string{"op", "result"})
	blobDuration = metrics.Factory.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "engram_blob_request_duration_seconds",
		Help:    "Time for an object storage call, by operation. Get measures until the object starts arriving.",
		Buckets: metrics.DurationBuckets,
	}, []string{"op"})
	blobBytes = metrics.Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_blob_bytes_total",
		Help: "Bytes written to (put) and read by range from (get_range) object storage.",
	}, []string{"direction"})
)

// Instrumented measures the calls made to s.
func Instrumented(s Store) Store { return instrumented{s} }

type instrumented struct{ s Store }

func observe(op string, start time.Time, err error) {
	result := "ok"
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound):
		result = "not_found"
	case errors.Is(err, ErrExists):
		result = "exists"
	default:
		result = "error"
	}
	blobRequests.WithLabelValues(op, result).Inc()
	blobDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
}

func (i instrumented) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	start := time.Now()
	err := i.s.Put(ctx, key, r, size)
	observe("put", start, err)
	if err == nil {
		blobBytes.WithLabelValues("put").Add(float64(size))
	}
	return err
}

func (i instrumented) PutIfAbsent(ctx context.Context, key string, r io.Reader, size int64) error {
	start := time.Now()
	err := i.s.PutIfAbsent(ctx, key, r, size)
	observe("put_if_absent", start, err)
	if err == nil {
		blobBytes.WithLabelValues("put").Add(float64(size))
	}
	return err
}

func (i instrumented) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	start := time.Now()
	rc, err := i.s.Get(ctx, key)
	observe("get", start, err)
	return rc, err
}

func (i instrumented) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	start := time.Now()
	b, err := i.s.GetRange(ctx, key, off, n)
	observe("get_range", start, err)
	blobBytes.WithLabelValues("get_range").Add(float64(len(b)))
	return b, err
}

func (i instrumented) Size(ctx context.Context, key string) (int64, error) {
	start := time.Now()
	n, err := i.s.Size(ctx, key)
	observe("size", start, err)
	return n, err
}

func (i instrumented) List(ctx context.Context, prefix string) ([]Object, error) {
	start := time.Now()
	objs, err := i.s.List(ctx, prefix)
	observe("list", start, err)
	return objs, err
}

func (i instrumented) Delete(ctx context.Context, key string) error {
	start := time.Now()
	err := i.s.Delete(ctx, key)
	observe("delete", start, err)
	return err
}
