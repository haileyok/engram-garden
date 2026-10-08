// Package metrics holds Engram's Prometheus metrics: one registry for the
// process, the HTTP middleware that measures requests, and the listener
// that serves /metrics on its own address, apart from the public one.
//
// Packages declare their metrics with Factory. Metric names start with
// engram_; docs/monitoring.md lists them.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds every metric of the process, the Go runtime's and the
// process's included.
var Registry = prometheus.NewRegistry()

// Factory registers metrics with Registry.
var Factory = promauto.With(Registry)

// Buckets for durations from a millisecond to a minute.
var DurationBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}

// Buckets for durations from a tenth of a second to an hour.
var SlowBuckets = []float64{.1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600}

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	version, revision := "unknown", "unknown"
	if bi, ok := debug.ReadBuildInfo(); ok {
		version = bi.Main.Version
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				revision = s.Value
			}
		}
	}
	Factory.NewGaugeVec(prometheus.GaugeOpts{
		Name: "engram_build_info",
		Help: "Always 1; labels give the build's module version and VCS revision.",
	}, []string{"version", "revision"}).WithLabelValues(version, revision).Set(1)
}

// Handler serves the registry in the Prometheus text format.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{Registry: Registry})
}

// Serve serves /metrics on addr until ctx ends. An empty addr serves
// nothing. The metrics listener is separate from the public one so that
// only the monitoring network can reach it.
func Serve(ctx context.Context, addr string, log *slog.Logger) error {
	if addr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", Handler())
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	log.Info("serving metrics", "addr", ln.Addr().String(), "path", "/metrics")
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics listener failed", "err", err)
		}
	}()
	return nil
}

var (
	httpRequests = Factory.NewCounterVec(prometheus.CounterOpts{
		Name: "engram_http_requests_total",
		Help: "HTTP requests served, by route pattern, method and status code.",
	}, []string{"route", "method", "code"})
	httpDuration = Factory.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "engram_http_request_duration_seconds",
		Help:    "Time to serve an HTTP request, by route pattern.",
		Buckets: DurationBuckets,
	}, []string{"route"})
	httpInFlight = Factory.NewGauge(prometheus.GaugeOpts{
		Name: "engram_http_requests_in_flight",
		Help: "HTTP requests being served.",
	})
)

// Instrument measures the requests h serves. Requests are labeled with the
// ServeMux pattern that matched them ("GET /xrpc/garden.engram.searchMemories"),
// so labels stay bounded whatever the paths requested.
func Instrument(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpInFlight.Inc()
		defer httpInFlight.Dec()
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		code := int(rec.code.Load())
		if code == 0 {
			code = http.StatusOK
		}
		httpRequests.WithLabelValues(route, method(r.Method), strconv.Itoa(code)).Inc()
		httpDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
	})
}

// method keeps the method label to the standard methods.
func method(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions:
		return m
	}
	return "other"
}

// recorder notes the status code. It passes Flush on, for streamed
// responses, and Unwrap lets http.ResponseController reach the original.
type recorder struct {
	http.ResponseWriter
	code atomic.Int32
}

func (r *recorder) WriteHeader(code int) {
	r.code.CompareAndSwap(0, int32(code))
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.code.CompareAndSwap(0, http.StatusOK)
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Flush() {
	r.code.CompareAndSwap(0, http.StatusOK)
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Result labels an outcome: "ok", or "error".
func Result(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}
